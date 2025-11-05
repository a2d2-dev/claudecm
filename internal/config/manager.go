package config

import (
	"fmt"
	"sync"
)

// Storage defines the storage interface for Manager
type Storage interface {
	SaveProfile(profile *Profile) error
	LoadProfile(name string) (*Profile, error)
	LoadAllProfiles() ([]*Profile, error)
	DeleteProfile(name string) error
	ProfileExists(name string) (bool, error)
	SaveState(state *State) error
	LoadState() (*State, error)
}

// Manager handles all configuration management operations
type Manager struct {
	storage   Storage
	validator Validator
	mu        sync.RWMutex
}

// NewManager creates a new Manager instance
func NewManager(storage Storage, validator Validator) *Manager {
	return &Manager{
		storage:   storage,
		validator: validator,
	}
}

// AddProfile creates a new profile after validation
func (m *Manager) AddProfile(profile *Profile) error {
	if profile == nil {
		return fmt.Errorf("profile cannot be nil")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Validate profile
	if err := m.validator.ValidateProfile(profile); err != nil {
		return fmt.Errorf("validation failed: %w", err)
	}

	// Check if profile already exists
	exists, err := m.storage.ProfileExists(profile.Name)
	if err != nil {
		return fmt.Errorf("failed to check profile existence: %w", err)
	}
	if exists {
		return fmt.Errorf("profile %q already exists", profile.Name)
	}

	// Save profile
	if err := m.storage.SaveProfile(profile); err != nil {
		return fmt.Errorf("failed to save profile: %w", err)
	}

	return nil
}

// ListProfiles retrieves all profiles
func (m *Manager) ListProfiles() ([]*Profile, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	profiles, err := m.storage.LoadAllProfiles()
	if err != nil {
		return nil, fmt.Errorf("failed to load profiles: %w", err)
	}

	return profiles, nil
}

// GetProfile retrieves a specific profile by name
func (m *Manager) GetProfile(name string) (*Profile, error) {
	if name == "" {
		return nil, fmt.Errorf("profile name cannot be empty")
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	profile, err := m.storage.LoadProfile(name)
	if err != nil {
		return nil, fmt.Errorf("failed to load profile: %w", err)
	}

	return profile, nil
}

// UpdateProfile modifies an existing profile
func (m *Manager) UpdateProfile(name string, updatedProfile *Profile) error {
	if name == "" {
		return fmt.Errorf("profile name cannot be empty")
	}
	if updatedProfile == nil {
		return fmt.Errorf("updated profile cannot be nil")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Check if profile exists
	exists, err := m.storage.ProfileExists(name)
	if err != nil {
		return fmt.Errorf("failed to check profile existence: %w", err)
	}
	if !exists {
		return fmt.Errorf("profile %q not found", name)
	}

	// Validate updated profile
	if err := m.validator.ValidateProfile(updatedProfile); err != nil {
		return fmt.Errorf("validation failed: %w", err)
	}

	// Update timestamp
	updatedProfile.Touch()

	// Save updated profile
	if err := m.storage.SaveProfile(updatedProfile); err != nil {
		return fmt.Errorf("failed to save updated profile: %w", err)
	}

	return nil
}

// DeleteProfile removes a profile
func (m *Manager) DeleteProfile(name string) error {
	if name == "" {
		return fmt.Errorf("profile name cannot be empty")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Check if this is the active profile
	state, err := m.storage.LoadState()
	if err != nil {
		return fmt.Errorf("failed to load state: %w", err)
	}

	if state.CurrentProfile == name {
		// Clear active profile if deleting it
		state.CurrentProfile = ""
		if err := m.storage.SaveState(state); err != nil {
			return fmt.Errorf("failed to update state: %w", err)
		}
	}

	// Delete profile
	if err := m.storage.DeleteProfile(name); err != nil {
		return fmt.Errorf("failed to delete profile: %w", err)
	}

	return nil
}

// SetActive marks a profile as the active one
func (m *Manager) SetActive(name string) error {
	if name == "" {
		return fmt.Errorf("profile name cannot be empty")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Verify profile exists
	exists, err := m.storage.ProfileExists(name)
	if err != nil {
		return fmt.Errorf("failed to check profile existence: %w", err)
	}
	if !exists {
		return fmt.Errorf("profile %q not found", name)
	}

	// Load current state
	state, err := m.storage.LoadState()
	if err != nil {
		return fmt.Errorf("failed to load state: %w", err)
	}

	// Update active profile
	state.SetCurrentProfile(name)

	// Save state
	if err := m.storage.SaveState(state); err != nil {
		return fmt.Errorf("failed to save state: %w", err)
	}

	return nil
}

// GetActive retrieves the currently active profile
func (m *Manager) GetActive() (*Profile, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	// Load state
	state, err := m.storage.LoadState()
	if err != nil {
		return nil, fmt.Errorf("failed to load state: %w", err)
	}

	if !state.HasActiveProfile() {
		return nil, fmt.Errorf("no active profile set")
	}

	// Load active profile
	profile, err := m.storage.LoadProfile(state.CurrentProfile)
	if err != nil {
		return nil, fmt.Errorf("failed to load active profile: %w", err)
	}

	return profile, nil
}

// GetActiveName returns the name of the active profile
func (m *Manager) GetActiveName() (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	state, err := m.storage.LoadState()
	if err != nil {
		return "", fmt.Errorf("failed to load state: %w", err)
	}

	return state.CurrentProfile, nil
}

// ProfileExists checks if a profile exists
func (m *Manager) ProfileExists(name string) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return m.storage.ProfileExists(name)
}
