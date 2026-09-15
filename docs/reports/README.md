# Reports

Point-in-time HTML snapshots. Unlike the plans and design docs in `docs/`, these are
**dated observations, not living documents** - they record what was true on the day they
were taken and are not updated. Read the date in the filename before trusting a number.

| Report | Date | What it captures |
|---|---|---|
| [aida-system-map-2026-07-20.html](aida-system-map-2026-07-20.html) | 2026-07-20 | aida's own architecture and gap analysis: the six-step pipeline (four LLM calls, not the three the docs claim), runtime topology, the two self-improving loops, a subsystem scorecard, and gaps by theme. |

A companion home-LAN survey report lived here too but was removed from the public tree
(it dumped local network topology); the finding worth keeping is folded into the gap
analysis above.

## Why this lives here

It started as a one-off answer and turned out to be the only written-down view of its
subject. The live successor to parts of it is the `/dashboard` page on the `aida serve`
daemon - see `docs/plan-dashboard-bifrost.md`. It stays as the "before" reference and
for the narrative context a dashboard cannot carry.

## Mermaid diagrams

`aida-system-map-*.html` contains three `<pre class="mermaid">` blocks. The 3.2 MB inlined
mermaid runtime was stripped when the file was committed (it made up 3.2 MB of the
original 3.35 MB), so **the diagrams render as source, not pictures**, when opened
directly in a browser.

To view them rendered, either paste a block into <https://mermaid.live>, or use the
`mermaid` MCP server (`mermaid_preview`), or re-add a renderer locally:

```html
<script type="module">
  import mermaid from 'https://cdn.jsdelivr.net/npm/mermaid@11/dist/mermaid.esm.min.mjs'
  mermaid.initialize({ startOnLoad: true })
</script>
```

The claude.ai iframe runtime was stripped from the file for the same reason: it is
publishing plumbing, not content.
