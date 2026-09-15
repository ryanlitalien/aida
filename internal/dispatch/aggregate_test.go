package dispatch

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ryanlitalien/aida/internal/llm"
	"github.com/ryanlitalien/aida/internal/roster"
)

func TestDispatcher_Single(t *testing.T) {
	d := &Dispatcher{} // nil LLM is fine: single never calls it.
	pm := &roster.Entry{Name: "product-manager", CallSign: "Pamela", Kind: roster.KindSubagent}

	t.Run("success returns the text verbatim", func(t *testing.T) {
		r := routed{entry: pm, result: roster.Result{Entry: pm.Name, Status: roster.StatusSuccess, Text: "the roadmap is on track"}}
		got := d.single(r)
		if got != "the roadmap is on track" {
			t.Errorf("single() = %q, want the verbatim text", got)
		}
	})

	t.Run("delegated mentions the handle and the display name", func(t *testing.T) {
		r := routed{entry: pm, result: roster.Result{Entry: pm.Name, Status: roster.StatusDelegated, JobID: "20260722-000000-abc"}}
		got := d.single(r)
		if !strings.Contains(got, "Delegated to") {
			t.Errorf("single() = %q, want it to contain %q", got, "Delegated to")
		}
		if !strings.Contains(got, pm.Display()) {
			t.Errorf("single() = %q, want it to contain the display name %q", got, pm.Display())
		}
	})

	failureCases := []string{roster.StatusError, roster.StatusEmpty, roster.StatusTimeout}
	for _, status := range failureCases {
		status := status
		t.Run(status+" mentions the display name and the status", func(t *testing.T) {
			r := routed{entry: pm, result: roster.Result{Entry: pm.Name, Status: status, Text: "details"}}
			got := d.single(r)
			if !strings.Contains(got, pm.Display()) {
				t.Errorf("single() = %q, want it to contain the display name %q", got, pm.Display())
			}
			if !strings.Contains(got, status) {
				t.Errorf("single() = %q, want it to contain the status %q", got, status)
			}
		})
	}
}

func TestResultText(t *testing.T) {
	e := &roster.Entry{Name: "devops", CallSign: "Devin", Kind: roster.KindSubagent}

	tests := []struct {
		name    string
		result  roster.Result
		wantSub []string
	}{
		{
			name:    "success returns the text verbatim",
			result:  roster.Result{Status: roster.StatusSuccess, Text: "deploy is green"},
			wantSub: []string{"deploy is green"},
		},
		{
			name:    "delegated mentions the background handle",
			result:  roster.Result{Status: roster.StatusDelegated, JobID: "some-run-id"},
			wantSub: []string{"started in the background", handleFor("some-run-id")},
		},
		{
			name:    "error mentions the status and the message",
			result:  roster.Result{Status: roster.StatusError, Text: "connection refused"},
			wantSub: []string{"no answer", roster.StatusError, "connection refused"},
		},
		{
			name:    "empty mentions the status",
			result:  roster.Result{Status: roster.StatusEmpty, Text: ""},
			wantSub: []string{"no answer", roster.StatusEmpty},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resultText(routed{entry: e, result: tt.result})
			for _, want := range tt.wantSub {
				if !strings.Contains(got, want) {
					t.Errorf("resultText() = %q, want it to contain %q", got, want)
				}
			}
		})
	}
}

func TestColonSuffix(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty string yields no suffix", "", ""},
		{"whitespace-only yields no suffix", "   ", ""},
		{"non-empty text is prefixed with a dash", "boom", " - boom"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := colonSuffix(tt.in)
			if got != tt.want {
				t.Errorf("colonSuffix(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestPlainConcat(t *testing.T) {
	parts := []llm.AggregatePart{
		{Display: "Pamela", Text: "the roadmap is on track"},
		{Display: "Devin", Text: "deploy is green"},
	}
	want := "Pamela: the roadmap is on track\n\nDevin: deploy is green"
	got := plainConcat(parts)
	if got != want {
		t.Errorf("plainConcat() = %q, want %q", got, want)
	}
}

func TestDispatcher_Aggregate(t *testing.T) {
	pm := &roster.Entry{Name: "product-manager", CallSign: "Pamela", Kind: roster.KindSubagent}
	devops := &roster.Entry{Name: "devops", CallSign: "Devin", Kind: roster.KindSubagent}

	successRoutes := []routed{
		{entry: pm, subtask: "draft the roadmap", result: roster.Result{Entry: pm.Name, Status: roster.StatusSuccess, Text: "roadmap drafted"}},
		{entry: devops, subtask: "check the deploy", result: roster.Result{Entry: devops.Name, Status: roster.StatusSuccess, Text: "deploy is green"}},
	}

	t.Run("synthesizes via the LLM when at least one route succeeded", func(t *testing.T) {
		d := &Dispatcher{LLM: &fakeCompleter{textResp: "combined answer"}}
		got := d.aggregate(context.Background(), "status check", successRoutes)
		if got != "combined answer" {
			t.Errorf("aggregate() = %q, want %q", got, "combined answer")
		}
	})

	t.Run("falls back to plain concat when the LLM errors", func(t *testing.T) {
		d := &Dispatcher{LLM: &fakeCompleter{textErr: errors.New("boom")}}
		got := d.aggregate(context.Background(), "status check", successRoutes)
		if !strings.Contains(got, "Pamela") || !strings.Contains(got, "Devin") {
			t.Errorf("aggregate() = %q, want the plain-concat fallback naming both agents", got)
		}
	})

	t.Run("falls back to plain concat with a nil LLM", func(t *testing.T) {
		d := &Dispatcher{}
		got := d.aggregate(context.Background(), "status check", successRoutes)
		if !strings.Contains(got, "Pamela") || !strings.Contains(got, "Devin") {
			t.Errorf("aggregate() = %q, want the plain-concat fallback naming both agents", got)
		}
	})

	t.Run("all routes failed: falls back to plain concat even with a working LLM", func(t *testing.T) {
		failedRoutes := []routed{
			{entry: pm, subtask: "draft the roadmap", result: roster.Result{Entry: pm.Name, Status: roster.StatusError, Text: "timed out"}},
			{entry: devops, subtask: "check the deploy", result: roster.Result{Entry: devops.Name, Status: roster.StatusTimeout}},
		}
		d := &Dispatcher{LLM: &fakeCompleter{textResp: "should never be seen"}}
		got := d.aggregate(context.Background(), "status check", failedRoutes)
		if strings.Contains(got, "should never be seen") {
			t.Fatalf("aggregate() = %q, the LLM should not be consulted when every route failed", got)
		}
		if !strings.Contains(got, "Pamela") || !strings.Contains(got, "Devin") {
			t.Errorf("aggregate() = %q, want the plain-concat fallback naming both agents", got)
		}
	})
}
