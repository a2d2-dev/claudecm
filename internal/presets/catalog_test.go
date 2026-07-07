package presets

import (
	"strings"
	"testing"

	"github.com/a2d2-dev/claudecm/internal/config"
)

func TestLookup_HappyBuiltInsExpandToProfileDraftData(t *testing.T) {
	for _, name := range []string{"deepseek", "glm", "moonshot", "qwen"} {
		p, err := Lookup(name)
		if err != nil {
			t.Fatalf("Lookup(%q): %v", name, err)
		}
		if p.Name != name {
			t.Fatalf("Lookup(%q).Name = %q", name, p.Name)
		}
		if p.ProviderKey == "" || p.BaseURL == "" || p.Model == "" {
			t.Fatalf("Lookup(%q) missing required non-secret fields: %+v", name, p)
		}
		if p.ProviderKey != name {
			t.Fatalf("Lookup(%q).ProviderKey = %q, want %q", name, p.ProviderKey, name)
		}
		if len(p.Secrets) != 1 || p.Secrets[0].Name != "api_key" {
			t.Fatalf("Lookup(%q).Secrets = %+v, want api_key only", name, p.Secrets)
		}
		if !strings.Contains(p.Disclaimer, "not official provider support") {
			t.Fatalf("Lookup(%q).Disclaimer missing no-official-support boundary: %q", name, p.Disclaimer)
		}
		codex, ok := p.Tools[config.ToolCodex]
		if !ok {
			t.Fatalf("Lookup(%q) missing codex overlay", name)
		}
		if got := codex.Raw["model"]; got != p.Model {
			t.Fatalf("Lookup(%q) codex raw model = %v, want %q", name, got, p.Model)
		}
		if got := codex.Raw["model_provider"]; got != p.ProviderKey {
			t.Fatalf("Lookup(%q) codex raw model_provider = %v, want %q", name, got, p.ProviderKey)
		}
		if got := codex.Raw["model_providers."+name+".base_url"]; got != p.BaseURL {
			t.Fatalf("Lookup(%q) codex raw base_url = %v, want %q", name, got, p.BaseURL)
		}
	}
}

func TestLookup_CaseInsensitiveCanonicalizes(t *testing.T) {
	p, err := Lookup("MoonShot")
	if err != nil {
		t.Fatalf("Lookup(MoonShot): %v", err)
	}
	if p.Name != "moonshot" {
		t.Fatalf("Name = %q, want moonshot", p.Name)
	}
}

func TestLookup_UnknownListsAvailable(t *testing.T) {
	_, err := Lookup("unknown")
	if err == nil {
		t.Fatalf("Lookup(unknown) = nil error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "available presets: deepseek, glm, moonshot, qwen") {
		t.Fatalf("error did not list presets: %v", err)
	}
}

func TestCatalogContainsNoSecretValues(t *testing.T) {
	for _, p := range Catalog {
		check := map[string]string{
			"display_name": p.DisplayName,
			"provider_key": p.ProviderKey,
			"base_url":     p.BaseURL,
			"model":        p.Model,
			"disclaimer":   p.Disclaimer,
		}
		for k, v := range check {
			if strings.Contains(strings.ToLower(v), "sk-") {
				t.Fatalf("preset %q field %s contains token-looking value %q", p.Name, k, v)
			}
		}
		for tool, ov := range p.Tools {
			if strings.Contains(strings.ToLower(ov.APIKey), "sk-") {
				t.Fatalf("preset %q tool %s carries API key %q", p.Name, tool, ov.APIKey)
			}
			for k, v := range ov.ExtraEnv {
				if strings.Contains(strings.ToLower(v), "sk-") {
					t.Fatalf("preset %q tool %s extra_env %s contains token-looking value", p.Name, tool, k)
				}
			}
			for k, v := range ov.Raw {
				if s, ok := v.(string); ok && strings.Contains(strings.ToLower(s), "sk-") {
					t.Fatalf("preset %q tool %s raw %s contains token-looking value", p.Name, tool, k)
				}
			}
		}
	}
}

func TestLookupReturnsDefensiveCopy(t *testing.T) {
	p, err := Lookup("qwen")
	if err != nil {
		t.Fatalf("Lookup(qwen): %v", err)
	}
	p.Tools[config.ToolCodex].Raw["model"] = "mutated"

	again, err := Lookup("qwen")
	if err != nil {
		t.Fatalf("Lookup(qwen) again: %v", err)
	}
	if got := again.Tools[config.ToolCodex].Raw["model"]; got == "mutated" {
		t.Fatalf("Lookup returned shared mutable overlay map")
	}
}
