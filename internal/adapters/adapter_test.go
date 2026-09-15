package adapters

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestFileAdapter_ReadsPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notes.md")
	want := "## Discussion\n\n- ship Symphony\n- review eval signals\n"
	if err := os.WriteFile(path, []byte(want), 0644); err != nil {
		t.Fatal(err)
	}
	a := NewFile(path)
	got, err := a.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got != want {
		t.Errorf("Read returned %q, want %q", got, want)
	}
}

func TestFileAdapter_NameIncludesPath(t *testing.T) {
	a := NewFile("/some/path.md")
	if got := a.Name(); got != "file:/some/path.md" {
		t.Errorf("Name() = %q, want file:/some/path.md", got)
	}
	if got := (FileAdapter{}).Name(); got != "file:(unset)" {
		t.Errorf("empty path Name() = %q, want file:(unset)", got)
	}
}

func TestFileAdapter_RejectsEmptyPath(t *testing.T) {
	if _, err := (FileAdapter{}).Read(context.Background()); err == nil {
		t.Errorf("empty path should error")
	}
}

func TestFileAdapter_RejectsCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewFile("/tmp/whatever").Read(ctx); err == nil {
		t.Errorf("cancelled context should error before read")
	}
}

func TestFileAdapter_PropagatesReadError(t *testing.T) {
	if _, err := NewFile("/nonexistent/path/here").Read(context.Background()); err == nil {
		t.Errorf("missing file should error")
	}
}

func TestStdinAdapter_NameIsStable(t *testing.T) {
	if (StdinAdapter{}).Name() != "stdin" {
		t.Errorf("Name() = %q, want stdin", (StdinAdapter{}).Name())
	}
}

func TestStdinAdapter_RespectsCancel(t *testing.T) {
	// Real stdin is a TTY in tests; substitute with a pipe so
	// the goroutine read doesn't block forever. We cancel ctx
	// before the read completes and expect ctx.Err() back.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	defer r.Close()

	origStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = origStdin }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err = NewStdin().Read(ctx)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}
