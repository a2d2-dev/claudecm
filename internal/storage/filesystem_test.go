package storage

import (
	"errors"
	"os"
	"testing"

	"github.com/a2d2-dev/claudecm/internal/config"
)

// TestSaveProfile_RefusesWithoutBootstrap documents the write-path invariant:
// SaveProfile refuses when `~/.claudecm/` does not exist. This is the
// enforcement side of removing Resolver.EnsureConfigDir — a caller who
// forgot to run storage.Bootstrap must fail loudly at the first write,
// not silently create a dir at whatever the process umask allows.
func TestSaveProfile_RefusesWithoutBootstrap(t *testing.T) {
	home := t.TempDir()
	r := mustResolver(t, home)
	// Deliberately NO Bootstrap call.

	// Sanity-check the premise: no .claudecm/ tree yet.
	if _, err := os.Stat(r.ConfigDir()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("premise violated: %s already exists (err=%v)", r.ConfigDir(), err)
	}

	fs := NewFileStorage(r)
	err := fs.SaveProfile(&config.Profile{
		Name: "example",
	})
	if err == nil {
		t.Fatal("SaveProfile without Bootstrap = nil; want ErrNotBootstrapped")
	}
	if !errors.Is(err, ErrNotBootstrapped) {
		t.Fatalf("SaveProfile err = %v; want ErrNotBootstrapped", err)
	}

	// The refusal must not have created the tree as a side effect —
	// otherwise the next call would silently succeed on a dir with the
	// wrong mode, which is exactly the footgun we're preventing.
	if _, err := os.Stat(r.ConfigDir()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("SaveProfile created %s despite refusal: err=%v", r.ConfigDir(), err)
	}
}

// TestSaveState_RefusesWithoutBootstrap mirrors TestSaveProfile_RefusesWithoutBootstrap
// for the state.yaml write path. Both funnel through the same Bootstrap
// invariant check.
func TestSaveState_RefusesWithoutBootstrap(t *testing.T) {
	home := t.TempDir()
	r := mustResolver(t, home)

	fs := NewFileStorage(r)
	err := fs.SaveState(config.NewState())
	if err == nil {
		t.Fatal("SaveState without Bootstrap = nil; want ErrNotBootstrapped")
	}
	if !errors.Is(err, ErrNotBootstrapped) {
		t.Fatalf("SaveState err = %v; want ErrNotBootstrapped", err)
	}
	if _, err := os.Stat(r.ConfigDir()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("SaveState created %s despite refusal: err=%v", r.ConfigDir(), err)
	}
}
