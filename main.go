package main

import (
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"
)

const (
	esc           = 0x1b
	ctrlZ         = 0x1a
	ctrlBackslash = 0x1c
	escTimeout    = 50 * time.Millisecond
)

// inputEvent represents either data or a control signal
type inputEvent struct {
	data    []byte
	suspend bool
	quit    bool
}

// inputFilter handles filtering of focus events and detection of control chars
// It uses a timeout to distinguish standalone ESC from escape sequences
type inputFilter struct {
	output    chan<- inputEvent
	pending   []byte // buffered bytes when in escape sequence
	timer     *time.Timer
	timerChan <-chan time.Time
}

func newInputFilter(output chan<- inputEvent) *inputFilter {
	return &inputFilter{
		output: output,
	}
}

func (f *inputFilter) processByte(b byte) {
	// Check for control characters first
	if b == ctrlZ {
		f.flush()
		f.output <- inputEvent{suspend: true}
		return
	}
	if b == ctrlBackslash {
		f.flush()
		f.output <- inputEvent{quit: true}
		return
	}

	if len(f.pending) == 0 {
		// Not in an escape sequence
		if b == esc {
			f.pending = append(f.pending, b)
			f.startTimer()
		} else {
			f.output <- inputEvent{data: []byte{b}}
		}
		return
	}

	// We have pending bytes (started with ESC)
	f.stopTimer()

	if len(f.pending) == 1 && f.pending[0] == esc {
		// We have just ESC pending
		if b == '[' {
			f.pending = append(f.pending, b)
			f.startTimer()
		} else if b == esc {
			// Another ESC - emit first one, keep second pending
			f.output <- inputEvent{data: []byte{esc}}
			f.pending = []byte{esc}
			f.startTimer()
		} else {
			// ESC followed by something else - emit both
			f.output <- inputEvent{data: []byte{esc, b}}
			f.pending = nil
		}
		return
	}

	if len(f.pending) == 2 && f.pending[0] == esc && f.pending[1] == '[' {
		// We have ESC[ pending
		if b == 'I' || b == 'O' {
			// Focus event - swallow it entirely
			f.pending = nil
		} else if b == esc {
			// New escape sequence starting - emit ESC[ first
			f.output <- inputEvent{data: []byte{esc, '['}}
			f.pending = []byte{esc}
			f.startTimer()
		} else {
			// Some other CSI sequence - emit all
			f.output <- inputEvent{data: []byte{esc, '[', b}}
			f.pending = nil
		}
		return
	}
}

func (f *inputFilter) flush() {
	f.stopTimer()
	if len(f.pending) > 0 {
		f.output <- inputEvent{data: f.pending}
		f.pending = nil
	}
}

func (f *inputFilter) startTimer() {
	f.timer = time.NewTimer(escTimeout)
	f.timerChan = f.timer.C
}

func (f *inputFilter) stopTimer() {
	if f.timer != nil {
		f.timer.Stop()
		f.timer = nil
		f.timerChan = nil
	}
}

func (f *inputFilter) timeout() <-chan time.Time {
	return f.timerChan
}

func (f *inputFilter) handleTimeout() {
	f.flush()
}

func main() {
	// Target command: real Claude Code CLI
	// Change this to whatever you actually use.
	target := os.Getenv("CLAUDE_CODE_BIN")
	if target == "" {
		target = "claude"
	}

	cmd := exec.Command(target, os.Args[1:]...)

	// Start the child under a PTY
	ptmx, err := pty.Start(cmd)
	if err != nil {
		log.Fatalf("failed to start PTY: %v", err)
	}
	defer func() { _ = ptmx.Close() }()

	// Handle window resizing
	if err := pty.InheritSize(os.Stdin, ptmx); err != nil {
		// Not fatal; just log
		log.Printf("warning: could not inherit size: %v", err)
	}
	resizeCh := make(chan os.Signal, 1)
	signal.Notify(resizeCh, syscall.SIGWINCH)
	go func() {
		for range resizeCh {
			_ = pty.InheritSize(os.Stdin, ptmx)
		}
	}()

	// Save original terminal state before going raw
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		log.Fatalf("failed to set raw mode: %v", err)
	}
	defer func() {
		_ = term.Restore(int(os.Stdin.Fd()), oldState)
	}()

	// Forward signals to the child process
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	go func() {
		for sig := range sigCh {
			if cmd.Process != nil {
				_ = cmd.Process.Signal(sig)
			}
		}
	}()

	// Wait for child process in a goroutine
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	// Copy child output -> our stdout
	go func() {
		_, _ = io.Copy(os.Stdout, ptmx)
	}()

	// Channel for filtered input events
	inputCh := make(chan inputEvent, 16)
	filter := newInputFilter(inputCh)

	// Channel for raw stdin data
	stdinCh := make(chan []byte, 16)
	stdinErrCh := make(chan error, 1)

	// Read from stdin in a goroutine
	go func() {
		buf := make([]byte, 1024)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				data := make([]byte, n)
				copy(data, buf[:n])
				stdinCh <- data
			}
			if err != nil {
				stdinErrCh <- err
				return
			}
		}
	}()

	// Main loop: coordinate input filtering, timeouts, and control signals
	for {
		select {
		case <-done:
			return

		case data := <-stdinCh:
			for _, b := range data {
				filter.processByte(b)
			}

		case <-filter.timeout():
			filter.handleTimeout()

		case ev := <-inputCh:
			if ev.suspend {
				// Restore terminal before suspending
				_ = term.Restore(int(os.Stdin.Fd()), oldState)
				// Stop the signal handler temporarily so SIGTSTP works
				signal.Reset(syscall.SIGTSTP)
				// Send SIGTSTP to our own process group
				_ = syscall.Kill(0, syscall.SIGTSTP)
				// When resumed, put terminal back into raw mode
				_, _ = term.MakeRaw(int(os.Stdin.Fd()))
			} else if ev.quit {
				// Restore terminal and quit
				_ = term.Restore(int(os.Stdin.Fd()), oldState)
				// Kill child process
				if cmd.Process != nil {
					_ = cmd.Process.Kill()
				}
				return
			} else if len(ev.data) > 0 {
				_, _ = ptmx.Write(ev.data)
			}

		case <-stdinErrCh:
			return
		}
	}
}
