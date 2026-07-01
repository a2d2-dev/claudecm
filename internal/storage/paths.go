// Package storage is the single gateway for every filesystem path claudecm
// touches. Per docs/architecture/coding-standards.md rule 3, no other package
// may build absolute paths from user input; they must route through here.
//
// Path resolution is provided by a value-type Resolver: construct one at the
// entry point (cmd/*) and thread it downstream. There is no package-level
// mutable state — rule 12 of the coding standards forbids it. Tests build
// their own Resolver via NewResolverWithHome(t.TempDir()); a future --home
// CLI flag will do the same.
//
// This file encodes the NFR-S3 (HOME sanity) and NFR-S5 (profile-name regex)
// invariants declared in docs/prd/prd-v1.md and docs/architecture.md §8.
package storage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/text/unicode/norm"
)

const (
	// ConfigDirName is the directory name for claudecm configuration.
	ConfigDirName = ".claudecm"

	// ProfilesDirName is the subdirectory for profile files.
	ProfilesDirName = "profiles"

	// BackupsDirName is the subdirectory for tool-config backups.
	BackupsDirName = "backups"

	// StateFileName is the state file name.
	StateFileName = "state.yaml"

	// AuditLogFileName is the retention audit log filename. Docs and code
	// agree on `~/.claudecm/audit.log` — see docs/architecture.md §retention
	// and docs/architecture/coding-standards.md rule 10.
	AuditLogFileName = "audit.log"

	// ProfileFileExt is the on-disk extension for profile files.
	ProfileFileExt = ".yaml"

	// MaxProfileNameLen mirrors the {0,63} bound in the profile-name regex
	// (1 leading char + up to 63 continuation chars).
	MaxProfileNameLen = 64
)

// profileNameRegex is the frozen NFR-S5 profile-name allowlist. See
// docs/architecture.md §8 and docs/plan/stories/E1-S2.md.
var profileNameRegex = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// ValidateProfileName enforces the NFR-S5 profile-name allowlist plus a
// defense-in-depth Unicode-NFKC check that catches homoglyphs decoding to
// reserved names (".", ".."). Errors always name the specific reason. This
// is a pure function on the input string and has no HOME dependency, so it
// lives at the package level instead of on *Resolver.
func ValidateProfileName(name string) error {
	if name == "" {
		return errors.New("profile name is empty")
	}
	// Defense in depth: NFKC-normalize before comparing against reserved
	// filesystem names. This blocks Unicode variants like fullwidth "．"
	// that would otherwise slip past a naive "==" check even though the
	// ASCII regex would also reject them below.
	normalized := norm.NFKC.String(name)
	if normalized == "." || normalized == ".." {
		return fmt.Errorf("profile name %q is a reserved filesystem name", name)
	}
	if strings.ContainsRune(name, 0x00) {
		return fmt.Errorf("profile name %q contains a NUL byte", name)
	}
	if strings.ContainsAny(name, "\n\r\t") {
		return fmt.Errorf("profile name %q contains a control character", name)
	}
	if strings.ContainsAny(name, "/\\") {
		return fmt.Errorf("profile name %q contains a path separator", name)
	}
	if len(name) > MaxProfileNameLen {
		return fmt.Errorf("profile name %q exceeds %d characters", name, MaxProfileNameLen)
	}
	if !profileNameRegex.MatchString(name) {
		return fmt.Errorf("profile name %q must match %s", name, profileNameRegex.String())
	}
	return nil
}

// Resolver owns a fully-validated HOME directory and produces every absolute
// path claudecm needs. It is immutable after construction and carries no
// package-level state. Construct one per command invocation (see Default)
// and thread it through to callers.
type Resolver struct {
	home string // absolute, cleaned, verified by NFR-S3
}

// NewResolver builds a Resolver from the real process HOME (os.UserHomeDir).
// The resolved directory is validated per NFR-S3: non-empty, absolute, not
// "/", exists, is a directory, and — when running as non-root — is neither
// "/root" nor root-owned.
func NewResolver() (*Resolver, error) {
	return newResolver("", "$HOME")
}

// NewResolverWithHome builds a Resolver from an explicit HOME string. This is
// the entry point that tests use to sandbox the path gateway under t.TempDir,
// and that a future --home CLI flag will call from cmd/*.
func NewResolverWithHome(home string) (*Resolver, error) {
	return newResolver(home, "--home")
}

// Default is a construction convenience for cmd/*: it returns a Resolver
// bound to the real process HOME. It is a thin wrapper over NewResolver so
// command entry points stay one-liners; it is NOT global state.
func Default() (*Resolver, error) {
	return NewResolver()
}

// newResolver is the shared HOME-validation core used by NewResolver and
// NewResolverWithHome. When raw is empty it falls back to os.UserHomeDir().
// source is used only in error messages so operators can tell which input
// tripped which check.
func newResolver(raw, source string) (*Resolver, error) {
	if raw == "" && source == "$HOME" {
		h, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("HOME resolution failed: %w", err)
		}
		raw = h
	}
	if raw == "" {
		return nil, fmt.Errorf("%s is empty; refuse to build any path from it", source)
	}
	if !filepath.IsAbs(raw) {
		return nil, fmt.Errorf("%s %q must be an absolute path", source, raw)
	}
	cleaned := filepath.Clean(raw)
	if cleaned == "/" {
		return nil, fmt.Errorf(`%s resolves to "/"; refuse to build any path from it`, source)
	}
	info, err := os.Stat(cleaned)
	if err != nil {
		return nil, fmt.Errorf("%s %q is not accessible: %w", source, cleaned, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s %q is not a directory", source, cleaned)
	}
	if err := verifyHomeOwnership(cleaned, info); err != nil {
		return nil, err
	}
	return &Resolver{home: cleaned}, nil
}

// Home returns the resolved, cleaned HOME directory this Resolver was built
// with. It is always absolute and always passes NFR-S3.
func (r *Resolver) Home() string { return r.home }

// ConfigDir returns the absolute path to `<HOME>/.claudecm/`, lexically under
// HOME after filepath.Clean; symlink resolution is deferred to writepath
// (FR-5 / NFR-S2).
func (r *Resolver) ConfigDir() string {
	return filepath.Join(r.home, ConfigDirName)
}

// ProfilesDir returns the absolute path to `<HOME>/.claudecm/profiles/`,
// lexically under HOME after filepath.Clean; symlink resolution is deferred
// to writepath (FR-5 / NFR-S2).
func (r *Resolver) ProfilesDir() string {
	return filepath.Join(r.ConfigDir(), ProfilesDirName)
}

// ProfilePath returns the absolute on-disk path for a profile YAML file. It
// validates the name via ValidateProfileName and verifies the result stays
// lexically under HOME after filepath.Clean; symlink resolution is deferred
// to writepath (FR-5 / NFR-S2).
func (r *Resolver) ProfilePath(name string) (string, error) {
	if err := ValidateProfileName(name); err != nil {
		return "", err
	}
	return ensureUnderHome(filepath.Join(r.ProfilesDir(), name+ProfileFileExt), r.home)
}

// StatePath returns the absolute path to `<HOME>/.claudecm/state.yaml`,
// verified lexically under HOME after filepath.Clean; symlink resolution is
// deferred to writepath (FR-5 / NFR-S2).
func (r *Resolver) StatePath() (string, error) {
	return ensureUnderHome(filepath.Join(r.ConfigDir(), StateFileName), r.home)
}

// AuditLogPath returns the absolute path to `<HOME>/.claudecm/audit.log`.
// This is the retention pruning audit log (mode 0600, append-only, one line
// per pruned backup) documented in docs/architecture.md §retention and
// docs/architecture/coding-standards.md rule 10. The path is lexically under
// HOME after filepath.Clean; symlink resolution is deferred to the caller
// (see internal/storage/retention.go), symmetric with StatePath / ProfilePath.
func (r *Resolver) AuditLogPath() string {
	return filepath.Join(r.ConfigDir(), AuditLogFileName)
}

// BackupsRoot returns the absolute path to `<HOME>/.claudecm/backups/`,
// lexically under HOME after filepath.Clean; symlink resolution is deferred
// to writepath (FR-5 / NFR-S2).
func (r *Resolver) BackupsRoot() string {
	return filepath.Join(r.ConfigDir(), BackupsDirName)
}

// BackupPath builds a per-(tool, file) backup path under BackupsRoot with a
// timestamp suffix. Every segment is validated against traversal and control
// characters; the final path is verified lexically under HOME after
// filepath.Clean. Symlink resolution is deferred to writepath (FR-5 / NFR-S2).
func (r *Resolver) BackupPath(tool, filename, ts string) (string, error) {
	if err := validatePathSegment(tool, "tool"); err != nil {
		return "", err
	}
	if err := validatePathSegment(filename, "filename"); err != nil {
		return "", err
	}
	if err := validatePathSegment(ts, "timestamp"); err != nil {
		return "", err
	}
	// backups/<tool>/<filename>.bak.<ts> matches the architecture §8 layout.
	return ensureUnderHome(filepath.Join(r.BackupsRoot(), tool, filename+".bak."+ts), r.home)
}

// LexicalToolConfigPath joins a HOME-relative path fragment onto the resolved
// HOME and verifies the result stays inside HOME after filepath.Clean.
//
// Lexical-only. Does NOT resolve symlinks. Callers that write to the
// returned path must additionally use writepath's symlink-following escape
// check (E1-S3 / FR-5) — the pair of checks is what actually defeats a
// symlink that points outside HOME. Absolute inputs are refused so callers
// cannot bypass HOME.
func (r *Resolver) LexicalToolConfigPath(homeRelative string) (string, error) {
	if homeRelative == "" {
		return "", errors.New("tool config path is empty")
	}
	if filepath.IsAbs(homeRelative) {
		return "", fmt.Errorf("tool config path %q must be relative to $HOME", homeRelative)
	}
	if strings.ContainsRune(homeRelative, 0x00) {
		return "", fmt.Errorf("tool config path %q contains a NUL byte", homeRelative)
	}
	return ensureUnderHome(filepath.Join(r.home, homeRelative), r.home)
}

// EnsureConfigDir creates the configuration directory structure if it does
// not exist, with the strict permissions declared in the architecture (dir
// 0700, files 0600).
func (r *Resolver) EnsureConfigDir() error {
	if err := os.MkdirAll(r.ConfigDir(), 0700); err != nil {
		return err
	}
	if err := os.MkdirAll(r.ProfilesDir(), 0700); err != nil {
		return err
	}
	return nil
}

// validatePathSegment rejects empty, reserved, or unsafe path components
// before they are joined into an absolute path.
func validatePathSegment(seg, kind string) error {
	if seg == "" {
		return fmt.Errorf("%s is empty", kind)
	}
	if seg == "." || seg == ".." {
		return fmt.Errorf("%s %q is a reserved filesystem name", kind, seg)
	}
	if strings.ContainsRune(seg, 0x00) {
		return fmt.Errorf("%s %q contains a NUL byte", kind, seg)
	}
	if strings.ContainsAny(seg, "/\\\n\r\t") {
		return fmt.Errorf("%s %q contains a forbidden character", kind, seg)
	}
	return nil
}

// ensureUnderHome cleans p and verifies it stays inside home. The returned
// path is always cleaned and absolute.
func ensureUnderHome(p, home string) (string, error) {
	cleaned := filepath.Clean(p)
	if !filepath.IsAbs(cleaned) {
		return "", fmt.Errorf("path %q is not absolute", cleaned)
	}
	cleanedHome := filepath.Clean(home)
	if cleaned == cleanedHome {
		return cleaned, nil
	}
	prefix := cleanedHome
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	if !strings.HasPrefix(cleaned, prefix) {
		return "", fmt.Errorf("path %q escapes HOME %q", cleaned, cleanedHome)
	}
	return cleaned, nil
}
