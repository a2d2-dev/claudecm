package envextract_test

// extractor_test.go — coverage backfill for the pre-E5-S3
// ExtractCurrentEnv / ToProfile / HasAuthToken / getEnvWithDefault
// helpers. Those helpers exist because cmd/add still calls
// ExtractCurrentEnv for the brownfield bootstrap (see cmd/add.go);
// they are not part of the new Lookup/Snapshot/AllExtantMatching
// API but must stay covered so the package meets the ≥90%
// coverage bar E5-S3 sets on internal/envextract.

import (
	"testing"

	"github.com/a2d2-dev/claudecm/internal/envextract"
)

// TestExtractCurrentEnv_HappyValuesPropagate confirms every env-var
// the extractor reads is mapped to the matching ExtractedEnv field.
// BASE_URL falls back to the documented default when unset elsewhere;
// this test sets it explicitly so the default path is exercised in a
// sibling test below.
func TestExtractCurrentEnv_HappyValuesPropagate(t *testing.T) {
	t.Setenv("ANTHROPIC_BASE_URL", "https://base.example")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "tok")
	t.Setenv("ANTHROPIC_MODEL", "m")
	t.Setenv("ANTHROPIC_SMALL_FAST_MODEL", "sm")

	got := envextract.ExtractCurrentEnv()
	if got.BaseURL != "https://base.example" {
		t.Fatalf("BaseURL=%q, want https://base.example", got.BaseURL)
	}
	if got.AuthToken != "tok" {
		t.Fatalf("AuthToken=%q, want tok", got.AuthToken)
	}
	if got.Model != "m" {
		t.Fatalf("Model=%q, want m", got.Model)
	}
	if got.SmallFastModel != "sm" {
		t.Fatalf("SmallFastModel=%q, want sm", got.SmallFastModel)
	}
	if !got.HasAuthToken() {
		t.Fatalf("HasAuthToken=false, want true")
	}
}

// TestExtractCurrentEnv_DefaultBaseURL confirms the getEnvWithDefault
// fallback fires when ANTHROPIC_BASE_URL is empty.
func TestExtractCurrentEnv_DefaultBaseURL(t *testing.T) {
	t.Setenv("ANTHROPIC_BASE_URL", "")
	got := envextract.ExtractCurrentEnv()
	if got.BaseURL != "https://api.anthropic.com" {
		t.Fatalf("BaseURL=%q, want default https://api.anthropic.com", got.BaseURL)
	}
}

// TestHasAuthToken_False checks the negative branch (extractor with
// no AuthToken reports false).
func TestHasAuthToken_False(t *testing.T) {
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	got := envextract.ExtractCurrentEnv()
	if got.HasAuthToken() {
		t.Fatalf("HasAuthToken=true, want false")
	}
}

// TestToProfile_MapsFields walks the ToProfile mapping surface — the
// Core.BaseURL / Core.APIKey slots come from NewProfile, the
// Core.Model / Core.SmallFastModel slots are set inline. Confirms
// each slot lands where the extractor promises so a future refactor
// cannot silently reroute a value.
func TestToProfile_MapsFields(t *testing.T) {
	e := &envextract.ExtractedEnv{
		BaseURL:        "https://base.example",
		AuthToken:      "tok",
		Model:          "opus",
		SmallFastModel: "haiku",
	}
	p := e.ToProfile("prof")
	if p.Name != "prof" {
		t.Fatalf("Profile.Name=%q, want prof", p.Name)
	}
	if p.Core.BaseURL != "https://base.example" {
		t.Fatalf("Core.BaseURL=%q, want https://base.example", p.Core.BaseURL)
	}
	if p.Core.APIKey != "tok" {
		t.Fatalf("Core.APIKey=%q, want tok", p.Core.APIKey)
	}
	if p.Core.Model != "opus" {
		t.Fatalf("Core.Model=%q, want opus", p.Core.Model)
	}
	if p.Core.SmallFastModel != "haiku" {
		t.Fatalf("Core.SmallFastModel=%q, want haiku", p.Core.SmallFastModel)
	}
}
