package llm

import (
	"fmt"
	"strings"
	"sync"
)

// CostTracker accumulates per-call token counts and USD costs for a
// single aida session. Thread-safe so concurrent source executions
// can record costs simultaneously.
type CostTracker struct {
	mu       sync.Mutex
	calls    []CallCost
	totalUSD float64
}

// CallCost records one LLM API call's cost.
type CallCost struct {
	Stage        string // "parse", "route", "execute", "synthesize", "quality", ""
	Model        string
	Provider     string
	InputTokens  int
	OutputTokens int
	CostUSD      float64
	DurationMs   int64
}

// NewCostTracker creates an empty tracker.
func NewCostTracker() *CostTracker {
	return &CostTracker{}
}

// Record logs a completed LLM call. Computes USD cost from the
// response's token counts using the built-in pricing table.
func (t *CostTracker) Record(stage string, resp *Response) {
	if t == nil || resp == nil {
		return
	}
	pricing := LookupPricing(resp.Model)
	cost := float64(resp.InputTokens)/1_000_000*pricing.InputPerM +
		float64(resp.OutputTokens)/1_000_000*pricing.OutputPerM

	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls = append(t.calls, CallCost{
		Stage:        stage,
		Model:        resp.Model,
		Provider:     resp.ProviderName,
		InputTokens:  resp.InputTokens,
		OutputTokens: resp.OutputTokens,
		CostUSD:      cost,
		DurationMs:   resp.DurationMs,
	})
	t.totalUSD += cost
}

// TagLastCall retroactively sets the stage name on the most recent call.
// Used by CompleteWithStage which calls Complete first then tags.
func (t *CostTracker) TagLastCall(stage string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.calls) > 0 {
		t.calls[len(t.calls)-1].Stage = stage
	}
}

// TotalUSD returns the cumulative cost.
func (t *CostTracker) TotalUSD() float64 {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.totalUSD
}

// TotalTokens returns total input and output tokens.
func (t *CostTracker) TotalTokens() (input, output int) {
	if t == nil {
		return 0, 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, c := range t.calls {
		input += c.InputTokens
		output += c.OutputTokens
	}
	return
}

// Calls returns a copy of all recorded call costs.
func (t *CostTracker) Calls() []CallCost {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]CallCost, len(t.calls))
	copy(out, t.calls)
	return out
}

// Summary returns a one-line cost breakdown like:
// "Cost: $0.0032 (5 calls: parse $0.0008, route $0.0006, ...)"
func (t *CostTracker) Summary() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.calls) == 0 {
		return ""
	}
	var parts []string
	for _, c := range t.calls {
		stage := c.Stage
		if stage == "" {
			stage = "call"
		}
		if c.CostUSD > 0 {
			parts = append(parts, fmt.Sprintf("%s $%.4f", stage, c.CostUSD))
		} else {
			parts = append(parts, fmt.Sprintf("%s (free)", stage))
		}
	}
	in, out := 0, 0
	for _, c := range t.calls {
		in += c.InputTokens
		out += c.OutputTokens
	}
	return fmt.Sprintf("$%.4f (%d calls: %s) | %d in / %d out tokens",
		t.totalUSD, len(t.calls), strings.Join(parts, ", "), in, out)
}
