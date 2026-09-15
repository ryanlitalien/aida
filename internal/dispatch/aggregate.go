package dispatch

import (
	"context"
	"fmt"
	"strings"

	"github.com/ryanlitalien/aida/internal/llm"
	"github.com/ryanlitalien/aida/internal/roster"
)

// aggregate turns the executed routes into one user-facing answer. A single
// route returns verbatim (with a status note for non-answers), so it costs
// zero LLM calls. Several routes with at least one real answer are synthesized
// in one LLM call; if the LLM is unavailable or every route failed, it falls
// back to a plain attributed concatenation.
func (d *Dispatcher) aggregate(ctx context.Context, task string, executed []routed) string {
	switch len(executed) {
	case 0:
		return ""
	case 1:
		return d.single(executed[0])
	}

	parts := make([]llm.AggregatePart, 0, len(executed))
	anySuccess := false
	for _, r := range executed {
		parts = append(parts, llm.AggregatePart{Display: r.entry.Display(), Text: resultText(r)})
		if r.result.Status == roster.StatusSuccess {
			anySuccess = true
		}
	}

	if d.LLM != nil && anySuccess {
		answer, err := d.LLM.CompleteWithStage(ctx, "synthesize",
			llm.RosterAggregateSystemPrompt,
			llm.RosterAggregateUserPrompt(task, parts))
		if err == nil {
			if trimmed := strings.TrimSpace(answer); trimmed != "" {
				return trimmed
			}
		}
	}

	return plainConcat(parts)
}

// single renders a one-route dispatch without an LLM call.
func (d *Dispatcher) single(r routed) string {
	display := r.entry.Display()
	switch r.result.Status {
	case roster.StatusDelegated:
		return fmt.Sprintf("Delegated to %s. I'll report back when it's done (%s).", display, handleFor(r.result.JobID))
	case roster.StatusSuccess:
		return r.result.Text
	default:
		return fmt.Sprintf("%s couldn't help (%s): %s", display, r.result.Status, r.result.Text)
	}
}

// resultText is the per-route representation fed to the aggregate LLM and the
// plain-concat fallback.
func resultText(r routed) string {
	switch r.result.Status {
	case roster.StatusSuccess:
		return r.result.Text
	case roster.StatusDelegated:
		return fmt.Sprintf("(started in the background, handle %s)", handleFor(r.result.JobID))
	default:
		return fmt.Sprintf("(no answer: %s%s)", r.result.Status, colonSuffix(r.result.Text))
	}
}

func colonSuffix(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return " - " + s
}

// plainConcat is the no-LLM fallback: one attributed block per route.
func plainConcat(parts []llm.AggregatePart) string {
	var b strings.Builder
	for i, p := range parts {
		if i > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "%s: %s", p.Display, strings.TrimSpace(p.Text))
	}
	return b.String()
}
