# Plan: MCP Task Edit + Missing Task Actions

Status: **implemented** on branch `task-edit`. Execution plan: `~/.claude/plans/make-a-plan-on-flickering-steele.md`.

## Summary

The `aida serve` MCP surface exposed three task tools (`tasks_list`, `tasks_add`, `tasks_done`) but was missing everything an agent needed to curate a task list: edit, read body, reopen, reference by stable `#N` ID.

This change adds **`tasks_edit`** plus three companion tools (`tasks_show`, `tasks_get`, `tasks_reopen`) and `#N`-ref support across all slug-accepting MCP tools. It backfills `aida tasks reopen` on the CLI and introduces `Brain.UpdateTask(slug, patch)` + `Brain.ReopenTask` + `Brain.ResolveTaskRef` in the store layer.

**No delete.** Per user direction, tasks are never deleted - "delete" is a synonym for "done" / "closed". No `tasks_delete`, no `aida tasks rm`, no `DeleteTask` method.

## Non-goals

- **Bulk operations.** One slug per call. Batch-edit is an LLM-loop problem, not a server problem.
- **Task-level permissions or audit.** Single-user, local brain. Git history is the audit log.
- **Rich search / semantic match on tasks.** `tasks_list` with `tag` substring is enough; `brain_search` already surfaces open tasks as a side-channel.
- **Subtasks, dependencies, or relations.** Out of scope; tags already serve grouping.
- **Changing the slug after creation.** Slugs are immutable by design (date-prefixed from title-at-create). Rename = delete + recreate.

## Current state

**MCP server** (`internal/mcp/server.go`):
- `toolsList()` at line 209 registers: `brain_search`, `brain_get_page`, `tasks_list`, `tasks_add`, `tasks_done`, `brain_stats`.
- `callTool()` at line 281 dispatches each. Task handlers are lines 333–405.

**CLI** (`internal/cli/tasks.go`):
- `aida tasks` (list, lines 31–102) - supports `--tag` (repeatable), `--all`, `--days`, `--limit`.
- `aida tasks add` (lines 104–178) - `--tag`, `--more` (editor for body).
- `aida tasks done` (lines 264–307) - slug, `#N`, or positional ("latest", "first").
- `aida tasks edit` (lines 309–366) - **opens `$EDITOR` on the markdown file, then calls `ReindexTask`.** No programmatic patch path.
- `aida tasks show` (lines 368–399) - prints raw markdown.
- No `reopen`. No `rm`/`delete`.

**Brain store** (`internal/brain/tasks.go`):
- `AddTask`, `CompleteTask`, `ListTasks`, `GetTask`, `GetTaskByID`, `ReindexTask`, `UpdateIssue`, `CloseIssue`.
- **No `UpdateTask(slug, patch)`.** The CLI `edit` flow is: write file by hand → `ReindexTask` re-parses → `UpsertTask` in DB.
- No `DeleteTask`. No `ReopenTask`.

**`TaskRecord`** (`internal/brain/db.go:437`): `Slug`, `Title`, `Status`, `Completed`, `Tags`, `Created`, `PagePath`, `Description`, `IssueNumber`, `TaskID`. Frontmatter carries `status|completed|tags|created|issue_number|task_id`.

### Why the CLI's `edit` can't just be re-exposed

`newTasksEditCmd` at line 309 shells out to `$EDITOR` - an interactive blocking process with a TTY. An MCP client (Claude Desktop, `claude --print`, another agent) has no terminal to hand to it. We need a non-interactive mutation path.

## Design

### New Brain method: `UpdateTask`

```go
// internal/brain/tasks.go

type TaskPatch struct {
    Title      *string  // nil = unchanged
    Body       *string  // nil = unchanged; "" = clear body
    Tags       *[]string // nil = unchanged; full replace
    AddTags    []string  // additive, dedup against existing
    RemoveTags []string  // subtractive, by exact match
    Status     *string  // "open" | "done"; nil = unchanged
}

func (b *Brain) UpdateTask(slug string, patch TaskPatch) (*TaskRecord, error)
```

Behavior:
1. Resolve slug via `FindTaskByPartialSlug` (same as `CompleteTask`).
2. Load the markdown file via `parseTaskFile` to get current record + body.
3. Apply patch. `AddTags` dedups; `RemoveTags` is a filter; if `Tags` (full replace) is set, `Add`/`Remove` are ignored with an error.
4. If `Status` changes, mirror to `Completed` (`"done"` → `true`, `"open"` → `false`).
5. Rewrite file with `writeTaskFile` and upsert DB via `DB.UpsertTask`.
6. If `IssueNumber > 0` and title/tags/status changed, call `UpdateIssue` (or `CloseIssue` if newly done).
7. Return updated `*TaskRecord`.

**Immutable fields:** `Slug`, `TaskID`, `Created`, `PagePath`. Patch cannot touch these; validate at entry.

### MCP tool: `tasks_edit`

```jsonc
{
  "name": "tasks_edit",
  "description": "Edit a brain task. Update title, body, tags, or status.",
  "inputSchema": {
    "type": "object",
    "properties": {
      "slug":        {"type": "string", "description": "Task slug, partial slug, or '#N' task ID"},
      "title":       {"type": "string", "description": "New title (optional)"},
      "body":        {"type": "string", "description": "Replace body text. Pass empty string to clear. (optional)"},
      "tags":        {"type": "string", "description": "Comma-separated tags - replaces all tags. Mutually exclusive with add_tags/remove_tags. (optional)"},
      "add_tags":    {"type": "string", "description": "Comma-separated tags to add. (optional)"},
      "remove_tags": {"type": "string", "description": "Comma-separated tags to remove. (optional)"},
      "status":      {"type": "string", "enum": ["open", "done"], "description": "New status. (optional)"}
    },
    "required": ["slug"]
  }
}
```

Handler (sketch, `internal/mcp/server.go` callTool):
```go
case "tasks_edit":
    slug, _ := params.Arguments["slug"].(string)
    if slug == "" { return errorResult("slug is required") }
    resolved, err := resolveSlugOrID(s.brain, slug, s.profile)
    if err != nil { return errorResult(err.Error()) }

    patch := brain.TaskPatch{}
    if v, ok := params.Arguments["title"].(string); ok { patch.Title = &v }
    if v, ok := params.Arguments["body"].(string); ok  { patch.Body  = &v }
    if v, ok := params.Arguments["tags"].(string); ok && v != "" {
        t := splitCSV(v); patch.Tags = &t
    }
    patch.AddTags    = splitCSV(strOrEmpty(params.Arguments["add_tags"]))
    patch.RemoveTags = splitCSV(strOrEmpty(params.Arguments["remove_tags"]))
    if v, ok := params.Arguments["status"].(string); ok && v != "" {
        patch.Status = &v
    }

    updated, err := s.brain.UpdateTask(resolved.Slug, patch)
    if err != nil { return errorResult(err.Error()) }
    if s.cfg.Brain.AutoSync { brain.CommitAndPush(s.cfg.BrainPath(), s.profile) }
    return textResult(fmt.Sprintf("Task updated: #%d %s - %s", updated.TaskID, updated.Slug, updated.Title))
```

### Other missing MCP tools

| Tool | Parameters | Delegates to | Rationale |
|------|-----------|--------------|-----------|
| **`tasks_show`** | `slug` (required) | `os.ReadFile(b.TaskFilePath(slug))`, strip frontmatter | Read the markdown body. Parity with `aida tasks show`. |
| **`tasks_get`** | `slug` (required) | `b.ResolveTaskRef(slug)` → JSON of `TaskRecord` | Structured metadata only (no body). Cheaper than `tasks_show` when an agent just wants tags/status/ID. |
| **`tasks_reopen`** | `slug` (required) | **new** `b.ReopenTask(slug)` | Undo a premature `tasks_done`. Today nothing exists - file must be hand-edited. |

`#N` ID support is threaded through all slug-accepting tools via `Brain.ResolveTaskRef`:
```go
func (b *Brain) ResolveTaskRef(ref string) (*TaskRecord, error) {
    if strings.HasPrefix(ref, "#") {
        n, _ := strconv.Atoi(strings.TrimPrefix(ref, "#"))
        return b.GetTaskByID(n)
    }
    if task, err := b.DB.GetTask(ref); err == nil {
        return task, nil
    }
    return b.DB.FindTaskByPartialSlugAny(ref) // matches open or done
}
```

This also fixes the existing `tasks_done` gap (line 398 currently only passes to `CompleteTask`, which accepts partial slug but not `#N`). `FindTaskByPartialSlugAny` is a new DB helper so partial-slug lookups can resolve completed tasks (needed by edit/reopen).

### Companion CLI backfills

For parity and discoverability, add:
- **`aida tasks reopen <ref>`** - delegates to `b.ReopenTask`.

No `aida tasks rm` / `aida tasks delete` - deletion is not a task-level operation in this system.

### New Brain methods beyond `UpdateTask`

```go
func (b *Brain) ReopenTask(slug string) (*TaskRecord, error)
// Sets status=open, completed=false. Mirrors CompleteTask. Reopens GitHub issue if closed.
// Idempotent: no-op when the task is already open.
```

## Testing

Unit tests for each new Brain method (`internal/brain/tasks_test.go`):
- `UpdateTask` - title-only, body + tag add/dedup, tag remove, tags/add_tags exclusivity guard, status transition (+ invalid status rejected), unknown-slug error.
- `ReopenTask` - done → open round-trip; idempotent when already open.
- `ResolveTaskRef` - `#N`, exact slug, partial slug, invalid `#N`, unknown ID, empty ref.

MCP handler tests (`internal/mcp/server_test.go`):
- `tasks_edit` happy path (title+tags round-trip through `tasks_get`).
- `tasks_done` `#N` form (via ref extracted from `tasks_list`).
- `tasks_reopen` after `tasks_done`.
- `tasks_show` strips frontmatter.
- `tasks_edit` rejects missing slug.
- `tasks_edit status=done` round-trips to `Completed=true`.
- `stripFrontmatter` / `splitCSV` unit coverage.

## Migration & compatibility

- No schema changes. `tasks` table fields unchanged.
- `TaskPatch` is new; existing callers unaffected.
- CLI `aida tasks edit` (editor-based) **stays** as the interactive path. New code only adds programmatic `UpdateTask` beneath it. Optionally refactor `newTasksEditCmd` to call `UpdateTask` after the editor returns (instead of direct `ReindexTask`), so both paths funnel through one mutation - but this is a nice-to-have, not required.
- `managed-mcp.json` (enterprise Claude Code) picks up new tools automatically on next `aida serve` spawn - no client config change.

## Rollout

One PR, one branch. Commits ordered for reviewability:
1. `brain: add UpdateTask, ReopenTask, ResolveTaskRef + tests` - pure store layer.
2. `cli: add aida tasks reopen` - CLI parity.
3. `mcp: add tasks_edit, tasks_show, tasks_get, tasks_reopen; #N in tasks_done` - expose over MCP via shared `ResolveTaskRef`.
4. `docs: update CLAUDE.md MCP section + serve help`.

After merge, bump `aida-config` if its `managed-mcp.json` snippet enumerates tools (check - it may just name the binary).

## Resolved decisions

- **No delete.** Tasks are never deleted; "delete" is treated as "done". If a future NL parser encounters "delete this task", it should map to the done action.
- **`tasks_show` strips frontmatter.** Frontmatter is metadata; `tasks_get` is the structured path.
- **`ResolveTaskRef` matches any status.** Edit/reopen need to look up completed tasks by partial slug - backed by new `DB.FindTaskByPartialSlugAny`.

## Follow-ups (not in this PR)

- `tasks_edit` could accept a `body_append` field for additive notes (changelog-style). Strictly additive, no migration risk.
- Refactor CLI `aida tasks edit` to funnel through `Brain.UpdateTask` after `$EDITOR` returns, so both paths share one mutation surface.
- Extend LLM task-intent parser (`HandleTaskIntent` in `cli/tasks.go:401`) so "delete task #3" maps to the done action instead of falling through.
