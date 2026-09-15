package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ryanlitalien/aida/internal/brain"
)

func claudeFactCount(t *testing.T, b *brain.Brain) int {
	t.Helper()
	recs, err := b.DB.ListMemory(brain.MemoryListOpts{
		Types:   []brain.MemoryType{brain.MemoryFact},
		Profile: brain.ClaudeMemoryProfile,
	})
	if err != nil {
		t.Fatalf("ListMemory: %v", err)
	}
	return len(recs)
}

func TestMemorySaveRegistered(t *testing.T) {
	b := openTestBrain(t)
	r := New(b, "", nil, nil, nil, nil, nil, false)
	found := false
	for _, tl := range r.All() {
		if tl.Name == "memory_save" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("memory_save not registered in the Jarvis tool registry")
	}
}

func TestMemorySaveProposeDoesNotWrite(t *testing.T) {
	b := openTestBrain(t)
	tool := memorySaveTool(b)
	out, err := tool.Run(context.Background(), json.RawMessage(`{"content":"Ryan owns a 2021 VW Atlas","key":"vehicle-atlas"}`))
	if err != nil {
		t.Fatalf("propose Run: %v", err)
	}
	if !strings.Contains(out, "PROPOSED") {
		t.Errorf("expected a PROPOSED read-back, got %q", out)
	}
	if n := claudeFactCount(t, b); n != 0 {
		t.Errorf("propose must not write; found %d records", n)
	}
}

func TestMemorySaveCommitWrites(t *testing.T) {
	b := openTestBrain(t)
	tool := memorySaveTool(b)
	if _, err := tool.Run(context.Background(), json.RawMessage(`{"content":"Ryan owns a 2021 VW Atlas","key":"vehicle-atlas"}`)); err != nil {
		t.Fatalf("propose: %v", err)
	}
	out, err := tool.Run(context.Background(), json.RawMessage(`{"confirm":true}`))
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if !strings.Contains(out, "Committed") {
		t.Errorf("expected a commit confirmation, got %q", out)
	}
	rec, err := b.DB.ActiveMemoryByKey(brain.MemoryFact, "vehicle-atlas", brain.ClaudeMemoryProfile)
	if err != nil {
		t.Fatalf("ActiveMemoryByKey: %v", err)
	}
	if !strings.Contains(rec.Body, "2021 VW Atlas") {
		t.Errorf("saved body wrong: %q", rec.Body)
	}
	if rec.Profile != brain.ClaudeMemoryProfile {
		t.Errorf("expected claude profile, got %q", rec.Profile)
	}
}

func TestMemorySaveCommitWithoutStage(t *testing.T) {
	b := openTestBrain(t)
	tool := memorySaveTool(b)
	out, err := tool.Run(context.Background(), json.RawMessage(`{"confirm":true}`))
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if !strings.Contains(strings.ToLower(out), "nothing staged") {
		t.Errorf("expected a nothing-staged message, got %q", out)
	}
	if n := claudeFactCount(t, b); n != 0 {
		t.Errorf("expected no write, found %d", n)
	}
}

func TestMemorySaveResaveSupersedes(t *testing.T) {
	b := openTestBrain(t)
	tool := memorySaveTool(b)
	// First save.
	if _, err := tool.Run(context.Background(), json.RawMessage(`{"content":"Ryan owns a 2021 VW Atlas","key":"vehicle-atlas"}`)); err != nil {
		t.Fatalf("propose 1: %v", err)
	}
	if _, err := tool.Run(context.Background(), json.RawMessage(`{"confirm":true}`)); err != nil {
		t.Fatalf("commit 1: %v", err)
	}
	// Re-save same key, new body.
	if _, err := tool.Run(context.Background(), json.RawMessage(`{"content":"Ryan owns a 2021 VW Atlas SE V6 4Motion","key":"vehicle-atlas"}`)); err != nil {
		t.Fatalf("propose 2: %v", err)
	}
	if _, err := tool.Run(context.Background(), json.RawMessage(`{"confirm":true}`)); err != nil {
		t.Fatalf("commit 2: %v", err)
	}

	rec, err := b.DB.ActiveMemoryByKey(brain.MemoryFact, "vehicle-atlas", brain.ClaudeMemoryProfile)
	if err != nil {
		t.Fatalf("ActiveMemoryByKey: %v", err)
	}
	if !strings.Contains(rec.Body, "SE V6 4Motion") {
		t.Errorf("expected superseding body, got %q", rec.Body)
	}
	// Exactly one active record for the key.
	recs, _ := b.DB.ListMemory(brain.MemoryListOpts{
		Types:   []brain.MemoryType{brain.MemoryFact},
		Profile: brain.ClaudeMemoryProfile,
	})
	active := 0
	for _, r := range recs {
		if r.Key == "vehicle-atlas" {
			active++
		}
	}
	if active != 1 {
		t.Errorf("expected 1 active record for key, got %d", active)
	}
}
