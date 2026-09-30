package taskstate

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestParseHandoff_RoundTripsTemplate(t *testing.T) {
	acc := Acceptance{
		Checks:       []string{"make test"},
		Deliverables: []string{"internal/taskstate/handoff.go"},
	}
	fixedNow := time.Date(2026, 9, 24, 3, 10, 0, 0, time.UTC)
	restore := stubNow(fixedNow)
	defer restore()

	tmpl := Template(123, "the-slug", "claude-max", 2, "Fix the thing", "Some task body.", acc)

	// Simulate the model filling in the blank sections before handing
	// off (the harness pre-fills Task and Acceptance; the model fills
	// the rest).
	filled := strings.Replace(tmpl, "## Done\n\n\n## Left", "## Done\n\nWrote the parser.\n\n## Left", 1)
	filled = strings.Replace(filled, "## Left\n\n\n## Tried and rejected", "## Left\n\nWire up Check.\n\n## Tried and rejected", 1)
	filled = strings.Replace(filled, "## Tried and rejected\n\n\n## Acceptance", "## Tried and rejected\n\nRegex splitting on commas - too fragile.\n\n## Acceptance", 1)

	h, err := ParseHandoff(filled)
	if err != nil {
		t.Fatalf("ParseHandoff(filled template): %v", err)
	}
	if h.TaskID != 123 {
		t.Errorf("TaskID = %d, want 123", h.TaskID)
	}
	if h.Slug != "the-slug" {
		t.Errorf("Slug = %q, want %q", h.Slug, "the-slug")
	}
	if h.Lane != "claude-max" {
		t.Errorf("Lane = %q, want %q", h.Lane, "claude-max")
	}
	if h.Attempt != 2 {
		t.Errorf("Attempt = %d, want 2", h.Attempt)
	}
	if !h.Written.Equal(fixedNow) {
		t.Errorf("Written = %v, want %v", h.Written, fixedNow)
	}
	if h.WorktreeHead != "" {
		t.Errorf("WorktreeHead = %q, want empty ((optional) placeholder unfilled)", h.WorktreeHead)
	}
	if !strings.Contains(h.Sections["Task"], "Fix the thing") {
		t.Errorf("Task section missing title: %q", h.Sections["Task"])
	}
	if !strings.Contains(h.Sections["Done"], "Wrote the parser") {
		t.Errorf("Done section missing content: %q", h.Sections["Done"])
	}
	if !strings.Contains(h.Sections["Left"], "Wire up Check") {
		t.Errorf("Left section missing content: %q", h.Sections["Left"])
	}
	if !strings.Contains(h.Sections["Acceptance"], "make test") {
		t.Errorf("Acceptance section missing check: %q", h.Sections["Acceptance"])
	}
}

func TestParseHandoff_UnfilledTemplateIsMalformed(t *testing.T) {
	acc := Acceptance{Checks: []string{"make test"}}
	tmpl := Template(1, "slug", "claude-max", 1, "Title", "", acc)
	_, err := ParseHandoff(tmpl)
	if err == nil {
		t.Fatal("expected an error parsing an unfilled template (Done/Left empty)")
	}
	if !errors.Is(err, ErrMalformed) {
		t.Errorf("expected ErrMalformed, got %v", err)
	}
}

func validHandoffText(overrides map[string]string) string {
	fields := map[string]string{
		"task":          "42",
		"slug":          "the-slug",
		"lane":          "claude-max",
		"attempt":       "1",
		"written":       "2026-09-24T03:00:00Z",
		"worktree_head": "abc123",
	}
	for k, v := range overrides {
		fields[k] = v
	}
	var b strings.Builder
	b.WriteString("---\n")
	for _, k := range []string{"task", "slug", "lane", "attempt", "written", "worktree_head"} {
		if v, ok := fields[k]; ok && v != "" {
			b.WriteString(k + ": " + v + "\n")
		}
	}
	b.WriteString("---\n\n")
	b.WriteString("## Task\n\nDo the thing.\n\n")
	b.WriteString("## Done\n\nPart of it.\n\n")
	b.WriteString("## Left\n\nThe rest.\n\n")
	b.WriteString("## Tried and rejected\n\nNothing yet.\n\n")
	b.WriteString("## Acceptance\n\nmake test\n")
	return b.String()
}

func TestParseHandoff_MalformedHeader(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{"no header at all", "## Task\n\nhi\n"},
		{"unterminated header", "---\ntask: 1\nslug: x\n"},
		{"bad header line", "---\ntask 1\n---\n\n## Task\n\nx\n"},
		{"bad task id", "---\ntask: not-a-number\nslug: x\nwritten: 2026-09-24T03:00:00Z\n---\n\n## Task\n\nx\n## Done\nd\n## Left\nl\n## Acceptance\na\n"},
		{"bad written timestamp", "---\ntask: 1\nslug: x\nwritten: not-a-time\n---\n\n## Task\nx\n## Done\nd\n## Left\nl\n## Acceptance\na\n"},
		{"missing slug", "---\ntask: 1\nwritten: 2026-09-24T03:00:00Z\n---\n\n## Task\nx\n## Done\nd\n## Left\nl\n## Acceptance\na\n"},
		{"missing written", "---\ntask: 1\nslug: x\n---\n\n## Task\nx\n## Done\nd\n## Left\nl\n## Acceptance\na\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseHandoff(tc.text)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !errors.Is(err, ErrMalformed) {
				t.Errorf("expected ErrMalformed, got %v", err)
			}
		})
	}
}

func TestParseHandoff_EmptyRequiredSections(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{"missing Done section entirely", "---\ntask: 1\nslug: x\nwritten: 2026-09-24T03:00:00Z\n---\n\n## Task\nx\n## Left\nl\n## Acceptance\na\n"},
		{"Done section present but empty", "---\ntask: 1\nslug: x\nwritten: 2026-09-24T03:00:00Z\n---\n\n## Task\nx\n## Done\n\n## Left\nl\n## Acceptance\na\n"},
		{"Acceptance section whitespace only", "---\ntask: 1\nslug: x\nwritten: 2026-09-24T03:00:00Z\n---\n\n## Task\nx\n## Done\nd\n## Left\nl\n## Acceptance\n   \n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseHandoff(tc.text)
			if !errors.Is(err, ErrMalformed) {
				t.Errorf("expected ErrMalformed, got %v", err)
			}
		})
	}
}

func TestLoadHandoff_Missing(t *testing.T) {
	dir := t.TempDir()
	_, err := LoadHandoff(dir, "no-such-task")
	if !errors.Is(err, ErrMissing) {
		t.Errorf("expected ErrMissing, got %v", err)
	}
}

func TestLoadHandoff_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	text := validHandoffText(nil)
	if err := os.MkdirAll(Dir(dir, "the-slug"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(HandoffPath(dir, "the-slug"), []byte(text), 0644); err != nil {
		t.Fatal(err)
	}
	h, err := LoadHandoff(dir, "the-slug")
	if err != nil {
		t.Fatalf("LoadHandoff: %v", err)
	}
	if h.TaskID != 42 || h.Slug != "the-slug" {
		t.Errorf("loaded handoff mismatch: %+v", h)
	}
}

func TestCheck(t *testing.T) {
	baseWritten := time.Date(2026, 9, 24, 3, 0, 0, 0, time.UTC)
	freshHandoff := func() *Handoff {
		return &Handoff{
			TaskID:       42,
			Slug:         "the-slug",
			Lane:         "claude-max",
			Attempt:      2,
			Written:      baseWritten,
			WorktreeHead: "abc123",
			Sections:     map[string]string{"Task": "t", "Done": "d", "Left": "l", "Acceptance": "a"},
		}
	}
	baseState := &State{Slug: "the-slug", TaskID: 42, Attempt: 2}

	tests := []struct {
		name        string
		h           *Handoff
		s           *State
		currentHead string
		now         time.Time
		maxAge      time.Duration
		wantErr     error // sentinel to check with errors.Is; nil means expect nil
		wantStale   bool
	}{
		{
			name:        "missing handoff",
			h:           nil,
			s:           baseState,
			currentHead: "abc123",
			now:         baseWritten,
			wantErr:     ErrMissing,
		},
		{
			name:        "attempt older than state is stale",
			h:           freshHandoff(),
			s:           &State{Slug: "the-slug", TaskID: 42, Attempt: 3},
			currentHead: "abc123",
			now:         baseWritten,
			wantErr:     ErrStaleAttempt,
			wantStale:   true,
		},
		{
			name:        "head mismatch is stale",
			h:           freshHandoff(),
			s:           baseState,
			currentHead: "def456",
			now:         baseWritten,
			wantErr:     ErrStaleHead,
			wantStale:   true,
		},
		{
			name:        "too old is stale",
			h:           freshHandoff(),
			s:           baseState,
			currentHead: "abc123",
			now:         baseWritten.Add(2 * time.Hour),
			maxAge:      1 * time.Hour,
			wantErr:     ErrStaleAge,
			wantStale:   true,
		},
		{
			name:        "wrong slug",
			h:           freshHandoff(),
			s:           &State{Slug: "other-slug", TaskID: 42, Attempt: 2},
			currentHead: "abc123",
			now:         baseWritten,
			wantErr:     ErrWrongTask,
		},
		{
			name:        "wrong task id",
			h:           freshHandoff(),
			s:           &State{Slug: "the-slug", TaskID: 99, Attempt: 2},
			currentHead: "abc123",
			now:         baseWritten,
			wantErr:     ErrWrongTask,
		},
		{
			name:        "fresh handoff passes",
			h:           freshHandoff(),
			s:           baseState,
			currentHead: "abc123",
			now:         baseWritten,
			maxAge:      1 * time.Hour,
			wantErr:     nil,
		},
		{
			name:        "fresh handoff passes with maxAge disabled",
			h:           freshHandoff(),
			s:           baseState,
			currentHead: "abc123",
			now:         baseWritten.Add(999 * time.Hour),
			maxAge:      0,
			wantErr:     nil,
		},
		{
			name:        "empty current head does not trigger stale head",
			h:           freshHandoff(),
			s:           baseState,
			currentHead: "",
			now:         baseWritten,
			wantErr:     nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := Check(tc.h, tc.s, tc.currentHead, tc.now, tc.maxAge)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("Check() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Check() = %v, want errors.Is(_, %v)", err, tc.wantErr)
			}
			if tc.wantStale && !IsStale(err) {
				t.Errorf("IsStale(%v) = false, want true", err)
			}
			if !tc.wantStale && IsStale(err) {
				t.Errorf("IsStale(%v) = true, want false", err)
			}
		})
	}
}

func TestResumePrompt(t *testing.T) {
	h := &Handoff{
		TaskID: 42,
		Slug:   "the-slug",
		Lane:   "claude-max",
		Sections: map[string]string{
			"Task":               "Fix the thing",
			"Done":               "Wrote half of it",
			"Left":               "Finish the other half",
			"Tried and rejected": "A regex approach - too fragile",
			"Acceptance":         "make test",
		},
	}
	s := &State{Slug: "the-slug", TaskID: 42}

	prompt := ResumePrompt(h, s)

	if !strings.Contains(prompt, "do not restart") {
		t.Errorf("prompt missing 'do not restart' instruction: %q", prompt)
	}
	for _, section := range []string{"Fix the thing", "Wrote half of it", "Finish the other half", "A regex approach", "make test"} {
		if !strings.Contains(prompt, section) {
			t.Errorf("prompt missing section content %q: %q", section, prompt)
		}
	}
	if !strings.Contains(prompt, "#42") {
		t.Errorf("prompt missing task id: %q", prompt)
	}
	if !strings.Contains(prompt, "claude-max") {
		t.Errorf("prompt missing lane: %q", prompt)
	}
	if !strings.Contains(prompt, "HANDOFF.md") {
		t.Errorf("prompt missing rewrite instruction: %q", prompt)
	}
}

func TestReconstructPrompt(t *testing.T) {
	prompt := ReconstructPrompt(42, "the-slug", "/brain/arbiter/the-slug/HANDOFF.md", "handoff is 3h old (max 1h)")
	for _, want := range []string{"#42", "the-slug", "git log", "git diff", "STATE.json", "handoff is 3h old (max 1h)", "/brain/arbiter/the-slug/HANDOFF.md"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("ReconstructPrompt missing %q: %q", want, prompt)
		}
	}
}

func TestTemplateWithoutAcceptanceStillParses(t *testing.T) {
	text := Template(7, "seven", "max", 1, "Title", "body", Acceptance{})
	// Only the model's own sections are filled; Acceptance is left exactly
	// as the harness rendered it, which is the case under test.
	text = strings.Replace(text, "## Done\n", "## Done\n- did it\n", 1)
	text = strings.Replace(text, "## Left\n", "## Left\n- nothing\n", 1)
	h, err := ParseHandoff(text)
	if err != nil {
		t.Fatalf("ParseHandoff on a no-acceptance template: %v", err)
	}
	if !strings.Contains(h.Sections["Acceptance"], NoMachineAcceptance) {
		t.Errorf("Acceptance section = %q, want the NoMachineAcceptance line", h.Sections["Acceptance"])
	}
}
