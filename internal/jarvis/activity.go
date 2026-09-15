package jarvis

// Live activity view for the Jarvis status page. The dome-build work was
// invisible mid-turn - the only trace was terminal stdout and the
// after-the-fact audit log, which is why the user "had no idea what was
// happening." Activity tracks, in memory:
//
//	(a) the tool currently executing (name + when it started), set before
//	    each tool runs and cleared once the turn's reply is produced; and
//	(b) a ring of the last few completed turns.
//
// This covers the synchronous-voice-turn half of the observability gap
// (minecraft_ask / aida_query), which never become jobs and so never
// appear on the /runs page. Concurrency-safe.

import (
	"sync"
	"time"

	"github.com/ryanlitalien/aida/internal/jarvis/audit"
)

// Activity is the concurrency-safe live/recent activity tracker.
type Activity struct {
	mu          sync.Mutex
	currentTool string
	currentAt   time.Time
	ring        []TurnActivity
	max         int
}

// TurnActivity is one completed turn in the recent-turns ring.
type TurnActivity struct {
	StartedAt string   `json:"started_at,omitempty"`
	Query     string   `json:"query"`
	Reply     string   `json:"reply,omitempty"`
	Error     string   `json:"error,omitempty"`
	Tools     []string `json:"tools,omitempty"`
	TookMs    int64    `json:"took_ms,omitempty"`
}

// ActivitySnapshot is a point-in-time copy returned by the status endpoint.
// Recent is newest-first.
type ActivitySnapshot struct {
	CurrentTool  string         `json:"current_tool"` // "" when idle
	CurrentForMs int64          `json:"current_for_ms,omitempty"`
	Recent       []TurnActivity `json:"recent"`
}

func newActivity() *Activity { return &Activity{max: 10} }

// SetCurrentTool marks a tool as executing now. Called before each tool runs.
func (a *Activity) SetCurrentTool(name string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.currentTool = name
	a.currentAt = time.Now()
	a.mu.Unlock()
}

// ClearCurrentTool marks Jarvis idle (no tool executing). Called once the
// turn's final reply is produced.
func (a *Activity) ClearCurrentTool() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.currentTool = ""
	a.currentAt = time.Time{}
	a.mu.Unlock()
}

// PushTurn appends a completed turn to the ring, trimming to the cap.
func (a *Activity) PushTurn(rec audit.Record) {
	if a == nil {
		return
	}
	t := TurnActivity{
		StartedAt: rec.StartedAt,
		Query:     rec.Query,
		Reply:     rec.Reply,
		Error:     rec.Error,
		TookMs:    rec.TookMs,
	}
	for _, c := range rec.ToolCalls {
		t.Tools = append(t.Tools, c.Name)
	}
	a.mu.Lock()
	a.ring = append(a.ring, t)
	if len(a.ring) > a.max {
		a.ring = a.ring[len(a.ring)-a.max:]
	}
	a.mu.Unlock()
}

// Snapshot returns a copy of the current + recent activity, newest-first.
func (a *Activity) Snapshot() ActivitySnapshot {
	if a == nil {
		return ActivitySnapshot{Recent: []TurnActivity{}}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	snap := ActivitySnapshot{CurrentTool: a.currentTool}
	if a.currentTool != "" && !a.currentAt.IsZero() {
		snap.CurrentForMs = time.Since(a.currentAt).Milliseconds()
	}
	snap.Recent = make([]TurnActivity, 0, len(a.ring))
	for i := len(a.ring) - 1; i >= 0; i-- {
		snap.Recent = append(snap.Recent, a.ring[i])
	}
	return snap
}
