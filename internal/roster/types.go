// Package roster is the registry behind Aida's dispatcher: a profile-scoped,
// backend-agnostic map of call-signs to the thing that answers for them. An
// entry can be a Claude Code subagent in a repo, an aida library source, a
// discovered MCP tool, or a background aida --agent job. See CLAUDE.md's
// "Aida dispatcher + agent roster" section for the design summary.
package roster

import (
	"context"
	"errors"

	"github.com/ryanlitalien/aida/internal/jobs"
	"github.com/ryanlitalien/aida/internal/mcp"
)

// Kind values for a roster Entry.
const (
	KindSubagent = "subagent"
	KindSource   = "source"
	KindMCP      = "mcp"
	KindJob      = "job"
	KindAida     = "aida" // reserved orchestrator entry; never a dispatch target
)

// Result.Status values.
const (
	StatusSuccess   = "success"
	StatusEmpty     = "empty"     // backend answered but with nothing usable (e.g. a refusal)
	StatusError     = "error"     // backend failed; Text carries the message
	StatusTimeout   = "timeout"   // backend exceeded its deadline
	StatusDelegated = "delegated" // work handed to a background job; JobID is set, Text empty
)

// Entry is one roster row, parsed from ~/.aida/roster.yaml (or synthesized by
// directory discovery). Exactly one backend spec block is populated, matching
// Kind, except for KindAida (no backend) and discovery parents (expanded away
// at load time).
type Entry struct {
	Name        string   `yaml:"-"`                   // map key, lowercase slug ("product-manager")
	CallSign    string   `yaml:"call_sign,omitempty"` // display/spoken name ("Pamela"); "" = Name
	Kind        string   `yaml:"kind"`                // subagent | source | mcp | job | aida
	Description string   `yaml:"description"`         // what this entry is FOR; feeds the select prompt
	Skills      []string `yaml:"skills,omitempty"`    // routing hints, same spirit as Source.Capabilities
	Aliases     []string `yaml:"aliases,omitempty"`   // spoken variants incl. whisper mishears ("pam")
	Profiles    []string `yaml:"profiles,omitempty"`  // empty = all (matches config.Source.Profiles)
	Mode        string   `yaml:"mode,omitempty"`      // "sync" | "async" | "" (auto; policy decides)

	// Discovery (BSI team freshness): a single parent entry expands into one
	// virtual Entry per persona found in a .claude/agents dir, so a growing
	// team never goes stale. A discovery parent is not itself a dispatch target.
	Discover      string `yaml:"discover,omitempty"`        // .claude/agents dir to scan and expand
	CallSignsFrom string `yaml:"call_signs_from,omitempty"` // org-chart file mapping slugs -> call-signs

	Subagent *SubagentSpec `yaml:"subagent,omitempty"`
	Source   *SourceSpec   `yaml:"source,omitempty"`
	MCP      *MCPSpec      `yaml:"mcp,omitempty"`
	Job      *JobSpec      `yaml:"job,omitempty"`

	// DiscoveredFrom records the parent Discover dir for a virtual entry;
	// empty for hand-authored entries. Provenance only, not serialized.
	DiscoveredFrom string `yaml:"-"`
}

// SubagentSpec targets a Claude Code subagent via `claude --print` in Dir.
type SubagentSpec struct {
	Dir     string `yaml:"dir"`                       // repo/folder to run claude in
	Agent   string `yaml:"agent,omitempty"`           // .claude/agents/ slug; "" = plain claude-project delegation
	Timeout int    `yaml:"timeout_seconds,omitempty"` // 0 = DefaultSubagentTimeout
}

// SourceSpec pins a request to one aida library source.
type SourceSpec struct {
	Source string `yaml:"source"` // library source name, e.g. "workouts"
}

// MCPSpec targets a discovered MCP tool. Empty Tool = pick the best tool on Server.
type MCPSpec struct {
	Server string `yaml:"server"`
	Tool   string `yaml:"tool,omitempty"`
}

// JobSpec spawns a background aida --agent job via the jobs queue.
type JobSpec struct {
	Kind string `yaml:"kind,omitempty"` // jobs kind: "agent" (default) | "investigate" | "pr_work"
	Cwd  string `yaml:"cwd,omitempty"`  // working dir for the spawned agent
}

// Display returns the spoken/printed name for an entry: CallSign if set, else Name.
func (e *Entry) Display() string {
	if e.CallSign != "" {
		return e.CallSign
	}
	return e.Name
}

// Request is one natural-language ask against one backend.
type Request struct {
	Task    string // natural-language task, verbatim (or the select step's subtask)
	Profile string
	Origin  string // "voice" | "cli" | "mcp"
}

// Result is what a Backend returns for a Request.
type Result struct {
	Entry  string // roster entry name, for attribution
	Status string // one of the Status* constants
	Text   string // the answer, or empty when delegated
	JobID  string // run-id when Status == StatusDelegated (spoken via jobs.DeriveHandle)
	TookMS int64
}

// Backend executes one request against one entry. Adding a backend kind is one
// implementation of this interface plus one line in the factory map.
type Backend interface {
	Kind() string
	Ask(ctx context.Context, req Request) (Result, error)
}

// Deps bundles the shared runtime handles a backend may need, injected by the
// caller the same way tools.New receives them today. A field may be nil when
// the active backends do not need it (the subagent backend needs none of them).
type Deps struct {
	Jobs      *jobs.Store
	Discovery *mcp.MCPDiscovery
	Profile   string
}

// Errors returned by Resolve.
var (
	// ErrNotFound: no entry matches the reference in the active profile and no
	// other profile claims it either.
	ErrNotFound = errors.New("roster: no matching entry")
	// ErrAmbiguous: the reference matched more than one entry.
	ErrAmbiguous = errors.New("roster: ambiguous reference")
)

// ErrOtherProfile is returned by Resolve when a reference matches an entry that
// exists but is scoped to a different profile than the active one. It carries
// the owning profile so the caller can offer to cross over ("that's on your
// work roster, ask anyway?").
type ErrOtherProfile struct {
	Ref     string
	Entry   *Entry
	Profile string // the profile that owns the matched entry
}

func (e *ErrOtherProfile) Error() string {
	return "roster: " + e.Ref + " belongs to the " + e.Profile + " profile"
}
