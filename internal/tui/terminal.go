package tui

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"
)

const (
	minTerminalWidth  = 40
	minTerminalHeight = 8
)

// Terminal isolates the process TTY operations used by the switch selector.
// Tests provide a fake implementation; production uses XTerm.
type Terminal interface {
	IsTerminal(f *os.File) bool
	Size(f *os.File) (width int, height int, err error)
	MakeRaw(f *os.File) (*term.State, error)
	Restore(f *os.File, state *term.State) error
	Env(name string) string
}

// XTerm is the production Terminal implementation backed by x/term.
type XTerm struct{}

func (XTerm) IsTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

func (XTerm) Size(f *os.File) (int, int, error) {
	if f == nil {
		return 0, 0, fmt.Errorf("terminal file is nil")
	}
	return term.GetSize(int(f.Fd()))
}

func (XTerm) MakeRaw(f *os.File) (*term.State, error) {
	if f == nil {
		return nil, fmt.Errorf("terminal file is nil")
	}
	return term.MakeRaw(int(f.Fd()))
}

func (XTerm) Restore(f *os.File, state *term.State) error {
	if f == nil {
		return fmt.Errorf("terminal file is nil")
	}
	if state == nil {
		return nil
	}
	return term.Restore(int(f.Fd()), state)
}

func (XTerm) Env(name string) string {
	return os.Getenv(name)
}

// CanOpenSelector reports whether bare switch may attempt the interactive
// selector. It deliberately checks both stdin and stdout.
func CanOpenSelector(t Terminal, stdin, stdout *os.File) bool {
	if t == nil {
		return false
	}
	return t.IsTerminal(stdin) && t.IsTerminal(stdout)
}

// CheckCapabilities validates terminal properties before raw mode is entered.
// A failure here should fall back to non-interactive usage output.
func CheckCapabilities(t Terminal, stdout *os.File) error {
	if t == nil {
		return fmt.Errorf("interactive selector unavailable: terminal probe is not configured")
	}
	if strings.EqualFold(strings.TrimSpace(t.Env("TERM")), "dumb") {
		return fmt.Errorf("interactive selector unavailable: TERM=dumb does not support cursor controls")
	}
	width, height, err := t.Size(stdout)
	if err != nil {
		return fmt.Errorf("interactive selector unavailable: terminal size could not be detected: %w", err)
	}
	if width < minTerminalWidth || height < minTerminalHeight {
		return fmt.Errorf("interactive selector unavailable: terminal size %dx%d is too small", width, height)
	}
	return nil
}
