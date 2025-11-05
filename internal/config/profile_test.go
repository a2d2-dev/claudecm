package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestNewProfile(t *testing.T) {
	name := "test-profile"
	baseURL := "https://api.test.com"
	token := "test-token"

	profile := NewProfile(name, baseURL, token)

	assert.Equal(t, name, profile.Name)
	assert.Equal(t, baseURL, profile.BaseURL)
	assert.Equal(t, token, profile.AuthToken)
	assert.NotNil(t, profile.CustomEnv)
	assert.False(t, profile.CreatedAt.IsZero())
	assert.False(t, profile.UpdatedAt.IsZero())
}

func TestProfile_String(t *testing.T) {
	tests := []struct {
		name      string
		profile   *Profile
		wantToken string
	}{
		{
			name: "short token",
			profile: &Profile{
				Name:      "test",
				BaseURL:   "https://api.test.com",
				AuthToken: "short",
			},
			wantToken: "short",
		},
		{
			name: "long token redacted",
			profile: &Profile{
				Name:      "test",
				BaseURL:   "https://api.test.com",
				AuthToken: "sk-ant-api03-very-long-token-here-xxxxx",
			},
			wantToken: "sk-a...xxxxx",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.profile.String()
			assert.Contains(t, result, tt.profile.Name)
			assert.Contains(t, result, tt.wantToken)
		})
	}
}

func TestProfile_Touch(t *testing.T) {
	profile := NewProfile("test", "https://api.test.com", "token")
	originalTime := profile.UpdatedAt

	time.Sleep(10 * time.Millisecond)
	profile.Touch()

	assert.True(t, profile.UpdatedAt.After(originalTime))
}

func TestProfile_Clone(t *testing.T) {
	original := NewProfile("test", "https://api.test.com", "token")
	original.Model = "claude-sonnet-4"
	original.CustomEnv["TEST_VAR"] = "value"

	cloned := original.Clone()

	assert.Equal(t, original.Name, cloned.Name)
	assert.Equal(t, original.BaseURL, cloned.BaseURL)
	assert.Equal(t, original.AuthToken, cloned.AuthToken)
	assert.Equal(t, original.Model, cloned.Model)
	assert.Equal(t, original.CustomEnv["TEST_VAR"], cloned.CustomEnv["TEST_VAR"])

	// Ensure deep copy
	cloned.CustomEnv["TEST_VAR"] = "changed"
	assert.NotEqual(t, original.CustomEnv["TEST_VAR"], cloned.CustomEnv["TEST_VAR"])
}
