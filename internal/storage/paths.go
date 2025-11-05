package storage

import (
	"os"
	"path/filepath"
)

const (
	// ConfigDirName is the directory name for claudecm configuration
	ConfigDirName = ".claudecm"

	// ProfilesDirName is the subdirectory for profile files
	ProfilesDirName = "profiles"

	// StateFileName is the state file name
	StateFileName = "state.yaml"
)

// GetConfigDir returns the path to the claudecm configuration directory
func GetConfigDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ConfigDirName), nil
}

// GetProfilesDir returns the path to the profiles directory
func GetProfilesDir() (string, error) {
	configDir, err := GetConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, ProfilesDirName), nil
}

// GetProfilePath returns the full path to a profile file
func GetProfilePath(profileName string) (string, error) {
	profilesDir, err := GetProfilesDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(profilesDir, profileName+".yaml"), nil
}

// GetStatePath returns the full path to the state file
func GetStatePath() (string, error) {
	configDir, err := GetConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, StateFileName), nil
}

// EnsureConfigDir creates the configuration directory structure if it doesn't exist
func EnsureConfigDir() error {
	configDir, err := GetConfigDir()
	if err != nil {
		return err
	}

	// Create config directory with 0700 permissions (owner only)
	if err := os.MkdirAll(configDir, 0700); err != nil {
		return err
	}

	// Create profiles subdirectory
	profilesDir := filepath.Join(configDir, ProfilesDirName)
	if err := os.MkdirAll(profilesDir, 0700); err != nil {
		return err
	}

	return nil
}
