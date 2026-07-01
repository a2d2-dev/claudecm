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
	"fmt"
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

// ErrPlanMismatch is returned by Apply when the WritePlan it was handed
// does not target one of this adapter's owned files. Defense in depth:
// Plan always produces WritePlans with Tool=ToolCodex and Target set to
// AuthPath(r) or ConfigPath(r), but the interface signature does not
// statically prevent a caller from handing us a Claude Code plan (or a
// hand-forged plan pointing elsewhere). Refusing loudly beats writing
// the wrong file. Symmetric with claudecode.ErrPlanMismatch.
var ErrPlanMismatch = errors.New("claudecm/codex: plan does not target codex config.toml or auth.json")

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

// Apply hands a single-file WritePlan to writepath.Apply so every byte
// this adapter emits goes through the FR-5 locked write-path. The
// adapter never opens the owned files for write itself — that would
// bypass the write-path pipeline (parse guard, backup, drift check,
// atomic rename, post-write reparse, auto-rollback) and violate
// coding-standards rule 1.
//
// Per-file dispatch. Codex owns TWO files (auth.json, config.toml) and
// Plan returns a slice of two WritePlans in auth-first order (see
// plan.go). Apply itself is per-file: it accepts a single WritePlan
// and hands it straight to writepath.Apply. Cross-file ordering and
// the two-phase commit that stages auth.json before config.toml is
// NOT this method's job — that lives in internal/commit (E7). This
// story deliberately produces the per-file piece only.
//
// Defense in depth. Plan always sets plan.Tool=ToolCodex and
// plan.Target to either AuthPath(r) or ConfigPath(r), but the
// Adapter interface signature does not statically prevent a caller
// from handing us a plan authored by another adapter or hand-forged
// to point elsewhere. Apply refuses any plan whose Tool or Target
// does not match this adapter's ownership with ErrPlanMismatch — the
// writepath will never see it.
//
// An empty plan.Tool is treated as a mismatch too. Plan sets Tool
// explicitly; an empty value at this boundary is a programming error
// upstream, and returning ErrPlanMismatch surfaces it loudly rather
// than silently claiming ownership of any target the caller supplied.
//
// Errors from writepath.Apply are wrapped with %w preserving errors.Is
// against all writepath sentinels. Any sentinel documented on
// writepath.Apply may surface here wrapped with codex context; the
// wrap preserves errors.Is. Including but not limited to:
//
//   - writepath.ErrLockTimeout
//   - writepath.ErrConcurrentEdit
//   - writepath.ErrParseFailed
//   - writepath.ErrOutsideHome
//   - writepath.ErrPostWriteReparse
//   - writepath.ErrRollback
//   - writepath.ErrRollbackFailed
//   - writepath.ErrDryRunUnownedTouched
//   - writepath.ErrBackupFailed
//   - writepath.ErrTargetExists
func (a *Adapter) Apply(ctx context.Context, r *storage.Resolver, plan writepath.WritePlan) (writepath.WriteReport, error) {
	// Refuse a Tool that does not identify this adapter. The empty
	// string is refused too — Plan always sets Tool; an empty Tool at
	// this boundary is a caller bug we prefer to surface loudly over
	// accepting the plan and letting writepath's ValidatePlan reject
	// it with a less specific error.
	if plan.Tool != string(adapter.ToolCodex) {
		return writepath.WriteReport{}, fmt.Errorf("%w: plan.Tool = %q, want %q", ErrPlanMismatch, plan.Tool, adapter.ToolCodex)
	}
	authPath := AuthPath(r)
	configPath := ConfigPath(r)
	if plan.Target != authPath && plan.Target != configPath {
		return writepath.WriteReport{}, fmt.Errorf("%w: plan.Target = %q, want one of {%q, %q}", ErrPlanMismatch, plan.Target, authPath, configPath)
	}
	report, err := writepath.Apply(ctx, r, plan)
	if err != nil {
		return report, fmt.Errorf("codex apply %q: %w", plan.Target, err)
	}
	return report, nil
}

// Project resolves the layered EffectiveView for Codex — walks both
// owned files (config.toml + auth.json) through the frozen precedence
// chain (BuiltInDefault < ProfileCore < ProfileOverlay <
// OnDiskToolConfig < EnvOverride) and emits one EffectiveField per
// owned key that any layer contributed to. Read-only. See project.go
// for the design notes and the env-var allowlist (NFR-E1) that gates
// which owned keys can be shadowed by process env.
//
// Returns ErrOutsideHome if either owned file is a symlink escaping
// HOME, ErrParseFailed if either exists but is malformed, or
// ctx.Err() if the caller cancelled before we touched the filesystem.
func (a *Adapter) Project(ctx context.Context, r *storage.Resolver, p config.Profile) (adapter.EffectiveView, error) {
	return a.projectFromProfile(ctx, r, p)
}

// init wires this adapter into adapter.DefaultRegistry so cmd/current
// and internal/commit find it via side-effect import.
func init() { adapter.Register(adapter.ToolCodex, New) }
