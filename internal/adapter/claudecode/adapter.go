// Package claudecode implements the Claude Code adapter — owning
// ~/.claude/settings.json (user scope only) per PRD §4.7 and
// architecture.md §3.1. Project-scope Claude Code settings files
// (the per-project ones under a project's local .claude directory)
// are explicitly out of v1 scope and are never referenced here.
//
// V1 scope (E3-S2): Detect + Files + the OwnedKeysSettingsJSON
// allowlist. Import / Plan / Apply / Project are declared but return
// ErrNotImplemented so this package compiles against the adapter.Adapter
// contract; the real implementations land in E3-S3..E3-S6 under the
// contract defined in internal/adapter/adapter.go.
package claudecode

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/a2d2-dev/claudecm/internal/adapter"
	"github.com/a2d2-dev/claudecm/internal/config"
	"github.com/a2d2-dev/claudecm/internal/storage"
	"github.com/a2d2-dev/claudecm/internal/writepath"
)

// ErrNotImplemented is returned by adapter methods this story does not
// yet ship (Import / Plan / Apply / Project). E3-S3..E3-S6 replace each
// stub with a real implementation. Sentinel so callers can errors.Is
// check without stringly-typed matching.
var ErrNotImplemented = errors.New("claudecm: claudecode adapter method not implemented")

// binaryName is the CLI executable claudecm looks for on PATH when
// probing tool presence. Named as a var (not const) so tests can swap
// it if they ever need to sandbox exec.LookPath — currently the tests
// t.Skip when the real binary is present or absent, so nothing swaps
// this, but keeping it a var future-proofs the seam.
var binaryName = "claude"

// Adapter is the concrete Claude Code adapter. It carries no state:
// every method takes the *storage.Resolver as a parameter, honouring
// coding-standards rule 3 (paths flow through storage only) and
// rule 12 (no package-level mutable state).
type Adapter struct{}

// New constructs a fresh Adapter. Returned as the interface type so the
// registry stores a homogenous constructor signature.
func New() adapter.Adapter { return &Adapter{} }

// ID names the tool this adapter targets.
func (a *Adapter) ID() adapter.ToolID { return adapter.ToolClaudeCode }

// SettingsPath returns the absolute path to ~/.claude/settings.json.
// Exported so cmd/current, cmd/explain, and tests can reference the
// exact path this adapter owns without duplicating the join logic.
func SettingsPath(r *storage.Resolver) string {
	return filepath.Join(r.Home(), ".claude", "settings.json")
}

// configDir returns the absolute path to ~/.claude/.
func configDir(r *storage.Resolver) string {
	return filepath.Join(r.Home(), ".claude")
}

// Files returns the owned files for Claude Code. V1 = user-scope
// settings only. Optional=true because a fresh Claude Code install may
// not have written the file yet; Import must proceed with the file
// missing rather than erroring out.
//
// Format is JSONC because Claude Code's real settings file may carry
// comments (PRD §4.7); the write-path's JSONC codec is a strict
// superset of JSON, so the choice does not restrict any real file.
func (a *Adapter) Files(r *storage.Resolver) adapter.OwnedFiles {
	return adapter.OwnedFiles{{
		Path:      SettingsPath(r),
		Format:    adapter.FormatJSONC,
		OwnedKeys: OwnedKeysSettingsJSON,
		Optional:  true,
	}}
}

// Detect returns a best-effort Presence. Read-only. Signals considered:
//
//   - Does ~/.claude/ exist as a directory? (ConfigDir signal)
//   - Does ~/.claude/settings.json exist? (Installed signal per E3-S2 AC)
//   - Is `claude` on PATH? (secondary Installed signal, mirrors kubecm's
//     "did the user actually install the tool" cross-check)
//
// The Resolver has already validated HOME per NFR-S3 so this method
// does not re-check it. Errors from os.Stat other than IsNotExist are
// surfaced — a permission error on ~/.claude is a real problem the
// operator should see, not something to silently paper over.
func (a *Adapter) Detect(ctx context.Context, r *storage.Resolver) (adapter.Presence, error) {
	p := adapter.Presence{}

	// Honour ctx cancellation before any filesystem work. Detect is
	// fast but cmd/* still propagates signals through here.
	if err := ctx.Err(); err != nil {
		return p, err
	}

	// dirClaimed tracks whether the config-dir signal fired. Only
	// probe settings.json if ~/.claude looks like a real directory —
	// stat'ing <file>/settings.json when ~/.claude is a file gives
	// ENOTDIR noise the operator doesn't need.
	dir := configDir(r)
	dirInfo, dirErr := os.Stat(dir)
	dirClaimed := false
	switch {
	case dirErr == nil && dirInfo.IsDir():
		p.ConfigDir = dir
		p.Detected = true
		dirClaimed = true
	case dirErr == nil:
		// ~/.claude exists but is not a directory. Do not claim
		// detection; leave a note so the operator can investigate.
		p.Notes = "found ~/.claude but it is not a directory"
	case errors.Is(dirErr, os.ErrNotExist):
		// Fall through — Notes populated below only if nothing else
		// fires.
	default:
		// Any other stat error (permissions, IO) — surface it in Notes
		// but do not fail Detect; the PATH probe below may still fire.
		p.Notes = "stat ~/.claude: " + dirErr.Error()
	}

	if dirClaimed {
		settings := SettingsPath(r)
		settingsInfo, settingsErr := os.Stat(settings)
		switch {
		case settingsErr == nil && !settingsInfo.IsDir():
			p.Installed = true
			p.Detected = true
			p.Files = append(p.Files, settings)
			p.Notes = "detected via " + settings
		case settingsErr == nil:
			// A directory sitting at the settings.json path is
			// anomalous but not our bug to solve; report it and do
			// not treat as installed.
			p.Notes = "found " + settings + " but it is a directory"
		case errors.Is(settingsErr, os.ErrNotExist):
			// Not installed via config; may still be detected via PATH.
		default:
			// Non-ErrNotExist error: leave a note, keep going.
			p.Notes = "stat " + settings + ": " + settingsErr.Error()
		}
	}

	// PATH probe — best-effort. LookPath returns exec.ErrNotFound for
	// misses; any other error is treated as "not found" rather than
	// bubbled up because a broken PATH entry should not fail Detect.
	if path, err := exec.LookPath(binaryName); err == nil {
		p.Installed = true
		p.Detected = true
		if p.Notes == "" {
			p.Notes = "detected via " + binaryName + " on PATH (" + path + ")"
		}
	}

	if !p.Detected && p.Notes == "" {
		p.Notes = "no .claude directory, no settings.json, no claude binary on PATH"
	}
	return p, nil
}

// Import is a stub — E3-S3 replaces this with the real
// tool-config-to-profile importer.
func (a *Adapter) Import(ctx context.Context, r *storage.Resolver) (adapter.CoreFromTool, adapter.OverlayFromTool, error) {
	return adapter.CoreFromTool{}, adapter.OverlayFromTool{}, ErrNotImplemented
}

// Plan is a stub — E3-S4 replaces this with the real WritePlan
// generator.
func (a *Adapter) Plan(ctx context.Context, r *storage.Resolver, p config.Profile) ([]writepath.WritePlan, error) {
	return nil, ErrNotImplemented
}

// Apply is a stub — E3-S5 wires this to writepath.Apply.
func (a *Adapter) Apply(ctx context.Context, r *storage.Resolver, plan writepath.WritePlan) (writepath.WriteReport, error) {
	return writepath.WriteReport{}, ErrNotImplemented
}

// Project is a stub — E3-S6 ships the layered resolver projection.
func (a *Adapter) Project(ctx context.Context, r *storage.Resolver, p config.Profile) (adapter.EffectiveView, error) {
	return adapter.EffectiveView{}, ErrNotImplemented
}

// init wires this adapter into adapter.DefaultRegistry so cmd/current
// and internal/commit find it via side-effect import.
func init() { adapter.Register(adapter.ToolClaudeCode, New) }
