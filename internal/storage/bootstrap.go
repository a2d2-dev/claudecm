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
package storage

import (
	"fmt"
	"os"
)

// bootstrapDirMode is the single source of truth for the layout directory
// mode. Every dir created by Bootstrap ends up at this mode; every dir found
// pre-existing at a looser mode gets chmod'd down to it. NFR-S4 declares the
// invariant; this constant makes it grep-able.
const bootstrapDirMode os.FileMode = 0700

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
// Bootstrap does not re-validate HOME — the Resolver constructor
// (NewResolver / NewResolverWithHome) already refuses HOME=/, missing dirs,
// non-directory targets, and root-owned dirs when running non-root. Anything
// that made it into a *Resolver is safe to write into.
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
// missing, then re-asserts mode regardless of pre-existing state. The chmod
// is unconditional so a pre-existing 0755 directory is tightened to the
// requested mode on every call; on the happy path where the directory is
// already at mode, chmod is a no-op syscall and the second call is
// idempotent in observable behavior.
func ensureDirMode(dir string, mode os.FileMode) error {
	if err := os.MkdirAll(dir, mode); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("stat %s: %w", dir, err)
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
