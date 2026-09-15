package brain

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestApplyConsolidation_WritesFactsAndInstructionsWithProvenance(t *testing.T) {
	b := newTestBrain(t)
	ctx := context.Background()

	ev1, err := b.WriteMemory(ctx, MemoryRecord{Type: MemoryEvent, Body: "user said they prefer dark mode"})
	if err != nil {
		t.Fatalf("write event 1: %v", err)
	}
	ev2, err := b.WriteMemory(ctx, MemoryRecord{Type: MemoryEvent, Body: "user confirmed dark mode again"})
	if err != nil {
		t.Fatalf("write event 2: %v", err)
	}

	resp := consolidateResponse{
		Facts: []consolidateFactOutput{
			{Body: "user prefers dark mode", Key: "user-theme", SupportingEventIDs: []string{ev1.ID, ev2.ID}},
		},
		Instructions: []consolidateFactOutput{
			{Body: "always default new UI to dark mode", Key: "default-dark-mode", SupportingEventIDs: []string{ev1.ID}},
		},
	}

	result, err := b.applyConsolidation(ctx, resp, false)
	if err != nil {
		t.Fatalf("applyConsolidation: %v", err)
	}
	if result.Dropped != 0 {
		t.Errorf("expected 0 dropped, got %d", result.Dropped)
	}

	if len(result.FactsWritten) != 1 {
		t.Fatalf("expected 1 fact written, got %d", len(result.FactsWritten))
	}
	f := result.FactsWritten[0]
	if f.Body != "user prefers dark mode" || f.Key != "user-theme" {
		t.Errorf("fact mismatch: %+v", f)
	}
	if len(f.Provenance) != 2 || f.Provenance[0] != ev1.ID || f.Provenance[1] != ev2.ID {
		t.Errorf("fact provenance mismatch: %v", f.Provenance)
	}
	if f.Confidence != 1.0 {
		t.Errorf("fact confidence = %v, want 1.0", f.Confidence)
	}
	if f.ID == "" {
		t.Error("expected a real write to assign an ID")
	}

	if len(result.InstructionsWritten) != 1 {
		t.Fatalf("expected 1 instruction written, got %d", len(result.InstructionsWritten))
	}
	ins := result.InstructionsWritten[0]
	if ins.Body != "always default new UI to dark mode" || ins.Key != "default-dark-mode" {
		t.Errorf("instruction mismatch: %+v", ins)
	}

	// Verify the persisted row (not just the in-memory result) carries
	// provenance - round-trip through brain.db.
	got, err := b.DB.GetMemory(f.ID)
	if err != nil {
		t.Fatalf("GetMemory: %v", err)
	}
	if len(got.Provenance) != 2 {
		t.Errorf("persisted provenance = %v, want 2 entries", got.Provenance)
	}
}

func TestApplyConsolidation_ProvenanceGuardDropsUnsupportedOutputs(t *testing.T) {
	b := newTestBrain(t)
	ctx := context.Background()

	resp := consolidateResponse{
		Facts: []consolidateFactOutput{
			{Body: "unsupported fact", Key: "k1", SupportingEventIDs: nil},
			{Body: "blank-id fact", Key: "k2", SupportingEventIDs: []string{"  ", ""}},
		},
		Instructions: []consolidateFactOutput{
			{Body: "unsupported instruction", Key: "k3", SupportingEventIDs: []string{}},
		},
		Supersedes: []consolidateSupersedeOutput{
			{OldID: "nonexistent-id", NewBody: "x", Contradiction: "natural", SupportingEventIDs: []string{"ev-1"}},
		},
	}

	result, err := b.applyConsolidation(ctx, resp, false)
	if err != nil {
		t.Fatalf("applyConsolidation: %v", err)
	}
	if len(result.FactsWritten) != 0 {
		t.Errorf("expected 0 facts written, got %d", len(result.FactsWritten))
	}
	if len(result.InstructionsWritten) != 0 {
		t.Errorf("expected 0 instructions written, got %d", len(result.InstructionsWritten))
	}
	if len(result.Superseded) != 0 {
		t.Errorf("expected 0 supersedes applied, got %d", len(result.Superseded))
	}
	// 2 facts (no provenance / blank-only ids) + 1 instruction (empty
	// slice) + 1 supersede (unresolvable old_id) = 4.
	if result.Dropped != 4 {
		t.Errorf("Dropped = %d, want 4", result.Dropped)
	}

	all, _ := b.DB.ListMemory(MemoryListOpts{Types: []MemoryType{MemoryFact, MemoryInstruction}})
	if len(all) != 0 {
		t.Errorf("expected nothing written to db, got %d row(s)", len(all))
	}
}

func TestApplyConsolidation_DryRunWritesNothing(t *testing.T) {
	b := newTestBrain(t)
	ctx := context.Background()

	resp := consolidateResponse{
		Facts: []consolidateFactOutput{
			{Body: "dry fact", Key: "dry-key", SupportingEventIDs: []string{"ev-1"}},
		},
		Instructions: []consolidateFactOutput{
			{Body: "dry instruction", Key: "dry-instr-key", SupportingEventIDs: []string{"ev-1"}},
		},
	}
	result, err := b.applyConsolidation(ctx, resp, true)
	if err != nil {
		t.Fatalf("applyConsolidation: %v", err)
	}
	if len(result.FactsWritten) != 1 || len(result.InstructionsWritten) != 1 {
		t.Fatalf("expected 1 proposed fact + 1 proposed instruction, got %d/%d",
			len(result.FactsWritten), len(result.InstructionsWritten))
	}
	if result.FactsWritten[0].ID != "" {
		t.Errorf("dry-run fact should have no ID (never written), got %q", result.FactsWritten[0].ID)
	}
	if result.FactsWritten[0].Body != "dry fact" {
		t.Errorf("dry-run should still report the proposed body: %+v", result.FactsWritten[0])
	}

	allFacts, _ := b.DB.ListMemory(MemoryListOpts{Types: []MemoryType{MemoryFact}})
	allInstr, _ := b.DB.ListMemory(MemoryListOpts{Types: []MemoryType{MemoryInstruction}})
	if len(allFacts) != 0 || len(allInstr) != 0 {
		t.Errorf("dry-run must not write; found %d fact(s), %d instruction(s)", len(allFacts), len(allInstr))
	}
}

func TestApplyConsolidation_SupersedeWritesReplacementAndRetiresOld(t *testing.T) {
	b := newTestBrain(t)
	ctx := context.Background()

	old, err := b.WriteMemory(ctx, MemoryRecord{Type: MemoryFact, Key: "user-units", Body: "user uses metric"})
	if err != nil {
		t.Fatalf("write old fact: %v", err)
	}
	ev, err := b.WriteMemory(ctx, MemoryRecord{Type: MemoryEvent, Body: "user said they actually use US units"})
	if err != nil {
		t.Fatalf("write event: %v", err)
	}

	resp := consolidateResponse{
		Supersedes: []consolidateSupersedeOutput{
			{
				OldID: old.ID, NewBody: "user uses US units", NewKey: "user-units",
				Contradiction: "harsh", SupportingEventIDs: []string{ev.ID},
			},
		},
	}
	result, err := b.applyConsolidation(ctx, resp, false)
	if err != nil {
		t.Fatalf("applyConsolidation: %v", err)
	}
	if len(result.Superseded) != 1 {
		t.Fatalf("expected 1 supersede, got %d", len(result.Superseded))
	}
	sup := result.Superseded[0]
	if sup.NewID == "" {
		t.Fatal("expected NewID to be set on a real (non-dry-run) supersede")
	}
	if sup.OldID != old.ID {
		t.Errorf("sup.OldID = %q, want %q", sup.OldID, old.ID)
	}

	// New record inherits the old record's type and carries the
	// harsh-contradiction confidence penalty.
	newRec, err := b.DB.GetMemory(sup.NewID)
	if err != nil {
		t.Fatalf("GetMemory new: %v", err)
	}
	if newRec.Confidence != 0.7 {
		t.Errorf("new record confidence = %v, want 0.7 for harsh contradiction", newRec.Confidence)
	}
	if newRec.Type != MemoryFact {
		t.Errorf("new record type = %q, want fact (inherited from old)", newRec.Type)
	}
	if len(newRec.Provenance) != 1 || newRec.Provenance[0] != ev.ID {
		t.Errorf("new record provenance = %v, want [%s]", newRec.Provenance, ev.ID)
	}

	// Old record now points to new - supersede chain intact.
	oldRec, err := b.DB.GetMemory(old.ID)
	if err != nil {
		t.Fatalf("GetMemory old: %v", err)
	}
	if oldRec.SupersededBy != sup.NewID {
		t.Errorf("old.SupersededBy = %q, want %q", oldRec.SupersededBy, sup.NewID)
	}

	// By-key active lookup resolves to the new record.
	active, err := b.DB.ActiveMemoryByKey(MemoryFact, "user-units", "test")
	if err != nil {
		t.Fatalf("ActiveMemoryByKey: %v", err)
	}
	if active.ID != sup.NewID {
		t.Errorf("active-by-key = %q, want %q", active.ID, sup.NewID)
	}
}

func TestApplyConsolidation_NaturalContradictionKeepsFullConfidenceAndFallsBackToOldKey(t *testing.T) {
	b := newTestBrain(t)
	ctx := context.Background()
	old, err := b.WriteMemory(ctx, MemoryRecord{Type: MemoryInstruction, Key: "k", Body: "old rule"})
	if err != nil {
		t.Fatalf("write old instruction: %v", err)
	}
	ev, _ := b.WriteMemory(ctx, MemoryRecord{Type: MemoryEvent, Body: "rule evolved"})

	resp := consolidateResponse{
		Supersedes: []consolidateSupersedeOutput{
			// NewKey left empty on purpose - should fall back to old.Key.
			{OldID: old.ID, NewBody: "new rule", Contradiction: "natural", SupportingEventIDs: []string{ev.ID}},
		},
	}
	result, err := b.applyConsolidation(ctx, resp, false)
	if err != nil {
		t.Fatalf("applyConsolidation: %v", err)
	}
	newRec, err := b.DB.GetMemory(result.Superseded[0].NewID)
	if err != nil {
		t.Fatalf("GetMemory: %v", err)
	}
	if newRec.Confidence != 1.0 {
		t.Errorf("confidence = %v, want 1.0 for natural contradiction", newRec.Confidence)
	}
	if newRec.Type != MemoryInstruction {
		t.Errorf("type = %q, want instruction (inherited)", newRec.Type)
	}
	if newRec.Key != "k" {
		t.Errorf("key = %q, want fallback to old key %q", newRec.Key, "k")
	}
}

func TestApplyConsolidation_DryRunSupersedeDoesNotRetireOld(t *testing.T) {
	b := newTestBrain(t)
	ctx := context.Background()
	old, err := b.WriteMemory(ctx, MemoryRecord{Type: MemoryFact, Key: "k", Body: "old"})
	if err != nil {
		t.Fatalf("write old fact: %v", err)
	}
	ev, _ := b.WriteMemory(ctx, MemoryRecord{Type: MemoryEvent, Body: "contradiction"})

	resp := consolidateResponse{
		Supersedes: []consolidateSupersedeOutput{
			{OldID: old.ID, NewBody: "new", Contradiction: "harsh", SupportingEventIDs: []string{ev.ID}},
		},
	}
	result, err := b.applyConsolidation(ctx, resp, true)
	if err != nil {
		t.Fatalf("applyConsolidation: %v", err)
	}
	if len(result.Superseded) != 1 || result.Superseded[0].NewID != "" {
		t.Errorf("dry-run supersede should report empty NewID, got %+v", result.Superseded)
	}
	oldRec, err := b.DB.GetMemory(old.ID)
	if err != nil {
		t.Fatalf("GetMemory: %v", err)
	}
	if oldRec.SupersededBy != "" {
		t.Errorf("dry-run must not retire the old record, got SupersededBy=%q", oldRec.SupersededBy)
	}
}

func TestSupersedeByID(t *testing.T) {
	b := newTestBrain(t)
	ctx := context.Background()
	a, err := b.WriteMemory(ctx, MemoryRecord{Type: MemoryFact, Key: "key-a", Body: "record a"})
	if err != nil {
		t.Fatalf("write a: %v", err)
	}
	c, err := b.WriteMemory(ctx, MemoryRecord{Type: MemoryInstruction, Key: "key-c", Body: "record c"})
	if err != nil {
		t.Fatalf("write c: %v", err)
	}

	if err := b.DB.SupersedeByID(a.ID, c.ID); err != nil {
		t.Fatalf("SupersedeByID: %v", err)
	}
	got, err := b.DB.GetMemory(a.ID)
	if err != nil {
		t.Fatalf("GetMemory: %v", err)
	}
	if got.SupersededBy != c.ID {
		t.Errorf("SupersededBy = %q, want %q", got.SupersededBy, c.ID)
	}

	// Dropped from the FTS corpus, mirroring SupersedePriorMemory.
	var count int
	if err := b.DB.conn.QueryRow(
		`SELECT COUNT(*) FROM corpus_fts WHERE doc_id = ?`, "memory:"+a.ID,
	).Scan(&count); err != nil {
		t.Fatalf("query corpus_fts: %v", err)
	}
	if count != 0 {
		t.Errorf("expected superseded record dropped from FTS corpus, found %d row(s)", count)
	}

	// Empty/missing ids no-op rather than error.
	if err := b.DB.SupersedeByID("", "x"); err != nil {
		t.Errorf("empty oldID should no-op without error: %v", err)
	}
	if err := b.DB.SupersedeByID("missing-id", c.ID); err != nil {
		t.Errorf("missing oldID should no-op without error: %v", err)
	}
}

func TestShouldAutoConsolidate_NoMetaFile(t *testing.T) {
	b := newTestBrain(t)
	ctx := context.Background()

	if b.ShouldAutoConsolidate() {
		t.Error("expected false with 0 events and no meta file")
	}

	for i := 0; i < consolidateAutoThreshold-1; i++ {
		if _, err := b.WriteMemory(ctx, MemoryRecord{Type: MemoryEvent, Body: fmt.Sprintf("event %d", i)}); err != nil {
			t.Fatalf("write event %d: %v", i, err)
		}
	}
	if b.ShouldAutoConsolidate() {
		t.Errorf("expected false with %d events (threshold=%d)", consolidateAutoThreshold-1, consolidateAutoThreshold)
	}

	if _, err := b.WriteMemory(ctx, MemoryRecord{Type: MemoryEvent, Body: "threshold event"}); err != nil {
		t.Fatalf("write threshold event: %v", err)
	}
	if !b.ShouldAutoConsolidate() {
		t.Error("expected true at threshold")
	}
}

func TestShouldAutoConsolidate_WithRecentRun(t *testing.T) {
	b := newTestBrain(t)
	ctx := context.Background()

	past := time.Now().Add(-1 * time.Hour).UTC()
	for i := 0; i < 15; i++ {
		ts := past.Add(time.Duration(i) * time.Second).Format(time.RFC3339)
		// Body must be unique per iteration - newMemoryID hashes
		// type|key|body, and identical bodies written within the same
		// millisecond would collide onto the same id (INSERT OR
		// REPLACE), silently collapsing distinct events into one row.
		if _, err := b.WriteMemory(ctx, MemoryRecord{Type: MemoryEvent, Body: fmt.Sprintf("old event %d", i), Created: ts}); err != nil {
			t.Fatalf("write old event %d: %v", i, err)
		}
	}

	// Write the trigger-state file directly (as TestShouldAutoCompile_
	// WithRecentCompile does for last_compile.json) rather than calling
	// MarkConsolidated, so the cutoff is a controlled timestamp well in
	// the past - RFC3339 is second-resolution, and a real MarkConsolidated
	// call here could tie with the "new event" writes below if the test
	// runs fast enough to land in the same second.
	consolidateTime := time.Now().Add(-30 * time.Minute).UTC()
	meta := autoConsolidateMeta{LastConsolidate: consolidateTime.Format(time.RFC3339), EventCount: 15}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		t.Fatalf("marshal meta: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(b.Path, "meta"), 0755); err != nil {
		t.Fatalf("mkdir meta: %v", err)
	}
	if err := os.WriteFile(consolidateMetaPath(b.Path), data, 0644); err != nil {
		t.Fatalf("write meta: %v", err)
	}

	if b.ShouldAutoConsolidate() {
		t.Error("expected false immediately after the consolidate timestamp with no new events")
	}

	for i := 0; i < consolidateAutoThreshold-1; i++ {
		ts := time.Now().Add(time.Duration(i) * time.Second).UTC().Format(time.RFC3339)
		if _, err := b.WriteMemory(ctx, MemoryRecord{Type: MemoryEvent, Body: fmt.Sprintf("new event %d", i), Created: ts}); err != nil {
			t.Fatalf("write new event %d: %v", i, err)
		}
	}
	if b.ShouldAutoConsolidate() {
		t.Errorf("expected false with %d new events since last run", consolidateAutoThreshold-1)
	}

	thresholdTS := time.Now().Add(1 * time.Minute).UTC().Format(time.RFC3339)
	if _, err := b.WriteMemory(ctx, MemoryRecord{Type: MemoryEvent, Body: "threshold event", Created: thresholdTS}); err != nil {
		t.Fatalf("write threshold event: %v", err)
	}
	if !b.ShouldAutoConsolidate() {
		t.Error("expected true after threshold new events since last run")
	}
}

func TestMarkConsolidated_WritesMetaFile(t *testing.T) {
	b := newTestBrain(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := b.WriteMemory(ctx, MemoryRecord{Type: MemoryEvent, Body: fmt.Sprintf("e-%d", i)}); err != nil {
			t.Fatalf("write event %d: %v", i, err)
		}
	}
	if err := b.MarkConsolidated(); err != nil {
		t.Fatalf("MarkConsolidated: %v", err)
	}
	meta, ok := b.readConsolidateMeta()
	if !ok {
		t.Fatal("expected meta file to be readable after MarkConsolidated")
	}
	if meta.EventCount != 3 {
		t.Errorf("EventCount = %d, want 3", meta.EventCount)
	}
	if meta.LastConsolidate == "" {
		t.Error("expected non-empty LastConsolidate timestamp")
	}
}
