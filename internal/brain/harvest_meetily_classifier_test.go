package brain

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// meetilyClassifierMemory mirrors harvestDistillResponse's inline memory
// item shape -- duplicated here (rather than reused) because that shape
// is an unexported anonymous struct field type in harvest.go.
type meetilyClassifierMemory struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Type        string `json:"type"`
	Body        string `json:"body"`
}

// meetilyClassifierFakeResponse is the full JSON shape a real distill call
// returns for a meetily session: memories plus the classification fields.
type meetilyClassifierFakeResponse struct {
	Memories      []meetilyClassifierMemory `json:"memories"`
	Tags          []string                  `json:"tags"`
	Participants  []string                  `json:"participants"`
	ActionItems   []string                  `json:"action_items"`
	KeyPoints     []string                  `json:"key_points"`
	IsNewCategory bool                      `json:"is_new_category"`
	ProposedTag   string                    `json:"proposed_tag"`
}

// fakeMeetilyClassifierDistill returns resp for every call, regardless of
// input -- exercises the SAME-response-drives-both-memories-and-
// classification path (TagsFromResponse) end to end.
func fakeMeetilyClassifierDistill(resp meetilyClassifierFakeResponse) DistillFunc {
	return func(_ context.Context, _, _ string, _ map[string]interface{}) (string, error) {
		data, err := json.Marshal(resp)
		return string(data), err
	}
}

func readMeetilyCallJSON(t *testing.T, dir string) meetilyCallJSON {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "call.json"))
	if err != nil {
		t.Fatalf("read call.json: %v", err)
	}
	var out meetilyCallJSON
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("parse call.json: %v", err)
	}
	return out
}

// TestHarvestMeetily_ClassifierTagsFlowIntoCallJSONAndRecords verifies the
// classifier fields from the SAME distill response land in call.json and
// that the returned tags -- filtered to the vocabulary, with "meetily-
// call" replacing the shared write path's default "meetily-code" -- are
// stamped onto the written memory record.
func TestHarvestMeetily_ClassifierTagsFlowIntoCallJSONAndRecords(t *testing.T) {
	root := t.TempDir()
	dir := writeMeetilyCallFixture(t, root, "2026-08-24-14-09-chief-tech-advisor",
		[]string{"We agreed to launch the CTA practice next month."},
		"Discussed CTA practice launch.", "CTA Practice Launch")

	vocabulary := []string{"cta", "acme-widgets"}
	distill := fakeMeetilyClassifierDistill(meetilyClassifierFakeResponse{
		Memories: []meetilyClassifierMemory{
			{Name: "cta-practice-launch", Description: "d", Type: "event", Body: "Agreed to launch the CTA practice."},
		},
		Tags:          []string{"cta", "not-in-vocabulary"},
		Participants:  []string{"Thor Odinson", "Ralph"},
		ActionItems:   []string{"Send the proposal by Friday"},
		KeyPoints:     []string{"Agreed to launch the CTA practice next month"},
		IsNewCategory: false,
		ProposedTag:   "",
	})

	b := newTestBrain(t)
	ctx := context.Background()

	result, err := b.harvestMeetily(ctx, root, distill, vocabulary, HarvestOptions{Now: mustParseTime(t, "2026-08-25T00:00:00Z")})
	if err != nil {
		t.Fatalf("harvestMeetily: %v", err)
	}
	if len(result.Sessions) != 1 || len(result.Sessions[0].Written) != 1 {
		t.Fatalf("expected 1 session with 1 memory, got %+v", result.Sessions)
	}
	rec := result.Sessions[0].Written[0]

	hasTag := func(tag string) bool {
		for _, tg := range rec.Tags {
			if tg == tag {
				return true
			}
		}
		return false
	}
	if !hasTag("meetily-call") {
		t.Errorf("expected \"meetily-call\" tag, got %v", rec.Tags)
	}
	if !hasTag("cta") {
		t.Errorf("expected vocabulary tag \"cta\", got %v", rec.Tags)
	}
	if hasTag("not-in-vocabulary") {
		t.Errorf("expected out-of-vocabulary tag to be filtered out, got %v", rec.Tags)
	}
	if hasTag("meetily-code") {
		t.Errorf("expected the generic \"meetily-code\" tag to be replaced, got %v", rec.Tags)
	}
	if hasTag("needs-review") {
		t.Errorf("expected no needs-review tag for a non-new-category call, got %v", rec.Tags)
	}

	callJSON := readMeetilyCallJSON(t, dir)
	if callJSON.Title != "CTA Practice Launch" {
		t.Errorf("call.json title = %q", callJSON.Title)
	}
	if len(callJSON.Tags) != 1 || callJSON.Tags[0] != "cta" {
		t.Errorf("call.json tags = %v, want [cta]", callJSON.Tags)
	}
	if len(callJSON.Participants) != 2 {
		t.Errorf("call.json participants = %v", callJSON.Participants)
	}
	if len(callJSON.ActionItems) != 1 || callJSON.ActionItems[0] != "Send the proposal by Friday" {
		t.Errorf("call.json action_items = %v", callJSON.ActionItems)
	}
	if len(callJSON.KeyPoints) != 1 {
		t.Errorf("call.json key_points = %v", callJSON.KeyPoints)
	}
	if callJSON.IsNewCategory {
		t.Error("call.json is_new_category = true, want false")
	}

	tasks, err := b.ListTasksAllProfiles(true, []string{"needs-review"}, 0, 0)
	if err != nil {
		t.Fatalf("ListTasksAllProfiles: %v", err)
	}
	if len(tasks) != 0 {
		t.Errorf("expected no review task for a non-new-category call, got %+v", tasks)
	}
}

// TestHarvestMeetily_NewCategoryTagsAndCreatesReviewTask verifies
// is_new_category true tags the record and call.json "needs-review" and
// opens an aida task titled with the proposed tag, call title, and date.
func TestHarvestMeetily_NewCategoryTagsAndCreatesReviewTask(t *testing.T) {
	root := t.TempDir()
	dir := writeMeetilyCallFixture(t, root, "2026-08-25-09-00-contoso-intro",
		[]string{"Nice to meet you, I'm from Contoso."},
		"Intro call with a new prospective consulting client.", "Contoso Intro Call")

	distill := fakeMeetilyClassifierDistill(meetilyClassifierFakeResponse{
		Memories:      nil,
		Tags:          nil,
		Participants:  []string{"Contoso Rep"},
		ActionItems:   nil,
		KeyPoints:     []string{"New prospective client, no existing tag fits"},
		IsNewCategory: true,
		ProposedTag:   "contoso",
	})

	b := newTestBrain(t)
	ctx := context.Background()

	result, err := b.harvestMeetily(ctx, root, distill, []string{"cta", "acme-widgets"}, HarvestOptions{Now: mustParseTime(t, "2026-08-25T12:00:00Z")})
	if err != nil {
		t.Fatalf("harvestMeetily: %v", err)
	}
	if len(result.Sessions) != 1 {
		t.Fatalf("expected 1 session, got %+v", result.Sessions)
	}

	callJSON := readMeetilyCallJSON(t, dir)
	if !callJSON.IsNewCategory {
		t.Error("call.json is_new_category = false, want true")
	}
	if callJSON.ProposedTag != "contoso" {
		t.Errorf("call.json proposed_tag = %q, want contoso", callJSON.ProposedTag)
	}

	tasks, err := b.ListTasksAllProfiles(true, []string{"needs-review"}, 0, 0)
	if err != nil {
		t.Fatalf("ListTasksAllProfiles: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 review task, got %d: %+v", len(tasks), tasks)
	}
	task := tasks[0]
	if !contains(task.Title, "contoso") || !contains(task.Title, "Contoso Intro Call") {
		t.Errorf("task title = %q, want it to reference the proposed tag and call title", task.Title)
	}
	hasTag := func(tag string) bool {
		for _, tg := range task.Tags {
			if tg == tag {
				return true
			}
		}
		return false
	}
	if !hasTag("calls") || !hasTag("needs-review") {
		t.Errorf("task tags = %v, want [calls needs-review]", task.Tags)
	}
}

// TestHarvestMeetily_DryRunSkipsCallJSONAndReviewTask verifies --dry-run
// still runs the real distill call (so the classifier signal is real) but
// writes neither call.json nor a review task, matching the harvester's
// existing dry-run contract for memory writes.
func TestHarvestMeetily_DryRunSkipsCallJSONAndReviewTask(t *testing.T) {
	root := t.TempDir()
	dir := writeMeetilyCallFixture(t, root, "2026-08-25-09-00-contoso-intro",
		[]string{"Nice to meet you."}, "Intro call.", "Contoso Intro Call")

	distill := fakeMeetilyClassifierDistill(meetilyClassifierFakeResponse{
		IsNewCategory: true,
		ProposedTag:   "contoso",
	})

	b := newTestBrain(t)
	ctx := context.Background()

	result, err := b.harvestMeetily(ctx, root, distill, nil, HarvestOptions{Now: mustParseTime(t, "2026-08-25T12:00:00Z"), DryRun: true})
	if err != nil {
		t.Fatalf("harvestMeetily: %v", err)
	}
	if len(result.Sessions) != 1 {
		t.Fatalf("expected 1 session, got %+v", result.Sessions)
	}

	if _, err := os.Stat(filepath.Join(dir, "call.json")); !os.IsNotExist(err) {
		t.Errorf("expected no call.json written on dry-run, stat err = %v", err)
	}

	tasks, err := b.ListTasksAllProfiles(true, []string{"needs-review"}, 0, 0)
	if err != nil {
		t.Fatalf("ListTasksAllProfiles: %v", err)
	}
	if len(tasks) != 0 {
		t.Errorf("expected no review task on dry-run, got %+v", tasks)
	}
}
