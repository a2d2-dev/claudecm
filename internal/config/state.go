package config

import "time"

// State tracks the currently active configuration profile.
type State struct {
	// Version is the state file format version (for future compatibility)
	Version string `yaml:"version"`

	// CurrentProfile is the name of the active profile
	CurrentProfile string `yaml:"current_profile"`

	// LastSwitched is when the active profile was set
	LastSwitched time.Time `yaml:"last_switched"`
}

// NewState creates a new State with default values
func NewState() *State {
	return &State{
		Version:      "1.0",
		LastSwitched: time.Now(),
	}
}

// SetCurrentProfile updates the current profile and timestamp
func (s *State) SetCurrentProfile(profileName string) {
	s.CurrentProfile = profileName
	s.LastSwitched = time.Now()
}

// HasActiveProfile returns true if there is an active profile set
func (s *State) HasActiveProfile() bool {
	return s.CurrentProfile != ""
}
