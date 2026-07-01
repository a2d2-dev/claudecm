package storage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/a2d2-dev/claudecm/internal/config"
	"gopkg.in/yaml.v3"
)

// ErrNotBootstrapped is returned by SaveProfile / SaveState when the
// `~/.claudecm/` tree does not yet exist. The write path deliberately does
// NOT lazily create the dir — Bootstrap owns that responsibility (and owns
// the mode-0700 enforcement). A lazy mkdir here would silently produce a
// dir at whatever the process umask allows, violating NFR-S4. Refusing
// loudly makes a forgotten Bootstrap a fast failure at the cmd/* wiring
// layer instead of a mode drift in production.
var ErrNotBootstrapped = errors.New("claudecm: storage not bootstrapped; call storage.Bootstrap first")

// All filesystem paths in this file are constructed through *Resolver
// (paths.go). Do NOT reintroduce inline filepath.Join over user input here —
// that is a coding-standards rule 3 violation (see
// docs/architecture/coding-standards.md).

// Storage defines the interface for profile and state persistence
type Storage interface {
	// SaveProfile writes a profile to disk
	SaveProfile(profile *config.Profile) error

	// LoadProfile reads a profile from disk
	LoadProfile(name string) (*config.Profile, error)

	// LoadAllProfiles reads all profiles from disk
	LoadAllProfiles() ([]*config.Profile, error)

	// DeleteProfile removes a profile file from disk
	DeleteProfile(name string) error

	// ProfileExists checks if a profile file exists
	ProfileExists(name string) (bool, error)

	// SaveState writes the state file
	SaveState(state *config.State) error

	// LoadState reads the state file
	LoadState() (*config.State, error)
}

// FileStorage implements Storage using the local filesystem. It routes every
// path through the injected *Resolver — the only source of HOME truth.
type FileStorage struct {
	r *Resolver
}

// NewFileStorage creates a new FileStorage bound to the given Resolver.
// Callers construct the Resolver once at the entry point (see storage.Default)
// and pass it downstream.
func NewFileStorage(r *Resolver) *FileStorage {
	return &FileStorage{r: r}
}

// SaveProfile writes a profile to disk atomically. It requires Bootstrap to
// have been called; the write path does not lazily create `~/.claudecm/`.
func (fs *FileStorage) SaveProfile(profile *config.Profile) error {
	if profile == nil {
		return fmt.Errorf("profile cannot be nil")
	}

	// ProfilePath validates the profile name (NFR-S5), refuses traversal,
	// and guarantees the result stays under the resolved HOME. Callers do
	// not need to re-check.
	profilePath, err := fs.r.ProfilePath(profile.Name)
	if err != nil {
		return fmt.Errorf("failed to get profile path: %w", err)
	}

	// Assert the Bootstrap invariant instead of repairing it. A missing
	// parent means someone forgot to call storage.Bootstrap at the cmd/*
	// entry point — repair silently would swallow the bug and produce a
	// dir at the process umask instead of 0700.
	if _, err := os.Stat(filepath.Dir(profilePath)); errors.Is(err, os.ErrNotExist) {
		return ErrNotBootstrapped
	}

	// Marshal to YAML through the schema gateway so schema_version is always
	// stamped on disk (NFR-M1 / NFR-S4).
	data, err := config.MarshalProfile(profile)
	if err != nil {
		return fmt.Errorf("failed to marshal profile: %w", err)
	}

	// Route through the single atomic-write primitive (E1-S3). All fsync +
	// rename + mode re-assertion logic lives there.
	if _, err := AtomicWrite(fs.r, profilePath, data, AtomicWriteOptions{Mode: 0600}); err != nil {
		return fmt.Errorf("failed to write profile: %w", err)
	}

	return nil
}

// LoadProfile reads a profile from disk
func (fs *FileStorage) LoadProfile(name string) (*config.Profile, error) {
	if name == "" {
		return nil, fmt.Errorf("profile name cannot be empty")
	}

	profilePath, err := fs.r.ProfilePath(name)
	if err != nil {
		return nil, fmt.Errorf("failed to get profile path: %w", err)
	}

	data, err := os.ReadFile(profilePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("profile %q not found", name)
		}
		return nil, fmt.Errorf("failed to read profile file: %w", err)
	}

	// Route through the schema gateway: this enforces schema_version policy
	// (refuse on v2+, migrate on legacy v0) and never returns a partially
	// populated profile (per E1-S1 AC).
	profile, err := config.ParseProfile(data)
	if err != nil {
		return nil, fmt.Errorf("failed to parse profile %q: %w", name, err)
	}

	return profile, nil
}

// LoadAllProfiles reads all profiles from disk
func (fs *FileStorage) LoadAllProfiles() ([]*config.Profile, error) {
	profilesDir := fs.r.ProfilesDir()

	// Check if profiles directory exists
	if _, err := os.Stat(profilesDir); os.IsNotExist(err) {
		return []*config.Profile{}, nil // Return empty slice if directory doesn't exist
	}

	entries, err := os.ReadDir(profilesDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read profiles directory: %w", err)
	}

	var profiles []*config.Profile
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}

		// Extract profile name from filename
		profileName := strings.TrimSuffix(entry.Name(), ".yaml")
		profile, err := fs.LoadProfile(profileName)
		if err != nil {
			// Loud skip: surface the reason so a user who dropped a file
			// with a bad name (NFR-S5) or invalid contents actually sees
			// why it was ignored. Return value is unchanged — LoadAll
			// still succeeds so a single bad file does not blank the list.
			fmt.Fprintf(os.Stderr, "claudecm: skipping %s: %v\n", entry.Name(), err)
			continue
		}

		profiles = append(profiles, profile)
	}

	return profiles, nil
}

// DeleteProfile removes a profile file from disk
func (fs *FileStorage) DeleteProfile(name string) error {
	if name == "" {
		return fmt.Errorf("profile name cannot be empty")
	}

	profilePath, err := fs.r.ProfilePath(name)
	if err != nil {
		return fmt.Errorf("failed to get profile path: %w", err)
	}

	if err := os.Remove(profilePath); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("profile %q not found", name)
		}
		return fmt.Errorf("failed to delete profile: %w", err)
	}

	return nil
}

// ProfileExists checks if a profile file exists
func (fs *FileStorage) ProfileExists(name string) (bool, error) {
	profilePath, err := fs.r.ProfilePath(name)
	if err != nil {
		return false, err
	}

	_, err = os.Stat(profilePath)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// SaveState writes the state file atomically. It requires Bootstrap to have
// been called; the write path does not lazily create `~/.claudecm/`.
func (fs *FileStorage) SaveState(state *config.State) error {
	if state == nil {
		return fmt.Errorf("state cannot be nil")
	}

	statePath, err := fs.r.StatePath()
	if err != nil {
		return fmt.Errorf("failed to get state path: %w", err)
	}

	// Assert the Bootstrap invariant instead of repairing it (see SaveProfile).
	if _, err := os.Stat(filepath.Dir(statePath)); errors.Is(err, os.ErrNotExist) {
		return ErrNotBootstrapped
	}

	// Marshal to YAML
	data, err := yaml.Marshal(state)
	if err != nil {
		return fmt.Errorf("failed to marshal state: %w", err)
	}

	// Route through the atomic-write primitive (E1-S3).
	if _, err := AtomicWrite(fs.r, statePath, data, AtomicWriteOptions{Mode: 0600}); err != nil {
		return fmt.Errorf("failed to write state: %w", err)
	}

	return nil
}

// LoadState reads the state file
func (fs *FileStorage) LoadState() (*config.State, error) {
	statePath, err := fs.r.StatePath()
	if err != nil {
		return nil, fmt.Errorf("failed to get state path: %w", err)
	}

	data, err := os.ReadFile(statePath)
	if err != nil {
		if os.IsNotExist(err) {
			// Return default state if file doesn't exist
			return config.NewState(), nil
		}
		return nil, fmt.Errorf("failed to read state file: %w", err)
	}

	var state config.State
	if err := yaml.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("failed to unmarshal state: %w", err)
	}

	return &state, nil
}
