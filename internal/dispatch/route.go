package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/ryanlitalien/aida/internal/llm"
	"github.com/ryanlitalien/aida/internal/roster"
)

// chooseRoutes decides which entries handle the task. Deterministic first: if
// the task text names any call-signs, route to exactly those (no LLM). Only
// when nobody is named does the LLM select step run. An empty return means
// "route to nobody" -- Dispatch then falls back to a general query.
func (d *Dispatcher) chooseRoutes(ctx context.Context, task string) []routeSel {
	if named := namedInText(d.Roster, task); len(named) > 0 {
		return named
	}
	if d.LLM == nil {
		return nil
	}
	routes, err := d.selectRoutes(ctx, task)
	if err != nil {
		// A select failure is non-fatal: fall back to a general query rather
		// than erroring the whole dispatch.
		return nil
	}
	return routes
}

// namedInText scans the task for any active entry named by call-sign, alias,
// or spaced-out slug, and returns one route per distinct entry mentioned (the
// whole task as each one's subtask). The reserved aida entry is never a target.
func namedInText(r *roster.Roster, task string) []routeSel {
	lower := strings.ToLower(task)
	var out []routeSel
	seen := map[string]bool{}

	for _, e := range r.Entries() {
		if e.Kind == roster.KindAida || seen[e.Name] {
			continue
		}
		for _, phrase := range namePhrases(e) {
			if phrase == "" {
				continue
			}
			if wordPresent(lower, strings.ToLower(phrase)) {
				out = append(out, routeSel{entry: e, subtask: task})
				seen[e.Name] = true
				break
			}
		}
	}
	return out
}

// namePhrases returns the human-facing tokens that should match an entry in
// free text: its call-sign, its aliases, and its slug with hyphens spaced out
// (so "product-manager" is reachable as "product manager"). Tokens shorter
// than 3 characters are dropped to avoid noise.
func namePhrases(e *roster.Entry) []string {
	var phrases []string
	add := func(s string) {
		if len(strings.TrimSpace(s)) >= 3 {
			phrases = append(phrases, s)
		}
	}
	add(e.CallSign)
	for _, a := range e.Aliases {
		add(a)
	}
	add(strings.ReplaceAll(e.Name, "-", " "))
	return phrases
}

// wordPresent reports whether phrase appears in text on word boundaries.
// phrase and text are both expected already-lowercased.
func wordPresent(text, phrase string) bool {
	re, err := regexp.Compile(`\b` + regexp.QuoteMeta(phrase) + `\b`)
	if err != nil {
		return strings.Contains(text, phrase)
	}
	return re.MatchString(text)
}

// selectRoutes runs the single LLM select call over the active roster and maps
// the chosen entry slugs back to entries, dropping any the model invented.
func (d *Dispatcher) selectRoutes(ctx context.Context, task string) ([]routeSel, error) {
	views := make([]llm.RosterEntryView, 0, len(d.Roster.Entries()))
	for _, e := range d.Roster.Entries() {
		if e.Kind == roster.KindAida {
			continue
		}
		views = append(views, llm.RosterEntryView{
			Name:        e.Name,
			CallSign:    e.CallSign,
			Description: e.Description,
			Kind:        e.Kind,
			Skills:      e.Skills,
		})
	}
	if len(views) == 0 {
		return nil, nil
	}

	raw, err := d.LLM.CompleteJSONWithStage(ctx, "route",
		llm.RosterSelectSystemPrompt,
		llm.RosterSelectUserPrompt(task, views),
		llm.RosterSelectSchema())
	if err != nil {
		return nil, fmt.Errorf("roster select: %w", err)
	}

	var decision struct {
		Routes []struct {
			Entry   string `json:"entry"`
			Subtask string `json:"subtask"`
		} `json:"routes"`
	}
	if err := json.Unmarshal([]byte(raw), &decision); err != nil {
		return nil, fmt.Errorf("parsing roster select: %w", err)
	}

	var out []routeSel
	seen := map[string]bool{}
	for _, r := range decision.Routes {
		e, ok := d.Roster.Get(r.Entry)
		if !ok || e.Kind == roster.KindAida || seen[e.Name] {
			continue
		}
		subtask := strings.TrimSpace(r.Subtask)
		if subtask == "" {
			subtask = task
		}
		out = append(out, routeSel{entry: e, subtask: subtask})
		seen[e.Name] = true
	}
	return out, nil
}
