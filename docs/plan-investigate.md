# Plan: `aida investigate` - Cloud Managed Agent Investigations

## Context

When a customer reports issues with specific IDs ("charges 123456, 123457 - why didn't I get paid?"), the current `aida query` pipeline gives a single-pass answer. But real investigations need iteration - run a query, follow the anomaly, check related tables, cross-reference logs. That's exactly what Claude Managed Agents excels at: autonomous, long-running tool use in a cloud container.

This plan adds `aida investigate <question>` which reuses Aida's local pipeline for fast parsing/classification/resolution (steps 1-3), then delegates the heavy investigation work to a managed agent session in the cloud. Each profile (work/personal) gets fully isolated credentials, agents, and environments.

## SDK: Upgrade to v1.34.0

The Go SDK v1.34.0 (released 2026-04-09) has full managed agents support:
- `client.Beta.Agents.New()` / `.Get()` / `.List()`
- `client.Beta.Sessions.New()` / `.Get()` / `.List()`
- `client.Beta.Environments.New()` / `.Get()`
- `client.Beta.Sessions.Events.Send()` / `.StreamEvents()` (SSE)

No raw HTTP needed - use the SDK directly. First step: `go get github.com/anthropics/anthropic-sdk-go@v1.34.0`

## 1. Config Changes

**File: `internal/config/config.go`**

Add `CloudConfig` to `Profile`:

```go
type Profile struct {
    Detect    DetectConfig      `yaml:"detect,omitempty"`
    ScanPaths []string          `yaml:"scan_paths,omitempty"`
    Tools     map[string]string `yaml:"tools,omitempty"`
    Cloud     *CloudConfig      `yaml:"cloud,omitempty"`       // NEW
}

type CloudConfig struct {
    APIKeyEnv     string   `yaml:"api_key_env,omitempty"`     // e.g. ANTHROPIC_API_KEY_WORK
    APIKeyFile    string   `yaml:"api_key_file,omitempty"`
    AgentID       string   `yaml:"agent_id,omitempty"`        // from "aida investigate setup"
    EnvironmentID string   `yaml:"environment_id,omitempty"`  // from "aida investigate setup"
    Model         string   `yaml:"model,omitempty"`           // default: claude-sonnet-4-6
    ContextPaths  []string `yaml:"context_paths,omitempty"`   // local folders to upload
}

func (c *CloudConfig) GetAPIKey(fallback *APIConfig) string { ... }
```

**Target config.yaml:**

```yaml
profiles:
    work:
        detect: { has_tool: snow }
        scan_paths: [~/dev]
        tools: { snow: snow, chrono: chrono, gh: gh, notion: mcp }
        cloud:
            api_key_env: ANTHROPIC_API_KEY_WORK
            model: claude-sonnet-4-6
            context_paths:
                - ~/dev/snowflake
    home:
        detect: { missing_tool: snow }
        scan_paths: [~/personal, ~/finances]
        tools: { gh: gh }
        cloud:
            api_key_env: ANTHROPIC_API_KEY
            context_paths:
                - ~/dev/workouts
                - ~/dev/butterstack
```

`agent_id` and `environment_id` are auto-populated on first `aida investigate` call. The user only needs to configure `api_key_env` and `context_paths`.

## 2. New Package: `internal/cloud/`

Thin wrapper around the Anthropic Go SDK's beta managed agents API. Creates a per-profile SDK client (different API keys per profile = different client instances).

| File | Purpose |
|------|---------|
| `client.go` | `NewClient(apiKey)` → `anthropic.NewClient(option.WithAPIKey(...))`, helper methods |
| `agents.go` | `EnsureAgent(ctx, cfg, profileName)` - get-or-create, writes ID back to config |
| `environments.go` | `EnsureEnvironment(ctx, cfg, profileName)` - get-or-create, writes ID back to config |
| `sessions.go` | `CreateSession(ctx, agentID, envID, resources)`, `GetSession(ctx, id)`, `ListSessions(ctx, agentID)` |
| `events.go` | `SendMessage(ctx, sessionID, text)`, `StreamEvents(ctx, sessionID, handler)` |
| `upload.go` | `PackageContext(paths) → []FileResource` - walks dirs, filters by extension, respects size limits |
| `brief.go` | `BuildBrief(intent, classified, resolved, contextFiles) → (systemPrompt, userMessage)` |

**Key design**: `cloud.Client` wraps `anthropic.Client` and exposes domain-specific methods. Each profile creates its own `cloud.Client` with its own API key - complete credential isolation.

## 3. CLI Commands

**File: `internal/cli/investigate.go`**

```
aida investigate 'why didnt customer get paid' - spawn investigation (default action)
aida investigate status [session-id] - show status (--stream for live SSE tail)
aida investigate results [session-id] - fetch final report
aida investigate list - show recent investigations
```

The question is the **default action** - `aida investigate 'do x and z'` just works. No `run` subcommand needed. Cobra's `Args: cobra.ArbitraryArgs` with `RunE` handles this: if the first arg isn't a known subcommand (`status`, `results`, `list`), treat all args as the question.

**Flags on main command:**
- `--stream` / `-s` - tail SSE events live instead of returning immediately
- `--open` / `-o` - open the session in the Anthropic Console web UI after spawning (uses `open` on macOS). URL format: `https://console.anthropic.com/workbench/sessions/{session-id}`
- `--dry-run` - show the investigation brief without creating a session

**Registration:** One line in `internal/cli/root.go`: `cmd.AddCommand(newInvestigateCmd())`

## 4. Investigation Flow

```
aida investigate 'charges 123456, 123457 - customer didnt get paid'

[LOCAL - fast, <2s]
  1. LoadConfig, detect profile (work)
  2. Validate profile.Cloud exists (api_key_env at minimum)
  3. AUTO-SETUP: if agent_id or environment_id missing:
     a. Create agent via API (model + system prompt + tools)
     b. Create environment via API (packages, networking)
     c. Write agent_id + environment_id back to config.yaml
     d. Print "✓ Provisioned cloud agent for profile 'work'"
  4. Load sources, partners, library, soul
  5. Parse (LLM #1 via Haiku) → Intent{action: investigate, entities: [123456, 123457]}
  6. Classify (deterministic) → StrategyInvestigate
  7. Resolve (deterministic) → partner=camp-butz, merchant_ari=ABC..., charge ARIs

[CONTEXT PACKAGING]
  8. Walk profile.Cloud.ContextPaths → collect .sql, .md, .yaml files
  9. Build investigation brief from steps 5-7 output

[CLOUD]
 10. Create cloud.Client with profile.Cloud.GetAPIKey()
 11. Create session (agent_id, environment_id, file resources)
 12. Send investigation brief as initial user message
 13. Save investigation record to ~/.aida/investigations/<session-id>.json

[OUTPUT]
 14. Print session ID + tracking commands
     (or if --stream: tail SSE events until idle)
     (or if --open: launch browser to console.anthropic.com session URL)
```

**Auto-setup detail**: The `cloud.EnsureAgent()` and `cloud.EnsureEnvironment()` functions are idempotent - they check if the stored ID still exists via a GET call. If the agent/environment was deleted externally, they re-provision and update config. This means the user never needs to run a separate setup command; their first `aida investigate` just works (adds ~2s one-time overhead for provisioning).

## 5. Context Upload

`cloud.PackageContext(paths []string)` walks each path:
- **Include:** `*.sql`, `*.md`, `*.yaml`, `*.yml`, `*.txt`, `*.json`, `*.toml`
- **Exclude:** `.git/`, `node_modules/`, files > 100KB
- **Preserve relative paths:** `~/dev/snowflake/queries/ari/lookup.sql` → `snowflake/queries/ari/lookup.sql`
- **Size guard:** warn > 10MB, hard-fail > 50MB

Files are uploaded as file resources on session creation via the SDK's resource mounting.

## 6. Investigation Brief

**Agent system prompt** (set at agent creation during auto-setup):

```
You are an autonomous investigator for Aida. You receive pre-analyzed
investigation briefs and deeply explore the question using available tools.

## Protocol
1. Review uploaded context files in /workspace/ for schemas and templates
2. Run queries iteratively - start broad, then follow anomalies
3. Keep a running log in /workspace/investigation.md
4. Write final report to /workspace/report.md with:
   - Executive summary (1-2 sentences)
   - Findings with evidence (query results, row counts, errors)
   - Timeline of events if applicable
   - Recommended next steps
Every claim must reference a specific query result. If you can't find it, say so.
```

**Initial user message** (built per-investigation by `cloud.BuildBrief()`):

```
## Investigation Brief
Question: {question}

### Parsed Intent
Action: {action}, Timeframe: {timeframe}
Keywords: {keywords}, Entities: {entities}

### Resolved Context
Partner: {partner_name}
Identifiers:
  - merchant_ari: ABC123...
  - charge_id: 123456, 123457

### Uploaded Context
{list of files with paths}

### Query Templates
{relevant SQL from source query assets}

Begin your investigation.
```

## 7. Local Session Tracking

**New package: `internal/investigations/investigation.go`**

Mirrors `internal/runs/run.go` pattern exactly.

```go
type Investigation struct {
    SessionID      string            `json:"session_id"`
    Profile        string            `json:"profile"`
    Question       string            `json:"question"`
    StartedAt      time.Time         `json:"started_at"`
    Status         string            `json:"status"` // running, idle, terminated
    Action         string            `json:"action"`
    Strategy       string            `json:"strategy"`
    Entities       []string          `json:"entities,omitempty"`
    Partner        string            `json:"partner,omitempty"`
    ResolvedValues map[string]string `json:"resolved_values,omitempty"`
    AgentID        string            `json:"agent_id"`
    EnvironmentID  string            `json:"environment_id"`
    Report         string            `json:"report,omitempty"`
}
```

Storage: `~/.aida/investigations/<session-id>.json`
Functions: `Save()`, `Load()`, `List()`, `Latest()`, `UpdateStatus()` - same patterns as `runs/`.

## 8. Files to Create/Modify

### New files (9)
- `internal/cloud/client.go` - SDK client wrapper, per-profile instantiation
- `internal/cloud/agents.go` - EnsureAgent (get-or-create + config writeback)
- `internal/cloud/environments.go` - EnsureEnvironment (get-or-create + config writeback)
- `internal/cloud/sessions.go` - CreateSession, GetSession, ListSessions
- `internal/cloud/events.go` - SendMessage, StreamEvents (SSE)
- `internal/cloud/upload.go` - PackageContext (walk dirs, filter, size guard)
- `internal/cloud/brief.go` - BuildBrief (system prompt + user message from pipeline output)
- `internal/investigations/investigation.go` - Local tracking (Save/Load/List/Latest)
- `internal/cli/investigate.go` - CLI command + subcommands (status, results, list)

### Modified files (2)
- `internal/config/config.go` - add `CloudConfig` struct, `Cloud` field on `Profile`, `GetAPIKey()` method
- `internal/cli/root.go` - add `cmd.AddCommand(newInvestigateCmd())`

### Dependency update (1)
- `go.mod` - upgrade `anthropic-sdk-go` from v1.26.0 → v1.34.0

## 9. Implementation Order

1. **SDK upgrade** - `go get github.com/anthropics/anthropic-sdk-go@v1.34.0`
2. **Config** - `CloudConfig` struct + `Cloud` field on `Profile`
3. **`internal/cloud/client.go`** - SDK client wrapper
4. **`internal/cloud/agents.go` + `environments.go`** - EnsureAgent/EnsureEnvironment with auto-setup
5. **`internal/cloud/sessions.go` + `events.go`** - session lifecycle + SSE streaming
6. **`internal/cloud/upload.go`** - context packaging
7. **`internal/cloud/brief.go`** - investigation brief builder
8. **`internal/investigations/`** - local tracking (follows `runs/` pattern)
9. **`internal/cli/investigate.go`** - all subcommands including --open/--stream
10. **`internal/cli/root.go`** - register command
11. **Manual test** - `aida investigate 'test question'` (auto-provisions on first run)

## 10. Verification

1. `make test` - existing tests pass (no engine changes)
2. `aida investigate 'test: what charges exist for partner X in last 24h'` - auto-provisions agent + env on first run, returns session ID
3. Verify `~/.aida/config.yaml` now has `agent_id` and `environment_id` populated
4. `aida investigate status` - shows running/idle status
5. `aida investigate status --stream` - live tails SSE events
6. `aida investigate results` - fetches report from completed session
7. `aida investigate list` - shows saved investigations
8. `aida investigate 'another question' --open` - opens session in browser
9. Switch profiles, repeat - verify isolation (different agent IDs, different API keys)
10. Re-run after deleting agent via API - verify auto-setup re-provisions gracefully
