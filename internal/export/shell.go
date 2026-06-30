package export

import (
	"fmt"
	"strings"

	"github.com/a2d2-dev/claudecm/internal/config"
)

// ToMap exports profile environment variables as a map
func ToMap(profile *config.Profile) map[string]string {
	if profile == nil {
		return nil
	}

	env := make(map[string]string)

	// Export base URL from core
	if profile.Core.BaseURL != "" {
		env["ANTHROPIC_BASE_URL"] = profile.Core.BaseURL
	}

	// Export API key from core
	if profile.Core.APIKey != "" {
		env["ANTHROPIC_AUTH_TOKEN"] = profile.Core.APIKey
	}

	// Export model from core
	if profile.Core.Model != "" {
		env["ANTHROPIC_MODEL"] = profile.Core.Model
	}

	// Export small-fast model from core
	if profile.Core.SmallFastModel != "" {
		env["ANTHROPIC_SMALL_FAST_MODEL"] = profile.Core.SmallFastModel
	}

	// Export extra_env passthrough
	for key, value := range profile.Core.ExtraEnv {
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
