package llm

import (
	"fmt"
	"strings"
)

// RosterEntryView is the llm-facing projection of a roster entry: just the
// fields the select step reasons over. dispatch maps its roster.Entry values
// into these so the llm package stays decoupled from the roster package.
type RosterEntryView struct {
	Name        string
	CallSign    string
	Description string
	Kind        string
	Skills      []string
}

// RosterSelectSchema is the JSON schema for the roster select step: which
// roster entry (or entries) should handle a request. One route means a single
// dispatch; several means a fan-out; an empty routes array means "none of
// these fit, fall back to a general query".
func RosterSelectSchema() map[string]interface{} {
	return map[string]interface{}{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]interface{}{
			"routes": map[string]interface{}{
				"type":        "array",
				"description": "Zero or more agents that should handle the request, each with the slice of the request meant for it.",
				"items": map[string]interface{}{
					"type":                 "object",
					"additionalProperties": false,
					"properties": map[string]interface{}{
						"entry": map[string]interface{}{
							"type":        "string",
							"description": "The exact roster entry name (the slug, not the call-sign) to route to.",
						},
						"subtask": map[string]interface{}{
							"type":        "string",
							"description": "The part of the user's request this agent should act on, phrased as a direct instruction.",
						},
					},
					"required": []interface{}{"entry", "subtask"},
				},
			},
		},
		"required": []interface{}{"routes"},
	}
}

// RosterSelectSystemPrompt instructs the model to act as Aida's router.
const RosterSelectSystemPrompt = `You are Aida, a chief-of-staff dispatcher. You route a user's request to the
right member(s) of a roster of agents, each of which owns a domain.

Rules:
- Return one route per agent that should act. If a single agent covers the
  whole request, return exactly one route.
- If the request spans multiple domains, return one route per relevant agent,
  and split the request so each route's "subtask" is only that agent's part.
- Use the exact "entry" slug (not the call-sign) from the roster.
- Only route to agents whose domain genuinely fits. Do not force a match.
- If no agent fits the request, return an empty "routes" array. A general
  assistant will handle it instead.
- Prefer the fewest routes that fully cover the request.`

// RosterSelectUserPrompt renders the roster table and the task for the select step.
func RosterSelectUserPrompt(task string, roster []RosterEntryView) string {
	var b strings.Builder
	b.WriteString("Roster of available agents:\n\n")
	for _, e := range roster {
		call := e.CallSign
		if call == "" {
			call = e.Name
		}
		fmt.Fprintf(&b, "- entry: %s\n  call-sign: %s\n  kind: %s\n  does: %s\n",
			e.Name, call, e.Kind, strings.TrimSpace(e.Description))
		if len(e.Skills) > 0 {
			fmt.Fprintf(&b, "  skills: %s\n", strings.Join(e.Skills, ", "))
		}
	}
	b.WriteString("\nUser request:\n")
	b.WriteString(strings.TrimSpace(task))
	b.WriteString("\n\nReturn the routes.")
	return b.String()
}

// AggregatePart is one agent's reply, for the aggregate step.
type AggregatePart struct {
	Display string // the agent's call-sign
	Text    string
}

// RosterAggregateSystemPrompt instructs the model to merge multiple agent
// replies into one answer for the user.
const RosterAggregateSystemPrompt = `You are Aida, a chief of staff summarizing back to the user. You are given the
user's original request and the replies from the agents you delegated to.

Combine them into one coherent answer. Attribute clearly where it helps
("Pamela found ...", "on the training side ..."), keep it tight, and do not
invent anything the agents did not report. If an agent reported nothing useful,
say so briefly rather than padding.`

// RosterAggregateUserPrompt renders the original task and each agent's reply.
func RosterAggregateUserPrompt(task string, parts []AggregatePart) string {
	var b strings.Builder
	b.WriteString("User's original request:\n")
	b.WriteString(strings.TrimSpace(task))
	b.WriteString("\n\nAgent replies:\n\n")
	for _, p := range parts {
		fmt.Fprintf(&b, "## %s\n%s\n\n", p.Display, strings.TrimSpace(p.Text))
	}
	b.WriteString("Write the combined answer for the user.")
	return b.String()
}
