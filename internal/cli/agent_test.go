package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ryanlitalien/aida/internal/engine"
	"github.com/ryanlitalien/aida/internal/engine/orchestrator"
	"github.com/ryanlitalien/aida/internal/jobs"
	"github.com/ryanlitalien/aida/internal/library"
)

// fakeLayerRegistry constructs an in-memory library.Registry with two
// available layers and one unavailable, used by the get_layer tool tests.
func fakeLayerRegistry(t *testing.T) *library.Registry {
	t.Helper()
	tmp := t.TempDir()
	makeLayer := func(name, body string) string {
		p := filepath.Join(tmp, name+".md")
		if err := os.WriteFile(p, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	root := &library.Root{Ref: library.RootRef{Name: "test"}, AbsPath: tmp}
	return &library.Registry{
		Roots: []*library.Root{root},
		Layers: map[string]*library.ResolvedLayer{
			"first-chair": {
				Name:      "first-chair",
				Root:      root,
				AbsFile:   makeLayer("first-chair", "# Source: first-chair\n\nFirst Chair partner integration.\n\n# Body\nDetails here.\n"),
				Available: true,
			},
			"missing-cli": {
				Name:         "missing-cli",
				Root:         root,
				AbsFile:      makeLayer("missing-cli", "# Source: missing-cli\n\nNeeds a missing tool.\n"),
				Available:    false,
				MissingTools: []string{"someCmd"},
			},
		},
	}
}

func callGetLayer(t *testing.T, reg *library.Registry, name string) (string, error) {
	t.Helper()
	tool := buildGetLayerTool(reg)
	input, _ := json.Marshal(map[string]string{"name": name})
	return tool.Execute(context.Background(), input)
}

func TestGetLayerTool_FetchesAvailableLayer(t *testing.T) {
	reg := fakeLayerRegistry(t)
	out, err := callGetLayer(t, reg, "first-chair")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "First Chair partner integration") {
		t.Errorf("expected layer body in output, got:\n%s", out)
	}
	if !strings.Contains(out, "<!-- layer: first-chair (test) -->") {
		t.Errorf("expected layer header in output, got:\n%s", out)
	}
}

func TestGetLayerTool_RejectsMissingName(t *testing.T) {
	reg := fakeLayerRegistry(t)
	tool := buildGetLayerTool(reg)
	_, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err == nil {
		t.Errorf("expected error for empty name, got nil")
	}
}

func TestGetLayerTool_UnknownLayerError(t *testing.T) {
	reg := fakeLayerRegistry(t)
	_, err := callGetLayer(t, reg, "does-not-exist")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' error, got %v", err)
	}
}

func TestGetLayerTool_UnavailableLayerError(t *testing.T) {
	reg := fakeLayerRegistry(t)
	_, err := callGetLayer(t, reg, "missing-cli")
	if err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Errorf("expected 'unavailable' error, got %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), "someCmd") {
		t.Errorf("expected missing-tool name in error, got %v", err)
	}
}

// upsertTool must replace a same-named palette entry (two tools with one
// name is an Anthropic API 400) and append genuinely new tools.
func TestUpsertTool(t *testing.T) {
	tools := []engine.AgentTool{
		{Name: "ask_user", Description: "stdin version"},
		{Name: "find_source"},
	}
	tools = upsertTool(tools, engine.AgentTool{Name: "ask_user", Description: "run-dir version"})
	if len(tools) != 2 {
		t.Fatalf("replace grew the palette: %d tools", len(tools))
	}
	if tools[0].Description != "run-dir version" {
		t.Errorf("ask_user not replaced: %q", tools[0].Description)
	}
	tools = upsertTool(tools, engine.AgentTool{Name: "request_approval"})
	if len(tools) != 3 {
		t.Fatalf("append missing: %d tools", len(tools))
	}
	seen := map[string]bool{}
	for _, tool := range tools {
		if seen[tool.Name] {
			t.Errorf("duplicate tool name %q survived", tool.Name)
		}
		seen[tool.Name] = true
	}
}

// TestFinalizeParamsFromResult covers the translation this package
// introduces from an orchestrator RunResult to jobs.FinalizeParams,
// the piece of the reported incident this task closes: an agent job
// that ran out of turns mid-task, never verified its work, and still
// got recorded state: done. Each case also runs the params through
// jobs.FinalizeState so the test asserts the actual terminal state a
// real run of this shape would earn, not just the intermediate struct.
func TestFinalizeParamsFromResult(t *testing.T) {
	cases := []struct {
		name       string
		result     *orchestrator.RunResult
		wantParams jobs.FinalizeParams
		wantState  string
	}{
		{
			name: "final response with no structured result -> incomplete, never done",
			result: &orchestrator.RunResult{
				Termination: orchestrator.TerminationFinalResponse,
				FinalOutput: "looks like I made progress",
				Result:      nil,
			},
			wantParams: jobs.FinalizeParams{
				Termination: jobs.TerminationFinalResponse,
				HasResult:   false,
			},
			wantState: jobs.StateIncomplete,
		},
		{
			name: "submit_result achieved -> done",
			result: &orchestrator.RunResult{
				Termination: orchestrator.TerminationFinalResponse,
				Result: &orchestrator.SubmitResult{
					Outcome: orchestrator.OutcomeAchieved,
					Summary: "upgraded to v2.3.1",
				},
			},
			wantParams: jobs.FinalizeParams{
				Termination: jobs.TerminationFinalResponse,
				HasResult:   true,
				Outcome:     jobs.OutcomeAchieved,
				Summary:     "upgraded to v2.3.1",
			},
			wantState: jobs.StateDone,
		},
		{
			name: "submit_result not_achieved -> failed",
			result: &orchestrator.RunResult{
				Termination: orchestrator.TerminationFinalResponse,
				Result: &orchestrator.SubmitResult{
					Outcome:     orchestrator.OutcomeNotAchieved,
					Summary:     "backup gate rejected every credential I tried",
					FailureCode: "auth_rejected",
				},
			},
			wantParams: jobs.FinalizeParams{
				Termination: jobs.TerminationFinalResponse,
				HasResult:   true,
				Outcome:     jobs.OutcomeNotAchieved,
				Summary:     "backup gate rejected every credential I tried",
				FailureCode: "auth_rejected",
			},
			wantState: jobs.StateFailed,
		},
		{
			name: "wall-clock deadline trip with no result -> incomplete",
			result: &orchestrator.RunResult{
				Termination: orchestrator.TerminationWallTimeLimit,
			},
			wantParams: jobs.FinalizeParams{
				Termination: jobs.TerminationWallTimeLimit,
				HasResult:   false,
			},
			wantState: jobs.StateIncomplete,
		},
		{
			name: "cost ceiling trip with no result -> incomplete",
			result: &orchestrator.RunResult{
				Termination: orchestrator.TerminationCostLimit,
			},
			wantParams: jobs.FinalizeParams{
				Termination: jobs.TerminationCostLimit,
				HasResult:   false,
			},
			wantState: jobs.StateIncomplete,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := finalizeParamsFromResult(c.result)
			if got != c.wantParams {
				t.Errorf("finalizeParamsFromResult = %+v, want %+v", got, c.wantParams)
			}
			if state := jobs.FinalizeState(got); state != c.wantState {
				t.Errorf("jobs.FinalizeState(%+v) = %q, want %q", got, state, c.wantState)
			}
		})
	}
}
