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

	assert.Equal(t, CurrentProfileSchemaVersion, profile.SchemaVersion)
	assert.Equal(t, name, profile.Name)
	assert.Equal(t, baseURL, profile.Core.BaseURL)
	assert.Equal(t, token, profile.Core.APIKey)
	assert.NotNil(t, profile.Core.ExtraEnv)
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
			// A short non-empty token (<= 10 chars) must be redacted whole —
			// printing it raw would defeat the purpose of redaction since a
			// 5-char secret has no useful prefix/suffix to preserve.
			name: "short token redacted to stars",
			profile: &Profile{
				Name: "test",
				Core: CoreConfig{
					BaseURL: "https://api.test.com",
					APIKey:  "short",
				},
			},
			wantToken: "APIKey: ***",
		},
		{
			name: "long token redacted",
			profile: &Profile{
				Name: "test",
				Core: CoreConfig{
					BaseURL: "https://api.test.com",
					APIKey:  "sk-ant-api03-very-long-token-here-xxxxx",
				},
			},
			// String() takes first 4 and last 4 chars of the API key with "..."
			// in between, so the trailing 5-x suffix yields exactly "xxxx".
			wantToken: "sk-a...xxxx",
		},
		{
			// An empty api_key has nothing to redact; existing convention is
			// an empty inline value rather than a literal "***" or an omitted
			// line. We assert the existing shape stays exactly "APIKey: ,"
			// (i.e. no stars, no missing field).
			name: "empty token renders empty inline",
			profile: &Profile{
				Name: "test",
				Core: CoreConfig{
					BaseURL: "https://api.test.com",
					APIKey:  "",
				},
			},
			wantToken: "APIKey: , ",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.profile.String()
			assert.Contains(t, result, tt.profile.Name)
			assert.Contains(t, result, tt.wantToken)
			// Defence in depth: a raw short secret must never appear in the
			// redacted form, regardless of what we asserted above.
			if tt.profile.Core.APIKey != "" && len(tt.profile.Core.APIKey) <= 10 {
				assert.NotContains(t, result, tt.profile.Core.APIKey,
					"raw short token leaked into Profile.String() output")
			}
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
	original.Core.Model = "claude-sonnet-4"
	original.Core.ExtraEnv["TEST_VAR"] = "value"
	original.Tools = map[ToolID]ToolOverlay{
		ToolClaudeCode: {
			Model:    "claude-opus-4",
			ExtraEnv: map[string]string{"CLAUDE_FOO": "bar"},
			Raw:      map[string]any{"experimental": true},
		},
	}

	cloned := original.Clone()

	assert.Equal(t, original.SchemaVersion, cloned.SchemaVersion)
	assert.Equal(t, original.Name, cloned.Name)
	assert.Equal(t, original.Core.BaseURL, cloned.Core.BaseURL)
	assert.Equal(t, original.Core.APIKey, cloned.Core.APIKey)
	assert.Equal(t, original.Core.Model, cloned.Core.Model)
	assert.Equal(t, original.Core.ExtraEnv["TEST_VAR"], cloned.Core.ExtraEnv["TEST_VAR"])
	assert.Equal(t, original.Tools[ToolClaudeCode].Model, cloned.Tools[ToolClaudeCode].Model)

	// Ensure deep copy for core extra_env
	cloned.Core.ExtraEnv["TEST_VAR"] = "changed"
	assert.NotEqual(t, original.Core.ExtraEnv["TEST_VAR"], cloned.Core.ExtraEnv["TEST_VAR"])

	// Ensure deep copy for overlay maps
	clonedOverlay := cloned.Tools[ToolClaudeCode]
	clonedOverlay.ExtraEnv["CLAUDE_FOO"] = "mutated"
	assert.Equal(t, "bar", original.Tools[ToolClaudeCode].ExtraEnv["CLAUDE_FOO"])
}
