package export

import (
	"fmt"
	"strings"

	"github.com/imneov/claudecm/internal/config"
)

// ToMap exports profile environment variables as a map
func ToMap(profile *config.Profile) map[string]string {
	if profile == nil {
		return nil
	}

	env := make(map[string]string)

	// Export base URL
	if profile.BaseURL != "" {
		env["ANTHROPIC_BASE_URL"] = profile.BaseURL
	}

	// Export auth token
	if profile.AuthToken != "" {
		env["ANTHROPIC_AUTH_TOKEN"] = profile.AuthToken
	}

	// Export model if set
	if profile.Model != "" {
		env["ANTHROPIC_MODEL"] = profile.Model
	}

	// Export custom environment variables
	for key, value := range profile.CustomEnv {
		if value != "" {
			env[key] = value
		}
	}

	return env
}

// ToShell exports profile as shell export statements
func ToShell(profile *config.Profile) string {
	if profile == nil {
		return ""
	}

	var exports []string

	// Get environment map
	envMap := ToMap(profile)

	// Convert to export statements
	for key, value := range envMap {
		if value != "" {
			exports = append(exports, fmt.Sprintf("export %s=%q", key, value))
		} else {
			exports = append(exports, fmt.Sprintf("export %s=", key))
		}
	}

	return strings.Join(exports, "\n")
}
