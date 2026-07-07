# v1.0.0 Release Runbook

This runbook is for the release engineer who owns the actual `v1.0.0` tag. Do not tag from a feature branch.

## Preflight Checklist

- Confirm `main` is current and green in CI.
- Confirm the working tree is clean.
- Confirm the tag is semantic and matches `v1.0.0`.
- Confirm the module path is `github.com/a2d2-dev/claudecm`.
- Run `go build ./...`, `go vet ./...`, and `go test ./...`.
- Run `bash scripts/release-smoke.sh` and confirm `claudecm version` prints the stamped version, commit, and build date.
- Run `goreleaser check` if GoReleaser is installed.
- Confirm the `TAP_GITHUB_TOKEN` repository secret is present and has write access to `a2d2-dev/homebrew-tap`.
- Review release notes for v1.0 scope boundaries: local-first profile switching for Claude Code and Codex CLI; plaintext `0600` local storage; no cloud, MCP, GUI, Gemini, or encryption-at-rest claims.

## Tag And Publish

1. Check out the green `main` commit selected for release.
2. Create the tag: `git tag v1.0.0`.
3. Push the tag: `git push origin v1.0.0`.
4. Watch the `Release` GitHub Actions workflow.
5. Confirm the workflow runs tests, the release smoke script, GoReleaser validation/build, checksum generation, and GitHub Release publication.
6. Verify the published release has archives for linux/darwin/windows x amd64/arm64, `checksums.txt`, `LICENSE`, and `README.md`.
7. Verify `a2d2-dev/homebrew-tap` received the Homebrew formula update for `claudecm`. The formula should point at GitHub Release artifact URLs for the tag, use checksums from `checksums.txt`, declare the MIT license, and install only the `claudecm` binary.
8. Verify `a2d2-dev/homebrew-tap` received the Scoop manifest update for `claudecm`. The manifest should point at the Windows amd64/arm64 GitHub Release zip artifacts, use checksums from `checksums.txt`, declare the MIT license, and expose only the `claudecm.exe` binary.
9. Run the Homebrew install smoke on macOS:

   ```bash
   brew tap a2d2-dev/tap
   brew install claudecm
   claudecm version
   claudecm --help
   ```

10. When a prior version exists, run `brew upgrade claudecm` and confirm `claudecm version` reports the new tag.
11. Run the Scoop install smoke on Windows:

   ```powershell
   scoop bucket add a2d2-dev https://github.com/a2d2-dev/homebrew-tap
   scoop install claudecm
   claudecm version
   claudecm --help
   ```

12. When a prior version exists, run `scoop update claudecm` and confirm `claudecm version` reports the new tag.

## Rollback Or Retry

If the workflow fails before publishing a usable release:

1. Delete the local tag if present: `git tag -d v1.0.0`.
2. Delete the remote tag: `git push origin :refs/tags/v1.0.0`.
3. Delete any partial GitHub Release or draft for `v1.0.0` so the next attempt is unambiguous.
4. Fix the issue on `main` through the normal PR process.
5. Re-run the preflight checklist.
6. Recreate and push the tag: `git tag v1.0.0 && git push origin v1.0.0`.

If a bad Homebrew formula or Scoop manifest was published after a usable GitHub Release:

1. Revert the bad package-manager commit in `a2d2-dev/homebrew-tap`.
2. Communicate the known-good version and advise users to `brew update && brew install claudecm` or `scoop update && scoop install claudecm` after the revert lands.
3. Fix the release configuration on `main` through the normal PR process before publishing another tag.
