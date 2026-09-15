package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ryanlitalien/aida/internal/brain"
)

// writeClaudeMemory seeds one active claude-profile memory record with an
// explicit created timestamp so recency ordering is deterministic.
func writeClaudeMemory(t *testing.T, b *brain.Brain, key, body, created string) {
	t.Helper()
	_, err := b.WriteMemory(context.Background(), brain.MemoryRecord{
		Type:    brain.MemoryFact,
		Key:     key,
		Body:    body,
		Tags:    []string{"claude-code", "scope:global"},
		Profile: brain.ClaudeMemoryProfile,
		Created: created,
	})
	if err != nil {
		t.Fatalf("WriteMemory(%s): %v", key, err)
	}
}

// TestClaudeMemoryRecallRegistered guards the Jarvis wiring: the voice
// registry must expose claude_memory_recall, or "what's my last Claude
// memory" regresses to "I don't persist memory".
func TestClaudeMemoryRecallRegistered(t *testing.T) {
	b := openTestBrain(t)
	r := New(b, "", nil, nil, nil, nil, nil, false)
	found := false
	for _, tl := range r.All() {
		if tl.Name == "claude_memory_recall" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("claude_memory_recall not registered in the Jarvis tool registry")
	}
}

// TestClaudeMemoryRecallRecent exercises the tool's Run end to end: with
// recent=true it returns the newest captured memory, newest-first.
func TestClaudeMemoryRecallRecent(t *testing.T) {
	b := openTestBrain(t)
	writeClaudeMemory(t, b, "claude:global:older", "the older captured memory", "2026-07-01T00:00:00Z")
	writeClaudeMemory(t, b, "claude:global:newer", "the newest captured memory", "2026-07-02T00:00:00Z")

	tool := claudeMemoryRecallTool(b)
	out, err := tool.Run(context.Background(), json.RawMessage(`{"recent":true,"limit":2}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "recently captured") {
		t.Errorf("expected recency header, got %q", out)
	}
	newer := strings.Index(out, "newest captured memory")
	older := strings.Index(out, "older captured memory")
	if newer < 0 || older < 0 {
		t.Fatalf("expected both memories in output, got %q", out)
	}
	if newer > older {
		t.Errorf("expected newest-first ordering, got %q", out)
	}
}

// TestClaudeMemoryRecallEmpty reports a clear message rather than an empty
// string when nothing is captured yet.
func TestClaudeMemoryRecallEmpty(t *testing.T) {
	b := openTestBrain(t)
	tool := claudeMemoryRecallTool(b)
	out, err := tool.Run(context.Background(), json.RawMessage(`{"recent":true}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(strings.ToLower(out), "no matching") {
		t.Errorf("expected a no-results message, got %q", out)
	}
}
