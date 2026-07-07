package tui

import (
	"errors"
	"os"
	"strings"
	"testing"

	"golang.org/x/term"
)

type fakeTerminal struct {
	tty        bool
	width      int
	height     int
	sizeErr    error
	restoreErr error
	env        map[string]string
}

func (f fakeTerminal) IsTerminal(*os.File) bool { return f.tty }

func (f fakeTerminal) Size(*os.File) (int, int, error) {
	if f.sizeErr != nil {
		return 0, 0, f.sizeErr
	}
	return f.width, f.height, nil
}

func (f fakeTerminal) MakeRaw(*os.File) (*term.State, error) { return nil, nil }

func (f fakeTerminal) Restore(*os.File, *term.State) error { return f.restoreErr }

func (f fakeTerminal) Env(name string) string { return f.env[name] }

func TestCanOpenSelectorRequiresBothTTYs(t *testing.T) {
	if CanOpenSelector(nil, os.Stdin, os.Stdout) {
		t.Fatal("nil terminal opened selector")
	}
	if !CanOpenSelector(fakeTerminal{tty: true}, os.Stdin, os.Stdout) {
		t.Fatal("both TTYs should open selector")
	}
	if CanOpenSelector(fakeTerminal{tty: false}, os.Stdin, os.Stdout) {
		t.Fatal("non-TTY should not open selector")
	}
}

func TestCheckCapabilities(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		err := CheckCapabilities(fakeTerminal{width: 80, height: 24}, os.Stdout)
		if err != nil {
			t.Fatalf("CheckCapabilities ok err=%v", err)
		}
	})
	t.Run("dumb", func(t *testing.T) {
		err := CheckCapabilities(fakeTerminal{width: 80, height: 24, env: map[string]string{"TERM": "dumb"}}, os.Stdout)
		if err == nil || !strings.Contains(err.Error(), "TERM=dumb") {
			t.Fatalf("CheckCapabilities dumb err=%v", err)
		}
	})
	t.Run("no size", func(t *testing.T) {
		err := CheckCapabilities(fakeTerminal{sizeErr: errors.New("no tty")}, os.Stdout)
		if err == nil || !strings.Contains(err.Error(), "terminal size could not be detected") {
			t.Fatalf("CheckCapabilities size err=%v", err)
		}
	})
	t.Run("too small", func(t *testing.T) {
		err := CheckCapabilities(fakeTerminal{width: 20, height: 4}, os.Stdout)
		if err == nil || !strings.Contains(err.Error(), "too small") {
			t.Fatalf("CheckCapabilities small err=%v", err)
		}
	})
}
