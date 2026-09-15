# A voice turn, with the latency budget

One wake-triggered turn through the always-on listener, timed from the
per-stage fields the audit log records on every real turn. Full mechanics in
`docs/notes/voice-pipeline.md`.

```mermaid
sequenceDiagram
    autonumber
    actor U as User
    participant E as Ear (VAD + whisper.cpp)
    participant A as Assistant (Haiku)
    participant T as Tools
    participant S as TTS + speaker

    U->>E: "Ok Jarvis, what's my day look like?"
    Note over E: EOU wait ≈ 1.2 s · transcribe ≈ 0.3–1 s
    E->>A: transcript (wake phrase stripped)
    Note over A: recall embed ≈ 0.1–0.3 s
    A->>T: tool call(s)
    Note over T: fast tool ≈ 0.1–1.5 s · slow tool ≈ 8–15 s
    T-->>A: results
    Note over A: LLM turn ≈ 1.1 s
    A->>S: final reply text
    Note over S: first sound ≈ 0.9 s · playback 2–6 s
    S-->>U: spoken answer
    S-->>E: flush mic backlog + pre-roll
```

**End to end: 2–4 s** for a fast-tool turn, **8–15 s** when the engine is
involved. Two budget lessons the numbers teach:

- **The fixed costs bracket the turn.** ~1.2 s of VAD silence-wait going in
  and 2–6 s of playback coming out are paid regardless of how smart the middle
  is: playback wall-clock usually exceeds all compute combined.
- **Perceived latency is governed by acknowledgment, not speed.** The canned
  ack before slow tools and a progress phrase at the 90 s mark of a long
  subprocess bought more perceived responsiveness than any pipeline
  optimization did.

The last arrow is a real step, not decoration: the turn ends by flushing the
assistant's own audio out of the mic pipeline: the fix for the day it
answered itself (`docs/notes/failure-stories.md`, story 1).
