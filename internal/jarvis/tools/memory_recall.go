package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ryanlitalien/aida/internal/brain"
)

// ─── claude_memory_recall ────────────────────────────────────────────────────

// claudeMemoryRecallTool lets Jarvis answer questions about the durable
// memories captured from Claude Code sessions (the Phase-3 recall path of
// the memory bridge). Without it, "what was the last Claude memory you
// have?" wrongly drew "I don't persist memory" - the memories live under
// Profile="claude" while Jarvis runs under the auto-detected home/work
// profile, and none of her other tools read memory_records. This tool calls
// brain.RecallMemories with the pinned "claude" profile, so it reaches them
// regardless of Jarvis's own profile.

type claudeMemoryRecallInput struct {
	Query  string `json:"query"`
	Recent bool   `json:"recent"`
	Limit  int    `json:"limit"`
}

func claudeMemoryRecallTool(b *brain.Brain) Tool {
	return Tool{
		Name: "claude_memory_recall",
		Description: "Recall the user's durable Claude Code memories - the facts, " +
			"preferences, and project notes their coding assistant has saved to the " +
			"brain (hundreds of records and growing). USE THIS whenever the user asks " +
			"what you remember or have noted, about \"my last/latest Claude memory\", " +
			"\"what did you save about <project/person/preference>\", \"what do you " +
			"know about <project/person/thing>\", or any \"what do you know about X\" " +
			"/ \"tell me about X\" question where X could plausibly be something " +
			"previously discussed or saved - try this before falling back to a " +
			"general web/engine query. Set recent=true for last/latest/newest " +
			"questions (returns the most recently captured); otherwise it semantically " +
			"searches by meaning. NEVER tell the user you don't persist memory - you " +
			"do, through this tool.",
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"query": map[string]interface{}{
					"type":        "string",
					"description": "what to recall, in natural language; optional when recent=true",
				},
				"recent": map[string]interface{}{
					"type":        "boolean",
					"description": "true for 'last/latest/newest memory' questions - returns the most recently captured memories instead of a semantic match",
				},
				"limit": map[string]interface{}{
					"type":        "integer",
					"description": "max memories to return (default 3)",
				},
			},
		},
		Run: func(_ context.Context, raw json.RawMessage) (string, error) {
			var in claudeMemoryRecallInput
			if len(raw) > 0 {
				if err := json.Unmarshal(raw, &in); err != nil {
					return "", err
				}
			}
			limit := in.Limit
			if limit <= 0 {
				limit = 3
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			res, err := b.RecallMemories(ctx, in.Query, limit, brain.ClaudeMemoryProfile, "", "", in.Recent)
			if err != nil {
				return "", fmt.Errorf("claude_memory_recall: %w", err)
			}
			if len(res.Memories) == 0 {
				return "No matching Claude memories found.", nil
			}
			var sb strings.Builder
			if res.BySimilarity {
				sb.WriteString("Most relevant Claude memories:\n")
			} else {
				sb.WriteString("Most recently captured Claude memories:\n")
			}
			for i, m := range res.Memories {
				body := strings.ReplaceAll(strings.TrimSpace(m.Record.Body), "\n", " ")
				if len(body) > 300 {
					body = body[:300] + "..."
				}
				fmt.Fprintf(&sb, "%d. %s\n", i+1, body)
			}
			return strings.TrimRight(sb.String(), "\n"), nil
		},
	}
}
