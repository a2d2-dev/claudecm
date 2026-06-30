package config

import (
	"fmt"
	"net/url"
	"regexp"
)

var (
	// profileNameRegex defines valid profile name characters
	profileNameRegex = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
)

// Validator defines the interface for configuration validation
type Validator interface {
	ValidateProfile(profile *Profile) error
	ValidateURL(rawURL string) error
	ValidateToken(token string) error
	ValidateProfileName(name string) error
}

// DefaultValidator implements basic validation logic
type DefaultValidator struct{}

// NewValidator creates a new DefaultValidator
func NewValidator() *DefaultValidator {
	return &DefaultValidator{}
}

// ValidateProfile performs comprehensive profile validation
func (v *DefaultValidator) ValidateProfile(profile *Profile) error {
	if profile == nil {
		return fmt.Errorf("profile cannot be nil")
	}

	// Validate profile name
	if err := v.ValidateProfileName(profile.Name); err != nil {
		return err
	}

	// Validate base URL on the core block.
	if err := v.ValidateURL(profile.Core.BaseURL); err != nil {
		return fmt.Errorf("invalid core.base_url: %w", err)
	}

	// Validate API key on the core block.
	if err := v.ValidateToken(profile.Core.APIKey); err != nil {
		return fmt.Errorf("invalid core.api_key: %w", err)
	}

	// Validate core extra_env keys.
	for key := range profile.Core.ExtraEnv {
		if err := v.ValidateEnvKey(key); err != nil {
			return fmt.Errorf("invalid core.extra_env key %q: %w", key, err)
		}
	}

	// Validate any tool overlay extra_env keys too.
	for tool, overlay := range profile.Tools {
		for key := range overlay.ExtraEnv {
			if err := v.ValidateEnvKey(key); err != nil {
				return fmt.Errorf("invalid tools.%s.extra_env key %q: %w", tool, key, err)
			}
		}
	}

	return nil
}

// ValidateProfileName checks if the profile name is valid
func (v *DefaultValidator) ValidateProfileName(name string) error {
	if name == "" {
		return fmt.Errorf("profile name cannot be empty")
	}

	if !profileNameRegex.MatchString(name) {
		return fmt.Errorf("profile name must contain only letters, numbers, hyphens, and underscores")
	}

	if len(name) > 64 {
		return fmt.Errorf("profile name too long (max 64 characters)")
	}

	return nil
}

// ValidateURL checks if the URL is well-formed and uses HTTP(S)
func (v *DefaultValidator) ValidateURL(rawURL string) error {
	if rawURL == "" {
		return fmt.Errorf("URL cannot be empty")
	}

	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL format: %w", err)
	}

	if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
		return fmt.Errorf("URL must use http or https scheme")
	}

	if parsedURL.Host == "" {
		return fmt.Errorf("URL must have a host")
	}

	return nil
}

// ValidateToken checks if the auth token is valid
func (v *DefaultValidator) ValidateToken(token string) error {
	if token == "" {
		return fmt.Errorf("token cannot be empty")
	}

	// Check for reasonable token length (most API tokens are > 10 chars)
	if len(token) < 10 {
		return fmt.Errorf("token appears too short (minimum 10 characters)")
	}

	if len(token) > 1024 {
		return fmt.Errorf("token too long (max 1024 characters)")
	}

	return nil
}

// ValidateEnvKey checks if an environment variable key is valid
func (v *DefaultValidator) ValidateEnvKey(key string) error {
	if key == "" {
		return fmt.Errorf("environment variable key cannot be empty")
	}

	// Environment variable keys should be uppercase with underscores
	envKeyRegex := regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
	if !envKeyRegex.MatchString(key) {
		return fmt.Errorf("environment variable key must start with uppercase letter and contain only uppercase letters, numbers, and underscores")
	}

	return nil
}
