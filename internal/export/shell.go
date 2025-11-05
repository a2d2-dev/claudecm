package export

import (
	"fmt"
	"strings"

	"github.com/imneov/claudecm/internal/config"
)

// ToShell exports profile as shell export statements
func ToShell(profile *config.Profile) string {
	if profile == nil {
		return ""
	}

	var exports []string

	// Export base URL
	if profile.BaseURL != "" {
		exports = append(exports, fmt.Sprintf("export ANTHROPIC_BASE_URL=%q", profile.BaseURL))
	}

	// Export auth token
	if profile.AuthToken != "" {
		exports = append(exports, fmt.Sprintf("export ANTHROPIC_AUTH_TOKEN=%q", profile.AuthToken))
	}

	// Export model if set
	if profile.Model != "" {
		exports = append(exports, fmt.Sprintf("export ANTHROPIC_MODEL=%q", profile.Model))
	}

	// Export custom environment variables
	for key, value := range profile.CustomEnv {
		exports = append(exports, fmt.Sprintf("export %s=%q", key, value))
	}

	return strings.Join(exports, "\n")
}
