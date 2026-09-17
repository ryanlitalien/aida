# Installing Aida

The honest version: **building Aida needs only Go.** Everything else - 
an LLM key, an embeddings key, a voice stack - is a *runtime* dependency
of a specific feature, and every feature degrades gracefully without its
key rather than crashing. Read this file once, then let `aida init`'s
closing diagnostic tell you exactly which gaps still apply to you.

## Build-time requirements

| Requirement | Why |
|---|---|
| Go 1.25+ (go.mod pins 1.25.7; older toolchains auto-download it with GOTOOLCHAIN=auto) | The whole binary. `make build` / `make install` / `go build ./...` |
| `make` | Wraps `go build`/`go install` plus codesigning and cross-compiles |
| A C toolchain (cgo) | `github.com/mattn/go-sqlite3` and a couple of other cgo deps need it - Xcode Command Line Tools on macOS, `build-essential` on Linux |

That's it for `make build`. No Python, no Node, no external services, at
build time, on any platform.

```bash
git clone https://github.com/ryanlitalien/aida.git
cd aida
make install      # go install ./cmd/aida, code-signed, symlinked onto PATH
export PATH="$HOME/bin:$PATH"   # or $(go env GOPATH)/bin
aida init         # bootstrap ~/.aida/ from nothing
aida "hello"      # first query
```

Neither install path is on a stock shell's PATH by default - add that
`export` line to your shell rc file (`~/.zshrc`, `~/.bashrc`, etc) so it
persists across terminal sessions.

`make install` is the one to use day to day - see the codesigning note
in `CLAUDE.md` for why `make build` alone isn't enough if you're also
using the push-to-talk voice input. `make build-all` cross-compiles
`darwin-arm64`, `darwin-amd64`, and `linux-amd64` in one shot.

## Dependency matrix, per feature

Nothing below is required to build the binary. Each row is a *runtime*
key or tool that a specific feature needs; everything else in Aida keeps
working without it.

| Feature | Needs | Degrades to, if missing |
|---|---|---|
| Engine (parse/classify/plan/execute/synthesize) | `ANTHROPIC_API_KEY`, **or** a local Ollama model configured as the offline fallback | Fails the query with a clear "no key" error; no silent wrong answers |
| Brain semantic recall | `VOYAGE_API_KEY` | Keyword/FTS search and recency-based recall still work; only cosine-similarity recall is unavailable |
| Voice layer (Jarvis/Aida, `aida serve`) | whisper.cpp model + a TTS voice (bundled Piper, MIT-licensed, is the default) - see below | Voice commands are unavailable; the engine, brain, MCP server, and web UIs are unaffected |
| Voice layer, ElevenLabs upgrade | `ELEVENLABS_API_KEY` (optional) | Falls back to the bundled Piper voice |
| Web search adapter | `BRAVE_API_KEY` (optional) | Falls back to a DuckDuckGo-only search path |
| Models panel / `aida models` usage probes | Provider-specific: a `claude-oauth` keychain login, `LITELLM_MASTER_KEY` for a LiteLLM proxy, etc. - see `~/.aida/models.yaml` | That one provider's usage bars show "no key"; the rest of the roster still probes fine |
| Android client (LMD listener) | Tailscale on the same tailnet | `aida serve`'s HTTP/voice/MCP surfaces are unaffected; only the phone client can't reach it |

## Platform support

| Platform | Engine (CLI, MCP server, `aida loop`) | Voice layer (Jarvis/Aida) |
|---|---|---|
| macOS (arm64, amd64) | Full support | Full support |
| Linux (amd64) | Full support - `make build-all` cross-compiles it | Not supported |
| Android | N/A | N/A - separate client app talks to the engine over the LMD protocol (see `docs/notes/lmd-protocol.md`) |

The voice layer is macOS-only by construction, not by neglect: it shells
out to `afplay` (audio playback) and `osascript` (permission prompts and
notifications), and its speech-to-text path uses whisper.cpp compiled
with Metal acceleration. None of those have a portable equivalent this
project has taken on. The engine itself has no such dependency - it's
plain Go plus cgo for SQLite - so it cross-compiles cleanly to
`linux-amd64` for running the CLI or the MCP server headless on a server
or in CI.

## The key chain, honestly

There's a real dependency chain here and it's worth stating plainly
instead of discovering it one failed query at a time:

1. **An LLM.** Either `ANTHROPIC_API_KEY` (the default, and the only
   path that's been exercised end to end), or an offline Ollama model
   configured in `config.yaml` if you'd rather run fully local and free.
   Without either, the engine cannot parse a query at all.
2. **Voyage, for embeddings.** Optional but recommended - without it the
   brain still answers keyword and recency queries, it just can't do
   semantic ("find me something like...") recall over lessons and
   entities.
3. **Voice, only if you want it.** A whisper.cpp STT model (downloaded by
   `make jarvis-deps`) plus either the bundled MIT-licensed Piper voice
   (no key needed) or an ElevenLabs key for higher-quality streamed
   speech, plus `ffmpeg` for mic capture. All three are `make
   jarvis-deps`'s job; nothing here needs to be installed by hand.

`examples/env.example` lists every environment variable the binary
reads, each with an obviously-fake value (`sk-ant-EXAMPLE-not-a-real-key`)
and a comment on what it unlocks. `examples/op.env.example` is the same
idea for machines that pull secrets from a 1Password service account
instead of a plain `.env` file. `aida init` copies both into `~/.aida/`
as `.example` files - never as live config - and its closing diagnostic
reads your actual environment and tells you, per feature, whether you're
set up or what's missing.

## Backing up your config and memory

Say this part plainly, because getting it wrong is the one mistake here
that can't be undone: **`~/.aida/brain/` holds everything you have ever
told Aida.** Every task, every lesson from a thumbs-down, every entity
page, every captured coding-agent memory, every voice-turn audit log - 
all of it, in plain files, forever, unless you delete it yourself. It
must never point at a public git remote.

`~/.aida/` (config, library, routes, roster) and `~/.aida/brain/`
(everything above) are both plain directories on disk - nothing here is
a managed service you have to trust. `aida init` initializes the brain
directory as a local git repo automatically, and commits to it after
every query, so version history and point-in-time recovery are already
happening locally with zero setup.

If you keep a wiki (the optional L4 tier - see the README's memory
section), that's a third directory of the same kind: `wiki.path` in
`~/.aida/config.yaml`, default `~/dev/aida-wiki`.

To back up or sync any of them across machines, treat them like
dotfiles. A **private** git repository per directory is the lowest
friction route, and the one aida assists with - it gives you history
and point-in-time recovery, and `aida init` can set the remotes for
you. But nothing here requires git: these are plain directories of
plain files, so an rsync to a local server or NAS, a Time Machine
target, or whatever backup you already run works just as well.

```bash
# Config
cd ~/.aida && git init -q 2>/dev/null; git remote add origin <your-private-config-repo>
git add -A && git commit -m "initial config" && git push -u origin main

# Brain (already a local git repo - aida init did this for you)
cd ~/.aida/brain && git remote add origin <your-private-brain-repo>
git push -u origin main

# Wiki (optional - only if you use L4)
cd ~/dev/aida-wiki && git remote add origin <your-private-wiki-repo>
git push -u origin main
```

Or set the remotes at bootstrap time instead of by hand:

```bash
aida init --config-remote <your-private-config-repo> --brain-remote <your-private-brain-repo>
```

Either way: **private**, not public. There is no scrub pass that makes
a brain directory safe to publish - it is, by design, a record of
whatever you've said to it, and a wiki built from your own archives is
the same kind of material.
