package envextract

import (
	"os"

	"github.com/a2d2-dev/claudecm/internal/config"
)

// ClaudeEnvVars defines the environment variables used by Claude Code
const (
	EnvBaseURL         = "ANTHROPIC_BASE_URL"
	EnvAuthToken       = "ANTHROPIC_AUTH_TOKEN"
	EnvModel           = "ANTHROPIC_MODEL"
	EnvSmallFastModel  = "ANTHROPIC_SMALL_FAST_MODEL"
)

// ExtractedEnv holds the extracted environment variables
type ExtractedEnv struct {
	BaseURL         string
	AuthToken       string
	Model           string
	SmallFastModel  string
}

// ExtractCurrentEnv extracts Claude-related environment variables from the current environment
func ExtractCurrentEnv() *ExtractedEnv {
	return &ExtractedEnv{
		BaseURL:        getEnvWithDefault(EnvBaseURL, "https://api.anthropic.com"),
		AuthToken:      os.Getenv(EnvAuthToken),
		Model:          os.Getenv(EnvModel),
		SmallFastModel: os.Getenv(EnvSmallFastModel),
	}
}

// ToProfile converts extracted environment variables to a v1 Profile.
// Name, CreatedAt, and UpdatedAt are set by NewProfile.
func (e *ExtractedEnv) ToProfile(name string) *config.Profile {
	profile := config.NewProfile(name, e.BaseURL, e.AuthToken)
	profile.Core.Model = e.Model
	profile.Core.SmallFastModel = e.SmallFastModel
	return profile
}

// HasAuthToken checks if an auth token is present
func (e *ExtractedEnv) HasAuthToken() bool {
	return e.AuthToken != ""
}

// getEnvWithDefault returns environment variable value or default if empty
func getEnvWithDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
