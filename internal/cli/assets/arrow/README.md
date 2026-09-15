# Vendored: @arrow-js/core

Reactive UI runtime for the `/dashboard` and `/bifrost` pages. Vendored rather than
loaded from a CDN so the pages work offline - `aida serve` is a loopback daemon that
should not need internet to render its own UI, and a third-party runtime fetched at
page load is a supply-chain dependency we do not want in a local control plane.

| | |
|---|---|
| Package | `@arrow-js/core` |
| Version | 1.0.6 |
| License | MIT (`LICENSE.txt`, Copyright 2022 Justin Schroeder) |
| Upstream | https://github.com/standardagents/arrow-js |
| Docs | https://arrow-js.com |

## Layout

The npm dist is **two** ESM files, not one. `index.mjs` carries a relative import of
`./chunks/internal-DchK7S7v.mjs`, so the directory structure must be preserved - the
whole tree is `go:embed`ed and served at `/static/arrow/`, letting the browser resolve
that relative import itself. Flattening the tree or renaming the chunk breaks the import.

## Updating

```sh
V=1.0.6   # bump
curl -sL "https://cdn.jsdelivr.net/npm/@arrow-js/core@$V/dist/index.mjs" -o index.mjs
# the chunk filename is content-hashed and changes between releases - read it back out:
CHUNK=$(grep -o "chunks/[A-Za-z0-9-]*\.mjs" index.mjs | head -1)
mkdir -p chunks
curl -sL "https://cdn.jsdelivr.net/npm/@arrow-js/core@$V/dist/$CHUNK" -o "$CHUNK"
```

Then delete the stale chunk and update the version above. No build step, no npm install,
nothing added to CI.

Exports used by the pages: `reactive`, `html`, `watch`, `nextTick`.
