package adapters

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestNotionAdapter_NameIncludesRef(t *testing.T) {
	a := NewNotion("https://www.notion.so/abc123")
	if got := a.Name(); got != "notion:https://www.notion.so/abc123" {
		t.Errorf("Name() = %q", got)
	}
	if got := (NotionAdapter{}).Name(); got != "notion:(unset)" {
		t.Errorf("empty PageRef Name() = %q, want notion:(unset)", got)
	}
}

func TestNotionAdapter_RejectsEmptyRef(t *testing.T) {
	if _, err := (NotionAdapter{}).Read(context.Background()); err == nil {
		t.Errorf("empty PageRef should error")
	}
}

func TestNotionAdapter_FailsWithoutClaude(t *testing.T) {
	// Point ClaudeBin at a name that definitely isn't on PATH.
	// The adapter should fail with a "binary not found" error
	// before attempting any subprocess work.
	a := &NotionAdapter{
		PageRef:   "anything",
		ClaudeBin: "claude-binary-that-does-not-exist-anywhere",
	}
	_, err := a.Read(context.Background())
	if err == nil {
		t.Fatalf("expected error for missing binary")
	}
}

func TestNotionAdapter_HappyPathWithFakeBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake binary; skipping on windows")
	}
	// Drop a tiny fake claude binary that ignores its args and
	// echoes a fixed page body. Lets us exercise the full
	// adapter path without hitting a real Notion API.
	dir := t.TempDir()
	fake := filepath.Join(dir, "claude")
	const body = "# Page Title\n\n- item one\n- item two\n"
	script := "#!/bin/sh\nprintf %s '" + body + "'\n"
	if err := os.WriteFile(fake, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	a := &NotionAdapter{PageRef: "test-page", ClaudeBin: fake}
	got, err := a.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got == "" {
		t.Errorf("Read returned empty body")
	}
}

func TestNotionAdapter_EmptyResponseIsError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake binary; skipping on windows")
	}
	dir := t.TempDir()
	fake := filepath.Join(dir, "claude")
	// Fake binary returns the literal "EMPTY" sentinel claude
	// is told to use when no content was found.
	script := "#!/bin/sh\nprintf 'EMPTY'\n"
	if err := os.WriteFile(fake, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	a := &NotionAdapter{PageRef: "missing-page", ClaudeBin: fake}
	if _, err := a.Read(context.Background()); err == nil {
		t.Errorf("expected error on EMPTY response")
	}
}
