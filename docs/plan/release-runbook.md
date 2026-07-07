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
- Review release notes for v1.0 scope boundaries: local-first profile switching for Claude Code and Codex CLI; plaintext `0600` local storage; no cloud, MCP, GUI, Gemini, or encryption-at-rest claims.

## Tag And Publish

1. Check out the green `main` commit selected for release.
2. Create the tag: `git tag v1.0.0`.
3. Push the tag: `git push origin v1.0.0`.
4. Watch the `Release` GitHub Actions workflow.
5. Confirm the workflow runs tests, the release smoke script, GoReleaser validation/build, checksum generation, and GitHub Release publication.
6. Verify the published release has archives for linux/darwin x amd64/arm64, `checksums.txt`, `LICENSE`, and `README.md`.
7. Download one artifact and run `claudecm version` as a package-manager smoke test.

## Rollback Or Retry

If the workflow fails before publishing a usable release:

1. Delete the local tag if present: `git tag -d v1.0.0`.
2. Delete the remote tag: `git push origin :refs/tags/v1.0.0`.
3. Delete any partial GitHub Release or draft for `v1.0.0` so the next attempt is unambiguous.
4. Fix the issue on `main` through the normal PR process.
5. Re-run the preflight checklist.
6. Recreate and push the tag: `git tag v1.0.0 && git push origin v1.0.0`.
