// Package storage is the single gateway for every filesystem path claudecm
// touches. Per docs/architecture/coding-standards.md rule 3, no other package
// may build absolute paths from user input; they must route through here.
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
	"sync"

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

	// ProfileFileExt is the on-disk extension for profile files.
	ProfileFileExt = ".yaml"

	// MaxProfileNameLen mirrors the {0,63} bound in the profile-name regex
	// (1 leading char + up to 63 continuation chars).
	MaxProfileNameLen = 64
)

// profileNameRegex is the frozen NFR-S5 profile-name allowlist. See
// docs/architecture.md §8 and docs/plan/stories/E1-S2.md.
var profileNameRegex = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

var (
	homeOverrideMu sync.RWMutex
	homeOverride   string // explicit --home; empty means "use os.UserHomeDir()"
)

// SetHomeOverride installs an explicit HOME override for path resolution.
// Passing "" clears the override. This is the hook that a future `--home`
// CLI flag will call from cmd/. Tests also use it to sandbox the path
// gateway under t.TempDir().
func SetHomeOverride(path string) {
	homeOverrideMu.Lock()
	homeOverride = path
	homeOverrideMu.Unlock()
}

// currentHomeOverride returns the currently-installed override, if any.
func currentHomeOverride() string {
	homeOverrideMu.RLock()
	defer homeOverrideMu.RUnlock()
	return homeOverride
}

// ValidateProfileName enforces the NFR-S5 profile-name allowlist plus a
// defense-in-depth Unicode-NFKC check that catches homoglyphs decoding to
// reserved names (".", ".."). Errors always name the specific reason.
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

// ResolveHome returns the effective HOME directory used by every path helper
// in this package. It honors an explicit override set via SetHomeOverride
// (populated by the future --home flag) and otherwise falls back to
// os.UserHomeDir().
//
// It refuses, with a clear error, any HOME that is: empty, "/", not an
// absolute path, not an existing directory, or (when the process is not
// running as root) is "/root" or is root-owned. This maps to NFR-S3.
func ResolveHome() (string, error) {
	raw := currentHomeOverride()
	source := "--home"
	if raw == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("HOME resolution failed: %w", err)
		}
		raw = h
		source = "$HOME"
	}
	if raw == "" {
		return "", fmt.Errorf("%s is empty; refuse to build any path from it", source)
	}
	if !filepath.IsAbs(raw) {
		return "", fmt.Errorf("%s %q must be an absolute path", source, raw)
	}
	cleaned := filepath.Clean(raw)
	if cleaned == "/" {
		return "", fmt.Errorf(`%s resolves to "/"; refuse to build any path from it`, source)
	}
	info, err := os.Stat(cleaned)
	if err != nil {
		return "", fmt.Errorf("%s %q is not accessible: %w", source, cleaned, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s %q is not a directory", source, cleaned)
	}
	if err := verifyHomeOwnership(cleaned, info); err != nil {
		return "", err
	}
	return cleaned, nil
}

// SafeToolConfigPath joins a HOME-relative path fragment onto the resolved
// HOME and verifies the result stays inside HOME after filepath.Clean.
// Adapter code will call this to build tool-config target paths without
// re-implementing the traversal-refusal logic. Absolute inputs are refused
// so callers cannot bypass HOME.
func SafeToolConfigPath(homeRelative string) (string, error) {
	if homeRelative == "" {
		return "", errors.New("tool config path is empty")
	}
	if filepath.IsAbs(homeRelative) {
		return "", fmt.Errorf("tool config path %q must be relative to $HOME", homeRelative)
	}
	if strings.ContainsRune(homeRelative, 0x00) {
		return "", fmt.Errorf("tool config path %q contains a NUL byte", homeRelative)
	}
	home, err := ResolveHome()
	if err != nil {
		return "", err
	}
	joined := filepath.Join(home, homeRelative)
	return ensureUnderHome(joined, home)
}

// ConfigDir returns the absolute path to `~/.claudecm/`.
func ConfigDir() (string, error) {
	home, err := ResolveHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ConfigDirName), nil
}

// ProfilesDir returns the absolute path to `~/.claudecm/profiles/`.
func ProfilesDir() (string, error) {
	cd, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cd, ProfilesDirName), nil
}

// ProfilePath returns the absolute on-disk path for a profile YAML file.
// It validates the name via ValidateProfileName and verifies the result
// stays under the resolved HOME.
func ProfilePath(name string) (string, error) {
	if err := ValidateProfileName(name); err != nil {
		return "", err
	}
	dir, err := ProfilesDir()
	if err != nil {
		return "", err
	}
	home, err := ResolveHome()
	if err != nil {
		return "", err
	}
	return ensureUnderHome(filepath.Join(dir, name+ProfileFileExt), home)
}

// StatePath returns the absolute path to `~/.claudecm/state.yaml`.
func StatePath() (string, error) {
	cd, err := ConfigDir()
	if err != nil {
		return "", err
	}
	home, err := ResolveHome()
	if err != nil {
		return "", err
	}
	return ensureUnderHome(filepath.Join(cd, StateFileName), home)
}

// BackupsRoot returns the absolute path to `~/.claudecm/backups/`.
func BackupsRoot() (string, error) {
	cd, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cd, BackupsDirName), nil
}

// BackupPath builds a per-(tool, file) backup path under BackupsRoot with a
// timestamp suffix. Every segment is validated against traversal and control
// characters; the final path is verified to stay under HOME.
func BackupPath(tool, filename, ts string) (string, error) {
	if err := validatePathSegment(tool, "tool"); err != nil {
		return "", err
	}
	if err := validatePathSegment(filename, "filename"); err != nil {
		return "", err
	}
	if err := validatePathSegment(ts, "timestamp"); err != nil {
		return "", err
	}
	root, err := BackupsRoot()
	if err != nil {
		return "", err
	}
	home, err := ResolveHome()
	if err != nil {
		return "", err
	}
	// backups/<tool>/<filename>.bak.<ts> matches the architecture §8 layout.
	return ensureUnderHome(filepath.Join(root, tool, filename+".bak."+ts), home)
}

// EnsureConfigDir creates the configuration directory structure if it does
// not exist, with the strict permissions declared in the architecture (dir
// 0700, files 0600).
func EnsureConfigDir() error {
	configDir, err := ConfigDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(configDir, 0700); err != nil {
		return err
	}
	profilesDir, err := ProfilesDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(profilesDir, 0700); err != nil {
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
