package arbiter

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTranscript(t *testing.T, dir string, lines []string) string {
	t.Helper()
	path := filepath.Join(dir, "transcript.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("writing fixture transcript: %v", err)
	}
	return path
}

func TestParseClaudeStopHookStringContent(t *testing.T) {
	dir := t.TempDir()
	transcript := writeTranscript(t, dir, []string{
		`{"type":"user","message":{"content":"do the thing"}}`,
		`{"type":"assistant","message":{"content":"working on it"}}`,
		`{"type":"assistant","message":{"content":"usage limit reached for this session"}}`,
	})
	payload := `{"session_id":"s1","transcript_path":"` + transcript + `","stop_hook_active":false}`

	gotPath, gotText, err := ParseClaudeStopHook(strings.NewReader(payload))
	if err != nil {
		t.Fatalf("ParseClaudeStopHook: %v", err)
	}
	if gotPath != transcript {
		t.Errorf("transcriptPath = %q, want %q", gotPath, transcript)
	}
	want := "usage limit reached for this session"
	if gotText != want {
		t.Errorf("lastAssistantText = %q, want %q", gotText, want)
	}
}

func TestParseClaudeStopHookBlockContent(t *testing.T) {
	dir := t.TempDir()
	transcript := writeTranscript(t, dir, []string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read"},{"type":"text","text":"first part"},{"type":"text","text":"second part"}]}}`,
	})
	payload := `{"session_id":"s1","transcript_path":"` + transcript + `"}`

	_, gotText, err := ParseClaudeStopHook(strings.NewReader(payload))
	if err != nil {
		t.Fatalf("ParseClaudeStopHook: %v", err)
	}
	want := "first part\nsecond part"
	if gotText != want {
		t.Errorf("lastAssistantText = %q, want %q", gotText, want)
	}
}

func TestParseClaudeStopHookIgnoresNonAssistantAndMalformedLines(t *testing.T) {
	dir := t.TempDir()
	transcript := writeTranscript(t, dir, []string{
		`{"type":"assistant","message":{"content":"first"}}`,
		`not valid json at all`,
		`{"type":"user","message":{"content":"ignored, not assistant"}}`,
		`{"type":"summary","summary":"session recap"}`,
		``,
	})
	payload := `{"transcript_path":"` + transcript + `"}`

	_, gotText, err := ParseClaudeStopHook(strings.NewReader(payload))
	if err != nil {
		t.Fatalf("ParseClaudeStopHook: %v", err)
	}
	if gotText != "first" {
		t.Errorf("lastAssistantText = %q, want %q", gotText, "first")
	}
}

func TestParseClaudeStopHookNoTranscriptPath(t *testing.T) {
	_, _, err := ParseClaudeStopHook(strings.NewReader(`{"session_id":"s1"}`))
	if err == nil {
		t.Fatal("expected an error when transcript_path is missing")
	}
}

func TestParseClaudeStopHookInvalidPayload(t *testing.T) {
	_, _, err := ParseClaudeStopHook(strings.NewReader(`not json`))
	if err == nil {
		t.Fatal("expected an error for invalid payload JSON")
	}
}

func TestParseClaudeStopHookMissingTranscriptFile(t *testing.T) {
	payload := `{"transcript_path":"/nonexistent/transcript.jsonl"}`
	_, _, err := ParseClaudeStopHook(strings.NewReader(payload))
	if err == nil {
		t.Fatal("expected an error when the transcript file doesn't exist")
	}
}

func TestParseClaudeStopHookNoAssistantMessages(t *testing.T) {
	dir := t.TempDir()
	transcript := writeTranscript(t, dir, []string{
		`{"type":"user","message":{"content":"hello"}}`,
	})
	payload := `{"transcript_path":"` + transcript + `"}`

	_, gotText, err := ParseClaudeStopHook(strings.NewReader(payload))
	if err != nil {
		t.Fatalf("ParseClaudeStopHook: %v", err)
	}
	if gotText != "" {
		t.Errorf("lastAssistantText = %q, want empty", gotText)
	}
}
