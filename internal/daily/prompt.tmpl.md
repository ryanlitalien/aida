Run the daily briefing routine. Complete all 5 steps below, then report a summary.

## Verbose progress logging

The briefing should complete in 5–10 minutes once MCPs are available. Emit a timestamped progress line to stdout **before AND after** each numbered step (0, 1, 2, 2.5, 3, 4, 5) using this exact format so the log is greppable:

```
[PROGRESS] 2026-04-21T08:02:13Z STEP_1_START  backup meetings
[PROGRESS] 2026-04-21T08:05:47Z STEP_1_END    3 meetings backed up (3m34s)
```

Also emit a `[PROGRESS]` line around any operation that could be slow on its own: each `listMessages` query and the final `sendMessage`. Include elapsed seconds on the END line so we can see which tool calls dominate.

Do not suppress these in your user-facing summary - just ensure stdout carries them so they land in `daily-briefing.log`.

## Step 0: gdrive MCP availability check (fail-fast)

Verify gdrive MCP is loaded in this session by calling `mcp__gdrive__listEvents` with a 1-hour window.

Notion is **not** checked here - the briefing no longer talks to Notion. The Notion meeting-transcript archive is owned by the independent `com.ryan.notion-backup` launchd job; this briefing only reads `meeting-backup.log` to report what that job has done (Step 1).

If gdrive is unavailable:
- For read (`listEvents`/`listMessages`): skip Steps 3 and 4. Note it.
- For send (`sendMessage`): you cannot email the briefing. Print a clear failure reason to stdout (which goes to `daily-briefing.log`) in the form: `BRIEFING FAILED: gdrive MCP unavailable - cannot send. Re-auth needed.` Then stop.

Include any gdrive failure in the email as an **ACTION NEEDED** section at the top of the body (see Step 5 format). Re-auth by invoking `mcp__gdrive__authenticate` from an interactive Claude Code session.

## Step 1: Report on recent meeting backups (read-only)

The briefing does NOT fetch from Notion. The Notion meeting-transcript backup is owned by the independent `com.ryan.notion-backup` launchd job (every 4h); this step only reports what that job has produced since the previous briefing.

Use Bash to gather two pieces of data for the email's MEETINGS BACKED UP block:

1. **New transcripts on disk since the previous briefing.** Take the most recent `=== Daily briefing finished at <RFC3339 timestamp> ===` line in `{{ .ProjectDir }}/daily-briefing.log` (it's from the prior run - the current run hasn't logged its finished marker yet) and use that timestamp as the cutoff:
   ```
   PREV=$(grep -E '^=== Daily briefing finished at ' {{ .ProjectDir }}/daily-briefing.log | tail -1 | awk '{print $6}')
   find {{ .ProjectDir }} -maxdepth 1 -name '*.md' ! -name 'AGENT.md' -newermt "$PREV" -print 2>/dev/null | sort
   ```
   If no prior finished marker exists, fall back to `-mtime -1` (last 24h).

2. **Last `=== Notion backup finished at` marker** so the email shows when the cron last completed:
   ```
   grep -E '^=== Notion backup finished at ' {{ .ProjectDir }}/meeting-backup.log | tail -1
   ```
   The `meeting-backup.log` is `claude -p` stream-json - there is no plain-text BACKUP SUMMARY line to grep, so don't try. The marker line above is the authoritative signal that a cron cycle completed.

If the timestamp from (2) is older than 6 hours (cron fires every 4h, so >6h means at least one missed slot), flag the backup cron as **stale** in the email's ACTION NEEDED block.

## Step 2: Gather tasks Ryan has left to do

Run `aida tasks --tag {{ .TaskTag }}` to get the tasks Ryan still has to act on - `open` and `in-progress` only. Do **not** pass `--all` and do **not** include `done`, `closed`, `hold`, or `deferred` tasks under any framing (no "completed today," no "what's been resolved," no "needs attention" subsection that pulls in held items). The briefing is forward-looking; "what's done" never appears in the email body, even though tasks now carry a `completed_at` timestamp in their frontmatter.

Parse the output to get each task's number, date, priority, title, and tags.

## Step 2.5: Sync Slack follow-ups from Google Tasks into aida tasks

Drains the `Slack-Zapier` Google Tasks list into `aida tasks` so bookmarked Slack messages enter Ryan's canonical queue. The Zap that populates this list is triggered when Ryan reacts `:bookmark:` on a Slack message.

**Task list ID:** `{{ .GoogleTasksListID }}` (title: `Slack-Zapier`)

Skip this entire step if gdrive was marked unavailable in Step 0. Note "Slack follow-up sync SKIPPED" in the ACTION NEEDED block.

1. Call `mcp__gdrive__listTasks` with `taskListId: {{ .GoogleTasksListID }}`, `showCompleted: false`, `maxResults: 100`.
2. For each returned task, skip if its `notes` field contains the literal string `[aida-synced]` (already synced on a prior run).
3. For each remaining task:
   - **Extract permalink:** first line of `notes` that starts with `https://`. If none found, use empty string.
   - **Build aida title:** use the Google Task's `title` as-is (it already includes the `[Slack]` prefix from the Zap). Append ` -- <permalink>` if a permalink was found. Use ASCII double-dash, not em-dash (same rationale as Step 5 subject line).
   - **Create aida task:** run `aida tasks add "<title>" --tag {{ .TaskTag }} --tag slack-followup` via Bash. Quote the title with double quotes; escape any inner double quotes as `\"`.
   - **Mark synced + completed:** call `mcp__gdrive__updateTask` with `taskListId: {{ .GoogleTasksListID }}`, `taskId: <task.id>`, `notes: <original notes> + "\n\n[aida-synced]"`, AND `status: "completed"`. Once the task is tracked in `aida tasks`, the GT entry is just a receipt - completing it keeps the active GT view clean. The `[aida-synced]` marker still protects against re-sync if the GT task is ever re-opened.
4. After syncing, **re-run `aida tasks --tag {{ .TaskTag }}`** and use the updated list for the briefing's OPEN WORK TASKS section. Newly-synced items won't appear otherwise.
5. Track a count of `N synced` for the briefing summary.

If any single task fails to sync (aida add errors, or updateTask fails), note it inline in the briefing under ACTION NEEDED and continue with the rest - do not abort the sync or the briefing.

## Step 3: Gather today's calendar

Call `mcp__gdrive__listEvents` with:
- `timeMin`: today at 00:00:00 in **US Eastern time** (the user's home timezone; adjust to yours) in RFC 3339 format. Use `-04:00` during EDT (Mar–Nov) or `-05:00` during EST (Nov–Mar). Example for EDT: `2026-04-14T00:00:00-04:00`.
- `timeMax`: today at 23:59:59 in US Eastern time, same offset rules. Example for EDT: `2026-04-14T23:59:59-04:00`.

Render every time in the briefing in **Eastern time (ET)** - never PDT. Label the calendar section header with `(ET)` so it's unambiguous.

## Step 4: Scan Gmail for emails needing attention

**First, build a label-name → label-ID map.** Call `mcp__gdrive__listLabels` once and extract the IDs for {{ range $i, $l := .GmailLabels }}{{ if $i }}, {{ end }}`{{ $l }}`{{ end }}, and all `{{ .GmailPartnerLabelPrefix }}*` labels. Store them - you'll need the IDs to correctly categorize Query 1 results (Gmail's `listMessages` returns opaque `Label_xxx` IDs, not names) and to classify Partner emails in Query 3.

The IDs are account-specific opaque strings (shape: `Label_xxxxxxxxxxxxxxxxxxx`) - always take them from `listLabels`; never hardcode them.

Then run three `mcp__gdrive__listMessages` queries to surface emails that may have slipped through the cracks. Deduplicate across queries (same message ID = show once, use the most specific label).

**Query 1 -- Flagged emails (user-action labels):**
- `query`: `{{ range $i, $l := .GmailLabels }}{{ if $i }} OR {{ end }}label:{{ $l }}{{ end }}`
- `maxResults`: 20
- **Do NOT add `is:unread`.** These labels are applied manually to mark "needs action." Once Ryan reads the email, it's still open work - read state is orthogonal to whether the item is done. Only the label being removed signals completion.
- Dedupe by `threadId` and show the most recent message per thread. Include the thread message count (e.g. "4 msgs") so Ryan can gauge activity at a glance.
- **Categorize each result by actual label ID, not by subject-matter guess.** For each message, check its `labelIds` array against the map you built. A message with multiple listed labels goes under whichever Ryan most recently saw action on - default to the first listed label if ambiguous. Never guess based on subject matter; heuristics misfire (observed 2026-04-23, all 5 FollowUp threads were miscategorized as TODO).

**Query 2 -- Stale unread in Primary tab (older than 24h, up to 7 days):**
- `query`: `is:unread in:inbox category:primary older_than:1d newer_than:7d {{ .GmailExclude }}`
- `maxResults`: 30
- Scoped to `category:primary` so Forums-tab noise (SRE incidents, program updates, team digests) is excluded entirely. The configured sender/subject exclusions are belt-and-suspenders in case anything slips into Primary.

**Query 3 -- Unread partner emails (last 7 days):**
- `query`: `is:unread newer_than:7d ({{ range $i, $p := .GmailPartners }}{{ if $i }} OR {{ end }}label:{{ $.GmailPartnerLabelPrefix }}{{ $p }}{{ end }})`
- `maxResults`: 20

For each unique message, note the snippet, from address, and which category it was caught by (label name or "Stale"). Do NOT call `getMessage` for each one -- the snippet from `listMessages` is enough for the briefing summary.

**Collapse Google Docs comment noise:** When multiple messages share the same `subject` AND are from `*@docs.google.com`, collapse them into one line per document with a count. Example: instead of listing 10 separate "Google Connected Accounts - Updated Flows" rows, show `Google Connected Accounts - Updated Flows (10 new comments/replies from Chaochao, Dawid, Paulina, John)`.

## Step 5: Email the daily briefing

Call `mcp__gdrive__sendMessage` with:
- **to:** `{{ .EmailTo }}`
- **subject:** `Daily Briefing -- YYYY-MM-DD` (today's date). IMPORTANT: Use ASCII double-dash `--` NOT an em dash, to avoid UTF-8 encoding issues in email subjects.
- **body:** Format as plain text with these sections. If any MCPs failed the Step 0 check, lead the email with an ACTION NEEDED block:

```
[ACTION NEEDED]  (only include this block if Step 0 found gdrive unavailable, or Step 1 found the backup cron stale)
- gdrive unavailable - <one-line reason if known, else "tools not exposed in this session">. Re-auth: open an interactive Claude Code session and invoke `mcp__gdrive__authenticate`.
- Notion backup cron stale - last `=== Notion backup finished at` marker is more than 6h old. Re-auth: invoke `mcp__notion__authenticate` from an interactive Claude Code session and check `meeting-backup.log`.
- Affected sections below are marked SKIPPED.

MEETINGS BACKED UP (since last briefing)
- N new transcripts on disk since previous briefing finished (<previous briefing finished ET timestamp>)
- Last backup cron finished: <ET timestamp from "=== Notion backup finished at" marker> (<"fresh -- Xh ago" if <=6h old, else "stale -- Xh ago">).

TODAY'S CALENDAR (ET)
- HH:MMam  Event Title
- HH:MMam  Event Title
- ...
(or "No events scheduled")

OPEN WORK TASKS (left to do)
- [p2] #1 Task title (created YYYY-MM-DD)
- [p3] #2 Task title (created YYYY-MM-DD)
- ...
(or "No open tasks")

# Never emit a "TASKS COMPLETED" / "CLOSED YESTERDAY" / "DONE THIS WEEK" /
# "RECENTLY RESOLVED" section. The brief is forward-looking only - what's
# left, never what's done. Even though tasks now carry `completed_at`, that
# field is for analytics/history queries, not the email body.

EMAILS NEEDING ATTENTION
[TODO]
- Subject line (from: sender, snippet preview)
- ...

[FollowUp]
- Subject line (from: sender, snippet preview)
- ...

[Stale Unread - Primary tab, 2+ days]
- Subject line (from: sender, N days old, snippet preview)
- Google Docs noise collapsed: "Doc Title (N new comments/replies from names)"
- ...

[Partner Emails]
- [first-chair] Subject line (from: sender, snippet preview)
- [Google] Subject line (from: sender, snippet preview)
- ...

(or "No emails needing attention")
```

After sending the email, apply the "Daily" Gmail label using `mcp__gdrive__modifyLabels` with the `Daily` label ID from the `listLabels` map on the returned message ID.

Print a confirmation with the subject line.
