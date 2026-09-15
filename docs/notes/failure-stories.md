# Engineering note: ten failure stories

Things that went wrong, told as symptom → wrong hypothesis → actual cause →
fix. Every story was verified against the git history before writing; where
the folklore version disagreed with the commits, the commits won and the
correction is noted. Commit references point at the archived private history
(see the note at the top of `HISTORY.md`).

## 1. The assistant answered itself

**Symptom.** A bare "Okay, Jarvis" armed the mic for a follow-up question.
Jarvis said "At your service, sir" - and then answered *that*, replying
"Indeed, sir. Standing by." to his own voice.

**Wrong hypothesis.** A phantom wake: the VAD gate too loose, or whisper
hallucinating a query out of room noise.

**Actual cause.** The mic pipeline kept capturing while the speakers played.
The listener loop blocks during playback, so the assistant's own TTS piled up
in the pipe backlog, and the armed loop transcribed the echo as the user's
follow-up. Latent for about six weeks - the always-on listener shipped
2026-05-10, the misfire was fixed 2026-06-24.

**Fix.** Two commits the same day: a `Flush` on the PCM stream to drain the
backlog, and a `spoke` signal from every turn that played audio, on which the
listener flushes the backlog *and* clears the stale pre-roll ring before
resuming VAD ("flush self-audio after Jarvis speaks").

## 2. The silent TCC death

**Symptom.** The hardware push-to-talk button stopped working after a rebuild.
No error anywhere: the HID open still succeeded, the "push-to-talk ready"
banner still printed - and no keyboard-page events ever arrived.

**Wrong hypothesis.** The HID layer - the device code that had just landed the
week before (button reading shipped 2026-07-13; the fix came 2026-07-20).

**Actual cause.** Adhoc codesigning. An adhoc signature's designated
requirement is a raw cdhash that changes on every rebuild, so each `make
install` produced a binary macOS considered *entirely different* - silently
voiding the Input Monitoring grant. macOS delivers no error for this; it just
stops delivering events. A second aggravator: two installed copies of the
binary meant two separate TCC grants to keep alive.

**Fix.** Sign with a stable Developer ID identity (identity-based designated
requirement, stable across rebuilds), collapse installation to one real binary
with a symlink, and add explicit TCC state checks that warn loudly instead of
failing silently.

## 3. The confidently stale cache

**Symptom.** The Tier-1 answer cache - which mines past runs to short-circuit
repeat questions before the pipeline - served old answers to time-sensitive
questions with full confidence.

**Wrong hypothesis.** That caching answers keyed by question similarity was
simply safe: a repeat question deserves the repeat answer.

**Actual cause.** No admission policy. "What's the P99 today" and "revenue
last week" are similarity-matched by wording, but their answers expire; the
cache also mis-stamped entry creation time from the caching moment rather
than the source run's start, making freshness checks lie.

**Fix.** Three guards (merged 2026-07-21, two days after the cache shipped
2026-07-19): a time-sensitivity regex that rejects volatile questions at
*admission* (later extended to backward-looking and year-to-date phrasings),
correct `Created` stamping from the source run, and a 30-day TTL backstop at
lookup. The launch-plan telling is accurate here; note only that the bug
lived two days, not an era.

## 4. The 10-second tax

**Symptom.** Every single query took 10+ extra seconds before doing anything.

**Wrong hypothesis.** Nobody had one - that's the story. The tax was accepted
as "how long queries take" until it was actually profiled. (The folklore
version says the rebuild ran "for weeks before anyone noticed"; git says the
brain landed 2026-04-11 and the fix landed 2026-04-15 - **four days**, not
weeks. Corrected here per the history.)

**Actual cause.** A self-defeating staleness check. `RecordLesson` wrote a
lesson JSON file *and* inserted into `brain.db` directly - so after every
query, one file on disk was newer than the database, `IsStale` returned true,
and the next query rebuilt the whole index it didn't need.

**Fix.** Two stages: require 3+ newer files before declaring staleness and
touch the db after indexing (2026-04-15); then move even legitimate rebuilds
out of the query path entirely - a detached `Setpgid` subprocess re-indexes in
the background while the current query proceeds in under a second
(2026-04-24).

## 5. Every daemon job died on turn 1

**Symptom.** Every voice- and web-spawned background agent failed instantly
with an API 400. Run-dir forensics showed identical failures going back six
days.

**Wrong hypothesis.** Something in the agent's prompt or the daemon's spawn
plumbing - the failures looked environmental, not structural.

**Actual cause.** A tool-name collision. The lazy tool palette registered a
generic stdin-reading `ask_user`; run-dir mode then *appended* its pausing
`ask_user` variant instead of replacing it. Two tools with one name is an
immediate "Tool names must be unique" 400 from the API - on the very first
turn, every time.

**Fix.** An `upsertTool` helper that replaces by name (2026-07-01), applied
defensively to `request_approval` too. The quiet lesson: a registry that
appends where it means to upsert is a time bomb with a six-day fuse.

## 6. The rename that lasted one day

**Symptom.** The project spent 2026-07-05 as K.E.V.I.N. - a thorough,
13-commit sweep: module path, binary, config dir, env prefix, launchd label,
run-id sentinels, even the banner backronym.

**Wrong hypothesis.** That the naming question was settled.

**Actual cause.** Naming is hard, and living with a name for 24 hours is data
you can't get any other way.

**Fix.** An equally systematic full sweep to A.I.D.A. the very next day,
2026-07-06 - step-for-step parallel to the first (git shows the two merges a
day apart). The real fix was procedural: the first sweep's commit structure
made the second one mechanical, and satellite-machine rename checklists
followed. If you must rename, rename *completely* - a half-rename would have
been far worse than either name.

## 7. The shell-injection guard

**Symptom.** Not an incident - a realization during review: exec-type sources
with a bare `{query}` command template meant whatever the LLM emitted went
straight into `sh -c`. Any prose, any backticks, any semicolons.

**Wrong hypothesis.** That "the LLM writes a query for the tool" and "the LLM
writes a shell command" were the same risk class. They aren't: a command
*prefix* constrains the blast radius; a bare template is arbitrary code
execution on someone else's judgment.

**Actual cause.** The exec-source mechanism (2026-04-07) predated any
validation of its templates; fourteen configured sources were raw
passthroughs by the time anyone looked.

**Fix.** A load-time validator in the library (landed 2026-04-24, bundled in
the same PR as story 4's detached rebuild): sources whose `exec.query` is a
bare `{query}`/`{search}`/`{command}` are rejected at registry load and
surfaced by `aida lint`. All fourteen auto-disabled; no more `sh -c` with LLM
prose. Guard the *config schema*, not each call site.

## 8. The hallucination filter's blind spot

**Symptom.** Turns where nothing was said produced transcripts - and past the
filter they went: bare words like "you" and "thank you", whisper's favorite
things to hear in silence.

**Wrong hypothesis (and a folklore correction).** The launch-plan telling - 
"hallucinations polluted the audit log until a filter was added" - is
backwards. Git shows the filter shipped *with* the audit log on day one
(2026-05-10, "whisper hallucinations are NOT logged"). The wrong hypothesis
was believing that filter was complete.

**Actual cause.** The original filter only matched bracketed/parenthesized
tags (`[BLANK_AUDIO]`, `(upbeat music)`), and it was duplicated in two places
with diverging word lists. The blind spot was latent on the desk mic because
the VAD never sent pure silence to whisper - then push-to-talk arrived and
did exactly that (hold button, say nothing), and the bare-word hallucinations
walked straight through.

**Fix.** One shared `stt.IsHallucination` (2026-07-20) matching bare-word
outputs against the whole normalized transcript - never substrings, so "thank
you for the update" still transcribes. The LMD phone surface later added an
RMS silence gate *before* whisper entirely (`docs/notes/lmd-protocol.md`).

## 9. The satellite speakers that got deleted

**Symptom.** A real feature, built and working: cast voice replies to a
room's smart speakers, so the assistant follows you around the house.

**Wrong hypothesis.** That it would feel like the movies.

**Actual cause.** Cast latency. The spin-up delay before each reply made
every exchange feel broken, and no amount of tuning (a dedicated
latency-tuning commit tried) closed the gap; the complexity never paid its
rent. A timeline correction per git: the feature was *built* 2026-05-21..27
but sat unmerged; it was merged to main on 2026-07-06 and **reverted the same
day** - it lived on main for hours, not weeks. The "latency never justified
the complexity" rationale is recorded in `HISTORY.md`; the revert commit
itself is a bare revert.

**Fix.** `git revert`, kept honestly as a we-deleted-it story. Deleting a
working feature that fails its own experience bar is a feature decision, not
a failure - the failure would have been keeping it.

## 10. The subprocess that billed the wrong wallet

**Symptom.** Subprocess `claude -p` calls silently billed API-key credit - 
and in one adapter, account-level connectors failed to load - despite a
perfectly good subscription login on the machine.

**Wrong hypothesis.** That subprocess auth "just works": the CLI is logged
in, so calls through it use that login.

**Actual cause.** Environment inheritance. The parent process carries
Anthropic API-key env vars for its own SDK calls; every spawned `claude`
subprocess inherited them, and the CLI prefers an explicit key over its own
OAuth session. Nothing failed - that's what made it invisible; the only
symptom was money leaving the wrong pocket.

**Fix.** Three waves, not one (a correction to any single-event telling):
scrub the creds at the first affected call site (2026-06-04), extract
`ScrubAnthropicCreds` for cross-package reuse when the same bug surfaced in
the claude-project adapter (2026-07-02), then widen the scrubbed variable set
beyond the two API-key vars (2026-07-19). The pattern generalizes: an agent
that spawns agents needs an explicit *env policy* per subprocess, because
inheritance is a default, not a decision.

---

**Why these are documented.** Failure-and-fix pairs are the most transferable
part of any engineering history - and several of the fixes above (the flush
signal, the upsert helper, the load-time validator, the env scrubber) are the
directly reusable artifacts this project would want from someone else's
blog.
