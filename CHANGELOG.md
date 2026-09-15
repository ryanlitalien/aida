# Changelog

This project versions from `v1.0.0`. Every PR that merges to `main` adds its entry under a heading for the version it will become once merged, computed as the latest `v1+` tag's minor plus one (`v1.0.0` if no `v1+` tag exists yet). CI's `changelog` job enforces this: a PR fails unless `CHANGELOG.md` changed and contains a `## vX.Y.0` heading matching that computed next version, followed by at least one `- ` bullet.

If another PR merges first and claims the version heading you were targeting, rebase onto `main` and bump your heading to the new next version before merging.

On every merge to `main`, the `release` job re-computes the same next version, tags it, and publishes a GitHub release using that version's changelog section as the release notes.

## v1.0.0

- Initial public release, cut from the private development repo.
- Main is now PR-only with a changelog gate: every PR must add an entry under the next version heading.
- Every merge to main automatically bumps the minor version and publishes a GitHub release from that changelog section.
