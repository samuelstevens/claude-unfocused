# claude-unfocused

A lightweight wrapper for Claude Code that filters out tmux focus events (`ESC[I` and `ESC[O`), which can interfere with Claude's TUI when switching tmux panes.

## Features

- Strips tmux focus events from input
- Preserves standalone ESC keypresses (for vim mode switching)
- Handles Ctrl-C, Ctrl-Z, and Ctrl-\ correctly
- Passes through all other input/output transparently

## Install

```sh
go install github.com/samstevens/claude-unfocused@latest
```

## Usage

```sh
# Use claude from PATH
claude-unfocused

# Use a specific claude binary
claude-unfocused /path/to/claude

# Pass arguments to claude
claude-unfocused --resume
claude-unfocused /path/to/claude --help
```

## Shell Aliases

### Fish

Add to `~/.config/fish/config.fish`:

```fish
alias claude="claude-unfocused"
```

Or to use a specific binary:

```fish
alias claude="claude-unfocused /path/to/claude"
```

### Bash

Add to `~/.bashrc`:

```bash
alias claude="claude-unfocused"
```

Or to use a specific binary:

```bash
alias claude="claude-unfocused /path/to/claude"
```
