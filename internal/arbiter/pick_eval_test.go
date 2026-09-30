package arbiter

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ryanlitalien/aida/internal/burndown"
	"gopkg.in/yaml.v3"
)

// evalCapacity is one testdata/lane_eval.yaml capacity row -- a plain,
// already-computed burndown.Capacity (the eval suite tests Pick's lane
// selection, not burndown's floor math, so headroom is given directly
// rather than derived from used/floor).
type evalCapacity struct {
	Provider string  `yaml:"provider"`
	Label    string  `yaml:"label"`
	UsedPct  float64 `yaml:"used_pct"`
	LeftPct  float64 `yaml:"left_pct"`
	Floor    float64 `yaml:"floor"`
	Headroom float64 `yaml:"headroom"`
	IsSpend  bool    `yaml:"is_spend"`
	ResetsAt string  `yaml:"resets_at"`
}

func (c evalCapacity) toCapacity(t *testing.T) burndown.Capacity {
	t.Helper()
	out := burndown.Capacity{
		Provider: c.Provider,
		Label:    c.Label,
		UsedPct:  c.UsedPct,
		LeftPct:  c.LeftPct,
		Floor:    c.Floor,
		Headroom: c.Headroom,
		IsSpend:  c.IsSpend,
	}
	if c.ResetsAt != "" {
		ts, err := time.Parse(time.RFC3339, c.ResetsAt)
		if err != nil {
			t.Fatalf("parsing resets_at %q: %v", c.ResetsAt, err)
		}
		out.ResetsAt = ts
	}
	return out
}

// evalCase is one testdata/lane_eval.yaml routing scenario.
type evalCase struct {
	Name                   string            `yaml:"name"`
	Tags                   []string          `yaml:"tags"`
	Role                   string            `yaml:"role"`
	Now                    string            `yaml:"now"`
	Signals                map[string]string `yaml:"signals"`
	Capacity               []evalCapacity    `yaml:"capacity"`
	ExpectLane             string            `yaml:"expect_lane"`
	ExpectModel            string            `yaml:"expect_model"`
	ExpectNoLane           bool              `yaml:"expect_no_lane"`
	ExpectRejectedContains []string          `yaml:"expect_rejected_contains"`
}

type evalFixture struct {
	Cases []evalCase `yaml:"cases"`
}

func loadEvalFixture(t *testing.T) evalFixture {
	t.Helper()
	data, err := os.ReadFile("testdata/lane_eval.yaml")
	if err != nil {
		t.Fatalf("reading testdata/lane_eval.yaml: %v", err)
	}
	var fx evalFixture
	if err := yaml.Unmarshal(data, &fx); err != nil {
		t.Fatalf("parsing testdata/lane_eval.yaml: %v", err)
	}
	if len(fx.Cases) == 0 {
		t.Fatal("testdata/lane_eval.yaml has no cases")
	}
	return fx
}

// formatRejected renders a Decision's rejection list as "lane: reason"
// lines, both for test-failure output and for the ExpectRejectedContains
// substring checks below.
func formatRejected(rejected []Rejection) string {
	lines := make([]string, len(rejected))
	for i, r := range rejected {
		lines[i] = fmt.Sprintf("%s: %s", r.Lane, r.Reason)
	}
	return strings.Join(lines, "\n")
}

// TestPickEval is the routing eval loop docs/arbiter-plan.md's build
// order calls for: every case in testdata/lane_eval.yaml runs against
// DefaultConfig(), so adding a routing scenario is a one-line fixture
// addition, not a new Go test function. Cases are seeded from
// docs/research/model-usage-burndown/routing-examples.txt and lanes.yaml
// -- see the fixture file's own comments for which worked example each
// one traces back to.
func TestPickEval(t *testing.T) {
	cfg := DefaultConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("DefaultConfig().Validate(): %v", err)
	}
	fx := loadEvalFixture(t)

	for _, tc := range fx.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			now, err := time.Parse(time.RFC3339, tc.Now)
			if err != nil {
				t.Fatalf("parsing now %q: %v", tc.Now, err)
			}

			var caps []burndown.Capacity
			for _, c := range tc.Capacity {
				caps = append(caps, c.toCapacity(t))
			}

			sig := make(mapSignalReader, len(tc.Signals))
			for lane, ts := range tc.Signals {
				parsed, err := time.Parse(time.RFC3339, ts)
				if err != nil {
					t.Fatalf("parsing signal time %q for lane %q: %v", ts, lane, err)
				}
				sig[lane] = parsed
			}

			req := Request{Tags: tc.Tags, Role: tc.Role, Now: now}
			dec, err := Pick(cfg, req, caps, sig)

			if tc.ExpectNoLane {
				if !errors.Is(err, ErrNoLane) {
					t.Fatalf("expected ErrNoLane, got err=%v, decision=%+v\nrejected:\n%s", err, dec, formatRejected(dec.Rejected))
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v\nrejected:\n%s", err, formatRejected(dec.Rejected))
				}
				if dec.Lane == nil {
					t.Fatalf("expected a lane, got nil\nrejected:\n%s", formatRejected(dec.Rejected))
				}
				if dec.Lane.ID != tc.ExpectLane {
					t.Errorf("lane = %q, want %q\nrejected:\n%s", dec.Lane.ID, tc.ExpectLane, formatRejected(dec.Rejected))
				}
				if dec.Model != tc.ExpectModel {
					t.Errorf("model = %q, want %q", dec.Model, tc.ExpectModel)
				}
			}

			rejectedText := formatRejected(dec.Rejected)
			for _, substr := range tc.ExpectRejectedContains {
				if !strings.Contains(rejectedText, substr) {
					t.Errorf("rejected list does not contain %q\nrejected:\n%s", substr, rejectedText)
				}
			}
		})
	}
}
