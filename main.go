package main

import (
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/spf13/pflag"
	"golang.org/x/term"
)

const (
	esc           = 0x1b
	ctrlZ         = 0x1a
	ctrlBackslash = 0x1c
	escTimeout    = 50 * time.Millisecond
)

type controlSignal int

const (
	sigNone controlSignal = iota
	sigSuspend
	sigQuit
)

func main() {
	// Use custom FlagSet to avoid automatic --help handling (let it pass through to claude)
	fs := pflag.NewFlagSet("claude-unfocused", pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.ParseErrorsWhitelist.UnknownFlags = true
	target := fs.String("claude", "claude", "path to claude binary")
	_ = fs.Parse(os.Args[1:])

	// Collect args to pass through (pflag drops unknown flags, so reconstruct manually)
	args := passthroughArgs(os.Args[1:])

	cmd := exec.Command(*target, args...)

	ptmx, err := pty.Start(cmd)
	if err != nil {
		log.Fatalf("failed to start PTY: %v", err)
	}
	defer func() { _ = ptmx.Close() }()

	// Handle window resizing
	if err := pty.InheritSize(os.Stdin, ptmx); err != nil {
		log.Printf("warning: could not inherit size: %v", err)
	}
	resizeCh := make(chan os.Signal, 1)
	signal.Notify(resizeCh, syscall.SIGWINCH)
	go func() {
		for range resizeCh {
			_ = pty.InheritSize(os.Stdin, ptmx)
		}
	}()

	// Raw mode
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		log.Fatalf("failed to set raw mode: %v", err)
	}
	defer func() { _ = term.Restore(int(os.Stdin.Fd()), oldState) }()

	// Forward signals to child
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	go func() {
		for sig := range sigCh {
			if cmd.Process != nil {
				_ = cmd.Process.Signal(sig)
			}
		}
	}()

	// Wait for child in background
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()

	// Copy child output to stdout
	go func() {
		_, _ = io.Copy(os.Stdout, ptmx)
	}()

	// Control signal channel from input processor
	ctrlCh := make(chan controlSignal, 1)

	// Process input: filter focus events, detect control chars, handle ESC timeout
	go func() {
		var pending []byte
		buf := make([]byte, 1024)

		// Use a pipe to make stdin reads interruptible by timeout
		stdinData := make(chan []byte)
		go func() {
			for {
				n, err := os.Stdin.Read(buf)
				if err != nil {
					close(stdinData)
					return
				}
				if n > 0 {
					data := make([]byte, n)
					copy(data, buf[:n])
					stdinData <- data
				}
			}
		}()

		var timerCh <-chan time.Time
		for {
			select {
			case <-done:
				return
			case <-timerCh:
				if len(pending) > 0 {
					_, _ = ptmx.Write(pending)
					pending = nil
				}
				timerCh = nil
			case data, ok := <-stdinData:
				if !ok {
					return
				}
				for _, b := range data {
					// Control characters
					if b == ctrlZ {
						if len(pending) > 0 {
							_, _ = ptmx.Write(pending)
							pending = nil
						}
						timerCh = nil
						ctrlCh <- sigSuspend
						continue
					}
					if b == ctrlBackslash {
						if len(pending) > 0 {
							_, _ = ptmx.Write(pending)
							pending = nil
						}
						timerCh = nil
						ctrlCh <- sigQuit
						continue
					}

					// State machine for ESC sequence filtering
					if len(pending) == 0 {
						if b == esc {
							pending = []byte{esc}
							timerCh = time.After(escTimeout)
						} else {
							_, _ = ptmx.Write([]byte{b})
						}
					} else if len(pending) == 1 {
						// Have ESC pending
						if b == '[' {
							pending = append(pending, '[')
							timerCh = time.After(escTimeout)
						} else if b == esc {
							_, _ = ptmx.Write([]byte{esc})
							pending = []byte{esc}
							timerCh = time.After(escTimeout)
						} else {
							_, _ = ptmx.Write([]byte{esc, b})
							pending = nil
							timerCh = nil
						}
					} else {
						// Have ESC[ pending
						if b == 'I' || b == 'O' {
							// Swallow focus event
							pending = nil
							timerCh = nil
						} else if b == esc {
							_, _ = ptmx.Write([]byte{esc, '['})
							pending = []byte{esc}
							timerCh = time.After(escTimeout)
						} else {
							_, _ = ptmx.Write([]byte{esc, '[', b})
							pending = nil
							timerCh = nil
						}
					}
				}
			}
		}
	}()

	// Main loop: wait for exit or control signals
	for {
		select {
		case <-done:
			return
		case sig := <-ctrlCh:
			switch sig {
			case sigSuspend:
				_ = term.Restore(int(os.Stdin.Fd()), oldState)
				signal.Reset(syscall.SIGTSTP)
				_ = syscall.Kill(0, syscall.SIGTSTP)
				_, _ = term.MakeRaw(int(os.Stdin.Fd()))
			case sigQuit:
				_ = term.Restore(int(os.Stdin.Fd()), oldState)
				if cmd.Process != nil {
					_ = cmd.Process.Kill()
				}
				return
			}
		}
	}
}

// passthroughArgs returns all args except --claude and its value
func passthroughArgs(rawArgs []string) []string {
	var args []string
	for i := 0; i < len(rawArgs); i++ {
		arg := rawArgs[i]
		switch {
		case arg == "--claude" && i+1 < len(rawArgs):
			i++ // skip value
		case strings.HasPrefix(arg, "--claude="):
			// skip
		default:
			args = append(args, arg)
		}
	}
	return args
}
