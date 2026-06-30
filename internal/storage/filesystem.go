package storage

import (
	"fmt"
	"os"
	"strings"

	"github.com/a2d2-dev/claudecm/internal/config"
	"gopkg.in/yaml.v3"
)

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

// FileStorage implements Storage using the local filesystem
type FileStorage struct{}

// NewFileStorage creates a new FileStorage instance
func NewFileStorage() *FileStorage {
	return &FileStorage{}
}

// SaveProfile writes a profile to disk atomically
func (fs *FileStorage) SaveProfile(profile *config.Profile) error {
	if profile == nil {
		return fmt.Errorf("profile cannot be nil")
	}

	// Ensure config directory exists
	if err := EnsureConfigDir(); err != nil {
		return fmt.Errorf("failed to ensure config directory: %w", err)
	}

	// Get profile path
	profilePath, err := GetProfilePath(profile.Name)
	if err != nil {
		return fmt.Errorf("failed to get profile path: %w", err)
	}

	// Validate path (prevent directory traversal)
	profilesDir, _ := GetProfilesDir()
	if !strings.HasPrefix(profilePath, profilesDir) {
		return fmt.Errorf("invalid profile name: path traversal detected")
	}

	// Marshal to YAML
	data, err := yaml.Marshal(profile)
	if err != nil {
		return fmt.Errorf("failed to marshal profile: %w", err)
	}

	// Atomic write: write to temp file then rename
	tmpPath := profilePath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0600); err != nil {
		return fmt.Errorf("failed to write temp file: %w", err)
	}

	if err := os.Rename(tmpPath, profilePath); err != nil {
		os.Remove(tmpPath) // Clean up temp file on error
		return fmt.Errorf("failed to rename temp file: %w", err)
	}

	return nil
}

// LoadProfile reads a profile from disk
func (fs *FileStorage) LoadProfile(name string) (*config.Profile, error) {
	if name == "" {
		return nil, fmt.Errorf("profile name cannot be empty")
	}

	profilePath, err := GetProfilePath(name)
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

	var profile config.Profile
	if err := yaml.Unmarshal(data, &profile); err != nil {
		return nil, fmt.Errorf("failed to unmarshal profile: %w", err)
	}

	return &profile, nil
}

// LoadAllProfiles reads all profiles from disk
func (fs *FileStorage) LoadAllProfiles() ([]*config.Profile, error) {
	profilesDir, err := GetProfilesDir()
	if err != nil {
		return nil, fmt.Errorf("failed to get profiles directory: %w", err)
	}

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
			// Log error but continue loading other profiles
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

	profilePath, err := GetProfilePath(name)
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
	profilePath, err := GetProfilePath(name)
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

// SaveState writes the state file atomically
func (fs *FileStorage) SaveState(state *config.State) error {
	if state == nil {
		return fmt.Errorf("state cannot be nil")
	}

	// Ensure config directory exists
	if err := EnsureConfigDir(); err != nil {
		return fmt.Errorf("failed to ensure config directory: %w", err)
	}

	statePath, err := GetStatePath()
	if err != nil {
		return fmt.Errorf("failed to get state path: %w", err)
	}

	// Marshal to YAML
	data, err := yaml.Marshal(state)
	if err != nil {
		return fmt.Errorf("failed to marshal state: %w", err)
	}

	// Atomic write
	tmpPath := statePath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0600); err != nil {
		return fmt.Errorf("failed to write temp file: %w", err)
	}

	if err := os.Rename(tmpPath, statePath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("failed to rename temp file: %w", err)
	}

	return nil
}

// LoadState reads the state file
func (fs *FileStorage) LoadState() (*config.State, error) {
	statePath, err := GetStatePath()
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
