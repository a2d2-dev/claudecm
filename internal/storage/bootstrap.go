// Package storage — Bootstrap wires together the layout invariants declared in
// docs/architecture.md §2.2 and §8 for a fresh install. It is deliberately a
// standalone file (rather than folded into paths.go) because it composes three
// separate concerns — dir creation, mode enforcement, and idempotence — that
// each want their own test seams. Keeping paths.go pure (resolution only) and
// bootstrap.go side-effectful (mkdir + chmod) matches the coding-standards
// rule 3 gateway split: resolution is a value-type operation; bootstrap is an
// explicit action invoked at the cmd/* entry point.
//
// Bootstrap is NOT called from NewResolver / Default. Construction stays pure
// and side-effect-free (see coding-standards rule 12). Command entry points
// call Bootstrap(r) after constructing the Resolver and before any
// FileStorage operation. Tests that only exercise path resolution do not
// touch the filesystem.
//
// Bootstrap contract: SaveProfile / SaveState require Bootstrap to have been
// called; they refuse to write into a non-existent .claudecm/ tree. There is
// no lazy dir creation on the write path — the invariant "Bootstrap ran" is
// asserted, not repaired, so a forgotten Bootstrap fails loudly instead of
// silently creating a dir with the wrong mode.
package storage

import (
	"errors"
	"fmt"
	"os"
)

// bootstrapDirMode is the single source of truth for the layout directory
// mode. Every dir created by Bootstrap ends up at this mode; every dir found
// pre-existing at a looser mode gets chmod'd down to it. NFR-S4 declares the
// invariant; this constant makes it grep-able.
const bootstrapDirMode os.FileMode = 0700

// ErrSymlinkedSubdir is returned by Bootstrap when a would-be layout
// subdirectory (e.g. `~/.claudecm/profiles`) already exists as a symlink.
// A chmod on a symlink follows the link and would tighten permissions on
// whatever the attacker aimed the symlink at — potentially outside HOME.
// Refusing is the only safe move.
var ErrSymlinkedSubdir = errors.New("claudecm: layout subdir is a symlink; refusing to chmod")

// Bootstrap ensures the on-disk layout under ~/.claudecm/ exists at the modes
// the architecture requires. It is idempotent: two consecutive calls produce
// the same filesystem state, with no chmod drift on the second call.
//
// Concretely, Bootstrap guarantees after it returns nil:
//   - <HOME>/.claudecm/         exists, mode 0700
//   - <HOME>/.claudecm/profiles/ exists, mode 0700
//   - <HOME>/.claudecm/backups/  exists, mode 0700
//
// It does NOT create state.yaml (SaveState creates it lazily on first write;
// LoadState tolerates its absence) and does NOT create audit.log (retention
// creates it lazily on first prune). This keeps a fresh install's disk
// footprint minimal and matches the E1-S7 story's "no silent file creation"
// posture.
//
// If a directory already exists at a looser mode (e.g. 0755 from an umask 022
// first-run), Bootstrap tightens it to 0700 via os.Chmod. This is a real
// threat vector (see TestBootstrap_ExistingDirsWithLooseMode); ignoring it
// would silently violate NFR-S4.
//
// Bootstrap does not re-validate HOME; the Resolver constructor already
// validates HOME. Bootstrap validates and creates the .claudecm/ subdirs
// with mode 0700, refusing on symlinked subdirs.
func Bootstrap(r *Resolver) error {
	if r == nil {
		return fmt.Errorf("storage.Bootstrap: nil resolver")
	}
	for _, dir := range []string{
		r.ConfigDir(),
		r.ProfilesDir(),
		r.BackupsRoot(),
	} {
		if err := ensureDirMode(dir, bootstrapDirMode); err != nil {
			return fmt.Errorf("storage.Bootstrap: %w", err)
		}
	}
	return nil
}

// ensureDirMode is the shared mkdir-then-chmod primitive. It creates dir if
// missing, then re-asserts mode. To defeat a symlink attack where an attacker
// pre-plants `~/.claudecm/profiles -> /etc` and MkdirAll happily follows it,
// ensureDirMode uses os.Lstat (not os.Stat) after MkdirAll and refuses when
// the entry is a symlink — chmod on a symlink would follow the link and
// tighten permissions on whatever the attacker aimed at, potentially outside
// HOME. chmod is applied only when the observed mode differs from the target,
// preserving mtime on the happy path.
func ensureDirMode(dir string, mode os.FileMode) error {
	if err := os.MkdirAll(dir, mode); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	// os.Lstat (not os.Stat) so a symlink is reported as a symlink instead of
	// silently followed to its target's stat.
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("lstat %s: %w", dir, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s", ErrSymlinkedSubdir, dir)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s exists but is not a directory", dir)
	}
	if info.Mode().Perm() != mode.Perm() {
		if err := os.Chmod(dir, mode); err != nil {
			return fmt.Errorf("chmod %s to %#o: %w", dir, mode, err)
		}
	}
	return nil
}
