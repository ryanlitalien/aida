package cli

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ryanlitalien/aida/internal/brain"
	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/jobs"
	"github.com/spf13/cobra"
)

//go:embed tasks_web.html
var tasksWebHTML []byte

//go:embed runs_web.html
var runsWebHTML []byte

func newTasksWebCmd() *cobra.Command {
	var allProfiles bool
	var standalone bool
	cmd := &cobra.Command{
		Use:   "web",
		Short: "Open the tasks web UI in your default browser",
		Long: `Opens http://localhost:1610/tasks (where 'aida serve' mounts the tasks
UI) in your default browser. Convenient for forgetting which port the
daemon binds.

If 'aida serve' isn't running, the browser will fail to connect - start
'aida serve' in another terminal first.

Use --standalone to run a one-off, ephemeral server on a random port (the
old behavior), useful when you don't want a long-lived daemon.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if standalone {
				return runStandaloneTasksWeb(allProfiles)
			}
			url := fmt.Sprintf("http://localhost:%d/tasks", JarvisHTTPPort)
			fmt.Fprintln(os.Stderr, "Opening", url)
			openBrowser(url)
			fmt.Fprintln(os.Stderr, "(if the page fails to load, run `aida serve` in another terminal)")
			return nil
		},
	}
	cmd.Flags().BoolVar(&allProfiles, "all-profiles", false, "show tasks from every profile (bypasses isolation)")
	cmd.Flags().BoolVar(&standalone, "standalone", false, "run a one-off ephemeral tasks UI on a random port (old behavior)")
	return cmd
}

func runStandaloneTasksWeb(allProfiles bool) error {
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

	jobsStore, err := jobs.Open(profileName)
	if err != nil {
		return fmt.Errorf("open jobs store: %w", err)
	}
	defer jobsStore.Close()

	return runTasksWeb(b, jobsStore, profileName, allProfiles)
}

func runTasksWeb(b *brain.Brain, jobsStore *jobs.Store, profileName string, allProfiles bool) error {
	mux := http.NewServeMux()
	registerTasksWebRoutes(mux, b, jobsStore, profileName, allProfiles)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("binding loopback listener: %w", err)
	}
	addr := ln.Addr().(*net.TCPAddr)
	url := fmt.Sprintf("http://127.0.0.1:%d", addr.Port)

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	fmt.Fprintf(os.Stderr, "Aida tasks UI: %s  (Ctrl-C to stop)\n", url)
	openBrowser(url)

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)

	select {
	case <-sig:
		fmt.Fprintln(os.Stderr, "shutting down...")
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		return srv.Shutdown(ctx)
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// registerTasksWebRoutes registers all tasks-UI routes onto mux. The HTML
// is served at htmlPath; if htmlPath is "" it defaults to "/" (standalone
// behavior). API routes live at /api/* regardless, so the embedded HTML's
// fetch calls work whether the page is mounted at / or /tasks.
func registerTasksWebRoutes(mux *http.ServeMux, b *brain.Brain, jobsStore *jobs.Store, profileName string, allProfilesDefault bool) {
	registerTasksWebRoutesAt(mux, b, jobsStore, profileName, allProfilesDefault, "")
}

func registerTasksWebRoutesAt(mux *http.ServeMux, b *brain.Brain, jobsStore *jobs.Store, profileName string, allProfilesDefault bool, htmlPath string) {
	if htmlPath == "" {
		htmlPath = "/"
	}
	htmlHandler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(tasksWebHTML)
	}
	if htmlPath == "/" {
		mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/" {
				http.NotFound(w, r)
				return
			}
			htmlHandler(w, r)
		})
	} else {
		// Mount at both `/tasks` and `/tasks/` so either link works.
		mux.HandleFunc("GET "+htmlPath, htmlHandler)
		mux.HandleFunc("GET "+htmlPath+"/", htmlHandler)
	}

	// The runs page (background jobs/swarms/loops). Self-contained SPA that
	// consumes /api/runs + the existing per-run endpoints. Mounted at both
	// `/runs` and `/runs/` so either link works.
	runsHandler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(runsWebHTML)
	}
	mux.HandleFunc("GET /runs", runsHandler)
	mux.HandleFunc("GET /runs/", runsHandler)

	mux.HandleFunc("GET /api/tasks", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		allProfiles := allProfilesDefault || r.URL.Query().Get("all_profiles") == "1"

		var tasks []brain.TaskRecord
		var err error
		if allProfiles {
			tasks, err = b.ListTasksAllProfiles(true, nil, 0, 0)
		} else {
			tasks, err = b.ListTasks(true, nil, 0, 0)
		}
		if err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if tasks == nil {
			tasks = []brain.TaskRecord{}
		}

		// Compute per-task draft status (none|queued|running|ready|failed)
		// from the newest job per task slug. The newest-row rule means
		// retries shadow older jobs cleanly. allProfiles widens the
		// scan to every profile's queue; otherwise we use the active
		// profile's store opened at server startup.
		slugs := make([]string, 0, len(tasks))
		for _, t := range tasks {
			slugs = append(slugs, t.Slug)
		}
		draftStatus, err := computeDraftStatus(jobsStore, slugs, allProfiles)
		if err != nil {
			// Don't fail the whole list - just omit the badge.
			draftStatus = map[string]draftStatusEntry{}
		}

		resp := tasksWebListResponse{
			Tasks:         tasks,
			Statuses:      brain.AllStatuses(),
			ActiveProfile: profileName,
			AllProfiles:   allProfiles,
			DraftStatus:   draftStatus,
		}
		writeJSON(w, http.StatusOK, resp)
	})

	mux.HandleFunc("GET /api/brain/stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		stats := b.GetStats()
		resp := brainStatsResponse{
			Lessons:       stats.LessonCount,
			JarvisLessons: stats.JarvisLessonCount,
			Entities:      stats.EntityCount,
			TasksOpen:     stats.TasksOpen,
			TasksDone:     stats.TasksDone,
			TasksByStatus: stats.TasksByStatus,
			LastLesson:    stats.LastLesson,
			DBSizeBytes:   stats.DBSize,
			HasEmbeddings: stats.HasEmbeddings,
			IsStale:       stats.IsStale,
			Profile:       profileName,
		}
		writeJSON(w, http.StatusOK, resp)
	})

	mux.HandleFunc("GET /api/tasks/{slug}/body", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		slug := r.PathValue("slug")
		task, err := b.ResolveTaskRef(slug)
		if err != nil {
			httpError(w, http.StatusNotFound, err.Error())
			return
		}
		data, readErr := os.ReadFile(b.TaskFilePath(task.Slug))
		if readErr != nil {
			httpError(w, http.StatusInternalServerError, "reading task: "+readErr.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{
			"slug": task.Slug,
			"body": string(data),
		})
	})

	mux.HandleFunc("POST /api/tasks/{slug}/status", func(w http.ResponseWriter, r *http.Request) {
		slug := r.PathValue("slug")
		if slug == "" {
			httpError(w, http.StatusBadRequest, "missing slug")
			return
		}

		var body struct {
			Status string `json:"status"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			httpError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if !brain.IsValidStatus(body.Status) {
			httpError(w, http.StatusBadRequest, fmt.Sprintf("invalid status %q", body.Status))
			return
		}

		task, err := b.SetTaskStatus(slug, body.Status)
		if err != nil {
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, task)
	})

	mux.HandleFunc("POST /api/tasks/{slug}/draft", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		slug := r.PathValue("slug")
		if slug == "" {
			httpError(w, http.StatusBadRequest, "missing slug")
			return
		}
		runID, err := enqueueDraftJob(b, jobsStore, profileName, slug)
		if err != nil {
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}
		w.WriteHeader(http.StatusAccepted)
		writeJSON(w, http.StatusAccepted, map[string]string{"run_id": runID})
	})

	// GET /api/runs - list background runs for the runs web page. Mirrors the
	// voice-side job_list tool: default (no state, or state=active) returns the
	// active set (queued + running + awaiting_input + awaiting_approval);
	// state=all returns everything; otherwise filter by a single valid state.
	// Each row carries its spoken handle so the page matches what Jarvis says.
	// One endpoint covers jobs, swarms, and loop runs - kind distinguishes them.
	mux.HandleFunc("GET /api/runs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if jobsStore == nil {
			writeJSON(w, http.StatusOK, runListResponse{Runs: []runListItem{}, ActiveProfile: profileName})
			return
		}
		limit := 100
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				limit = n
			}
		}
		state := strings.TrimSpace(r.URL.Query().Get("state"))

		var rows []jobs.Job
		var err error
		switch state {
		case "", "active":
			// SQL can't OR states through ListOpts; query each and concat.
			for _, s := range []string{jobs.StateQueued, jobs.StateRunning, jobs.StateAwaitingInput, jobs.StateAwaitingApproval} {
				rs, lerr := jobsStore.List(jobs.ListOpts{State: s, Limit: limit})
				if lerr != nil {
					err = lerr
					break
				}
				rows = append(rows, rs...)
			}
		case "all":
			rows, err = jobsStore.List(jobs.ListOpts{Limit: limit})
		default:
			if !jobs.IsValidState(state) {
				httpError(w, http.StatusBadRequest, fmt.Sprintf("invalid state %q", state))
				return
			}
			rows, err = jobsStore.List(jobs.ListOpts{State: state, Limit: limit})
		}
		if err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}

		// Newest-first across the concatenated active set, then re-apply
		// the limit: the active path queries four states with `limit`
		// each, so the concat can hold up to 4x what the caller asked for.
		sort.Slice(rows, func(i, j int) bool { return rows[i].EnqueuedAt.After(rows[j].EnqueuedAt) })
		if len(rows) > limit {
			rows = rows[:limit]
		}

		items := make([]runListItem, 0, len(rows))
		for _, j := range rows {
			effState, _, _ := jobs.EffectiveState(&j)
			items = append(items, runListItem{
				Job:            j,
				Handle:         jobs.DeriveHandle(j.RunID),
				EffectiveState: effState,
				Outcome:        jobs.DescribeOutcome(&j),
			})
		}
		writeJSON(w, http.StatusOK, runListResponse{Runs: items, ActiveProfile: profileName})
	})

	mux.HandleFunc("GET /api/runs/{run_id}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		runID := r.PathValue("run_id")
		j, err := jobsStore.Get(runID)
		if err != nil {
			httpError(w, http.StatusNotFound, err.Error())
			return
		}
		// Tail of recent events so the UI has something to show
		// when the user drills in. Cheap - events files stay small.
		events := readEventsTailLines(profileName, runID, 50)

		// Surface destination + artifact_url from the manifest so the
		// modal can render an "Open PR" link / Slack-friendly Copy
		// without a second round-trip (issue #58). Manifest read
		// errors are non-fatal - the modal falls back to its universal
		// Copy/Save controls when these are absent.
		effState, _, _ := jobs.EffectiveState(j)
		resp := map[string]any{
			"job":             j,
			"events":          events,
			"effective_state": effState,
			"outcome":         jobs.DescribeOutcome(j),
		}
		if m, mErr := jobs.ReadManifest(profileName, runID); mErr == nil {
			if m.Destination != nil {
				resp["destination"] = m.Destination
			}
			if m.ArtifactURL != "" {
				resp["artifact_url"] = m.ArtifactURL
			}
		}
		writeJSON(w, http.StatusOK, resp)
	})

	mux.HandleFunc("GET /api/runs/{run_id}/output", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		runID := r.PathValue("run_id")
		path := jobs.OutputPath(profileName, runID)
		data, err := os.ReadFile(path)
		if err != nil {
			httpError(w, http.StatusNotFound, "no output yet (job still running or failed before writing)")
			return
		}
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		_, _ = w.Write(data)
	})

	mux.HandleFunc("GET /api/runs/{run_id}/stream", func(w http.ResponseWriter, r *http.Request) {
		runID := r.PathValue("run_id")
		streamRunEvents(w, r, profileName, runID, jobsStore)
	})

	mux.HandleFunc("POST /api/runs/{run_id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		runID := r.PathValue("run_id")
		if err := cancelJob(jobsStore, profileName, runID); err != nil {
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
	})

	// POST /api/runs/{run_id}/input - deliver a user reply to a job
	// in state=awaiting_input. The body may be raw text or a JSON
	// object {"text": "..."}; the payload is written atomically to
	// <run-dir>/input.txt. The agent's polling ask_user tool picks
	// it up on its next 2s tick.
	//
	// 202 Accepted: the write succeeded; the canonical confirmation
	// that the agent has consumed the reply is the state transition
	// awaiting_input → running, observable via /api/runs/{id}.
	mux.HandleFunc("POST /api/runs/{run_id}/input", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		runID := r.PathValue("run_id")
		if err := deliverRunInput(jobsStore, profileName, runID, r); err != nil {
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "delivered"})
	})

	mux.HandleFunc("POST /api/tasks/{slug}/priority", func(w http.ResponseWriter, r *http.Request) {
		slug := r.PathValue("slug")
		if slug == "" {
			httpError(w, http.StatusBadRequest, "missing slug")
			return
		}

		var body struct {
			Priority string `json:"priority"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			httpError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		switch body.Priority {
		case "", "p1", "p2", "p3":
		default:
			httpError(w, http.StatusBadRequest, fmt.Sprintf("invalid priority %q (want p1, p2, p3, or empty)", body.Priority))
			return
		}

		current, err := b.ResolveTaskRef(slug)
		if err != nil {
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}
		newTags := make([]string, 0, len(current.Tags)+1)
		for _, t := range current.Tags {
			if t == "p1" || t == "p2" || t == "p3" {
				continue
			}
			newTags = append(newTags, t)
		}
		if body.Priority != "" {
			newTags = append(newTags, body.Priority)
		}
		updated, err := b.UpdateTask(slug, brain.TaskPatch{Tags: &newTags})
		if err != nil {
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, updated)
	})
}

type tasksWebListResponse struct {
	Tasks         []brain.TaskRecord          `json:"tasks"`
	Statuses      []string                    `json:"statuses"`
	ActiveProfile string                      `json:"active_profile"`
	AllProfiles   bool                        `json:"all_profiles"`
	DraftStatus   map[string]draftStatusEntry `json:"draft_status,omitempty"`
}

// runListItem is one row in the /api/runs response: the full job plus its
// derived spoken handle (so the page can show "amber-otter" matching what
// Jarvis says). Embedding promotes all jobs.Job fields inline.
//
// EffectiveState and Outcome are computed via jobs.EffectiveState /
// jobs.DescribeOutcome rather than left for a page to re-derive from
// the embedded Job.State column. That raw column stays exactly what's
// on disk (useful for debugging), but a legacy pre-result-contract
// "done" row must never be read as a verified success just because
// nothing consulted EffectiveState. Additive fields; existing
// consumers reading .State are unaffected.
type runListItem struct {
	jobs.Job
	Handle string `json:"Handle"`
	// EffectiveState is jobs.EffectiveState's corrected state, what a
	// caller should act on and display, not necessarily what's stored.
	EffectiveState string `json:"effective_state"`
	// Outcome is jobs.DescribeOutcome's evidence-based account: the
	// verified outcome (or its explicit absence) plus a summary/error
	// when one exists. Empty-ish ("state: <state>") for a non-terminal
	// job, which has nothing to verify yet.
	Outcome string `json:"outcome"`
}

type runListResponse struct {
	Runs          []runListItem `json:"runs"`
	ActiveProfile string        `json:"active_profile"`
}

// brainStatsResponse is the JSON view of brain.Stats for the dashboard page.
// Deliberately omits Stats.BrainPath: it's a local filesystem path the page
// has no use for, and there's no reason to hand out filesystem layout over
// HTTP. Do not add it back.
type brainStatsResponse struct {
	Lessons       int            `json:"lessons"`
	JarvisLessons int            `json:"jarvis_lessons"`
	Entities      int            `json:"entities"`
	TasksOpen     int            `json:"tasks_open"`
	TasksDone     int            `json:"tasks_done"`
	TasksByStatus map[string]int `json:"tasks_by_status,omitempty"`
	LastLesson    string         `json:"last_lesson,omitempty"`
	DBSizeBytes   int64          `json:"db_size_bytes"`
	HasEmbeddings bool           `json:"has_embeddings"`
	IsStale       bool           `json:"is_stale"`
	Profile       string         `json:"profile"`
}

// draftStatusEntry is the per-task draft state surfaced to the UI.
// State is the newest job's state; RunID lets the UI link directly
// to /api/runs/<id> for view-draft / SSE-stream actions.
type draftStatusEntry struct {
	State string `json:"state"`            // none|queued|running|ready|failed|incomplete
	RunID string `json:"run_id,omitempty"` // empty when state == "none"
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// computeDraftStatus folds jobs.db rows into a per-task draft state.
// The "newest job per task slug" rule means a retry replaces the
// previous status cleanly; the prior failed run still exists in the
// queue for history (`aida jobs list --task <slug>`).
//
// allProfiles widens the scan via every profile dir under
// ~/.aida/jobs/. Single-profile mode uses the store opened at
// server-start time.
func computeDraftStatus(store *jobs.Store, slugs []string, allProfiles bool) (map[string]draftStatusEntry, error) {
	out := make(map[string]draftStatusEntry, len(slugs))
	if len(slugs) == 0 {
		return out, nil
	}
	wantSlug := make(map[string]bool, len(slugs))
	for _, s := range slugs {
		wantSlug[s] = true
	}

	scanStore := func(s *jobs.Store) error {
		// One pass over recent rows; first row per task slug wins
		// because List returns newest-first.
		rows, err := s.List(jobs.ListOpts{Limit: 5000})
		if err != nil {
			return err
		}
		for _, j := range rows {
			if j.TaskSlug == "" || !wantSlug[j.TaskSlug] {
				continue
			}
			if _, seen := out[j.TaskSlug]; seen {
				continue
			}
			// EffectiveState first: a legacy pre-result-contract "done"
			// row must never badge as "ready" just because stateToBadge
			// saw the raw column value. See jobs.EffectiveState.
			effState, _, _ := jobs.EffectiveState(&j)
			out[j.TaskSlug] = draftStatusEntry{
				State: stateToBadge(effState),
				RunID: j.RunID,
			}
		}
		return nil
	}

	if !allProfiles {
		return out, scanStore(store)
	}
	// Multi-profile: walk every profile directory.
	dirs, err := listProfileDirs()
	if err != nil {
		return out, err
	}
	for _, p := range dirs {
		s, err := jobs.Open(p)
		if err != nil {
			continue
		}
		_ = scanStore(s)
		s.Close()
	}
	return out, nil
}

// stateToBadge maps a job state to a UI badge label. "done" → "ready"
// because that's how the UI describes a finished draft; everything
// else, including the new "incomplete" state, passes through as-is.
// Callers must feed this the EffectiveState-corrected value (see
// computeDraftStatus), never the raw column, so a legacy "done" with
// no verified result can't badge as "ready".
func stateToBadge(s string) string {
	if s == jobs.StateDone {
		return "ready"
	}
	return s
}

// enqueueDraftJob is the POST /api/tasks/{slug}/draft handler body.
// Resolves the task → exec-plan, enqueues a job, spawns the agent
// subprocess in a background goroutine, returns the run id.
func enqueueDraftJob(b *brain.Brain, store *jobs.Store, profile, slug string) (string, error) {
	task, err := b.ResolveTaskRef(slug)
	if err != nil {
		return "", err
	}

	aidaBin, err := exec.LookPath("aida")
	if err != nil {
		return "", fmt.Errorf("aida binary not on PATH: %w", err)
	}

	// Typed destinations rode the removed partner registry (issue
	// #58); drafts from the web UI use the universal flow.
	var dest *jobs.Destination

	// Build the same agent prompt shape autoSolveIngestedTasks uses
	// so drafts produced from web + ingest are interchangeable.
	baseQuestion := buildAutoSolveQuestion("web:tasks", "", task.Title, task.Description)

	// Plan id derived from slug - most ingest-created plans use the
	// same slug as the task. If the plan doesn't exist, the draft
	// still produces output.md but won't append to a decision log.
	planID := task.Slug

	job, err := store.Enqueue("draft", task.Slug, planID, baseQuestion, "web:tasks/"+task.Slug)
	if err != nil {
		return "", fmt.Errorf("enqueue: %w", err)
	}

	// Stash destination on the job after Enqueue so the web modal can
	// pick the right renderer. Failure is non-fatal - universal
	// Copy/Save still works. Goes through the store (not a direct
	// manifest read/write) so Destination also lands on the SQL row
	// and survives every later manifest rewrite (Complete, Fail,
	// MarkNotified, ...) -- see Store.SetDestination.
	if dest != nil {
		_ = store.SetDestination(job.RunID, dest)
	}

	// Build the actual subprocess prompt with destination-specific
	// instructions when applicable.
	question := appendDestinationInstructions(baseQuestion, job.RunID, dest)
	runDir := jobs.RunDir(profile, job.RunID)

	// Spawn detached. cmd.Start returns immediately; we wait in a
	// goroutine so the HTTP handler doesn't block. cmd.Wait + the
	// store transition are the closure's responsibility.
	cmd := exec.Command(aidaBin, "--agent", "--run-dir", runDir, question)
	cmd.Env = append(os.Environ(),
		"NO_COLOR=1", "CLICOLOR=0", "TERM=dumb",
		"AIDA_PROFILE="+profile,
	)
	cmd.Stdout = nil
	cmd.Stderr = nil
	// Detach from the parent process group so a server SIGINT
	// doesn't kill the in-flight draft. A future Reap pass on
	// next server start will catch any orphan whose pid died.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		_ = store.Fail(job.RunID, "spawn: "+err.Error())
		return "", fmt.Errorf("spawn: %w", err)
	}

	go func() {
		runErr := cmd.Wait()
		if runErr != nil {
			_ = store.Fail(job.RunID, runErr.Error())
			if planID != "" {
				_ = brain.AppendDecisionLog(b.Path, planID, "auto-solve",
					fmt.Sprintf("aida --agent failed: %s", runErr))
			}
			return
		}
		// Read output.md and append to plan decision log on success.
		outBytes, err := os.ReadFile(jobs.OutputPath(profile, job.RunID))
		if err != nil {
			_ = store.Fail(job.RunID, "read output: "+err.Error())
			return
		}
		draft := strings.TrimRight(string(outBytes), "\n")
		if planID != "" {
			_ = brain.AppendDecisionLog(b.Path, planID, "auto-solve",
				"draft answer:\n"+draft)
		}
		_ = store.Complete(job.RunID)
	}()

	return job.RunID, nil
}

// readEventsTailLines returns the last n lines of events.ndjson as
// raw strings. Each line is already valid JSON; the UI parses on
// arrival.
func readEventsTailLines(profile, runID string, n int) []string {
	data, err := os.ReadFile(jobs.EventsPath(profile, runID))
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

// streamRunEvents is the SSE handler for /api/runs/{id}/stream.
// Sends every existing event line in events.ndjson, then polls for
// new appends and forwards them. Closes when the job reaches a
// terminal state OR the client disconnects (whichever first).
func streamRunEvents(w http.ResponseWriter, r *http.Request, profile, runID string, store *jobs.Store) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable proxy buffering if any
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	// Send a comment line immediately so the browser commits to the
	// SSE stream - without this, browsers may buffer the response
	// until they see the first `data:` line, and a slow file-open
	// can make the connection look frozen. Comments are valid SSE
	// (lines starting with ":") and don't trigger the EventSource
	// onmessage handler.
	fmt.Fprintf(w, ": connected to run %s\n\n", runID)
	flusher.Flush()

	path := jobs.EventsPath(profile, runID)
	f, err := os.Open(path)
	if err != nil {
		// File may not exist yet (job just enqueued). Wait briefly,
		// emitting periodic keep-alive comments so the browser keeps
		// the connection alive and the user sees a live spinner
		// instead of a frozen "starting agent..." string.
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			f, err = os.Open(path)
			if err == nil {
				break
			}
			fmt.Fprintf(w, ": waiting for events.ndjson\n\n")
			flusher.Flush()
			time.Sleep(200 * time.Millisecond)
		}
		if err != nil {
			// Surface the wait failure as a real `data:` event so
			// the browser's onmessage hander shows it, not as an
			// `event: error` (which would only fire a custom-name
			// listener the UI doesn't have).
			payload := `{"type":"error","data":{"message":"events.ndjson did not appear within 5s - subprocess may have crashed before writing"}}`
			fmt.Fprintf(w, "data: %s\n\n", payload)
			flusher.Flush()
			return
		}
	}
	defer f.Close()

	reader := bufio.NewReader(f)
	ctx := r.Context()
	terminalSeen := false
	lastKeepalive := time.Now()

	emit := func(line string) {
		if line == "" {
			return
		}
		fmt.Fprintf(w, "data: %s\n\n", line)
		flusher.Flush()
		lastKeepalive = time.Now()
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		line, err := reader.ReadString('\n')
		if line != "" {
			emit(strings.TrimRight(line, "\n"))
			if strings.Contains(line, `"type":"complete"`) || strings.Contains(line, `"type":"error"`) {
				terminalSeen = true
			}
		}
		if err == io.EOF {
			if terminalSeen {
				return
			}
			// Also check the SQL state - covers the case where the
			// subprocess crashed without writing a terminal event.
			if j, jerr := store.Get(runID); jerr == nil && jobs.IsTerminalState(j.State) {
				// One last attempt at seeing a final newline append.
				time.Sleep(100 * time.Millisecond)
				if line2, _ := reader.ReadString('\n'); line2 != "" {
					emit(strings.TrimRight(line2, "\n"))
				}
				// Synthesize a terminal event so the browser closes
				// its EventSource cleanly even when the subprocess
				// exited without writing one (crash, kill -9, etc.).
				if !terminalSeen {
					payload := fmt.Sprintf(
						`{"type":"%s","data":{"message":"job is %s but no terminal event was emitted"}}`,
						terminalEventTypeFor(j.State), j.State,
					)
					fmt.Fprintf(w, "data: %s\n\n", payload)
					flusher.Flush()
				}
				return
			}
			// Idle keep-alive every 15s so a hung agent doesn't
			// look like a dropped connection to the browser.
			if time.Since(lastKeepalive) > 15*time.Second {
				fmt.Fprintf(w, ": keepalive\n\n")
				flusher.Flush()
				lastKeepalive = time.Now()
			}
			time.Sleep(250 * time.Millisecond)
			continue
		}
		if err != nil {
			payload := fmt.Sprintf(`{"type":"error","data":{"message":%q}}`, err.Error())
			fmt.Fprintf(w, "data: %s\n\n", payload)
			flusher.Flush()
			return
		}
	}
}

// terminalEventTypeFor maps a job state to the synthesized SSE event
// type for the no-terminal-event recovery path. done → complete;
// everything else, including the new incomplete state, → error, since
// there is no dedicated "incomplete" SSE event type on the browser
// side. An unverified run is closer to "don't trust this" than to a
// clean finish, so it takes the same event type failed does here.
func terminalEventTypeFor(state string) string {
	if state == jobs.StateDone {
		return "complete"
	}
	return "error"
}

// deliverRunInput writes a reply to <run-dir>/input.txt for a job
// in state=awaiting_input. Atomic rename ensures the polling agent
// either sees the prior input.txt (or nothing) or the complete new
// reply - never a half-written file.
//
// Accepts two body shapes for convenience:
//   - raw text body (Content-Type: text/plain or unspecified)
//   - JSON {"text": "..."}
//
// The job must currently be in state=awaiting_input. Sending input
// to a running or terminal job returns 400 so a stale UI button
// can't pollute a run-dir.
func deliverRunInput(store *jobs.Store, profile, runID string, r *http.Request) error {
	if runID == "" {
		return fmt.Errorf("missing run_id")
	}
	j, err := store.Get(runID)
	if err != nil {
		return err
	}
	if j.State != jobs.StateAwaitingInput {
		return fmt.Errorf("job is not awaiting input (state=%s)", j.State)
	}

	// Read up to 64KB. Voice / textual replies are tiny; cap is a
	// sanity check, not a real limit.
	body, err := io.ReadAll(io.LimitReader(r.Body, 64*1024))
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	text := string(body)

	// JSON unwrap: tasks-web hits this with application/json bodies;
	// voice / curl may send raw text. Be flexible.
	if strings.HasPrefix(strings.TrimSpace(text), "{") {
		var payload struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(body, &payload); err == nil && payload.Text != "" {
			text = payload.Text
		}
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return fmt.Errorf("empty reply")
	}

	dir := jobs.RunDir(profile, runID)
	tmp := filepath.Join(dir, fmt.Sprintf(".input.txt.tmp.%d", time.Now().UnixNano()))
	if err := os.WriteFile(tmp, []byte(text), 0644); err != nil {
		return fmt.Errorf("write tmp input: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, "input.txt")); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename input.txt: %w", err)
	}
	return nil
}

// cancelJob cancels any non-terminal job and transitions the row to
// failed with a "cancelled by user" message. Queued jobs have no
// subprocess yet, so they just flip state; running / awaiting jobs
// (the paused agent process stays alive polling for input) get
// SIGTERM, 5s grace, then SIGKILL. Foreign-host jobs return an
// error; only the local host can signal a pid.
func cancelJob(store *jobs.Store, profile, runID string) error {
	j, err := store.Get(runID)
	if err != nil {
		return err
	}
	if jobs.IsTerminalState(j.State) {
		return fmt.Errorf("job already finished (state=%s)", j.State)
	}
	if j.State != jobs.StateQueued {
		host, _ := os.Hostname()
		if j.ClaimedByHost != "" && j.ClaimedByHost != host {
			return fmt.Errorf("cannot cancel - job is on host %q", j.ClaimedByHost)
		}
		if j.ClaimedByPID <= 0 {
			return fmt.Errorf("no pid recorded for this job")
		}
		proc, err := os.FindProcess(j.ClaimedByPID)
		if err != nil {
			return err
		}
		_ = proc.Signal(syscall.SIGTERM)
		go func() {
			time.Sleep(5 * time.Second)
			_ = proc.Kill()
		}()
	}
	if err := store.Fail(runID, "cancelled by user"); err != nil {
		return err
	}
	// Best-effort PID-file cleanup so a later Reap doesn't second-
	// guess the cancellation.
	_ = os.Remove(filepath.Join(jobs.RunDir(profile, runID), "pid"))
	return nil
}
