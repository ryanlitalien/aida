// Package runs records every aida invocation to ~/.aida/runs/ as a
// structured JSON file. This makes prompt tuning a normal Claude Code
// workflow: you run a query, see what happened, edit prompts, then replay.
package runs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ryanlitalien/aida/internal/config"
)

const RunsDirName = "runs"

// Run is the durable record of one aida invocation.
type Run struct {
	ID        string    `json:"id"`
	StartedAt time.Time `json:"started_at"`
	TotalMs   int64     `json:"total_ms"`
	Question  string    `json:"question"`
	Cwd       string    `json:"cwd"`
	Profile   string    `json:"profile"`
	Action    string    `json:"action"`
	Strategy  string    `json:"strategy"`
	Entities  []string  `json:"entities,omitempty"`

	// Library bundle info (route-driven layer / source resolution).
	LibraryLayers  []string `json:"library_layers,omitempty"`
	LibrarySources []string `json:"library_sources,omitempty"`
	RouteMatches   int      `json:"route_matches"`

	// Plan + per-source outcomes.
	Phases []PhaseRun `json:"phases"`
	Answer string     `json:"answer"`
	Errors []string   `json:"errors,omitempty"`

	// Agent mode tool call audit trail.
	AgentToolCalls []AgentToolCallRun `json:"agent_tool_calls,omitempty"`

	// Tracing (Phase 3 observability).
	Tracing *TracingData `json:"tracing,omitempty"`
}

// AgentToolCallRun records a single tool invocation in agent mode.
type AgentToolCallRun struct {
	Turn   int    `json:"turn"`
	Tool   string `json:"tool"`
	Input  string `json:"input"`
	Output string `json:"output,omitempty"`
	Error  string `json:"error,omitempty"`
}

// TracingData captures detailed timing and cost breakdown for a run.
type TracingData struct {
	Spans     []TracingSpan `json:"spans"`
	TotalCost float64       `json:"total_cost_usd"`
	TotalIn   int           `json:"total_input_tokens"`
	TotalOut  int           `json:"total_output_tokens"`
	LLMCalls  int           `json:"llm_calls"`
}

// TracingSpan records one timed component of the pipeline.
type TracingSpan struct {
	Name       string  `json:"name"` // "parse", "classify", "route", "execute:sqlite", "verify", "synthesize", "score"
	DurationMs int64   `json:"duration_ms"`
	CostUSD    float64 `json:"cost_usd,omitempty"`
	InTokens   int     `json:"input_tokens,omitempty"`
	OutTokens  int     `json:"output_tokens,omitempty"`
	Model      string  `json:"model,omitempty"`
	Status     string  `json:"status,omitempty"` // "ok", "error", "timeout"
}

// PhaseRun records one execution phase.
type PhaseRun struct {
	Name     string      `json:"name"`
	Parallel bool        `json:"parallel"`
	Sources  []SourceRun `json:"sources"`
}

// SourceRun records the outcome of one source within a phase.
type SourceRun struct {
	Name          string `json:"name"`
	Score         int    `json:"score,omitempty"`
	Command       string `json:"command,omitempty"`
	Status        string `json:"status"`
	Summary       string `json:"summary,omitempty"`
	ArtifactCount int    `json:"artifact_count"`
	DurationMs    int64  `json:"duration_ms"`
}

// Dir returns ~/.aida/runs/.
func Dir() string {
	return filepath.Join(config.Dir(), RunsDirName)
}

// Save writes the run to ~/.aida/runs/<id>.json. The ID is set if empty.
func Save(r *Run) (string, error) {
	if err := os.MkdirAll(Dir(), 0755); err != nil {
		return "", err
	}
	if r.ID == "" {
		r.ID = NewID(r.StartedAt, r.Question)
	}
	path := filepath.Join(Dir(), r.ID+".json")
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		return "", err
	}
	return path, nil
}

// Load reads a run by id from ~/.aida/runs/<id>.json.
func Load(id string) (*Run, error) {
	path := filepath.Join(Dir(), id+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r Run
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// List returns IDs of recorded runs sorted by id descending (newest first).
func List() ([]string, error) {
	entries, err := os.ReadDir(Dir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		ids = append(ids, strings.TrimSuffix(name, ".json"))
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	return ids, nil
}

// Latest returns the most recently recorded run, or nil if there are none.
func Latest() (*Run, error) {
	ids, err := List()
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	return Load(ids[0])
}

// FindLatestInCwd returns the most recent run in the given cwd within
// withinMinutes, or nil if none. Used to resolve referential follow-up
// queries ("do the same thing, but for issues") to the prior turn's
// scope. This is ORDINAL retrieval - most-recent-in-directory, not
// similarity - because anaphora ("same", "again") need a timeline
// pointer, not a content match.
func FindLatestInCwd(cwd string, withinMinutes int) (*Run, error) {
	if cwd == "" || withinMinutes <= 0 {
		return nil, nil
	}
	ids, err := List()
	if err != nil {
		return nil, err
	}
	cutoff := time.Now().UTC().Add(-time.Duration(withinMinutes) * time.Minute)
	for _, id := range ids {
		r, err := Load(id)
		if err != nil || r == nil {
			continue
		}
		if r.Cwd != cwd {
			continue
		}
		if r.StartedAt.UTC().Before(cutoff) {
			// List is newest-first; once we fall off the cutoff nothing
			// older will match either.
			return nil, nil
		}
		return r, nil
	}
	return nil, nil
}

// NewID builds a run id from a timestamp and question slug.
func NewID(t time.Time, question string) string {
	if t.IsZero() {
		t = time.Now()
	}
	slug := slugify(question)
	if slug == "" {
		slug = "query"
	}
	return fmt.Sprintf("%s-%s", t.UTC().Format("20060102-150405"), slug)
}

func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '_':
			b.WriteRune('-')
		}
		if b.Len() >= 40 {
			break
		}
	}
	return strings.Trim(b.String(), "-")
}
