# Memory levels L0–L4

Aida's memory is a tiered stack. Records move **up** through automated hooks
(solid arrows) and are designed to move **down** by losing weight through
decay scoring, never by deletion (dashed arrow). Files are the source of
truth at every level; `brain.db` is a derived index that rebuilds itself,
detached, when stale.

```mermaid
flowchart BT
    L0["L0 · ephemeral<br/>run records, --explain traces<br/>written by every query"]:::tier
    L1["L1 · feedback lessons<br/>engine + voice lesson stores<br/>(SQLite rows + embeddings)"]:::tier
    L2["L2 · captured agent memories<br/>per-tool memory profiles:<br/>hook-mirrored + LLM-distilled<br/>session harvests (watermarked)"]:::tier
    L3["L3 · curated knowledge<br/>entity pages, domain docs,<br/>per-source layer docs"]:::tier
    L4["L4 · consolidated wiki<br/>consolidation, decay scoring,<br/>supersession (OKF bundle)"]:::tier

    L0 -- "thumbs-up / thumbs-down<br/>promotes a run into an<br/>embedded lesson" --> L1
    SESS(["coding-agent sessions<br/>(capture hooks + harvest sweeps)"]):::ext -- "distill 0–5 durable<br/>records per session" --> L2
    L2 -- "compile pass (roadmap):<br/>cluster hot memories by<br/>entity/domain, propose merges" --> L3
    L3 -- "brain consolidate (roadmap):<br/>event → fact promotion<br/>with provenance" --> L4
    L4 -. "decay scoring demotes:<br/>tiers move down by losing<br/>weight, not by deletion" .-> L3

    classDef tier stroke-width:2.5px;
    classDef ext stroke-dasharray: 5 4;
```

| Level | Store | Written by | Recalled by |
|---|---|---|---|
| L0 ephemeral | run records, `--explain` traces | every query | `aida runs`, debugging |
| L1 feedback lessons | engine + voice lesson tables (SQLite + embeddings) | thumbs up/down, notes | router boosts, voice-turn prompt injection |
| L2 captured agent memories | per-tool brain memory profiles (hook-mirrored, or LLM-distilled from session transcripts, watermarked and incremental) | coding-agent sessions | `brain_recall` (MCP), `aida brain recall`, voice recall tool |
| L3 curated knowledge | entity pages, domain docs, per-source layer docs | human + LLM compile passes | `brain_search`, source context injection |
| L4 consolidated wiki | the wiki bundle (OKF) | `aida brain consolidate` (planned) | wiki commands, brain |

The through-line: **feedback and hooks promote; decay demotes; humans curate
exceptions, not the pipeline.** The tiers only earn their keep because records
move between them without a human shepherding each one: session→L2 and
L0→L1 movement is fully automated today; the L2→L3 compile pass and L3→L4
consolidation are the roadmap's promotion hooks. Prior art credited in
`docs/notes/inspiration.md` (Elastic's Atlas for promotion/decay, OpenViking
as convergent evolution).
