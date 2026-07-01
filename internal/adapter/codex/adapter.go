// Package codex implements the Codex CLI adapter — owning
// ~/.codex/config.toml and ~/.codex/auth.json per PRD §4.7 and
// architecture.md §3.1.
//
// V1 scope: Detect + Files + owned-key allowlists have landed (E4-S1).
// Import / Plan / Apply / Project are stubbed here and will land in
// E4-S2..E4-S6 under the contract declared in
// internal/adapter/adapter.go.
package codex

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

// ErrNotImplemented is returned by adapter method stubs while their
// implementations land epic-by-epic. E4-S2..E4-S6 will remove each
// return-of-ErrNotImplemented in turn; the sentinel is retained until
// every method has shipped so any downstream errors.Is checks continue
// to compile through the epic.
var ErrNotImplemented = errors.New("claudecm: codex adapter method not implemented")

// binaryName is the CLI executable claudecm looks for on PATH when
// probing tool presence. Kept as a const so we do not carry
// package-level mutable state (coding-standards rule 12).
const binaryName = "codex"

// Adapter is the concrete Codex CLI adapter. It carries no state:
// every method takes the *storage.Resolver as a parameter, honouring
// coding-standards rule 3 (paths flow through storage only) and
// rule 12 (no package-level mutable state).
type Adapter struct{}

// New constructs a fresh Adapter. Returned as the interface type so the
// registry stores a homogenous constructor signature.
func New() adapter.Adapter { return &Adapter{} }

// ID names the tool this adapter targets.
func (a *Adapter) ID() adapter.ToolID { return adapter.ToolCodex }

// ConfigPath returns the absolute path to ~/.codex/config.toml.
// Exported so cmd/current, cmd/explain, and tests can reference the
// exact path this adapter owns without duplicating the join logic.
func ConfigPath(r *storage.Resolver) string {
	return filepath.Join(r.Home(), ".codex", "config.toml")
}

// AuthPath returns the absolute path to ~/.codex/auth.json. Exported
// for the same reasons as ConfigPath.
func AuthPath(r *storage.Resolver) string {
	return filepath.Join(r.Home(), ".codex", "auth.json")
}

// configDir returns the absolute path to ~/.codex/.
func configDir(r *storage.Resolver) string {
	return filepath.Join(r.Home(), ".codex")
}

// Files returns the owned files for Codex CLI.
//
// Slice ordering is meaningful: auth.json comes first. Two-phase commit
// (architecture §5, internal/commit, FR-16) stages entries in this
// order so a downstream config.toml failure leaves credentials
// self-consistent — the auth-first invariant is that Codex never ends
// up with a config referencing a provider whose credentials never
// landed on disk.
//
// Both files are Optional=true because a fresh Codex install may not
// yet have written either — the operator may only have OPENAI_API_KEY
// in env, or only a partial config.toml. Import proceeds with either
// missing rather than erroring out.
func (a *Adapter) Files(r *storage.Resolver) adapter.OwnedFiles {
	return adapter.OwnedFiles{
		{
			Path:      AuthPath(r),
			Format:    adapter.FormatJSON,
			OwnedKeys: OwnedKeysAuthJSON,
			Optional:  true,
		},
		{
			Path:      ConfigPath(r),
			Format:    adapter.FormatTOML,
			OwnedKeys: OwnedKeysConfigTOML,
			Optional:  true,
		},
	}
}

// Detect returns a best-effort Presence. Read-only. Signals considered:
//
//   - Does ~/.codex/ exist as a directory? (ConfigDir signal)
//   - Does ~/.codex/auth.json OR ~/.codex/config.toml exist? (Installed
//     signal per E4-S1 AC)
//   - Is `codex` on PATH? (secondary Installed signal)
//
// The Resolver has already validated HOME per NFR-S3 so this method
// does not re-check it. Errors from os.Stat other than IsNotExist are
// surfaced in Notes but do not fail Detect — a permission error on one
// probe should not mask the other signals.
//
// Symlinks are probed with Lstat first so Detect does not silently
// follow a link out of HOME. The write-path (E1-S3 / E2-S2) refuses to
// write through an out-of-HOME symlink, so if Detect followed the link
// and reported the file as owned, the operator would get "installed"
// here and a refusal at apply time. We annotate the note instead and
// omit the symlinked path from Presence.Files — the underlying target's
// existence still drives Detected/Installed. This mirrors the
// claudecode adapter's F3-regression handling in adapter.go.
func (a *Adapter) Detect(ctx context.Context, r *storage.Resolver) (adapter.Presence, error) {
	p := adapter.Presence{}

	// Honour ctx cancellation before any filesystem work. Detect is
	// fast but cmd/* still propagates signals through here.
	if err := ctx.Err(); err != nil {
		return p, err
	}

	// appendNote accumulates diagnostics into p.Notes instead of
	// overwriting. When the first probe surfaces a permission-denied
	// or "not a directory" diagnostic and a later probe finds the
	// owned file anyway, both signals must remain visible — the
	// original F4 code overwrote the earlier note and hid the shape.
	// Separator is "; " so the composite note reads as one sentence.
	// All call-sites pass non-empty literals; no empty-string guard.
	appendNote := func(note string) {
		if p.Notes == "" {
			p.Notes = note
			return
		}
		p.Notes = p.Notes + "; " + note
	}

	// dirClaimed tracks whether the config-dir signal fired. Only
	// probe the owned files if ~/.codex looks like a real directory —
	// stat'ing <file>/auth.json when ~/.codex is a file gives ENOTDIR
	// noise the operator does not need.
	dir := configDir(r)
	dirLstat, dirLstatErr := os.Lstat(dir)
	dirIsSymlink := dirLstatErr == nil && dirLstat.Mode()&os.ModeSymlink != 0
	dirInfo, dirErr := os.Stat(dir)
	dirClaimed := false
	switch {
	case dirErr == nil && dirInfo.IsDir():
		p.ConfigDir = dir
		p.Detected = true
		dirClaimed = true
		if dirIsSymlink {
			appendNote("warning: " + dir + " is a symlink; treated as present but activation will require symlink-aware writepath (E1-S3 semantics apply)")
		}
	case dirErr == nil:
		// ~/.codex exists but is not a directory. Do not claim
		// detection; leave a note so the operator can investigate.
		appendNote("found ~/.codex but it is not a directory")
	case errors.Is(dirErr, os.ErrNotExist):
		// Fall through — Notes populated below only if nothing else
		// fires.
	default:
		// Any other stat error (permissions, IO) — surface it in Notes
		// but do not fail Detect; the PATH probe below may still fire.
		appendNote("stat ~/.codex: " + dirErr.Error())
	}

	if dirClaimed {
		// Probe auth.json first so its presence populates Files in the
		// same auth-first order Files() advertises. Then probe
		// config.toml.
		for _, probe := range []struct {
			path  string
			label string
		}{
			{AuthPath(r), "auth.json"},
			{ConfigPath(r), "config.toml"},
		} {
			fileLstat, fileLstatErr := os.Lstat(probe.path)
			fileIsSymlink := fileLstatErr == nil && fileLstat.Mode()&os.ModeSymlink != 0
			fileInfo, fileErr := os.Stat(probe.path)
			switch {
			case fileErr == nil && !fileInfo.IsDir():
				p.Installed = true
				p.Detected = true
				if fileIsSymlink {
					// Do NOT add symlinked file to Files: the
					// write-path will refuse to write through an
					// out-of-HOME symlink, so Files must not promise
					// ownership of a path the writer will reject.
					appendNote("warning: " + probe.path + " is a symlink; treated as present but activation will require symlink-aware writepath (E1-S3 semantics apply)")
				} else {
					p.Files = append(p.Files, probe.path)
					appendNote("detected via " + probe.path)
				}
			case fileErr == nil:
				// A directory sitting at an owned-file path is
				// anomalous but not our bug to solve; report it and
				// do not treat as installed via this probe.
				appendNote("found " + probe.path + " but it is a directory")
			case errors.Is(fileErr, os.ErrNotExist):
				// Not installed via this file; keep probing.
			default:
				// Non-ErrNotExist error: leave a note, keep going. The
				// earlier probe's diagnostic (if any) survives via the
				// appendNote accumulator — the F4 fix.
				appendNote("stat " + probe.path + ": " + fileErr.Error())
			}
		}
	}

	// PATH probe — best-effort. LookPath returns exec.ErrNotFound for
	// misses; any other error is treated as "not found" rather than
	// bubbled up because a broken PATH entry should not fail Detect.
	if path, err := exec.LookPath(binaryName); err == nil {
		p.Installed = true
		p.Detected = true
		appendNote("detected via " + binaryName + " on PATH (" + path + ")")
	}

	if !p.Detected && p.Notes == "" {
		p.Notes = "no .codex directory, no config.toml/auth.json, no codex binary on PATH"
	}
	return p, nil
}

// Import reads ~/.codex/config.toml and ~/.codex/auth.json and
// returns the (CoreFromTool, OverlayFromTool) pair capturing the
// operator's current Codex configuration. See import.go for the
// design notes (two-file coordination, refuse-on-malformed,
// symlink-follow-in-HOME, OPENAI_API_KEY vs OAuth-bundle policy).
//
// Returns ErrNoConfig when BOTH files are absent, ErrParseFailed
// when either exists but is malformed, and ErrOutsideHome when
// either is a symlink escaping HOME. Body lives in import.go.
func (a *Adapter) Import(ctx context.Context, r *storage.Resolver) (adapter.CoreFromTool, adapter.OverlayFromTool, error) {
	return a.importFromCodex(ctx, r)
}

// Plan produces the ordered per-file WritePlan slice needed to
// activate a profile for Codex. Two WritePlans are returned in
// auth-first order (auth.json then config.toml) so the two-phase
// commit stages credentials before the config that references
// them. Body lives in plan.go.
//
// Special case: when the profile carries zero auth-related content
// AND the on-disk auth.json is missing or whitespace-only, only
// the config.toml plan is returned (length-1 slice). See
// plan.go file godoc.
func (a *Adapter) Plan(ctx context.Context, r *storage.Resolver, p config.Profile) ([]writepath.WritePlan, error) {
	return a.planFromProfile(ctx, r, p)
}

// Apply will hand a single-file WritePlan to writepath.Apply. Stubbed
// until E4-S5.
func (a *Adapter) Apply(ctx context.Context, r *storage.Resolver, plan writepath.WritePlan) (writepath.WriteReport, error) {
	return writepath.WriteReport{}, ErrNotImplemented
}

// Project will resolve the layered EffectiveView for Codex. Stubbed
// until E4-S6.
func (a *Adapter) Project(ctx context.Context, r *storage.Resolver, p config.Profile) (adapter.EffectiveView, error) {
	return adapter.EffectiveView{}, ErrNotImplemented
}

// init wires this adapter into adapter.DefaultRegistry so cmd/current
// and internal/commit find it via side-effect import.
func init() { adapter.Register(adapter.ToolCodex, New) }
