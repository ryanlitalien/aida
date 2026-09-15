# The six-step pipeline

The signature picture: **LLM at the edges, deterministic middle**. Exactly
three core LLM calls per query: parse, execute, synthesize, plus one bounded
router call. Everything between them is config-driven, testable, and
reproducible: the model never freely decides where to look.

Thick-bordered nodes spend LLM tokens; dashed nodes are pure deterministic
code. (Shapes carry the distinction so the diagram survives light and dark
themes.)

```mermaid
flowchart TD
    Q(["question in plain English"]) --> P

    P["1 · PARSE: LLM call #1<br/>language → Intent struct<br/>(action, entities, keywords, timeframe)"]:::llm
    P --> C

    C["2 · CLASSIFY: deterministic<br/>regex-type the entities (IDs, tokens, URLs)<br/>pick a strategy: lookup · query ·<br/>investigate · record · execute · search"]:::det
    C --> R

    R["3 · RESOLVE: deterministic<br/>match entities against sources' entities: tokens<br/>and routes.yaml match_entity / match_cwd"]:::det
    R --> PL

    PL["4 · PLAN: deterministic<br/>score every source: name / topic /<br/>capability / keyword / route buckets<br/>(--explain prints the table)"]:::det
    PL --> RT

    RT["ROUTER: one bounded LLM call<br/>picks 1–3 sources from the ranked list;<br/>feedback lessons apply ±100 BEFORE the prompt"]:::llm
    RT --> E

    E["5 · EXECUTE: LLM call #2<br/>generate each source's query,<br/>run adapters in parallel<br/>(bounded fan-out, limit 5)"]:::llm
    E --> S1[("source A")]
    E --> S2[("source B")]
    E --> S3[("source C")]
    S1 --> SY
    S2 --> SY
    S3 --> SY

    SY["6 · SYNTHESIZE: LLM call #3<br/>one grounded answer with<br/>(source: artifact) citations"]:::llm
    SY --> A(["answer + recorded run"])
    A -. "thumbs-up / thumbs-down<br/>becomes a lesson the router<br/>reads next time" .-> RT

    classDef llm stroke-width:3.5px;
    classDef det stroke-dasharray: 6 4;
```

Why this shape:

- **The LLM parses language in and prose out; the middle is a rulebook you can
  read.** Routing is data (source YAML capabilities, `entities:` topic tokens,
  `routes.yaml`), so adding a knowledge source is writing a config file, and a
  routing bug is reproducible instead of a vibe.
- **The router is bounded, not sovereign.** It chooses among candidates the
  deterministic planner already ranked, and user corrections adjust those
  ranks as arithmetic before the model sees the prompt
  (`docs/notes/feedback-mechanics.md`).
- **The feedback edge is the compounding loop**: every rated run makes the
  next similar question route better, mechanically.
