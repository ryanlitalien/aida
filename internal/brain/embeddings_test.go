package brain

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestEmbeddingClient_NoKey_NotAvailable(t *testing.T) {
	t.Setenv("AIDA_TEST_VOYAGE_MISSING", "")
	c := NewEmbeddingClient("AIDA_TEST_VOYAGE_MISSING")
	if c.Available() {
		t.Fatal("Available() should be false when the referenced env var is empty")
	}
}

func TestEmbeddingClient_WithKey_Available(t *testing.T) {
	t.Setenv("AIDA_TEST_VOYAGE_SET", "fake-key-value")
	c := NewEmbeddingClient("AIDA_TEST_VOYAGE_SET")
	if !c.Available() {
		t.Fatal("Available() should be true when the env var is populated")
	}
}

func TestEmbeddingClient_EmptyEnvName_NotAvailable(t *testing.T) {
	// When the caller passes "" (no key env configured at all), the
	// client must still be safe to construct -- just inert.
	c := NewEmbeddingClient("")
	if c.Available() {
		t.Fatal("Available() should be false for an empty env var name")
	}
}

// TestWarnIfEmbeddingsMisconfigured_MissingKey verifies the startup
// warning fires once when the user declares a Voyage key env var but
// it's unset. The silent degrade this guards against was the bug that
// left 363 lessons unembedded without any visible error.
func TestWarnIfEmbeddingsMisconfigured_MissingKey(t *testing.T) {
	// Reset the sync.Once so this test can exercise the warn path.
	// Safe in -count=1 mode because tests share the package binary only
	// within a single run, not across runs.
	startupWarnOnce = sync.Once{}

	t.Setenv("AIDA_TEST_MISSING_KEY", "")

	stderr := captureStderr(t, func() {
		// DB is nil -- the missing-key branch runs before the DB check.
		warnIfEmbeddingsMisconfigured("AIDA_TEST_MISSING_KEY", &Brain{})
	})

	if !strings.Contains(stderr, "AIDA_TEST_MISSING_KEY is unset") {
		t.Errorf("expected warning about missing key env var, got: %q", stderr)
	}
	if !strings.Contains(stderr, "semantic search disabled") {
		t.Errorf("expected user-facing explanation in warning, got: %q", stderr)
	}
}

func TestWarnIfEmbeddingsMisconfigured_NoEnvConfigured(t *testing.T) {
	startupWarnOnce = sync.Once{}
	stderr := captureStderr(t, func() {
		warnIfEmbeddingsMisconfigured("", &Brain{})
	})
	if stderr != "" {
		t.Errorf("no warning expected when no key env is configured, got: %q", stderr)
	}
}

func TestWarnIfEmbeddingsMisconfigured_KeySetNoDB(t *testing.T) {
	startupWarnOnce = sync.Once{}
	t.Setenv("AIDA_TEST_PRESENT_KEY", "xx")
	stderr := captureStderr(t, func() {
		warnIfEmbeddingsMisconfigured("AIDA_TEST_PRESENT_KEY", &Brain{})
	})
	if stderr != "" {
		t.Errorf("no warning expected when key is set and DB nil, got: %q", stderr)
	}
}

// TestWarnIfEmbeddingsMisconfigured_MissingEmbeddings verifies the second
// branch: key is set but brain.db has lessons without embeddings (needs
// backfill). Uses a real in-memory-ish DB under t.TempDir().
func TestWarnIfEmbeddingsMisconfigured_MissingEmbeddings(t *testing.T) {
	startupWarnOnce = sync.Once{}
	t.Setenv("AIDA_TEST_BACKFILL_KEY", "present")

	dir := t.TempDir()
	db, err := OpenDB(dir)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	// Insert 20 lessons with no embedding to exceed the >10 total and
	// >10% missing thresholds.
	for i := 0; i < 20; i++ {
		l := &LessonRecord{
			ID:        filepath.Join("lesson", string(rune('a'+i))),
			Timestamp: "2026-04-17T00:00:00Z",
			Profile:   "work",
			Question:  "q",
			Action:    "task",
			Strategy:  "list",
		}
		if err := db.InsertLesson(l); err != nil {
			t.Fatalf("InsertLesson: %v", err)
		}
	}

	b := &Brain{DB: db, profile: "work"}
	stderr := captureStderr(t, func() {
		warnIfEmbeddingsMisconfigured("AIDA_TEST_BACKFILL_KEY", b)
	})
	if !strings.Contains(stderr, "missing embeddings") {
		t.Errorf("expected backfill warning, got: %q", stderr)
	}
	if !strings.Contains(stderr, "aida brain reembed") {
		t.Errorf("expected warning to point at `aida brain reembed`, got: %q", stderr)
	}
}

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w

	done := make(chan struct{})
	var buf bytes.Buffer
	go func() {
		io.Copy(&buf, r)
		close(done)
	}()

	fn()

	w.Close()
	<-done
	os.Stderr = orig
	return buf.String()
}
