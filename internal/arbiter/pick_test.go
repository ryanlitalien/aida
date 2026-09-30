package arbiter

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ryanlitalien/aida/internal/burndown"
)

// testLanes is a small, self-contained roster used by pick_test.go so
// these unit tests don't depend on (or break with) changes to
// DefaultConfig's real six-lane roster -- the eval suite in
// pick_eval_test.go is what exercises DefaultConfig itself.
func testLanes() *Config {
	return &Config{Lanes: []Lane{
		{
			ID:          "lane-a",
			Order:       1,
			Provider:    "Provider A",
			Windows:     []string{"5h"},
			DataClasses: []DataClass{DataClassPersonal, DataClassPublic},
			Runner:      RunnerClaude,
			Auth:        AuthSubscription,
			Models:      map[string]string{RoleExecutor: "model-a-exec", RoleThinker: "model-a-thinker"},
		},
		{
			ID:          "lane-b",
			Order:       2,
			Provider:    "Provider B",
			Windows:     []string{"5h"},
			DataClasses: []DataClass{DataClassButterstack, DataClassPersonal, DataClassPublic},
			Roles:       []string{RoleThinker},
			Runner:      RunnerClaude,
			Auth:        AuthSubscription,
			Models:      map[string]string{RoleExecutor: "model-b-exec"},
		},
		{
			ID:            "lane-c",
			Order:         3,
			DataClasses:   []DataClass{DataClassButterstack, DataClassGames, DataClassPublic},
			Runner:        RunnerClaude,
			Auth:          AuthAPIKey,
			AllowUnprobed: true,
			Models:        map[string]string{RoleExecutor: "model-c-exec"},
		},
	}}
}

func fiveHour(provider string, headroom float64) burndown.Capacity {
	return burndown.Capacity{Provider: provider, Label: "5-hour", UsedPct: 100 - headroom, LeftPct: 100 - (100 - headroom), Headroom: headroom}
}

type mapSignalReader map[string]time.Time

func (m mapSignalReader) EmptyUntil(lane string, now time.Time) (time.Time, bool) {
	t, ok := m[lane]
	if !ok {
		return time.Time{}, false
	}
	if now.Before(t) {
		return t, true
	}
	return time.Time{}, false
}

func TestPickHappyPath(t *testing.T) {
	cfg := testLanes()
	caps := []burndown.Capacity{fiveHour("Provider A", 90)}
	req := Request{Tags: []string{"personal"}, Role: RoleExecutor}
	dec, err := Pick(cfg, req, caps, nil)
	if err != nil {
		t.Fatalf("Pick error: %v", err)
	}
	if dec.Lane == nil || dec.Lane.ID != "lane-a" {
		t.Fatalf("expected lane-a, got %+v", dec.Lane)
	}
	if dec.Model != "model-a-exec" {
		t.Errorf("Model = %q, want model-a-exec", dec.Model)
	}
	if dec.Class != DataClassPersonal {
		t.Errorf("Class = %q, want personal", dec.Class)
	}
}

func TestPickRejectsDataClassNotAllowed(t *testing.T) {
	cfg := testLanes()
	// butterstack is not allowed on lane-a; lane-b allows it but only
	// serves the thinker role; lane-c allows it and serves executor,
	// and is unprobed+allowed, so it should win.
	caps := []burndown.Capacity{}
	dec, err := Pick(cfg, Request{Tags: []string{"butterstack"}, Role: RoleExecutor}, caps, nil)
	if err != nil {
		t.Fatalf("Pick error: %v", err)
	}
	if dec.Lane == nil || dec.Lane.ID != "lane-c" {
		t.Fatalf("expected lane-c, got %+v", dec.Lane)
	}
	found := false
	for _, r := range dec.Rejected {
		if r.Lane == "lane-a" && strings.Contains(r.Reason, "data class") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a data-class rejection for lane-a, got %+v", dec.Rejected)
	}
}

func TestPickRejectsRoleNotServed(t *testing.T) {
	cfg := testLanes()
	caps := []burndown.Capacity{fiveHour("Provider B", 90)}
	dec, err := Pick(cfg, Request{Tags: []string{"butterstack"}, Role: RoleThinker}, caps, nil)
	if err != nil {
		t.Fatalf("Pick error: %v", err)
	}
	// lane-b allows butterstack and serves thinker.
	if dec.Lane == nil || dec.Lane.ID != "lane-b" {
		t.Fatalf("expected lane-b, got %+v", dec.Lane)
	}
}

func TestPickRoleNotServedRejectionReason(t *testing.T) {
	cfg := &Config{Lanes: []Lane{
		{
			ID:          "thinker-only",
			Order:       1,
			DataClasses: []DataClass{DataClassPersonal},
			Roles:       []string{RoleThinker},
			Runner:      RunnerClaude,
			Auth:        AuthSubscription,
			Models:      map[string]string{RoleExecutor: "m"},
		},
	}}
	dec, err := Pick(cfg, Request{Tags: []string{"personal"}, Role: RoleExecutor}, nil, nil)
	if !errors.Is(err, ErrNoLane) {
		t.Fatalf("expected ErrNoLane, got %v", err)
	}
	if len(dec.Rejected) != 1 || !strings.Contains(dec.Rejected[0].Reason, `does not serve role "executor"`) {
		t.Errorf("Rejected = %+v", dec.Rejected)
	}
}

func TestPickRejectsSignalledEmpty(t *testing.T) {
	cfg := testLanes()
	caps := []burndown.Capacity{fiveHour("Provider B", 90)}
	now := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	sig := mapSignalReader{"lane-b": now.Add(time.Hour)}
	// tags=butterstack, role=thinker: lane-a doesn't allow butterstack,
	// lane-b would normally win (allows butterstack, serves thinker,
	// good capacity) but is signalled empty, so lane-c (allow_unprobed)
	// should win instead.
	dec, err := Pick(cfg, Request{Tags: []string{"butterstack"}, Role: RoleThinker, Now: now}, caps, sig)
	if err != nil {
		t.Fatalf("Pick error: %v", err)
	}
	if dec.Lane == nil || dec.Lane.ID != "lane-c" {
		t.Fatalf("expected lane-c after lane-b is signalled empty, got %+v", dec.Lane)
	}
	var reason string
	for _, r := range dec.Rejected {
		if r.Lane == "lane-b" {
			reason = r.Reason
		}
	}
	if !strings.Contains(reason, "signalled empty until") {
		t.Errorf("reason = %q, want it to mention signalled empty until", reason)
	}
}

func TestPickRejectsUnprobedWithoutAllow(t *testing.T) {
	cfg := &Config{Lanes: []Lane{
		{
			ID:          "no-probe",
			Order:       1,
			Provider:    "Nowhere",
			Windows:     []string{"5h"},
			DataClasses: []DataClass{DataClassPersonal},
			Runner:      RunnerClaude,
			Auth:        AuthSubscription,
			Models:      map[string]string{RoleExecutor: "m"},
		},
	}}
	dec, err := Pick(cfg, Request{Tags: []string{"personal"}}, nil, nil)
	if !errors.Is(err, ErrNoLane) {
		t.Fatalf("expected ErrNoLane, got %v", err)
	}
	if len(dec.Rejected) != 1 || dec.Rejected[0].Reason != "unprobed lane without allow_unprobed" {
		t.Errorf("Rejected = %+v", dec.Rejected)
	}
}

func TestPickAllowsUnprobedWhenConfigured(t *testing.T) {
	cfg := testLanes()
	dec, err := Pick(cfg, Request{Tags: []string{"games"}}, nil, nil)
	if err != nil {
		t.Fatalf("Pick error: %v", err)
	}
	if dec.Lane == nil || dec.Lane.ID != "lane-c" {
		t.Fatalf("expected lane-c (allow_unprobed), got %+v", dec.Lane)
	}
}

func TestPickRejectsMissingWindow(t *testing.T) {
	cfg := &Config{Lanes: []Lane{
		{
			ID:          "two-window",
			Order:       1,
			Provider:    "Provider A",
			Windows:     []string{"5h", "7d"},
			DataClasses: []DataClass{DataClassPersonal},
			Runner:      RunnerClaude,
			Auth:        AuthSubscription,
			Models:      map[string]string{RoleExecutor: "m"},
		},
	}}
	caps := []burndown.Capacity{fiveHour("Provider A", 90)}
	dec, err := Pick(cfg, Request{Tags: []string{"personal"}}, caps, nil)
	if !errors.Is(err, ErrNoLane) {
		t.Fatalf("expected ErrNoLane, got %v", err)
	}
	if len(dec.Rejected) != 1 || !strings.Contains(dec.Rejected[0].Reason, "missing window 7d") {
		t.Errorf("Rejected = %+v", dec.Rejected)
	}
}

func TestPickRejectsHeadroomBelowMin(t *testing.T) {
	cfg := &Config{Lanes: []Lane{
		{
			ID:          "tight",
			Order:       1,
			Provider:    "Provider A",
			Windows:     []string{"5h"},
			DataClasses: []DataClass{DataClassPersonal},
			Runner:      RunnerClaude,
			Auth:        AuthSubscription,
			Models:      map[string]string{RoleExecutor: "m"},
		},
	}}
	caps := []burndown.Capacity{fiveHour("Provider A", 2)} // below default min of 5
	dec, err := Pick(cfg, Request{Tags: []string{"personal"}}, caps, nil)
	if !errors.Is(err, ErrNoLane) {
		t.Fatalf("expected ErrNoLane, got %v", err)
	}
	if len(dec.Rejected) != 1 || !strings.Contains(dec.Rejected[0].Reason, "headroom") {
		t.Errorf("Rejected = %+v", dec.Rejected)
	}
}

func TestPickNoLaneEligible(t *testing.T) {
	cfg := &Config{}
	dec, err := Pick(cfg, Request{Tags: []string{"personal"}}, nil, nil)
	if !errors.Is(err, ErrNoLane) {
		t.Fatalf("expected ErrNoLane, got %v", err)
	}
	if dec.Lane != nil {
		t.Errorf("expected nil Lane, got %+v", dec.Lane)
	}
}

func TestPickPropagatesClassifyTagsError(t *testing.T) {
	cfg := testLanes()
	_, err := Pick(cfg, Request{Tags: []string{"personal", "butterstack"}}, nil, nil)
	if err == nil {
		t.Fatal("expected an error for conflicting tags")
	}
	if errors.Is(err, ErrNoLane) {
		t.Error("expected the ClassifyTags error, not ErrNoLane")
	}
}

func TestPickDefaultRoleIsExecutor(t *testing.T) {
	cfg := testLanes()
	caps := []burndown.Capacity{fiveHour("Provider A", 90)}
	dec, err := Pick(cfg, Request{Tags: []string{"personal"}}, caps, nil)
	if err != nil {
		t.Fatalf("Pick error: %v", err)
	}
	if dec.Model != "model-a-exec" {
		t.Errorf("Model = %q, want model-a-exec (default role executor)", dec.Model)
	}
}

func TestPickNearExhaustion(t *testing.T) {
	cfg := testLanes()
	caps := []burndown.Capacity{fiveHour("Provider A", 6)} // above min headroom 5, but that means UsedPct=94
	dec, err := Pick(cfg, Request{Tags: []string{"personal"}}, caps, nil)
	if err != nil {
		t.Fatalf("Pick error: %v", err)
	}
	if !dec.NearExhaustion {
		t.Error("expected NearExhaustion = true at 94% used (>= default 90% handoff threshold)")
	}
}
