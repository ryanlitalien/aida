Run the Notion meeting transcript backup. This is a focused, archival-only job - no email, no tasks, no calendar. Goal: pick up any new meeting notes and write them to disk as markdown.

## Verbose progress logging

Emit timestamped progress lines to stdout so the run is greppable in `meeting-backup.log`:

```
[PROGRESS] 2026-04-29T04:00:00Z STEP_0_START  notion availability check
[PROGRESS] 2026-04-29T04:00:08Z STEP_0_END    notion ok (8s)
[PROGRESS] 2026-04-29T04:00:08Z STEP_1_START  backup meetings
[PROGRESS] 2026-04-29T04:01:30Z STEP_1_fetch  meeting 1/3: Foo Sync (12s)
[PROGRESS] 2026-04-29T04:03:10Z STEP_1_END    3 meetings backed up (3m02s)
```

Include elapsed seconds on END lines and around each individual `notion-fetch` so we can see which calls dominate.

## Step 0: Notion availability check (fail-fast)

Attempt one lightweight `mcp__notion__notion-query-meeting-notes` call (a tight date filter is fine - you just need to confirm the tool exists and responds). If Notion is unavailable, print `BACKUP FAILED: notion unavailable` to stdout and stop. Do not retry forever - one re-attempt is fine, then bail.

## Step 1: Backup new Notion meeting transcripts

Read `{{ .ProjectDir }}/AGENT.md` and follow its backup procedure:

1. List `.md` files in `{{ .ProjectDir }}/meetings/` and find the most recent by filename to determine the last backup date.
2. Query `mcp__notion__notion-query-meeting-notes` with a `created_time` filter for meetings created **after the last backup date** through today.
3. **Fetch and write meetings one at a time - NOT in parallel.** For each new meeting:
   - Call `mcp__notion__notion-fetch` with `include_transcript: true`
   - Write the file immediately (naming convention + structure per `AGENT.md`)
   - Only then move to the next meeting

   Sequential fetches preserve history on mid-run failures - the next run's file listing skips meetings already on disk. Parallel batching is fragile: a 2026-04-23 run died during a 12-meeting parallel fetch with an API stream idle timeout.
4. Skip empty meetings (no content, no transcript) - note them in the summary.

If there are no new meetings, print "Up to date - no new meetings since YYYY-MM-DD" and exit cleanly.

## Final summary (stdout)

Print a short summary block before exiting. Examples:

```
BACKUP SUMMARY
- Up to date - no new meetings since 2026-04-28

BACKUP SUMMARY
- 3 meetings backed up (2026-04-28 → 2026-04-29):
  - 2026-04-28 [butterstack <> first-chair] Weekly Tech Call
  - 2026-04-28 Ryan / Luke 1:1
  - 2026-04-29 Foo Sync
- Skipped (empty): none

BACKUP SUMMARY
- BACKUP FAILED: <one-line reason>
```

The briefing job (`aida daily`, no flag) reads `meeting-backup.log` to decide whether the morning email should mention any backup errors - so make the failure line greppable and self-contained.
