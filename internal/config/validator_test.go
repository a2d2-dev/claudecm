package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidator_ValidateProfileName(t *testing.T) {
	validator := NewValidator()

	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"valid name", "test-profile", false},
		{"valid with underscore", "test_profile", false},
		{"valid with numbers", "profile123", false},
		{"empty name", "", true},
		{"invalid chars", "test profile", true},
		{"invalid special chars", "test@profile", true},
		{"too long", "this-is-a-very-long-profile-name-that-exceeds-the-maximum-allowed-length", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validator.ValidateProfileName(tt.input)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestValidator_ValidateURL(t *testing.T) {
	validator := NewValidator()

	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"valid https", "https://api.anthropic.com", false},
		{"valid http", "http://localhost:8080", false},
		{"empty url", "", true},
		{"invalid scheme", "ftp://api.test.com", true},
		{"no scheme", "api.test.com", true},
		{"no host", "https://", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validator.ValidateURL(tt.input)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestValidator_ValidateToken(t *testing.T) {
	validator := NewValidator()

	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"valid token", "sk-ant-api03-xxxxx", false},
		{"empty token", "", true},
		{"too short", "short", true},
		{"too long", string(make([]byte, 1025)), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validator.ValidateToken(tt.input)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestValidator_ValidateProfile(t *testing.T) {
	validator := NewValidator()

	tests := []struct {
		name    string
		profile *Profile
		wantErr bool
	}{
		{
			name: "valid profile",
			profile: &Profile{
				Name:      "test",
				BaseURL:   "https://api.test.com",
				AuthToken: "valid-token-here",
			},
			wantErr: false,
		},
		{
			name:    "nil profile",
			profile: nil,
			wantErr: true,
		},
		{
			name: "invalid name",
			profile: &Profile{
				Name:      "test profile",
				BaseURL:   "https://api.test.com",
				AuthToken: "valid-token-here",
			},
			wantErr: true,
		},
		{
			name: "invalid url",
			profile: &Profile{
				Name:      "test",
				BaseURL:   "invalid-url",
				AuthToken: "valid-token-here",
			},
			wantErr: true,
		},
		{
			name: "invalid token",
			profile: &Profile{
				Name:      "test",
				BaseURL:   "https://api.test.com",
				AuthToken: "",
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validator.ValidateProfile(tt.profile)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestValidator_ValidateEnvKey(t *testing.T) {
	validator := NewValidator()

	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"valid key", "API_KEY", false},
		{"valid with numbers", "API_KEY_123", false},
		{"valid with underscores", "API_KEY_TEST", false},
		{"empty key", "", true},
		{"lowercase", "api_key", true},
		{"starts with number", "123_API", true},
		{"contains special chars", "API-KEY", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validator.ValidateEnvKey(tt.input)
			if tt.wantErr {
				require.Error(t, err, "Expected error for input: %s", tt.input)
			} else {
				require.NoError(t, err, "Expected no error for input: %s", tt.input)
			}
		})
	}
}
