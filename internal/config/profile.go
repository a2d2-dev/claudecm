package config

import (
	"fmt"
	"time"
)

// CurrentProfileSchemaVersion is the on-disk Profile schema version this build understands.
// See docs/architecture.md §2.1 and ADR-0001 §Locked Decisions #2.
const CurrentProfileSchemaVersion = 1

// ToolID identifies a supported AI coding tool. v1 enumerates exactly two values
// per ADR-0001 §Locked Decisions #1.
type ToolID string

const (
	ToolClaudeCode ToolID = "claude_code"
	ToolCodex      ToolID = "codex"
)

// Profile is the canonical on-disk unit. One YAML file per profile under
// ~/.claudecm/profiles/<name>.yaml. Schema version is REQUIRED on read for
// any non-legacy file (see ParseProfile for the legacy v0 migration path).
type Profile struct {
	// SchemaVersion is the wire-format version. Must be CurrentProfileSchemaVersion
	// (1) for v1. Missing on disk is interpreted as legacy v0 and migrated.
	SchemaVersion int `yaml:"schema_version"`

	// Name is the unique profile identifier (e.g., "anthropic-us").
	Name string `yaml:"name"`

	// Description is an optional human-readable description.
	Description string `yaml:"description,omitempty"`

	// CreatedAt is the profile creation timestamp.
	CreatedAt time.Time `yaml:"created_at"`

	// UpdatedAt is the last modification timestamp.
	UpdatedAt time.Time `yaml:"updated_at"`

	// Core carries the shared (provider-agnostic) configuration.
	Core CoreConfig `yaml:"core"`

	// Tools holds per-tool overlays. Sparse: an absent tool means "no overlay,
	// fall through to Core". Only ToolClaudeCode and ToolCodex are valid in v1.
	Tools map[ToolID]ToolOverlay `yaml:"tools,omitempty"`
}

// CoreConfig is the shared configuration used by every tool unless an overlay
// (see ToolOverlay) overrides a given field.
type CoreConfig struct {
	// Provider identifies the upstream API provider (e.g., "anthropic", "openai-compat").
	Provider string `yaml:"provider,omitempty"`

	// BaseURL is the API base URL.
	BaseURL string `yaml:"base_url"`

	// APIKey is the plaintext API credential. Redacted on display unless --reveal.
	APIKey string `yaml:"api_key"`

	// Model is the default model name.
	Model string `yaml:"model,omitempty"`

	// SmallFastModel is the auxiliary fast/cheap model (e.g., haiku-class).
	SmallFastModel string `yaml:"small_fast_model,omitempty"`

	// ExtraEnv holds extra pass-through environment variables. Resolver-allowlisted.
	ExtraEnv map[string]string `yaml:"extra_env,omitempty"`
}

// ToolOverlay sparsely overrides Core for a specific tool. An absent field
// means "fall through to Core" — see resolver layer order in
// docs/architecture.md §6.
type ToolOverlay struct {
	BaseURL        string            `yaml:"base_url,omitempty"`
	APIKey         string            `yaml:"api_key,omitempty"`
	Model          string            `yaml:"model,omitempty"`
	SmallFastModel string            `yaml:"small_fast_model,omitempty"`
	ExtraEnv       map[string]string `yaml:"extra_env,omitempty"`

	// Raw is the escape hatch for tool-private overlay knobs not yet promoted
	// into the typed overlay. Adapters opt into specific Raw keys.
	Raw map[string]any `yaml:"raw,omitempty"`
}

// NewProfile constructs a v1 Profile with schema_version and timestamps set.
// Core fields are seeded from the legacy (name, baseURL, apiKey) trio so callers
// migrating from the pre-E1-S1 NewProfile signature do not have to be rewritten.
func NewProfile(name, baseURL, apiKey string) *Profile {
	now := time.Now()
	return &Profile{
		SchemaVersion: CurrentProfileSchemaVersion,
		Name:          name,
		Core: CoreConfig{
			BaseURL:  baseURL,
			APIKey:   apiKey,
			ExtraEnv: make(map[string]string),
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// String returns a string representation with the API key redacted.
func (p *Profile) String() string {
	token := p.Core.APIKey
	if len(token) > 10 {
		token = token[:4] + "..." + token[len(token)-4:]
	}
	return fmt.Sprintf("Profile{Name: %s, BaseURL: %s, APIKey: %s, Model: %s}",
		p.Name, p.Core.BaseURL, token, p.Core.Model)
}

// Touch updates the UpdatedAt timestamp.
func (p *Profile) Touch() {
	p.UpdatedAt = time.Now()
}

// Clone returns a deep copy of the profile.
func (p *Profile) Clone() *Profile {
	cp := &Profile{
		SchemaVersion: p.SchemaVersion,
		Name:          p.Name,
		Description:   p.Description,
		CreatedAt:     p.CreatedAt,
		UpdatedAt:     p.UpdatedAt,
		Core: CoreConfig{
			Provider:       p.Core.Provider,
			BaseURL:        p.Core.BaseURL,
			APIKey:         p.Core.APIKey,
			Model:          p.Core.Model,
			SmallFastModel: p.Core.SmallFastModel,
		},
	}

	if p.Core.ExtraEnv != nil {
		cp.Core.ExtraEnv = make(map[string]string, len(p.Core.ExtraEnv))
		for k, v := range p.Core.ExtraEnv {
			cp.Core.ExtraEnv[k] = v
		}
	}

	if p.Tools != nil {
		cp.Tools = make(map[ToolID]ToolOverlay, len(p.Tools))
		for id, ov := range p.Tools {
			cp.Tools[id] = cloneOverlay(ov)
		}
	}

	return cp
}

func cloneOverlay(ov ToolOverlay) ToolOverlay {
	out := ToolOverlay{
		BaseURL:        ov.BaseURL,
		APIKey:         ov.APIKey,
		Model:          ov.Model,
		SmallFastModel: ov.SmallFastModel,
	}
	if ov.ExtraEnv != nil {
		out.ExtraEnv = make(map[string]string, len(ov.ExtraEnv))
		for k, v := range ov.ExtraEnv {
			out.ExtraEnv[k] = v
		}
	}
	if ov.Raw != nil {
		out.Raw = make(map[string]any, len(ov.Raw))
		for k, v := range ov.Raw {
			out.Raw[k] = v
		}
	}
	return out
}
