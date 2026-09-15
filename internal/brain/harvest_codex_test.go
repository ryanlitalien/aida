package brain

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writeCodexFixture lays down a minimal ~/.codex-shaped tree: a
// session_index.jsonl with one entry and a matching rollout file under
// sessions/<YYYY>/<MM>/<DD>/.
func writeCodexFixture(t *testing.T, codexHome, id, threadName, updatedAt, cwd string, messages [][2]string) {
	t.Helper()

	idx := map[string]string{"id": id, "thread_name": threadName, "updated_at": updatedAt}
	line, err := json.Marshal(idx)
	if err != nil {
		t.Fatalf("marshal session_index line: %v", err)
	}
	indexPath := filepath.Join(codexHome, "session_index.jsonl")
	f, err := os.OpenFile(indexPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("open session_index.jsonl: %v", err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		t.Fatalf("write session_index.jsonl: %v", err)
	}
	f.Close()

	sessDir := filepath.Join(codexHome, "sessions", "2026", "08", "09")
	if err := os.MkdirAll(sessDir, 0755); err != nil {
		t.Fatalf("mkdir sessions dir: %v", err)
	}

	var lines []map[string]interface{}
	lines = append(lines, map[string]interface{}{
		"type":    "session_meta",
		"payload": map[string]interface{}{"cwd": cwd, "id": id},
	})
	for _, m := range messages {
		role, text := m[0], m[1]
		lines = append(lines, map[string]interface{}{
			"type": "response_item",
			"payload": map[string]interface{}{
				"type": "message",
				"role": role,
				"content": []map[string]interface{}{
					{"type": textTypeForRole(role), "text": text},
				},
			},
		})
	}
	// Interleave a tool-noise line to verify it's skipped.
	lines = append(lines, map[string]interface{}{
		"type":    "response_item",
		"payload": map[string]interface{}{"type": "function_call", "name": "shell", "arguments": "{}"},
	})

	rolloutPath := filepath.Join(sessDir, "rollout-2026-08-09T13-00-00-"+id+".jsonl")
	rf, err := os.Create(rolloutPath)
	if err != nil {
		t.Fatalf("create rollout file: %v", err)
	}
	defer rf.Close()
	for _, l := range lines {
		data, err := json.Marshal(l)
		if err != nil {
			t.Fatalf("marshal rollout line: %v", err)
		}
		if _, err := rf.Write(append(data, '\n')); err != nil {
			t.Fatalf("write rollout line: %v", err)
		}
	}
}

func textTypeForRole(role string) string {
	if role == "user" {
		return "input_text"
	}
	return "output_text"
}

func TestListCodexSessionMetas(t *testing.T) {
	home := t.TempDir()
	writeCodexFixture(t, home, "sess-a", "Thread A", "2026-08-09T13:00:00Z", "/Users/fakehome/dev/aida", [][2]string{
		{"user", "hello"}, {"assistant", "hi"},
	})
	writeCodexFixture(t, home, "sess-b", "Thread B", "2026-08-10T13:00:00Z", "/Users/fakehome/dev/aida", [][2]string{
		{"user", "world"},
	})

	metas, err := listCodexSessionMetas(home)
	if err != nil {
		t.Fatalf("listCodexSessionMetas: %v", err)
	}
	if len(metas) != 2 {
		t.Fatalf("expected 2 metas, got %d", len(metas))
	}
}

func TestListCodexSessionMetas_MissingFile(t *testing.T) {
	home := t.TempDir()
	metas, err := listCodexSessionMetas(home)
	if err != nil {
		t.Fatalf("expected no error for missing session_index.jsonl, got %v", err)
	}
	if metas != nil {
		t.Errorf("expected nil metas, got %v", metas)
	}
}

func TestBuildCodexHarvestSession(t *testing.T) {
	home := t.TempDir()
	writeCodexFixture(t, home, "sess-a", "Thread A", "2026-08-09T13:00:00Z", "/Users/fakehome/dev/aida", [][2]string{
		{"user", "I prefer tabs over spaces."},
		{"assistant", "Got it, I'll use tabs."},
	})

	metas, err := listCodexSessionMetas(home)
	if err != nil || len(metas) != 1 {
		t.Fatalf("listCodexSessionMetas: %v, %v", metas, err)
	}

	sess, ok, err := buildCodexHarvestSession(home, metas[0])
	if err != nil {
		t.Fatalf("buildCodexHarvestSession: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true")
	}
	if sess.Scope != "project:-Users-fakehome-dev-aida" {
		t.Errorf("scope = %q", sess.Scope)
	}
	if !contains(sess.Transcript, "I prefer tabs over spaces.") {
		t.Errorf("transcript missing user text: %q", sess.Transcript)
	}
	if !contains(sess.Transcript, "Got it, I'll use tabs.") {
		t.Errorf("transcript missing assistant text: %q", sess.Transcript)
	}
	if contains(sess.Transcript, "function_call") || contains(sess.Transcript, "shell") {
		t.Errorf("transcript should not contain tool-call noise: %q", sess.Transcript)
	}
}

func TestBuildCodexHarvestSession_NoMatchingRollout(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "sessions"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	meta := codexSessionMeta{ID: "does-not-exist"}
	_, ok, err := buildCodexHarvestSession(home, meta)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if ok {
		t.Error("expected ok=false when no rollout file matches")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (func() bool {
		for i := 0; i+len(substr) <= len(s); i++ {
			if s[i:i+len(substr)] == substr {
				return true
			}
		}
		return false
	})()
}

// fakeDistillOne returns a single fixed memory for any input -- used to
// exercise the codex/gemini orchestrators end to end without a live
// LLM.
func fakeDistillOne(name, body string) DistillFunc {
	return fakeDistill(harvestDistillResponse{
		Memories: []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Type        string `json:"type"`
			Body        string `json:"body"`
		}{
			{Name: name, Description: "d", Type: "fact", Body: body},
		},
	})
}

// TestHarvestCodex_EndToEnd exercises the full codex orchestrator
// (harvestCodex) against a small fixture tree and a fake DistillFunc:
// list -> filter -> read rollout -> distill -> write -> watermark.
func TestHarvestCodex_EndToEnd(t *testing.T) {
	home := t.TempDir()
	writeCodexFixture(t, home, "sess-a", "Thread A", "2026-08-01T00:00:00Z", "/Users/fakehome/dev/aida", [][2]string{
		{"user", "I prefer tabs over spaces."},
	})

	b := newTestBrain(t)
	ctx := context.Background()
	distill := fakeDistillOne("editor-tabs", "The user prefers tabs.")

	result, err := b.harvestCodex(ctx, home, distill, HarvestOptions{Now: mustParseTime(t, "2026-08-17T00:00:00Z")})
	if err != nil {
		t.Fatalf("harvestCodex: %v", err)
	}
	if result.CandidatesTotal != 1 {
		t.Errorf("CandidatesTotal = %d, want 1", result.CandidatesTotal)
	}
	if result.Selected != 1 {
		t.Errorf("Selected = %d, want 1", result.Selected)
	}
	if len(result.Sessions) != 1 || len(result.Sessions[0].Written) != 1 {
		t.Fatalf("expected 1 session with 1 memory, got %+v", result.Sessions)
	}
	if result.Sessions[0].Written[0].Key != "codex:sess-a:editor-tabs" {
		t.Errorf("key = %q", result.Sessions[0].Written[0].Key)
	}

	// Second run: nothing new (watermark advanced past this session).
	result2, err := b.harvestCodex(ctx, home, distill, HarvestOptions{Now: mustParseTime(t, "2026-08-17T00:00:00Z")})
	if err != nil {
		t.Fatalf("harvestCodex (2nd): %v", err)
	}
	if result2.Selected != 0 {
		t.Errorf("second run Selected = %d, want 0", result2.Selected)
	}
}

// TestHarvestCodex_SkipsStillRunningSessions verifies a session updated
// moments ago (within the quiet window) is excluded.
func TestHarvestCodex_SkipsStillRunningSessions(t *testing.T) {
	home := t.TempDir()
	now := mustParseTime(t, "2026-08-17T12:00:00Z")
	writeCodexFixture(t, home, "sess-live", "Live", now.Add(-2*60_000_000_000).Format("2006-01-02T15:04:05Z"), "/Users/fakehome/dev/aida", [][2]string{
		{"user", "still typing"},
	})

	b := newTestBrain(t)
	ctx := context.Background()
	distill := fakeDistillOne("x", "y")

	result, err := b.harvestCodex(ctx, home, distill, HarvestOptions{Now: now})
	if err != nil {
		t.Fatalf("harvestCodex: %v", err)
	}
	if result.SkippedQuiet != 1 {
		t.Errorf("SkippedQuiet = %d, want 1", result.SkippedQuiet)
	}
	if result.Selected != 0 {
		t.Errorf("Selected = %d, want 0", result.Selected)
	}
}
