package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ryanlitalien/aida/internal/arbiter"
	"github.com/ryanlitalien/aida/internal/burndown"
	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/models"
)

// arbiter_status.go adds `aida arbiter status`: a read-only view over
// what `aida serve --arbiter` (serve_arbiter.go) is doing right now --
// its own state.json, whether ShouldRun is true against a live (or
// --capacity-json canned) snapshot, which lanes are currently marked
// empty, and the last wave it ran. Nothing here runs or mutates anything;
// it only reads the same state files the scheduler writes and the
// dashboard's GET /api/arbiter also reads.

// arbiterStatusView is the full status payload, shared by the table
// renderer and --json.
type arbiterStatusView struct {
	Scheduler       schedulerState       `json:"scheduler"`
	ShouldRunNow    bool                 `json:"should_run_now"`
	ShouldRunReason string               `json:"should_run_reason"`
	LaneMarks       []laneMarkView       `json:"lane_marks"`
	LastWave        *arbiter.WaveSummary `json:"last_wave,omitempty"`
	LastWaveSpoken  string               `json:"last_wave_spoken,omitempty"`
}

func newArbiterStatusCmd() *cobra.Command {
	var jsonOut bool
	var capacityJSONPath string

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the overnight scheduler's current state, next wake, and last wave",
		Long: "Reads `aida serve --arbiter`'s two state files (scheduler.json,\n" +
			"last-wave.json) and its lane marks under ~/.aida/arbiter/, and\n" +
			"separately evaluates arbiter.ShouldRun against a live capacity\n" +
			"snapshot (or --capacity-json, the same offline/test path `aida\n" +
			"arbiter plan` uses) so \"should it be running right now\" is\n" +
			"answerable even if the scheduler process itself is down. This\n" +
			"command never starts, stops, or mutates anything.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.LoadConfig()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			stateDir := filepath.Join(config.Dir(), "arbiter")

			sched, err := readSchedulerState(stateDir)
			if err != nil {
				return fmt.Errorf("read scheduler state: %w", err)
			}

			bcfg, err := burndown.Load(cfg.BurndownPath())
			if err != nil {
				return fmt.Errorf("loading burndown config: %w", err)
			}

			var caps []burndown.Capacity
			if capacityJSONPath != "" {
				caps, err = loadCapacityJSON(capacityJSONPath)
				if err != nil {
					return fmt.Errorf("load --capacity-json %q: %w", capacityJSONPath, err)
				}
			} else {
				r, err := models.Load(cfg.ModelsPath())
				if err != nil {
					return fmt.Errorf("loading models roster: %w", err)
				}
				ctx, cancel := context.WithTimeout(cmd.Context(), modelsProbeTimeout)
				defer cancel()
				usages := models.ProbeWithOptions(ctx, r, models.ProbeOptions{})
				caps = burndown.Report(r.Providers, usages, bcfg, time.Now())
			}

			schedCfg := arbiter.DefaultScheduleConfig(bcfg.Overnight)
			should, reason := arbiter.ShouldRun(schedCfg, caps, time.Now())

			marks, err := readLaneMarks(stateDir)
			if err != nil {
				return fmt.Errorf("read lane marks: %w", err)
			}

			wave, haveWave, err := readWaveSummary(stateDir)
			if err != nil {
				return fmt.Errorf("read last wave: %w", err)
			}

			view := arbiterStatusView{
				Scheduler:       sched,
				ShouldRunNow:    should,
				ShouldRunReason: reason,
				LaneMarks:       marks,
			}
			if haveWave {
				view.LastWave = &wave
				view.LastWaveSpoken = arbiter.SpeakSummary(wave)
			}

			if jsonOut {
				data, err := json.MarshalIndent(view, "", "  ")
				if err != nil {
					return fmt.Errorf("marshaling status: %w", err)
				}
				fmt.Println(string(data))
				return nil
			}
			fmt.Print(renderArbiterStatus(view))
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "dump the full status as JSON")
	cmd.Flags().StringVar(&capacityJSONPath, "capacity-json", "", "read a JSON array of burndown.Capacity from a file instead of probing (offline/test path, like `aida arbiter plan`)")
	return cmd
}

// renderArbiterStatus is the pure render step behind `aida arbiter
// status`'s plain-text output, factored out so it's unit-testable
// against a canned arbiterStatusView with no config, no probe, and no
// state directory on disk.
func renderArbiterStatus(v arbiterStatusView) string {
	var b strings.Builder

	state := v.Scheduler.State
	if state == "" {
		state = "unknown (no scheduler.json yet - has `aida serve --arbiter` ever run?)"
	}
	fmt.Fprintf(&b, "scheduler: %s\n", state)
	if !v.Scheduler.NextWake.IsZero() {
		fmt.Fprintf(&b, "next wake: %s\n", v.Scheduler.NextWake.Format(time.RFC3339))
	}
	if v.Scheduler.Reason != "" {
		fmt.Fprintf(&b, "reason: %s\n", v.Scheduler.Reason)
	}
	fmt.Fprintf(&b, "should run now: %v (%s)\n", v.ShouldRunNow, v.ShouldRunReason)

	b.WriteString("\nlane marks:\n")
	if len(v.LaneMarks) == 0 {
		b.WriteString("  none\n")
	} else {
		for _, m := range v.LaneMarks {
			fmt.Fprintf(&b, "  %s: empty until %s (source %s, ambiguous %v)\n",
				m.Lane, m.EmptyUntil.Format(time.RFC3339), m.Source, m.Ambiguous)
		}
	}

	b.WriteString("\nlast wave: ")
	if v.LastWave == nil {
		b.WriteString("none yet\n")
		return b.String()
	}
	fmt.Fprintf(&b, "%s\n", v.LastWaveSpoken)
	fmt.Fprintf(&b, "  tasks=%d passed=%d held=%d no-lane=%d\n",
		v.LastWave.Tasks, v.LastWave.Passed, v.LastWave.Held, v.LastWave.NoLane)
	var lanes []string
	for l := range v.LastWave.ByLane {
		lanes = append(lanes, l)
	}
	sort.Strings(lanes)
	for _, l := range lanes {
		fmt.Fprintf(&b, "  lane %s: %d attempt(s)\n", l, v.LastWave.ByLane[l])
	}
	return b.String()
}
