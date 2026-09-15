# Changelog

This project versions from `v1.0.0`. Every PR that merges to `main` adds its entry under a heading for the version it will become once merged, computed as the latest `v1+` tag's minor plus one (`v1.0.0` if no `v1+` tag exists yet). CI's `changelog` job enforces this: a PR fails unless `CHANGELOG.md` changed and contains a `## vX.Y.0` heading matching that computed next version, followed by at least one `- ` bullet.

If another PR merges first and claims the version heading you were targeting, rebase onto `main` and bump your heading to the new next version before merging.

On every merge to `main`, the `release` job re-computes the same next version, tags it, and publishes a GitHub release using that version's changelog section as the release notes.

## v1.2.0

- LMD (Android client) turns now default to whisper.cpp's tiny.en model instead of small.en, overridable via profile `serve.lmd_whisper_model`, since the LMD daemon may run on weak/headless hardware where small.en's transcription time is prohibitive.
- LMD turns are now instrumented per-stage (`stt_ms`/`llm_ms`/`tts_ms` in the `/lmd/v1/turn` response, a `📱 LMD turn: ...` log line, and an audit record tagged `source: "lmd"`), matching the desk-mic listener's observability.

## v1.1.0

- Plan doc status reflects the 2026-09-15 fresh-start cut.
- Dropped the @claude and auto-review workflows from the public repo; they need a secret this repo does not carry and would let any commenter start an agent run.

## v1.0.0

- Initial public release, cut from the private development repo.
- Main is now PR-only with a changelog gate: every PR must add an entry under the next version heading.
- Every merge to main automatically bumps the minor version and publishes a GitHub release from that changelog section.
