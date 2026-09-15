# Contributing

## Running the build and tests

```bash
make build          # bin/aida (not on PATH - see the make install note below)
make install        # go install ./cmd/aida, code-signed, symlinked onto PATH
make test           # go test ./...
make fmt            # go fmt ./...
make vet            # go vet ./...
make oss-scan       # regression scan for secrets/PII patterns (also runs in CI)
```

**Use `make install`, not `make build`, when you actually want to run
`aida <command>` and see your change.** `make build` only produces
`bin/aida` inside the repo, which is never on `PATH`; `make install`
puts a code-signed binary at `$(go env GOPATH)/bin/aida` and symlinks
`~/bin/aida` to it. Running the old command after a code change and
getting old behavior back is a stale-install symptom, not a regression -
`make install` first.

Run a single test:

```bash
go test ./internal/engine/ -run TestClassifier
go test ./internal/config/ -run TestConfigRoundTrip
go test ./internal/engine/ -run TestGoldenRouting
```

Before opening a PR, all of these should be green:

```bash
go build ./... && go vet ./... && go test ./... && ./scripts/oss-scan.sh
```

## The golden-eval harness

`aida golden` and `scripts/run-goldens.sh` audit routing accuracy across
a question set. The harness is generic tooling and ships in this repo;
the *questions themselves* are user data and belong in `~/.aida/`, not
in a source-controlled fork. `aida init` seeds a small fictional set
from `examples/golden/` so the harness is runnable out of the box - 
`make golden` (or `./scripts/run-goldens.sh`) exercises that seed set
against `examples/sources/*.yaml`.

## Commit policy

Every commit is one logical, independently revertible change - **never
squash**, not on merge (`gh pr merge --merge`, not `--squash`) and not
while rebasing. If a single file has hunks for two unrelated changes,
split them across commits (stage one hunk with a patch and `git apply
--cached`) instead of bundling. History has to stay bisectable: any one
commit should be revertible without unpicking work that has nothing to
do with it.

## Where user data lives

Nothing personal belongs in this repo, ever - not in a test fixture, not
in a docs example, not in a "just for now" commit. All of it lives
outside version control here, under `~/.aida/`:

| Directory | What |
|---|---|
| `~/.aida/config.yaml`, `library/`, `roster.yaml` | Your sources, routes, and agent roster |
| `~/.aida/brain/` | Everything you've ever told Aida - lessons, tasks, entity pages, captured agent memories - auto-committed to its own local git repo after every query |
| `~/.aida/.env`, `~/.aida/op.env` | API keys and secrets |

See `INSTALL.md`'s "Backing up your config and memory" section before
pointing either directory at a remote - the brain in particular must
never point at a public one.

If you're adding a test fixture, an example, or a docs snippet: use
obviously-fake data (`sk-ant-EXAMPLE-not-a-real-key`, fictional names,
`/Users/fakehome/...` paths). `scripts/oss-scan.sh` checks for a list of
known-bad patterns in CI, but it's a regression net, not a substitute
for not writing real data down in the first place.
