package brain

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writeGeminiCLISessionFixture lays down one
// ~/.gemini/tmp/<hash>/chats/session-*.json file.
func writeGeminiCLISessionFixture(t *testing.T, geminiHome, hash, sessionID, lastUpdated string, messages [][2]string) {
	t.Helper()

	chatsDir := filepath.Join(geminiHome, "tmp", hash, "chats")
	if err := os.MkdirAll(chatsDir, 0755); err != nil {
		t.Fatalf("mkdir chats dir: %v", err)
	}

	type msg struct {
		ID        string `json:"id"`
		Timestamp string `json:"timestamp"`
		Type      string `json:"type"`
		Content   string `json:"content"`
	}
	var msgs []msg
	for i, m := range messages {
		msgs = append(msgs, msg{ID: string(rune('a' + i)), Timestamp: lastUpdated, Type: m[0], Content: m[1]})
	}

	sess := map[string]interface{}{
		"sessionId":   sessionID,
		"projectHash": hash,
		"startTime":   lastUpdated,
		"lastUpdated": lastUpdated,
		"messages":    msgs,
	}
	data, err := json.Marshal(sess)
	if err != nil {
		t.Fatalf("marshal session fixture: %v", err)
	}
	path := filepath.Join(chatsDir, "session-"+sessionID+".json")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write session fixture: %v", err)
	}
}

func TestListGeminiCLISessionFiles(t *testing.T) {
	home := t.TempDir()
	writeGeminiCLISessionFixture(t, home, "hash1", "sess-1", "2026-08-01T00:00:00Z", [][2]string{
		{"user", "hello"}, {"gemini", "hi"},
	})
	writeGeminiCLISessionFixture(t, home, "hash2", "sess-2", "2026-08-02T00:00:00Z", [][2]string{
		{"user", "world"},
	})

	files, err := listGeminiCLISessionFiles(home)
	if err != nil {
		t.Fatalf("listGeminiCLISessionFiles: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d: %v", len(files), files)
	}
}

func TestListGeminiCLISessionFiles_MissingTmpDir(t *testing.T) {
	home := t.TempDir()
	files, err := listGeminiCLISessionFiles(home)
	if err != nil {
		t.Fatalf("expected no error for missing tmp dir, got %v", err)
	}
	if files != nil {
		t.Errorf("expected nil files, got %v", files)
	}
}

func TestReadGeminiCLISession(t *testing.T) {
	home := t.TempDir()
	writeGeminiCLISessionFixture(t, home, "hash1", "sess-1", "2026-08-01T00:00:00Z", [][2]string{
		{"user", "I like dark mode."},
		{"gemini", "Noted, dark mode it is."},
	})

	files, err := listGeminiCLISessionFiles(home)
	if err != nil || len(files) != 1 {
		t.Fatalf("listGeminiCLISessionFiles: %v, %v", files, err)
	}

	sess, ok, err := readGeminiCLISession(files[0])
	if err != nil {
		t.Fatalf("readGeminiCLISession: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true")
	}
	if sess.ID != "gemini-cli:sess-1" {
		t.Errorf("ID = %q", sess.ID)
	}
	if sess.Scope != "project:gemini-hash1" {
		t.Errorf("scope = %q", sess.Scope)
	}
	if !contains(sess.Transcript, "I like dark mode.") || !contains(sess.Transcript, "Noted, dark mode it is.") {
		t.Errorf("transcript missing content: %q", sess.Transcript)
	}
}

func TestReadGeminiCLISession_UnparseableIsError(t *testing.T) {
	home := t.TempDir()
	chatsDir := filepath.Join(home, "tmp", "hash1", "chats")
	if err := os.MkdirAll(chatsDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(chatsDir, "session-bad.json")
	if err := os.WriteFile(path, []byte("not json"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, ok, err := readGeminiCLISession(path)
	if err == nil {
		t.Error("expected an error for unparseable session log")
	}
	if ok {
		t.Error("expected ok=false")
	}
}

func TestListGeminiAntigravityMarkdown(t *testing.T) {
	home := t.TempDir()
	convDir := filepath.Join(home, "antigravity-cli", "brain", "conv-1")
	if err := os.MkdirAll(convDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(convDir, "walkthrough.md"), []byte("# Walkthrough\n\nDid the thing."), 0644); err != nil {
		t.Fatalf("write md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(convDir, "screenshot.png"), []byte("not markdown"), 0644); err != nil {
		t.Fatalf("write png: %v", err)
	}

	files, err := listGeminiAntigravityMarkdown(home)
	if err != nil {
		t.Fatalf("listGeminiAntigravityMarkdown: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("expected 1 md file (png skipped), got %d: %v", len(files), files)
	}
	if files[0].ConvID != "conv-1" || files[0].RelPath != "walkthrough.md" {
		t.Errorf("unexpected file entry: %+v", files[0])
	}
}

func TestListGeminiAntigravityMarkdown_MissingBrainDir(t *testing.T) {
	home := t.TempDir()
	files, err := listGeminiAntigravityMarkdown(home)
	if err != nil {
		t.Fatalf("expected no error for missing brain dir, got %v", err)
	}
	if files != nil {
		t.Errorf("expected nil files, got %v", files)
	}
}

// seedAntigravityDB creates a minimal conversation_summaries.db with the
// one column this harvester reads (workspace_uris), matching the real
// schema verified on disk.
func seedAntigravityDB(t *testing.T, dbPath, convID, workspaceURI string) {
	t.Helper()
	con, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer con.Close()

	if _, err := con.Exec(`CREATE TABLE conversation_summaries (
		conversation_id TEXT PRIMARY KEY,
		workspace_uris TEXT
	)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	uris, _ := json.Marshal([]string{workspaceURI})
	if _, err := con.Exec(`INSERT INTO conversation_summaries (conversation_id, workspace_uris) VALUES (?, ?)`, convID, string(uris)); err != nil {
		t.Fatalf("insert row: %v", err)
	}
}

func TestLoadGeminiAntigravityScopes(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "antigravity-cli"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dbPath := filepath.Join(home, "antigravity-cli", "conversation_summaries.db")
	seedAntigravityDB(t, dbPath, "conv-1", "file:///Users/fakehome/dev/butter_stack")

	scopes := loadGeminiAntigravityScopes(dbPath)
	if scopes["conv-1"] != "project:-Users-fakehome-dev-butter_stack" {
		t.Errorf("scopes[conv-1] = %q", scopes["conv-1"])
	}
}

func TestLoadGeminiAntigravityScopes_MissingDB(t *testing.T) {
	scopes := loadGeminiAntigravityScopes(filepath.Join(t.TempDir(), "does-not-exist.db"))
	if len(scopes) != 0 {
		t.Errorf("expected empty scopes for missing db, got %v", scopes)
	}
}

func TestHarvestGeminiAntigravityItems_UnknownConversationFallsBackToGlobal(t *testing.T) {
	home := t.TempDir()
	convDir := filepath.Join(home, "antigravity-cli", "brain", "conv-unknown")
	if err := os.MkdirAll(convDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(convDir, "plan.md"), []byte("the plan"), 0644); err != nil {
		t.Fatalf("write md: %v", err)
	}
	// No conversation_summaries.db at all -- degrade gracefully.

	items, err := harvestGeminiAntigravityItems(home)
	if err != nil {
		t.Fatalf("harvestGeminiAntigravityItems: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].Scope != "global" {
		t.Errorf("scope = %q, want global", items[0].Scope)
	}
	if items[0].Type != MemoryEvent {
		t.Errorf("type = %q, want event", items[0].Type)
	}
	if items[0].Key != "gemini:antigravity:conv-unknown:plan" {
		t.Errorf("key = %q", items[0].Key)
	}
}

func TestHarvestGeminiMDItem(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "GEMINI.md"), []byte("# Gemini instructions\n\nAlways be terse."), 0644); err != nil {
		t.Fatalf("write GEMINI.md: %v", err)
	}

	item, err := harvestGeminiMDItem(home)
	if err != nil {
		t.Fatalf("harvestGeminiMDItem: %v", err)
	}
	if item == nil {
		t.Fatal("expected a non-nil item")
	}
	if item.Key != "gemini:global:GEMINI" || item.Type != MemoryInstruction || item.Scope != "global" {
		t.Errorf("unexpected item: %+v", item)
	}
}

func TestHarvestGeminiMDItem_Missing(t *testing.T) {
	home := t.TempDir()
	item, err := harvestGeminiMDItem(home)
	if err != nil {
		t.Fatalf("expected no error for missing GEMINI.md, got %v", err)
	}
	if item != nil {
		t.Errorf("expected nil item, got %+v", item)
	}
}

// TestHarvestGemini_EndToEnd exercises the full gemini orchestrator
// (harvestGemini) against a fixture tree covering all three
// sub-sources: a CLI session log (LLM-distilled), an Antigravity
// walkthrough.md (direct mirror), and GEMINI.md (direct mirror).
func TestHarvestGemini_EndToEnd(t *testing.T) {
	home := t.TempDir()
	writeGeminiCLISessionFixture(t, home, "hash1", "sess-1", "2026-08-01T00:00:00Z", [][2]string{
		{"user", "I like dark mode."},
	})

	convDir := filepath.Join(home, "antigravity-cli", "brain", "conv-1")
	if err := os.MkdirAll(convDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(convDir, "walkthrough.md"), []byte("We shipped the feature."), 0644); err != nil {
		t.Fatalf("write walkthrough: %v", err)
	}
	seedAntigravityDB(t, filepath.Join(home, "antigravity-cli", "conversation_summaries.db"), "conv-1", "file:///Users/fakehome/dev/pilot_light")

	if err := os.WriteFile(filepath.Join(home, "GEMINI.md"), []byte("Be terse."), 0644); err != nil {
		t.Fatalf("write GEMINI.md: %v", err)
	}

	b := newTestBrain(t)
	ctx := context.Background()
	distill := fakeDistillOne("dark-mode-pref", "The user prefers dark mode.")

	result, err := b.harvestGemini(ctx, home, distill, HarvestOptions{Now: mustParseTime(t, "2026-08-17T00:00:00Z")})
	if err != nil {
		t.Fatalf("harvestGemini: %v", err)
	}
	if result.CandidatesTotal != 1 {
		t.Errorf("CandidatesTotal = %d, want 1", result.CandidatesTotal)
	}
	if len(result.Sessions) != 1 || len(result.Sessions[0].Written) != 1 {
		t.Fatalf("expected 1 session with 1 memory, got %+v", result.Sessions)
	}
	if len(result.DirectMirrored) != 2 {
		t.Fatalf("expected 2 direct-mirrored records (walkthrough + GEMINI.md), got %d: %+v", len(result.DirectMirrored), result.DirectMirrored)
	}

	// Second run: everything already harvested, nothing new.
	result2, err := b.harvestGemini(ctx, home, distill, HarvestOptions{Now: mustParseTime(t, "2026-08-17T00:00:00Z")})
	if err != nil {
		t.Fatalf("harvestGemini (2nd): %v", err)
	}
	if result2.Selected != 0 {
		t.Errorf("second run Selected = %d, want 0", result2.Selected)
	}
	if len(result2.DirectMirrored) != 0 {
		t.Errorf("second run DirectMirrored = %d, want 0", len(result2.DirectMirrored))
	}
}
