package config

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestParseProfile_HappyV1RoundTrip exercises E1-S1 AC #1 and the "happy"
// test-plan row: a canonical profile (core + sparse claude_code overlay +
// sparse codex overlay) round-trips through MarshalProfile → ParseProfile
// to an equal value.
func TestParseProfile_HappyV1RoundTrip(t *testing.T) {
	created := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	updated := time.Date(2026, 7, 1, 9, 30, 0, 0, time.UTC)
	original := &Profile{
		SchemaVersion: CurrentProfileSchemaVersion,
		Name:          "round-trip",
		Description:   "test profile",
		CreatedAt:     created,
		UpdatedAt:     updated,
		Core: CoreConfig{
			Provider:       "anthropic",
			BaseURL:        "https://api.anthropic.com",
			APIKey:         "sk-ant-api03-aaaaaaaaaaaaaaaa",
			Model:          "claude-sonnet-4",
			SmallFastModel: "claude-haiku-4",
			ExtraEnv:       map[string]string{"FOO": "bar"},
		},
		Tools: map[ToolID]ToolOverlay{
			ToolClaudeCode: {
				Model: "claude-opus-4",
			},
			ToolCodex: {
				BaseURL: "https://codex.example.test",
			},
		},
	}

	data, err := MarshalProfile(original)
	require.NoError(t, err)
	require.Contains(t, string(data), "schema_version: 1")

	got, err := ParseProfile(data)
	require.NoError(t, err)

	assert.Equal(t, original.SchemaVersion, got.SchemaVersion)
	assert.Equal(t, original.Name, got.Name)
	assert.Equal(t, original.Description, got.Description)
	assert.Equal(t, original.Core, got.Core)
	assert.Equal(t, original.Tools, got.Tools)
	// Times round-trip through YAML, but may lose monotonic clock — compare
	// instants only.
	assert.True(t, original.CreatedAt.Equal(got.CreatedAt))
	assert.True(t, original.UpdatedAt.Equal(got.UpdatedAt))
}

// TestParseProfile_FallThroughResolution exercises the "edge: both core and a
// tool overlay that sets only one field — fall-through resolution returns
// core value for the unset overlay key" row from the story test plan.
// ParseProfile must preserve sparseness (absent overlay fields remain zero
// values, callers compute fall-through).
func TestParseProfile_FallThroughResolution(t *testing.T) {
	yamlData := `schema_version: 1
name: sparse
core:
  base_url: https://core.example.test
  api_key: core-key-aaaaaaaaaaaa
  model: core-model
  small_fast_model: core-small
tools:
  claude_code:
    model: overlay-model
`
	got, err := ParseProfile([]byte(yamlData))
	require.NoError(t, err)

	overlay, ok := got.Tools[ToolClaudeCode]
	require.True(t, ok, "claude_code overlay should be present")

	// Overlay overrode model.
	assert.Equal(t, "overlay-model", overlay.Model)
	// Overlay did NOT set small_fast_model; the zero value signals
	// "fall through to core".
	assert.Empty(t, overlay.SmallFastModel)
	// Fall-through value comes from core.
	assert.Equal(t, "core-small", got.Core.SmallFastModel)
}

// TestParseProfile_LegacyV0Migration covers the legacy migration path: a YAML
// produced by the pre-E1-S1 Profile struct (auth_token / custom_env at the
// root, no schema_version) is migrated to v1 in-memory. The next MarshalProfile
// stamps schema_version: 1 on disk.
func TestParseProfile_LegacyV0Migration(t *testing.T) {
	legacyYAML := `name: legacy-profile
base_url: https://api.legacy.test
auth_token: legacy-token-xxxxxx
model: claude-sonnet-3
custom_env:
  ANTHROPIC_SMALL_FAST_MODEL: claude-haiku-3
  CUSTOM_FLAG: enabled
description: imported from v0
`

	got, err := ParseProfile([]byte(legacyYAML))
	require.NoError(t, err)

	assert.Equal(t, CurrentProfileSchemaVersion, got.SchemaVersion)
	assert.Equal(t, "legacy-profile", got.Name)
	assert.Equal(t, "https://api.legacy.test", got.Core.BaseURL)
	assert.Equal(t, "legacy-token-xxxxxx", got.Core.APIKey)
	assert.Equal(t, "claude-sonnet-3", got.Core.Model)
	// The lifted small-fast model field.
	assert.Equal(t, "claude-haiku-3", got.Core.SmallFastModel)
	// Non-lifted custom_env survives.
	assert.Equal(t, "enabled", got.Core.ExtraEnv["CUSTOM_FLAG"])
	// The lifted key must not leak back into ExtraEnv.
	_, present := got.Core.ExtraEnv["ANTHROPIC_SMALL_FAST_MODEL"]
	assert.False(t, present, "small-fast model must not double-live in extra_env")
	assert.Equal(t, "imported from v0", got.Description)

	// On the next write, schema_version: 1 must land on disk (this is the
	// "rewrite on next save" property — see schema.go MarshalProfile).
	bytes, err := MarshalProfile(got)
	require.NoError(t, err)
	assert.Contains(t, string(bytes), "schema_version: 1")
}

// TestParseProfile_MissingSchemaVersionMigratedFromLegacy covers the
// "missing schema_version → migrated (legacy v0)" branch separately from the
// fully populated legacy fixture above: an empty-ish file (no
// schema_version, only known legacy keys) still migrates without error and
// produces a v1-shaped Profile that the next save will stamp.
func TestParseProfile_MissingSchemaVersionMigratedFromLegacy(t *testing.T) {
	bareLegacyYAML := `name: bare
base_url: https://bare.example.test
auth_token: bare-token-xxxxxxxxxx
`
	got, err := ParseProfile([]byte(bareLegacyYAML))
	require.NoError(t, err)
	assert.Equal(t, CurrentProfileSchemaVersion, got.SchemaVersion)
	assert.Equal(t, "bare", got.Name)
	assert.Equal(t, "https://bare.example.test", got.Core.BaseURL)
	assert.Equal(t, "bare-token-xxxxxxxxxx", got.Core.APIKey)
}

// TestParseProfile_FutureSchemaVersionRefused covers AC #3: a file written by
// a newer claudecm (schema_version >= 2) is refused with a clear error. We
// MUST NOT silently misread the file.
func TestParseProfile_FutureSchemaVersionRefused(t *testing.T) {
	futureYAML := `schema_version: 2
name: from-the-future
core:
  base_url: https://api.future.test
  api_key: future-key-aaaaaaaaaa
`
	got, err := ParseProfile([]byte(futureYAML))
	require.Error(t, err)
	assert.Nil(t, got, "must not return a partially populated profile on future-version refusal")
	assert.Contains(t, err.Error(), "newer claudecm")
}

// TestParseProfile_MissingSchemaVersionWithUnknownKeysRefused asserts the
// no-fallback-writes rule: a file without schema_version that contains a key
// outside the legacy v0 set is refused — we do not best-effort migrate
// unknown shapes (NFR-M1, CLAUDE.md global no-fallback rule).
func TestParseProfile_MissingSchemaVersionWithUnknownKeysRefused(t *testing.T) {
	weirdYAML := `name: weird
base_url: https://api.weird.test
auth_token: token-xxxxxxxxxxxxxx
mystery_field: something
`
	got, err := ParseProfile([]byte(weirdYAML))
	require.Error(t, err)
	assert.Nil(t, got)
	assert.Contains(t, err.Error(), "unknown key")
}

// TestParseProfile_MalformedYAMLErrors covers AC: malformed YAML → error,
// never a partial Profile. No rewrite is attempted (no-fallback-writes).
func TestParseProfile_MalformedYAMLErrors(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			name: "broken indentation",
			body: "schema_version: 1\nname: bad\n  core:\n    base_url: x\n",
		},
		{
			name: "non-integer schema_version",
			body: "schema_version: not-a-number\nname: bad\n",
		},
		{
			name: "negative schema_version",
			body: "schema_version: -1\nname: bad\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseProfile([]byte(tc.body))
			require.Error(t, err, "want error for %s", tc.name)
			assert.Nil(t, got, "want nil profile on error for %s", tc.name)
		})
	}
}

// TestMarshalProfile_StampsSchemaVersion asserts the writer always stamps
// CurrentProfileSchemaVersion regardless of what the caller set, so a
// migrated-in-memory legacy profile gets correctly persisted.
func TestMarshalProfile_StampsSchemaVersion(t *testing.T) {
	p := &Profile{
		// Deliberately zero — simulates a freshly migrated legacy profile
		// that the caller forgot to stamp.
		SchemaVersion: 0,
		Name:          "stamp-me",
		Core: CoreConfig{
			BaseURL: "https://api.example.test",
			APIKey:  "key-xxxxxxxxxxxxx",
		},
	}
	data, err := MarshalProfile(p)
	require.NoError(t, err)
	assert.True(t, strings.Contains(string(data), "schema_version: 1"),
		"MarshalProfile must always stamp schema_version: 1; got:\n%s", string(data))
}
