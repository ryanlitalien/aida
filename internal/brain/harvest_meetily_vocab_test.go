package brain

import "testing"

func TestIsMechanicalTaskTag(t *testing.T) {
	mechanical := []string{
		"profile:work", "due:2026-08-25", "source-hash:abc123", "tool:aida",
		"owner:ryan", "from-slack", "today", "tomorrow", "jarvis-error", "",
	}
	for _, tag := range mechanical {
		if !isMechanicalTaskTag(tag) {
			t.Errorf("isMechanicalTaskTag(%q) = false, want true", tag)
		}
	}

	topical := []string{
		"project:-Users-ryan-dev-aida", "butterstack", "its", "cta", "hoa",
		"camp-butz", "first-chair", "plt", "personal",
	}
	for _, tag := range topical {
		if isMechanicalTaskTag(tag) {
			t.Errorf("isMechanicalTaskTag(%q) = true, want false", tag)
		}
	}
}

func TestComputeMeetilyTagVocabulary(t *testing.T) {
	taskTags := []string{
		"project:-Users-ryan-dev-butter_stack", "butterstack", "cta",
		"profile:work", "due:2026-09-01", "source-hash:deadbeef", "today",
		"tomorrow", "from-jarvis", "jarvis-error", "tool:aida", "owner:ryan",
		// A duplicate across two tasks must not appear twice.
		"cta",
	}
	entitySlugs := []string{"kevin", "butterstack"} // "butterstack" overlaps a task tag
	libraryEntities := []string{"camp-butz", "thrive"}

	got := computeMeetilyTagVocabulary(taskTags, entitySlugs, libraryEntities)

	want := map[string]bool{
		"project:-Users-ryan-dev-butter_stack": true,
		"butterstack":                          true,
		"cta":                                  true,
		"kevin":                                true,
		"camp-butz":                            true,
		"thrive":                               true,
	}
	if len(got) != len(want) {
		t.Fatalf("computeMeetilyTagVocabulary = %v, want exactly %v", got, want)
	}
	for _, v := range got {
		if !want[v] {
			t.Errorf("unexpected vocabulary entry %q (mechanical tag leaked through?)", v)
		}
	}

	// Sorted and deduplicated.
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Errorf("vocabulary not sorted/deduplicated: %v", got)
			break
		}
	}
}

func TestComputeMeetilyTagVocabulary_EmptyInputsYieldEmptyVocabulary(t *testing.T) {
	got := computeMeetilyTagVocabulary(nil, nil, nil)
	if len(got) != 0 {
		t.Errorf("expected empty vocabulary, got %v", got)
	}
}

func TestComputeMeetilyTagVocabulary_AllMechanicalTagsExcluded(t *testing.T) {
	taskTags := []string{"profile:home", "due:2026-01-01", "today", "tool:x"}
	got := computeMeetilyTagVocabulary(taskTags, nil, nil)
	if len(got) != 0 {
		t.Errorf("expected all-mechanical task tags to be excluded, got %v", got)
	}
}

// TestBuildMeetilyTagVocabulary_ReadsTasksAndEntities exercises the I/O
// orchestrator against a real (temp-dir) Brain: writes tasks via AddTask
// and entities via DB.UpsertEntity, then verifies the resulting
// vocabulary reflects both, with mechanical task tags dropped and the
// caller-supplied library entities folded in too.
func TestBuildMeetilyTagVocabulary_ReadsTasksAndEntities(t *testing.T) {
	b := newTestBrain(t)

	if _, err := b.AddTask("Ship the CTA landing page", []string{"cta", "due:2026-09-01"}, ""); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	if _, err := b.AddTask("Fix HOA insurance paperwork", []string{"hoa", "today"}, ""); err != nil {
		t.Fatalf("AddTask: %v", err)
	}

	if err := b.DB.UpsertEntity(&EntityRecord{Slug: "butterstack", Type: "tools", Name: "ButterStack"}); err != nil {
		t.Fatalf("UpsertEntity: %v", err)
	}

	vocab := b.buildMeetilyTagVocabulary([]string{"camp-butz"})

	want := map[string]bool{"cta": true, "hoa": true, "butterstack": true, "camp-butz": true}
	if len(vocab) != len(want) {
		t.Fatalf("buildMeetilyTagVocabulary = %v, want exactly %v", vocab, want)
	}
	for _, v := range vocab {
		if !want[v] {
			t.Errorf("unexpected vocabulary entry %q", v)
		}
	}
	// AddTask auto-adds a profile:<name> tag (see AddTask); that must not
	// leak into the vocabulary, and neither must "today"/"due:...".
	for _, v := range vocab {
		if isMechanicalTaskTag(v) {
			t.Errorf("mechanical tag %q leaked into buildMeetilyTagVocabulary's output", v)
		}
	}
}
