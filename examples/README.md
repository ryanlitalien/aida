# examples

Worked, fictional files that `aida init` copies into a fresh `~/.aida/` so
the first query has something to route to. Nothing here points at a real
machine, person, or company - swap the paths and content for your own once
you've seen how the pieces fit together.

| File | What it is | What `aida init` does with it |
|---|---|---|
| `sources/codebase-example.yaml` | A `type: codebase` source - Aida reads its `context:` file (CLAUDE.md/README.md) and can grep/shell into the repo for deeper questions. | Copied into `~/.aida/library/sources/` |
| `sources/docs-example.yaml` | A `type: docs` source with `search: {mode: grep}` - full-text search over a docs tree too large to paste into context whole. | Copied into `~/.aida/library/sources/` |
| `sources/exec-example.yaml` | A `type: tool` source wired to shell out to a CLI (`exec.query`). Shows the required real-command-prefix shape (`aida lint` rejects a bare `{query}` template as a shell-injection risk). | Copied into `~/.aida/library/sources/` |
| `roster.yaml` | A worked example of Aida's dispatcher roster (call-signs → backends: a discovered subagent team, source-backed agents, an MCP-backed agent, a background job agent). | Copied to `~/.aida/roster.yaml` if you don't already have one; see CLAUDE.md's "Aida dispatcher + agent roster" section |
| `env.example` | Every environment variable the binary reads, with obviously-fake values and a comment on what each one unlocks. | Copied to `~/.aida/env.example` (never as a live `.env` - you fill in real values yourself) |
| `op.env.example` | The 1Password service-account token shape `~/.aida/op.env` expects, for machines that pull secrets from 1Password instead of a plain `.env`. | Copied to `~/.aida/op.env.example` |
| `golden/questions.txt`, `golden/expectations.yaml` | A small fictional golden-eval seed set exercising the three example sources above, in the shape `cmd/golden-log`/`scripts/run-goldens.sh` read. | Copied to `~/.aida/golden-questions.txt` / `~/.aida/golden-expectations.yaml` |

Real golden seeds are personal data and belong in your own config
repo (aida-config, or wherever you keep `~/.aida/` backed up), never in
this repo -- replace the fictional questions/expectations above with your
own real ones once you have real sources configured; `aida golden run`'s
own suite (`testdata/golden-queries.yaml`, a separate richer format with
routing + LLM-judge assertions) works the same way.

None of the source YAMLs will resolve to anything on your machine as shipped -
the paths (`~/dev/avengers-hq`, etc.) and the roster's discovered team are
illustrative. Edit `path:`/`entities:`/`exec.query:` to point at your own
codebases, docs, and tools, or delete the files you don't need. Run
`aida lint` after editing to catch misconfigurations before your first query.

`aida lint` reports each example's missing placeholder path as a warning,
not an error, so a fresh `aida init` + `aida lint` shows 0 errors out of
the box (it recognizes a source either by its `-example.yaml` file name or
an explicit `example: true` key) - `aida lint --strict` still fails on
these warnings like any other, so silence them the same way you'd silence
a real one: edit the source's `path:` to something real, or delete the
file.
