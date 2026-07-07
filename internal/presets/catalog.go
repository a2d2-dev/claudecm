// Package presets declares the closed built-in provider preset catalog.
//
// Presets are transparent local data templates. They expand into the
// ordinary v1 Profile shape and do not carry secrets, probe endpoints, or
// validate credentials online.
package presets

import (
	"fmt"
	"sort"
	"strings"

	"github.com/a2d2-dev/claudecm/internal/config"
)

const noOfficialSupportDisclaimer = "Convenience template only; not official provider support, certification, endorsement, or a compatibility guarantee."

// SecretField describes a required secret the normal add flow must collect.
type SecretField struct {
	Name        string
	Description string
}

// Preset is one immutable catalog entry.
type Preset struct {
	Name        string
	DisplayName string
	ProviderKey string
	BaseURL     string
	Model       string

	Tools      map[config.ToolID]config.ToolOverlay
	Secrets    []SecretField
	Disclaimer string
}

// Catalog is the closed built-in preset list. It is exported so command help,
// tests, and docs generators can inspect the shipped templates without
// duplicating data.
//
// DO NOT mutate this slice at runtime. It is package-level only because Go has
// no read-only slice literals; treat it as constant, mirroring adapter owned-key
// allowlist vars.
var Catalog = []Preset{
	{
		Name:        "deepseek",
		DisplayName: "DeepSeek",
		ProviderKey: "deepseek",
		BaseURL:     "https://api.deepseek.com",
		Model:       "deepseek-v4",
		Tools: map[config.ToolID]config.ToolOverlay{
			config.ToolCodex: codexOverlay("deepseek", "DeepSeek", "https://api.deepseek.com", "deepseek-v4", "OPENAI_API_KEY"),
		},
		Secrets:    apiKeySecret(),
		Disclaimer: noOfficialSupportDisclaimer,
	},
	{
		Name:        "glm",
		DisplayName: "GLM",
		ProviderKey: "glm",
		BaseURL:     "https://open.bigmodel.cn/api/paas/v4",
		Model:       "glm-4.5",
		Tools: map[config.ToolID]config.ToolOverlay{
			config.ToolCodex: codexOverlay("glm", "GLM", "https://open.bigmodel.cn/api/paas/v4", "glm-4.5", "OPENAI_API_KEY"),
		},
		Secrets:    apiKeySecret(),
		Disclaimer: noOfficialSupportDisclaimer,
	},
	{
		Name:        "moonshot",
		DisplayName: "Moonshot AI",
		ProviderKey: "moonshot",
		BaseURL:     "https://api.moonshot.cn/v1",
		Model:       "kimi-k2-0711-preview",
		Tools: map[config.ToolID]config.ToolOverlay{
			config.ToolCodex: codexOverlay("moonshot", "Moonshot AI", "https://api.moonshot.cn/v1", "kimi-k2-0711-preview", "OPENAI_API_KEY"),
		},
		Secrets:    apiKeySecret(),
		Disclaimer: noOfficialSupportDisclaimer,
	},
	{
		Name:        "qwen",
		DisplayName: "Qwen",
		ProviderKey: "qwen",
		BaseURL:     "https://dashscope.aliyuncs.com/compatible-mode/v1",
		Model:       "qwen3-coder-plus",
		Tools: map[config.ToolID]config.ToolOverlay{
			config.ToolCodex: codexOverlay("qwen", "Qwen", "https://dashscope.aliyuncs.com/compatible-mode/v1", "qwen3-coder-plus", "OPENAI_API_KEY"),
		},
		Secrets:    apiKeySecret(),
		Disclaimer: noOfficialSupportDisclaimer,
	},
}

// Lookup resolves user input case-insensitively and returns a defensive copy.
func Lookup(name string) (Preset, error) {
	needle := strings.ToLower(strings.TrimSpace(name))
	for _, p := range Catalog {
		if p.Name == needle {
			return clonePreset(p), nil
		}
	}
	return Preset{}, fmt.Errorf("unknown preset %q (available presets: %s)", name, strings.Join(Names(), ", "))
}

// Names returns canonical preset names in display order.
func Names() []string {
	out := make([]string, len(Catalog))
	for i, p := range Catalog {
		out[i] = p.Name
	}
	return out
}

func init() {
	names := make([]string, 0, len(Catalog))
	seen := make(map[string]struct{}, len(Catalog))
	for _, p := range Catalog {
		if p.Name == "" {
			panic("presets.Catalog: preset name cannot be empty")
		}
		if p.Name != strings.ToLower(p.Name) {
			panic("presets.Catalog: preset names must be lowercase")
		}
		if _, dup := seen[p.Name]; dup {
			panic("presets.Catalog: duplicate preset name: " + p.Name)
		}
		seen[p.Name] = struct{}{}
		names = append(names, p.Name)
		if p.DisplayName == "" || p.ProviderKey == "" || p.BaseURL == "" || p.Model == "" {
			panic("presets.Catalog: preset " + p.Name + " is missing required fields")
		}
		if p.ProviderKey != p.Name {
			panic("presets.Catalog: preset " + p.Name + " provider key must match canonical name")
		}
		if p.Disclaimer == "" {
			panic("presets.Catalog: preset " + p.Name + " is missing disclaimer")
		}
		for _, secret := range p.Secrets {
			if strings.Contains(strings.ToLower(secret.Name), "sk-") ||
				strings.Contains(strings.ToLower(secret.Description), "sk-") {
				panic("presets.Catalog: preset " + p.Name + " secret metadata contains a token-looking value")
			}
		}
	}
	if !sort.StringsAreSorted(names) {
		panic("presets.Catalog: preset names must be sorted")
	}
}

func apiKeySecret() []SecretField {
	return []SecretField{{
		Name:        "api_key",
		Description: "API key supplied by the user through claudecm add",
	}}
}

func codexOverlay(provider, displayName, baseURL, model, envKey string) config.ToolOverlay {
	return config.ToolOverlay{
		Raw: map[string]any{
			"model":          model,
			"model_provider": provider,
			"model_providers." + provider + ".base_url": baseURL,
			"model_providers." + provider + ".env_key":  envKey,
			"model_providers." + provider + ".name":     displayName,
			"model_providers." + provider + ".wire_api": "chat",
		},
	}
}

func clonePreset(p Preset) Preset {
	out := p
	if p.Tools != nil {
		out.Tools = make(map[config.ToolID]config.ToolOverlay, len(p.Tools))
		for id, ov := range p.Tools {
			out.Tools[id] = cloneOverlay(ov)
		}
	}
	if p.Secrets != nil {
		out.Secrets = make([]SecretField, len(p.Secrets))
		copy(out.Secrets, p.Secrets)
	}
	return out
}

func cloneOverlay(ov config.ToolOverlay) config.ToolOverlay {
	out := config.ToolOverlay{
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
