package config

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// ParseProfile decodes a profile YAML byte slice into a *Profile, applying the
// legacy v0 → v1 migration when the file pre-dates the unified schema. The
// decision tree is intentionally narrow:
//
//   1. Malformed YAML        → error (no fallback writes, NFR-S1).
//   2. schema_version absent → treat as legacy v0; migrate to v1 in-memory.
//      The next save will rewrite the file under the v1 shape.
//   3. schema_version == 1   → decode as v1.
//   4. schema_version >= 2   → refuse with a "newer claudecm wrote this" error
//                              (NFR-M1: never silently misread a future schema).
//   5. Any other value (e.g. negative)
//                            → refuse: schema version is structurally invalid.
//
// MarshalProfile is the symmetric writer; it always stamps
// CurrentProfileSchemaVersion on the output.
func ParseProfile(data []byte) (*Profile, error) {
	// First pass: discover the schema_version without committing to a shape.
	// We intentionally avoid unmarshalling into *Profile here because the legacy
	// v0 shape has fields (`auth_token`, `custom_env`) that do not exist on the
	// new struct, and silently dropping them would violate NFR-M1.
	var probe map[string]any
	if err := yaml.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("malformed profile YAML: %w", err)
	}

	raw, hasVersion := probe["schema_version"]
	if !hasVersion {
		// Legacy v0: no schema_version key at all. Migrate.
		return migrateLegacyV0(data)
	}

	version, ok := toInt(raw)
	if !ok {
		return nil, fmt.Errorf("profile schema_version must be an integer, got %T", raw)
	}

	switch {
	case version == CurrentProfileSchemaVersion:
		var p Profile
		if err := yaml.Unmarshal(data, &p); err != nil {
			return nil, fmt.Errorf("malformed v1 profile YAML: %w", err)
		}
		if p.SchemaVersion != CurrentProfileSchemaVersion {
			// Defensive: yaml.Unmarshal succeeded but did not populate the
			// version. This indicates a malformed file we should not best-effort.
			return nil, fmt.Errorf("profile schema_version did not survive decoding")
		}
		return &p, nil

	case version > CurrentProfileSchemaVersion:
		return nil, fmt.Errorf(
			"profile schema_version %d is newer than this build supports (max %d); "+
				"a newer claudecm wrote this file — upgrade claudecm to read it",
			version, CurrentProfileSchemaVersion,
		)

	default:
		// Zero or negative — treat as structurally invalid rather than legacy,
		// because an explicit zero is not the same as an absent key.
		return nil, fmt.Errorf("profile schema_version %d is not valid", version)
	}
}

// MarshalProfile serialises a Profile to YAML. It always stamps
// CurrentProfileSchemaVersion, guaranteeing that any read that subsequently
// follows will land in the v1 branch of ParseProfile.
func MarshalProfile(p *Profile) ([]byte, error) {
	if p == nil {
		return nil, fmt.Errorf("cannot marshal nil profile")
	}
	out := *p
	out.SchemaVersion = CurrentProfileSchemaVersion
	return yaml.Marshal(&out)
}

// legacyV0 mirrors the pre-E1-S1 on-disk shape (see git history for the
// previous Profile struct). It exists only to deserialize legacy files for
// migration; nothing else in the codebase should reference it.
type legacyV0 struct {
	Name        string            `yaml:"name"`
	BaseURL     string            `yaml:"base_url"`
	AuthToken   string            `yaml:"auth_token"`
	Model       string            `yaml:"model,omitempty"`
	CustomEnv   map[string]string `yaml:"custom_env,omitempty"`
	Description string            `yaml:"description,omitempty"`
	CreatedAt   time.Time         `yaml:"created_at,omitempty"`
	UpdatedAt   time.Time         `yaml:"updated_at,omitempty"`
}

func migrateLegacyV0(data []byte) (*Profile, error) {
	var legacy legacyV0
	if err := yaml.Unmarshal(data, &legacy); err != nil {
		return nil, fmt.Errorf("malformed legacy profile YAML: %w", err)
	}

	// Strict no-silent-drop check: the legacy shape had exactly these fields.
	// Unknown keys in a no-schema_version file are a fail-fast condition
	// rather than a "best effort" rewrite, per CLAUDE.md no-fallback-writes
	// rule and NFR-M1.
	var probe map[string]any
	if err := yaml.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("malformed legacy profile YAML: %w", err)
	}
	for k := range probe {
		switch k {
		case "name", "base_url", "auth_token", "model", "custom_env",
			"description", "created_at", "updated_at":
			// known legacy key
		default:
			return nil, fmt.Errorf(
				"profile has no schema_version and contains unknown key %q; "+
					"refusing to migrate — fix the file or add schema_version: %d",
				k, CurrentProfileSchemaVersion,
			)
		}
	}

	// SmallFastModel was historically tucked into custom_env under the
	// ANTHROPIC_SMALL_FAST_MODEL key. Lift it into the typed field; preserve
	// the rest of custom_env verbatim under Core.ExtraEnv.
	const smallFastEnvKey = "ANTHROPIC_SMALL_FAST_MODEL"
	smallFast := ""
	extraEnv := map[string]string{}
	for k, v := range legacy.CustomEnv {
		if k == smallFastEnvKey {
			smallFast = v
			continue
		}
		extraEnv[k] = v
	}
	if len(extraEnv) == 0 {
		extraEnv = nil
	}

	p := &Profile{
		SchemaVersion: CurrentProfileSchemaVersion,
		Name:          legacy.Name,
		Description:   legacy.Description,
		CreatedAt:     legacy.CreatedAt,
		UpdatedAt:     legacy.UpdatedAt,
		Core: CoreConfig{
			BaseURL:        legacy.BaseURL,
			APIKey:         legacy.AuthToken,
			Model:          legacy.Model,
			SmallFastModel: smallFast,
			ExtraEnv:       extraEnv,
		},
	}
	return p, nil
}

// toInt normalises the YAML-decoded numeric type for schema_version. yaml.v3
// decodes into int by default, but some inputs may produce int64/float64.
func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case uint64:
		return int(n), true
	case float64:
		if float64(int(n)) != n {
			return 0, false
		}
		return int(n), true
	default:
		return 0, false
	}
}
