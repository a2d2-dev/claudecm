package config

import (
	"fmt"
	"time"
)

// Profile represents a Claude Code environment configuration with all necessary environment variables.
type Profile struct {
	// Name is the unique identifier for the profile (e.g., "anthropic-us", "moonshot-dev")
	Name string `yaml:"name"`

	// BaseURL is the API base URL (e.g., "https://api.anthropic.com")
	BaseURL string `yaml:"base_url"`

	// AuthToken is the API authentication token (sensitive data)
	AuthToken string `yaml:"auth_token"`

	// Model is the default model name (e.g., "claude-sonnet-4")
	Model string `yaml:"model,omitempty"`

	// CustomEnv contains additional custom environment variables
	CustomEnv map[string]string `yaml:"custom_env,omitempty"`

	// Description is an optional human-readable description
	Description string `yaml:"description,omitempty"`

	// CreatedAt is the profile creation timestamp
	CreatedAt time.Time `yaml:"created_at"`

	// UpdatedAt is the last modification timestamp
	UpdatedAt time.Time `yaml:"updated_at"`
}

// NewProfile creates a new Profile with timestamps initialized
func NewProfile(name, baseURL, authToken string) *Profile {
	now := time.Now()
	return &Profile{
		Name:       name,
		BaseURL:    baseURL,
		AuthToken:  authToken,
		CustomEnv:  make(map[string]string),
		CreatedAt:  now,
		UpdatedAt:  now,
	}
}

// String returns a string representation of the profile with redacted token
func (p *Profile) String() string {
	token := p.AuthToken
	if len(token) > 10 {
		token = token[:4] + "..." + token[len(token)-4:]
	}
	return fmt.Sprintf("Profile{Name: %s, BaseURL: %s, Token: %s, Model: %s}",
		p.Name, p.BaseURL, token, p.Model)
}

// Touch updates the UpdatedAt timestamp
func (p *Profile) Touch() {
	p.UpdatedAt = time.Now()
}

// Clone creates a deep copy of the profile
func (p *Profile) Clone() *Profile {
	customEnv := make(map[string]string, len(p.CustomEnv))
	for k, v := range p.CustomEnv {
		customEnv[k] = v
	}

	return &Profile{
		Name:        p.Name,
		BaseURL:     p.BaseURL,
		AuthToken:   p.AuthToken,
		Model:       p.Model,
		CustomEnv:   customEnv,
		Description: p.Description,
		CreatedAt:   p.CreatedAt,
		UpdatedAt:   p.UpdatedAt,
	}
}
