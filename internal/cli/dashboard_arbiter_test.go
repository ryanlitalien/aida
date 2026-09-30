package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ryanlitalien/aida/internal/arbiter"
)

// TestArbiterEndpointServesEmptyState covers a box where the scheduler has
// never run: GET /api/arbiter must still succeed (200), not 500, with
// zero-value fields rather than erroring on missing state files.
func TestArbiterEndpointServesEmptyState(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	deps := newTestDeps(nil)
	deps.ArbiterStateDir = t.TempDir()

	mux := http.NewServeMux()
	registerDashboardRoutes(mux, deps)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/arbiter")
	if err != nil {
		t.Fatalf("GET /api/arbiter: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got arbiterAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Scheduler.State != "" || got.LastWave != nil || len(got.LaneMarks) != 0 {
		t.Errorf("got %+v, want all-zero-value fields for a box that never ran the scheduler", got)
	}
}

// TestArbiterEndpointServesPopulatedState covers the normal case: with
// scheduler.json, last-wave.json, and a lane mark all written under a
// temp state dir, GET /api/arbiter reflects every one of them.
func TestArbiterEndpointServesPopulatedState(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stateDir := t.TempDir()

	nextWake := time.Date(2026, 9, 25, 23, 0, 0, 0, time.UTC)
	if err := writeSchedulerState(stateDir, schedulerState{State: "sleeping", NextWake: nextWake, Reason: "outside the overnight window"}); err != nil {
		t.Fatalf("writeSchedulerState: %v", err)
	}
	wave := arbiter.WaveSummary{Tasks: 2, Passed: 2, ByLane: map[string]int{"claude-max": 2}}
	if err := writeWaveSummary(stateDir, wave); err != nil {
		t.Fatalf("writeWaveSummary: %v", err)
	}
	store, err := arbiter.OpenStore(stateDir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	if err := store.Record(arbiter.Signal{Lane: "claude-pro-bs", Source: "runner", Verdict: arbiter.VerdictEmpty, EmptyUntil: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	deps := newTestDeps(nil)
	deps.ArbiterStateDir = stateDir

	mux := http.NewServeMux()
	registerDashboardRoutes(mux, deps)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/arbiter")
	if err != nil {
		t.Fatalf("GET /api/arbiter: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got arbiterAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Scheduler.State != "sleeping" {
		t.Errorf("scheduler.state = %q, want sleeping", got.Scheduler.State)
	}
	if !got.NextWake.Equal(nextWake) {
		t.Errorf("next_wake = %v, want %v", got.NextWake, nextWake)
	}
	if got.LastWave == nil || got.LastWave.Tasks != 2 {
		t.Fatalf("last_wave = %+v, want Tasks=2", got.LastWave)
	}
	if len(got.LaneMarks) != 1 || got.LaneMarks[0].Lane != "claude-pro-bs" {
		t.Fatalf("lane_marks = %+v, want one claude-pro-bs entry", got.LaneMarks)
	}
}
