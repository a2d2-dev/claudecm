// Package adapter declares the contract every supported AI-coding-tool
// adapter implements, plus the shared value types that flow across the
// adapter boundary. This package is TYPES-ONLY. No adapter logic lives
// here — concrete implementations land in
// internal/adapter/claudecode/ (E3-S2..E3-S7) and internal/adapter/codex/
// (E4-S1..E4-S7).
//
// Authority. This package mirrors the interface defined in
// docs/architecture.md §3 and the tool-owned key allowlists frozen in
// PRD §4.7. Where this file disagrees with either, those win.
//
// V1-scope. ToolID is a typed string constrained to the two v1 tools
// only: "claude_code" and "codex" (ADR-0001 Decision 1, architecture
// §2.1). Adding a third tool post-v1 is a new adapter package plus a
// commit-order registration in internal/commit — with no changes to
// writepath, resolver, or this file's interface shape.
package adapter

import (
	"context"
	"sort"
	"sync"

	"github.com/a2d2-dev/claudecm/internal/config"
	"github.com/a2d2-dev/claudecm/internal/storage"
	"github.com/a2d2-dev/claudecm/internal/writepath"
)

// ToolID names a supported tool. It aliases config.ToolID so profiles,
// state, and adapter code all speak one type. v1-legal values are the
// two constants below (ADR-0001 Decision 1).
type ToolID = config.ToolID

// v1 ToolID values. Re-exported from internal/config so cmd/*, adapters,
// and tests can spell them without importing both packages.
const (
	ToolClaudeCode = config.ToolClaudeCode // "claude_code"
	ToolCodex      = config.ToolCodex      // "codex"
)

// Format identifies the parse/emit format of an owned file. The set is
// closed at v1: adding a new format is an ADR-scale change because it
// implies a new codec dependency (see architecture/tech-stack.md).
type Format string

// Supported file formats. FormatJSONC is a superset of FormatJSON for
// Claude Code's settings file, which may contain comments.
const (
	FormatJSON  Format = "json"
	FormatJSONC Format = "jsonc"
	FormatTOML  Format = "toml"
)

// Presence describes whether a tool appears installed / configured on
// this system. Populated by Adapter.Detect. Read-only, best-effort — the
// zero value means "unknown / not detected".
type Presence struct {
	// Installed is true when a signal for the tool's presence fired
	// (binary on PATH or a known config file exists). Best-effort.
	Installed bool

	// ConfigDir is the absolute path to the tool's config directory
	// when known (e.g. ~/.claude, ~/.codex). Empty if not detected.
	ConfigDir string

	// Files lists absolute paths of expected owned files that
	// currently exist on disk. Empty when nothing was found.
	Files []string

	// Version is a best-effort version string; may be empty.
	Version string

	// Detected mirrors Installed for callers that read Presence as a
	// yes/no signal without inspecting Files. When Detected is false
	// the other fields carry no information.
	Detected bool

	// Notes is a human-readable one-line detection detail, e.g.
	// "detected via ~/.claude/settings.json". Never contains secrets.
	Notes string
}

// OwnedFile describes one tool-owned file: its absolute path, format,
// and the frozen allowlist of fully-flattened key paths claudecm
// manages inside it. Everything outside OwnedKeys is preserved verbatim
// by the write-path (PRD §4.7 merge-preserve, coding-standards rule 4).
type OwnedFile struct {
	// Path is the absolute on-disk path. Constructed only through
	// internal/storage/paths.go (coding-standards rule 3).
	Path string

	// Format identifies the codec the write-path must use.
	Format Format

	// OwnedKeys is the frozen allowlist of fully-flattened key paths
	// this file owns. Exact-string match against the output of
	// writepath.Flatten. Wildcards and prefix matches are out of v1
	// scope. Adding an entry requires a code + fixture-matrix update.
	OwnedKeys []string

	// Optional is true when Adapter.Import can proceed with the file
	// missing (e.g. Codex ~/.codex/auth.json on a fresh install with
	// no OpenAI key configured yet).
	Optional bool
}

// Files is the ordered set of files an Adapter owns for its tool. The
// slice order is meaningful: it is the order in which two-phase commit
// (internal/commit, FR-16) will stage and rename entries.
type Files = []OwnedFile

// CoreFromTool is the core-shape intent an adapter infers when reading
// the tool's current on-disk state. Aliased to config.CoreConfig so
// import + edit + switch round-trips are byte-shape identical.
type CoreFromTool = config.CoreConfig

// OverlayFromTool is the tool-specific overlay an adapter infers from
// the current on-disk state. Aliased to config.ToolOverlay.
type OverlayFromTool = config.ToolOverlay

// WritePlan is re-exported from internal/writepath. Adapters produce
// WritePlan values; the write-path pipeline consumes them (FR-5).
type WritePlan = writepath.WritePlan

// ApplyReport is re-exported from internal/writepath (where it is
// spelled WriteReport). Named ApplyReport at the adapter boundary
// because it is what Adapter.Apply returns per file.
type ApplyReport = writepath.WriteReport

// Layer names the precedence layer a resolved value came from. Ordered
// lowest → highest precedence (architecture §6). Kept as strings rather
// than an iota int so `explain` output is stable across binary rebuilds
// and grep-friendly in operator logs.
type Layer string

// Precedence layers, low → high. EnvOverride wins.
const (
	LayerDefault     Layer = "default"
	LayerCore        Layer = "core"
	LayerOverlay     Layer = "overlay"
	LayerOnDisk      Layer = "on-disk"
	LayerEnvOverride Layer = "env"
)

// EffectiveField is one resolved config value plus its provenance.
// Populated by Adapter.Project and rendered by `current` / `explain`.
type EffectiveField struct {
	// Value is the resolved effective value. May be nil for unset
	// fields that still have a shadowed entry.
	Value any

	// WinningLayer is the layer this value came from.
	WinningLayer Layer

	// Source is a human-readable pointer to where the winning value
	// lives — env var name for LayerEnvOverride, absolute file path
	// (optionally suffixed with a JSON pointer) for LayerOnDisk,
	// "profile.overlay" / "profile.core" for the profile layers, and
	// "builtin-default" for the built-in default layer.
	Source string

	// Shadowed lists every lower-precedence layer that also carried
	// a value, ordered older → newer (i.e. same order as the Layer
	// precedence chain). Used by `explain`. Empty when nothing is
	// shadowed.
	Shadowed []ShadowedLayer
}

// ShadowedLayer records one lower-precedence layer that lost to a
// winning layer. Keeping value+source paired here means `explain` never
// has to reach back into raw config to render its diagnostic chain.
type ShadowedLayer struct {
	Layer  Layer
	Source string
	Value  any
}

// EffectiveView is the per-tool projection Adapter.Project returns. It
// is what `claudecm current` summarises and `claudecm explain` details.
// Read-only; no writes ever happen inside Project.
type EffectiveView struct {
	// Tool identifies which tool this view describes.
	Tool ToolID

	// Fields maps a flat key path (writepath.Flatten shape) to its
	// resolved effective field. Deterministic in `explain` output
	// because callers sort the keys before rendering.
	Fields map[string]EffectiveField

	// ExternalDriftDetected is true when any owned file's on-disk
	// SHA256 differs from state.LastAppliedPerTool[Tool].SHA256
	// (architecture §6.2). Never taken as an automatic action —
	// reported as a warning only.
	ExternalDriftDetected bool

	// ExternalDriftFile is the absolute path of the drifting file
	// when ExternalDriftDetected is true. Empty otherwise.
	ExternalDriftFile string
}

// Adapter is the contract every supported tool implements. Concrete
// adapters live under internal/adapter/<tool>/ and are opaque to
// cmd/*, resolver, and commit — those callers deal only with this
// interface plus the shared types above.
//
// References. PRD §4.7 (owned-key allowlists, merge-preserve
// semantics) and architecture.md §3 (adapter shape and per-tool
// package layout).
//
// Purity. Detect, Files, Import, Plan, and Project must be pure with
// respect to the tool-owned files on disk: they read, they never write.
// Apply is the ONLY method permitted to write, and every byte it emits
// MUST go through internal/writepath.Apply. Bypassing the write-path is
// a coding-standards rule 1 violation.
//
// Context. Methods that touch the filesystem accept a context.Context
// so cmd/* can propagate cancellation from signal handlers. Methods
// that are pure over their arguments (ID) omit it.
type Adapter interface {
	// ID names the tool this adapter targets. Must return one of the
	// two v1 ToolID constants above; adapters that return anything
	// else are rejected at Registry-registration time.
	ID() ToolID

	// Detect returns a best-effort Presence for this tool on this
	// system. Read-only. Never mutates on-disk state, never writes.
	Detect(ctx context.Context, r *storage.Resolver) (Presence, error)

	// Files returns the owned files this adapter manages, in the
	// order the two-phase commit must stage them (FR-16, architecture
	// §5 auth-first ordering). Each entry carries its owned-key
	// allowlist. Pure — no I/O.
	Files(r *storage.Resolver) Files

	// Import reads the current on-disk state of this tool's owned
	// files and produces (a) the core intent the user appears to be
	// running and (b) a candidate overlay capturing tool-specific
	// deviations. Refuses on parse failure per NFR-S1 (no silent
	// fallback rewriting). Read-only.
	Import(ctx context.Context, r *storage.Resolver) (CoreFromTool, OverlayFromTool, error)

	// Plan produces the ordered per-file WritePlan slice needed to
	// activate the given Profile for this tool. Pure — no writes.
	// Slice ordering matches Files() so downstream two-phase commit
	// stages entries in the right order.
	Plan(ctx context.Context, r *storage.Resolver, profile config.Profile) ([]WritePlan, error)

	// Apply activates a single WritePlan by handing it to
	// writepath.Apply. Adapters MUST NOT open files for writing
	// themselves; the write-path is the only legal path to disk for
	// owned files (coding-standards rule 1, PRD FR-5). Returns the
	// per-file ApplyReport. Multi-file activation is orchestrated by
	// internal/commit calling this once per plan.
	Apply(ctx context.Context, r *storage.Resolver, plan WritePlan) (ApplyReport, error)

	// Project resolves the layered EffectiveView for this tool given
	// a Profile plus current env and on-disk state. Read-only; used
	// by cmd/current and cmd/explain (FR-6, FR-7).
	Project(ctx context.Context, r *storage.Resolver, profile config.Profile) (EffectiveView, error)
}

// Registry maps a ToolID to a constructor for its Adapter. Sub-packages
// register themselves from init() so cmd/* need only import them for
// side effect. Get/List are safe for concurrent use after registration
// has finished; Register is expected to run before any concurrent
// reader appears (init-time), and panics on double-registration to
// match the http.Handle discipline — silent overwrite would let two
// adapters silently disagree on ownership of the same tool.
type Registry struct {
	mu    sync.RWMutex
	ctors map[ToolID]func() Adapter
}

// NewRegistry returns an empty Registry. Prefer the package-level
// DefaultRegistry unless writing tests that need isolation.
func NewRegistry() *Registry {
	return &Registry{ctors: make(map[ToolID]func() Adapter)}
}

// Register wires a constructor for the given ToolID. Panics if id is
// empty, ctor is nil, or id has already been registered. The panic on
// duplicate matches net/http's DefaultServeMux.Handle: silent overwrite
// would hide a real bug (two adapters claiming the same tool).
func (r *Registry) Register(id ToolID, ctor func() Adapter) {
	if id == "" {
		panic("adapter.Register: empty ToolID")
	}
	if ctor == nil {
		panic("adapter.Register: nil constructor for " + string(id))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.ctors[id]; dup {
		panic("adapter.Register: duplicate registration for ToolID " + string(id))
	}
	r.ctors[id] = ctor
}

// Get constructs a fresh Adapter for id. Returns (nil, false) when
// nothing is registered for that id. Callers that hold onto the
// returned Adapter get their own instance; adapters are expected to be
// cheap to construct.
func (r *Registry) Get(id ToolID) (Adapter, bool) {
	r.mu.RLock()
	ctor, ok := r.ctors[id]
	r.mu.RUnlock()
	if !ok {
		return nil, false
	}
	return ctor(), true
}

// List returns registered ToolIDs sorted lexicographically. Sort keeps
// `claudecm list` output stable across runs and across binaries built
// from the same tree.
func (r *Registry) List() []ToolID {
	r.mu.RLock()
	ids := make([]ToolID, 0, len(r.ctors))
	for id := range r.ctors {
		ids = append(ids, id)
	}
	r.mu.RUnlock()
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// DefaultRegistry is the process-wide Registry adapter sub-packages
// register into from init(). This is the ONE documented exception to
// coding-standards rule 12 (no package-level mutable state): the
// registry is written to only during init(), read from thereafter, and
// the panic-on-duplicate rule makes accidental overwrites impossible.
// The pattern matches Go stdlib practice (net/http.DefaultServeMux,
// database/sql.Register). If a caller wants isolation — tests do —
// build a fresh Registry via NewRegistry instead.
var DefaultRegistry = NewRegistry()

// Register is a package-level shortcut that delegates to
// DefaultRegistry.Register. Adapter packages should call this from
// init(), never at request time.
func Register(id ToolID, ctor func() Adapter) { DefaultRegistry.Register(id, ctor) }

// Get is a package-level shortcut for DefaultRegistry.Get.
func Get(id ToolID) (Adapter, bool) { return DefaultRegistry.Get(id) }

// List is a package-level shortcut for DefaultRegistry.List.
func List() []ToolID { return DefaultRegistry.List() }
