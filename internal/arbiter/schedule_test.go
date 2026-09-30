package arbiter

import (
	"strings"
	"testing"
	"time"

	"github.com/ryanlitalien/aida/internal/burndown"
)

func testOvernight() burndown.OvernightConfig {
	return burndown.OvernightConfig{Start: "23:00", End: "08:00", Timezone: "UTC"}
}

func TestShouldRun(t *testing.T) {
	cfg := DefaultScheduleConfig(testOvernight())
	base := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC) // noon, outside overnight

	cases := []struct {
		name    string
		now     time.Time
		caps    []burndown.Capacity
		want    bool
		wantSub string
	}{
		{
			name: "inside overnight window",
			now:  time.Date(2026, 9, 25, 23, 30, 0, 0, time.UTC),
			want: true, wantSub: "overnight window",
		},
		{
			name: "weekly reset in 20h",
			now:  base,
			caps: []burndown.Capacity{{Provider: "P", Label: "7-day", ResetsAt: base.Add(20 * time.Hour)}},
			want: true, wantSub: "weekly drain",
		},
		{
			name: "weekly reset in 30h (outside drain)",
			now:  base,
			caps: []burndown.Capacity{{Provider: "P", Label: "7-day", ResetsAt: base.Add(30 * time.Hour)}},
			want: false, wantSub: "outside the overnight window",
		},
		{
			name: "spend rows ignored even with a near reset",
			now:  base,
			caps: []burndown.Capacity{{Provider: "P", Label: "7-day", IsSpend: true, ResetsAt: base.Add(1 * time.Hour)}},
			want: false,
		},
		{
			name: "5h reset ignored (not a weekly window)",
			now:  base,
			caps: []burndown.Capacity{{Provider: "P", Label: "5-hour", ResetsAt: base.Add(1 * time.Hour)}},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := ShouldRun(cfg, tc.caps, tc.now)
			if got != tc.want {
				t.Fatalf("ShouldRun() = (%v, %q), want %v", got, reason, tc.want)
			}
			if tc.wantSub != "" && !strings.Contains(reason, tc.wantSub) {
				t.Errorf("reason = %q, want it to contain %q", reason, tc.wantSub)
			}
		})
	}
}

func TestNextWake(t *testing.T) {
	cfg := DefaultScheduleConfig(testOvernight())

	t.Run("evening before 23:00 picks tonight's nightly", func(t *testing.T) {
		now := time.Date(2026, 9, 25, 20, 0, 0, 0, time.UTC)
		got, reason := NextWake(cfg, nil, now)
		want := time.Date(2026, 9, 25, 23, 0, 0, 0, time.UTC)
		if !got.Equal(want) {
			t.Errorf("NextWake = %v, want %v", got, want)
		}
		if !strings.Contains(reason, "nightly") {
			t.Errorf("reason = %q, want it to mention nightly", reason)
		}
	})

	t.Run("after midnight before today's 23:00 still picks today", func(t *testing.T) {
		now := time.Date(2026, 9, 26, 1, 0, 0, 0, time.UTC)
		got, _ := NextWake(cfg, nil, now)
		want := time.Date(2026, 9, 26, 23, 0, 0, 0, time.UTC)
		if !got.Equal(want) {
			t.Errorf("NextWake = %v, want %v", got, want)
		}
	})

	t.Run("just past nightly rolls to tomorrow", func(t *testing.T) {
		now := time.Date(2026, 9, 25, 23, 30, 0, 0, time.UTC)
		got, _ := NextWake(cfg, nil, now)
		want := time.Date(2026, 9, 26, 23, 0, 0, 0, time.UTC)
		if !got.Equal(want) {
			t.Errorf("NextWake = %v, want %v", got, want)
		}
	})

	t.Run("7d reset minus weekly-drain lands before tonight's nightly", func(t *testing.T) {
		now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
		// resets at 12:00 tomorrow -> drain trigger at 12:00 today, well
		// before tonight's 23:00 nightly wake.
		resetsAt := now.Add(24 * time.Hour)
		caps := []burndown.Capacity{{Provider: "P", Label: "7-day", ResetsAt: resetsAt}}
		got, reason := NextWake(cfg, caps, now.Add(-time.Hour)) // now is 11:00, drain opens at 12:00 today (still ahead)
		want := now
		if !got.Equal(want) {
			t.Errorf("NextWake = %v, want %v (%s)", got, want, reason)
		}
		if !strings.Contains(reason, "weekly drain") {
			t.Errorf("reason = %q, want it to mention weekly drain", reason)
		}
	})

	t.Run("a 5h reset in 2h wins over a distant nightly", func(t *testing.T) {
		now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC) // nightly is 11h away
		caps := []burndown.Capacity{{Provider: "P", Label: "5-hour", ResetsAt: now.Add(2 * time.Hour)}}
		got, reason := NextWake(cfg, caps, now)
		want := now.Add(2 * time.Hour)
		if !got.Equal(want) {
			t.Errorf("NextWake = %v, want %v", got, want)
		}
		if !strings.Contains(reason, "resets at") {
			t.Errorf("reason = %q, want it to mention the reset", reason)
		}
	})

	t.Run("degenerate config with no nightly and no caps still returns something ahead of now", func(t *testing.T) {
		badCfg := cfg
		badCfg.NightlyAt = "not-a-time"
		now := time.Now()
		got, reason := NextWake(badCfg, nil, now)
		if !got.After(now) {
			t.Errorf("NextWake = %v, want it after now (%v)", got, now)
		}
		if reason == "" {
			t.Error("expected a non-empty reason even in the degenerate case")
		}
	})
}

func TestBuildWave(t *testing.T) {
	tasks := []WaveTask{
		{Slug: "a", Verifier: "check: make test"},
		{Slug: "b", Verifier: ""},
		{Slug: "c", Verifier: "deliverables: 2"},
	}

	t.Run("requireVerifier drops unverified tasks", func(t *testing.T) {
		wave, skipped := BuildWave(tasks, true)
		if len(wave) != 2 || wave[0].Slug != "a" || wave[1].Slug != "c" {
			t.Fatalf("wave = %+v, want [a, c]", wave)
		}
		if len(skipped) != 1 || skipped[0].Slug != "b" {
			t.Fatalf("skipped = %+v, want [b]", skipped)
		}
		if !strings.Contains(skipped[0].Reason, "no machine verifier") {
			t.Errorf("skip reason = %q, want it to explain the missing verifier", skipped[0].Reason)
		}
	})

	t.Run("requireVerifier false keeps everything", func(t *testing.T) {
		wave, skipped := BuildWave(tasks, false)
		if len(wave) != 3 {
			t.Fatalf("wave = %+v, want all 3 tasks", wave)
		}
		if len(skipped) != 0 {
			t.Errorf("skipped = %+v, want none", skipped)
		}
	})
}

func mkEntry(at time.Time, slug, lane, verdict string, before, after []WindowSnapshot) Entry {
	return Entry{At: at, TaskSlug: slug, Lane: lane, Verdict: verdict, Before: before, After: after}
}

func TestSummarizeWaveAndSpeakSummary(t *testing.T) {
	start := time.Date(2026, 9, 25, 23, 0, 0, 0, time.UTC)
	end := start.Add(8 * time.Hour)

	entries := []Entry{
		mkEntry(start.Add(1*time.Minute), "task-a", "claude-max", "fail",
			[]WindowSnapshot{{Provider: "Anthropic / Claude", Label: "5-hour", UsedPct: 12}}, nil),
		mkEntry(start.Add(10*time.Minute), "task-a", "claude-max", "pass",
			nil, []WindowSnapshot{{Provider: "Anthropic / Claude", Label: "5-hour", UsedPct: 71}}),
		mkEntry(start.Add(15*time.Minute), "task-b", "claude-max", "fail", nil, nil),
		mkEntry(start.Add(20*time.Minute), "task-c", "claude-max", "no-lane", nil, nil),
		// Outside the span: must not be counted.
		mkEntry(end.Add(time.Hour), "task-d", "codex", "pass", nil, nil),
	}

	s := SummarizeWave(entries, start, end)
	if s.Tasks != 3 {
		t.Fatalf("Tasks = %d, want 3", s.Tasks)
	}
	if s.Passed != 1 {
		t.Errorf("Passed = %d, want 1", s.Passed)
	}
	if s.Held != 1 {
		t.Errorf("Held = %d, want 1 (task-b's last verdict is fail)", s.Held)
	}
	if s.NoLane != 1 {
		t.Errorf("NoLane = %d, want 1", s.NoLane)
	}
	if s.ByLane["claude-max"] != 4 {
		t.Errorf("ByLane[claude-max] = %d, want 4 (every entry in span)", s.ByLane["claude-max"])
	}
	if s.ByVerdict["fail"] != 2 || s.ByVerdict["pass"] != 1 || s.ByVerdict["no-lane"] != 1 {
		t.Errorf("ByVerdict = %+v, unexpected", s.ByVerdict)
	}
	if len(s.Windows) != 1 {
		t.Fatalf("Windows = %+v, want exactly 1", s.Windows)
	}
	w := s.Windows[0]
	if w.Provider != "Anthropic / Claude" || w.Label != "5-hour" || w.UsedBefore != 12 || w.UsedAfter != 71 {
		t.Errorf("Windows[0] = %+v, want provider=Anthropic / Claude label=5-hour before=12 after=71", w)
	}

	speech := SpeakSummary(s)
	for _, want := range []string{"3 tasks", "1 passed", "1 on hold", "1 waiting on a lane", "Anthropic / Claude 5-hour went from 12 to 71 percent used"} {
		if !strings.Contains(speech, want) {
			t.Errorf("SpeakSummary() = %q, want it to contain %q", speech, want)
		}
	}
}

func TestSummarizeWaveEmpty(t *testing.T) {
	start := time.Date(2026, 9, 25, 23, 0, 0, 0, time.UTC)
	end := start.Add(8 * time.Hour)
	s := SummarizeWave(nil, start, end)
	if s.Tasks != 0 {
		t.Fatalf("Tasks = %d, want 0", s.Tasks)
	}
	if s.Note == "" {
		t.Error("expected a Note explaining the empty wave")
	}
	speech := SpeakSummary(s)
	if !strings.HasPrefix(speech, "No overnight wave ran:") {
		t.Errorf("SpeakSummary() = %q, want the empty-wave form", speech)
	}
}
