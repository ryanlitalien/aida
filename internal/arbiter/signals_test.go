package arbiter

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestClassifySignal(t *testing.T) {
	tests := []struct {
		name        string
		exitCode    int
		text        string
		wantVerdict Verdict
		wantMatched string
	}{
		{"usage limit reached", 1, "Error: usage limit reached for this session", VerdictEmpty, "usage limit reached"},
		{"you've hit your limit", 1, "You've hit your limit for the 5-hour window", VerdictEmpty, "you've hit your limit"},
		{"you have hit your limit", 1, "You have hit your limit.", VerdictEmpty, "you have hit your limit"},
		{"rate limit phrase", 1, "429 Too Many Requests: rate limit exceeded", VerdictEmpty, "rate limit"},
		{"rate_limit_error code", 1, `{"type":"rate_limit_error"}`, VerdictEmpty, "rate_limit_error"},
		{"bare 429", 0, "HTTP 429", VerdictEmpty, "429"},
		{"quota exceeded", 1, "Quota exceeded for today", VerdictEmpty, "quota exceeded"},
		{"resets at phrase", 0, "Your limit resets at 3pm ET", VerdictEmpty, "resets at"},
		{"limit will reset", 1, "The limit will reset in 2 hours", VerdictEmpty, "limit will reset"},
		{"out of extra usage", 1, "You are out of extra usage this week", VerdictEmpty, "out of extra usage"},
		{"insufficient_quota", 1, "insufficient_quota: please add a payment method", VerdictEmpty, "insufficient_quota"},
		{"overloaded_error", 1, "overloaded_error: the model is overloaded", VerdictEmpty, "overloaded_error"},
		{"case insensitive", 1, "USAGE LIMIT REACHED", VerdictEmpty, "usage limit reached"},
		{"clean success", 0, "Here is the diff you asked for.", VerdictNotEmpty, ""},
		{"nonzero exit no phrase", 1, "panic: something unrelated broke", VerdictAmbiguous, ""},
		{"empty text zero exit", 0, "", VerdictAmbiguous, ""},
		{"empty text nonzero exit", 1, "", VerdictAmbiguous, ""},
		{"weak limit word alone, zero exit", 0, "there is a limit somewhere", VerdictAmbiguous, ""},
		{"weak limit word alone, nonzero exit", 1, "hit a limit maybe", VerdictAmbiguous, ""},
		{"limit as part of another word does not weak-match", 0, "the delimiter was wrong", VerdictNotEmpty, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotVerdict, gotMatched := ClassifySignal(tt.exitCode, tt.text)
			if gotVerdict != tt.wantVerdict {
				t.Errorf("ClassifySignal(%d, %q) verdict = %q, want %q", tt.exitCode, tt.text, gotVerdict, tt.wantVerdict)
			}
			if gotMatched != tt.wantMatched {
				t.Errorf("ClassifySignal(%d, %q) matched = %q, want %q", tt.exitCode, tt.text, gotMatched, tt.wantMatched)
			}
		})
	}
}

func TestResolveEmptyUntil(t *testing.T) {
	now := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)

	t.Run("known future reset wins", func(t *testing.T) {
		resetsAt := now.Add(3 * time.Hour)
		got := ResolveEmptyUntil(now, resetsAt, time.Hour)
		if !got.Equal(resetsAt) {
			t.Errorf("got %v, want %v", got, resetsAt)
		}
	})

	t.Run("past reset falls back", func(t *testing.T) {
		resetsAt := now.Add(-time.Hour)
		got := ResolveEmptyUntil(now, resetsAt, 30*time.Minute)
		want := now.Add(30 * time.Minute)
		if !got.Equal(want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("zero reset falls back to default", func(t *testing.T) {
		got := ResolveEmptyUntil(now, time.Time{}, 0)
		want := now.Add(DefaultEmptyFallback)
		if !got.Equal(want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})
}

func TestStoreRecordAndEmptyUntil(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	now := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	empty := Signal{
		At:      now,
		Lane:    "claude-max",
		Source:  "stop-hook",
		Verdict: VerdictEmpty,
		Matched: "usage limit reached",
		Raw:     "usage limit reached",
	}
	if err := s.Record(empty); err != nil {
		t.Fatalf("Record: %v", err)
	}

	until, ok := s.EmptyUntil("claude-max", now.Add(time.Minute))
	if !ok {
		t.Fatal("expected claude-max to be marked empty")
	}
	wantUntil := now.Add(DefaultEmptyFallback)
	if !until.Equal(wantUntil) {
		t.Errorf("EmptyUntil = %v, want %v", until, wantUntil)
	}

	if _, ok := s.EmptyUntil("claude-max", until.Add(time.Second)); ok {
		t.Error("expected the mark to have expired")
	}

	if _, ok := s.EmptyUntil("some-other-lane", now); ok {
		t.Error("expected an unrelated lane to have no mark")
	}
}

func TestStoreAmbiguousMarksEmptyAndLogsAmbiguous(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	now := time.Now()
	sig := Signal{At: now, Lane: "codex-plus", Source: "wrapper", Verdict: VerdictAmbiguous, Raw: "panic: unrelated"}
	if err := s.Record(sig); err != nil {
		t.Fatalf("Record: %v", err)
	}

	if _, ok := s.EmptyUntil("codex-plus", now.Add(time.Second)); !ok {
		t.Error("expected an ambiguous signal to fail open and mark the lane empty")
	}

	sigs, err := s.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(sigs) != 1 {
		t.Fatalf("expected 1 signal, got %d", len(sigs))
	}
	if !sigs[0].Ambiguous {
		t.Error("expected the logged line to carry ambiguous=true")
	}
	if sigs[0].Verdict != VerdictAmbiguous {
		t.Errorf("Verdict = %q, want %q", sigs[0].Verdict, VerdictAmbiguous)
	}
}

func TestStoreNotEmptyDoesNotMarkLane(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	if err := s.Record(Signal{Lane: "claude-max", Verdict: VerdictNotEmpty}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if _, ok := s.EmptyUntil("claude-max", time.Now()); ok {
		t.Error("expected a NotEmpty signal to leave the lane unmarked")
	}
}

func TestStoreClear(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	now := time.Now()
	if err := s.Record(Signal{At: now, Lane: "claude-max", Verdict: VerdictEmpty}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if _, ok := s.EmptyUntil("claude-max", now); !ok {
		t.Fatal("expected the lane to be marked empty before Clear")
	}
	if err := s.Clear("claude-max"); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if _, ok := s.EmptyUntil("claude-max", now); ok {
		t.Error("expected Clear to remove the mark")
	}
}

func TestStoreRawTruncation(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	longRaw := make([]byte, 1000)
	for i := range longRaw {
		longRaw[i] = 'x'
	}
	if err := s.Record(Signal{Lane: "claude-max", Verdict: VerdictNotEmpty, Raw: string(longRaw)}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	sigs, err := s.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(sigs) != 1 {
		t.Fatalf("expected 1 signal, got %d", len(sigs))
	}
	if len(sigs[0].Raw) != maxRawLen {
		t.Errorf("Raw length = %d, want %d", len(sigs[0].Raw), maxRawLen)
	}
}

func TestFalsePositiveSummary(t *testing.T) {
	sigs := []Signal{
		{Verdict: VerdictEmpty, Ambiguous: false},
		{Verdict: VerdictAmbiguous, Ambiguous: true},
		{Verdict: VerdictNotEmpty, Ambiguous: false},
		{Verdict: VerdictAmbiguous, Ambiguous: true},
	}
	total, ambiguous := FalsePositiveSummary(sigs)
	if total != 4 {
		t.Errorf("total = %d, want 4", total)
	}
	if ambiguous != 2 {
		t.Errorf("ambiguous = %d, want 2", ambiguous)
	}
}

func TestStoreReadAllTolerateCorruptLine(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	if err := s.Record(Signal{Lane: "claude-max", Verdict: VerdictNotEmpty}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	// Append a corrupt line directly.
	path := filepath.Join(dir, "signals.ndjson")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open for append: %v", err)
	}
	if _, err := f.WriteString("not json at all\n"); err != nil {
		t.Fatalf("write corrupt line: %v", err)
	}
	f.Close()

	if err := s.Record(Signal{Lane: "codex-plus", Verdict: VerdictEmpty}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	sigs, err := s.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(sigs) != 2 {
		t.Fatalf("expected 2 valid signals despite the corrupt line, got %d", len(sigs))
	}
}
