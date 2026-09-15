package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ryanlitalien/aida/internal/brain"
	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/jarvis/audit"
	"github.com/ryanlitalien/aida/internal/ui"
)

// newJarvisThumbsUpCmd records positive feedback on recent Jarvis VOICE turns
// into the jarvis_lessons table - the same store the voice tool jarvis_thumbs_up
// writes, but targetable from the CLI for the last N turns. This is distinct
// from the engine-level `aida thumbs-up <run-id>`, which rates `aida <query>`
// engine runs; pure voice turns (inline replies, weather, task tools) carry no
// engine run id and belong here.
func newJarvisThumbsUpCmd() *cobra.Command {
	var last int
	var because string
	cmd := &cobra.Command{
		Use:   "thumbs-up",
		Short: "Record positive feedback on the last N Jarvis voice turns (jarvis_lessons)",
		Long: "Marks the most recent substantive Jarvis voice turns (from the audit\n" +
			"log) as thumbs-up in the jarvis_lessons table, so similar future voice\n" +
			"queries recall them as positive examples. This rates VOICE turns; use\n" +
			"`aida thumbs-up <run-id>` to rate an `aida <query>` engine run instead.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if last < 1 {
				last = 1
			}
			cfg, err := config.LoadConfig()
			if err != nil {
				return err
			}
			_, profileName := cfg.ActiveProfileConfig()
			b, err := brain.Open(cfg.BrainPath(), profileName, cfg.VoyageKeyEnv(), cfg.Brain.GitHubRepo())
			if err != nil {
				return err
			}
			defer b.Close()

			auditPath := filepath.Join(cfg.BrainPath(), "jarvis", "audit.ndjson")
			recs, err := readSubstantiveAuditTail(auditPath, last)
			if err != nil {
				return err
			}
			if len(recs) == 0 {
				fmt.Println("No substantive Jarvis turns found to rate.")
				return nil
			}

			ctx := context.Background()
			for _, rec := range recs {
				toolCalls := make([]brain.JarvisToolCall, len(rec.ToolCalls))
				for i, c := range rec.ToolCalls {
					toolCalls[i] = brain.JarvisToolCall{Name: c.Name, TookMs: c.TookMs, EngineRunID: c.EngineRunID}
				}
				lesson := &brain.JarvisLesson{
					Timestamp:          time.Now().UTC().Format(time.RFC3339),
					RatedTurnStartedAt: rec.StartedAt,
					Transcript:         rec.Transcript,
					Query:              rec.Query,
					Reply:              rec.Reply,
					ToolCalls:          toolCalls,
					Feedback:           "up",
					FeedbackReason:     because,
				}
				id, rerr := b.RecordJarvisLesson(ctx, lesson)
				if rerr != nil {
					return fmt.Errorf("record thumbs-up for %q: %w", rec.Query, rerr)
				}
				q := rec.Query
				if len(q) > 60 {
					q = q[:57] + "..."
				}
				fmt.Printf("%s thumbs-up: %s  (%s)\n", ui.SuccessIcon, q, id)
			}
			noun := "turns"
			if len(recs) == 1 {
				noun = "turn"
			}
			fmt.Printf("Recorded %d thumbs-up %s.\n", len(recs), noun)
			return nil
		},
	}
	cmd.Flags().IntVar(&last, "last", 1, "number of most-recent substantive voice turns to rate")
	cmd.Flags().StringVar(&because, "because", "", "optional reason applied to each rated turn")
	return cmd
}

// readSubstantiveAuditTail reads the Jarvis audit NDJSON log and returns the
// last n SUBSTANTIVE turns (non-empty reply, no error, not wake-only),
// oldest-first. A missing log is not an error (returns nil).
func readSubstantiveAuditTail(path string, n int) ([]audit.Record, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var subs []audit.Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1024*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r audit.Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			continue
		}
		if r.Error != "" || r.WakeOnly || strings.TrimSpace(r.Reply) == "" {
			continue
		}
		subs = append(subs, r)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(subs) > n {
		subs = subs[len(subs)-n:]
	}
	return subs, nil
}
