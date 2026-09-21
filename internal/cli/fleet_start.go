package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/fleet"
	"github.com/ryanlitalien/aida/internal/jarvis/audio"
	"github.com/ryanlitalien/aida/internal/jobs"
	"github.com/ryanlitalien/aida/internal/remotex"
	"github.com/ryanlitalien/aida/internal/ui"
)

// fleetStartTimeout bounds each individual remote call `aida fleet
// start` issues itself (the task-file write, plus the three calls in
// fleet.StartAgent) -- distinct from `aida fleet watch --timeout`,
// which bounds the whole completion poll and runs in a separate,
// already-detached process by the time this command returns.
const fleetStartTimeout = 20 * time.Second

// fleetStartOpts holds `aida fleet start`'s flags.
type fleetStartOpts struct {
	Name     string
	Task     string
	TaskFile string
	Host     string
	User     string
	Cwd      string
	Agent    string
	Model    string
	Effort   string
	Repo     string
	Question string
	KeepOpen int
	NoWatch  bool
}

// newFleetStartCmd builds `aida fleet start`: launch a lane 4 (Ryan's
// own herdr) agent on minty and record it as a job from the moment it
// starts, closing the gap `aida fleet watch` (iteration B) only closes
// after the fact for a hand-launched run. Iteration C of
// ~/.aida/team/fitz/briefs/2026-09-02-lane4-aida-gaps.md.
//
// Design constraint (Ryan-approved, see the brief's "Needs Ryan"
// section): the task's own text is delivered to minty ONLY from this
// CLI command, via remotex.RunRemote (internal/fleet.WriteTask /
// TaskFileWriteArgv) -- never through an HTTP route. An endpoint that
// could write arbitrary text to a file a herdr pane is about to `cat`
// straight into `claude -p` would be a remote-code-execution primitive
// with a JSON wrapper (the same reasoning as bifrost.Client.StartPreset
// deliberately having no caller-supplied-argv method, see
// internal/bifrost/client.go); keeping task delivery CLI-only means
// only whoever already holds a terminal on this binary can ever set it.
func newFleetStartCmd() *cobra.Command {
	var o fleetStartOpts
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Launch a lane 4 herdr agent on minty and record it as a job",
		Long: "Writes the task text to minty (see the CLI-only design note above),\n" +
			"refuses if a herdr agent with --name already exists, creates a herdr\n" +
			"workspace labeled --name, starts the agent in it running\n" +
			"lane4-run.sh (see ~/dev/aida-agents/scripts/lane4-run.sh), enqueues a\n" +
			"kind=fleet job row, and -- unless --no-watch -- spawns a detached\n" +
			"`aida fleet watch` to complete that job once the run's EXIT marker\n" +
			"shows up in its log.",
		Example: "  aida fleet start --name scarlett-978 --task \"tracer bullet: issue #978\" \\\n" +
			"    --repo AcmeWidgets/acme_widgets\n\n" +
			"  aida fleet start --name fleet-start-selftest --task \"Reply with exactly the single word: pong.\" \\\n" +
			"    --agent none --cwd /home/ryan --question \"fleet start self-test\" --keep-open 20",
		RunE: func(_ *cobra.Command, _ []string) error {
			return validateFleetStartOpts(&o)
		},
	}
	cmd.Flags().StringVar(&o.Name, "name", "", "herdr agent name (required; also the .task/.log filename stem under agents-lane4/)")
	cmd.Flags().StringVar(&o.Task, "task", "", "task text for the agent (required unless --task-file)")
	cmd.Flags().StringVar(&o.TaskFile, "task-file", "", "read task text from this local file instead of --task")
	cmd.Flags().StringVar(&o.Host, "host", "", "ssh host the agent runs on (default: the devices: entry with herdr: true, like /bifrost)")
	cmd.Flags().StringVar(&o.User, "user", "ryan", "herdr lane -- remote user to sudo -iu into (see remotex.RunRemote)")
	cmd.Flags().StringVar(&o.Cwd, "cwd", "/home/ryan/dev/butter_stack", "absolute working directory on the remote host (no tilde expansion on the remote argv)")
	cmd.Flags().StringVar(&o.Agent, "agent", "swe", "claude -p --agent persona lane4-run.sh launches (\"none\" for no --agent)")
	cmd.Flags().StringVar(&o.Model, "model", "claude-opus-4-6", "model alias -- passed to lane4-run.sh --opus and recorded on the job")
	cmd.Flags().StringVar(&o.Effort, "effort", "max", "effort level passed to lane4-run.sh --effort")
	cmd.Flags().StringVar(&o.Repo, "repo", "", "owner/name -- passed through to aida fleet watch --repo for its \"PR #N\" fallback")
	cmd.Flags().StringVar(&o.Question, "question", "", "job question; defaults to the task's first 80 characters")
	cmd.Flags().IntVar(&o.KeepOpen, "keep-open", 0, "seconds lane4-run.sh keeps its pane open after EXIT (0 = its own 600s default)")
	cmd.Flags().BoolVar(&o.NoWatch, "no-watch", false, "don't spawn `aida fleet watch` after starting the agent")
	return cmd
}

// validateFleetStartOpts checks what cobra can't validate itself
// (same no-MarkFlagRequired precedent as validateFleetWatchOpts) --
// crucially, fleet.ValidateName runs before runFleetStart touches the
// jobs store or any remote host, so a bad --name never gets as far as
// an ssh call.
func validateFleetStartOpts(o *fleetStartOpts) error {
	if err := fleet.ValidateName(o.Name); err != nil {
		return err
	}
	if o.Host == "" {
		host, err := defaultHerdrHost()
		if err != nil {
			return err
		}
		o.Host = host
	}
	if o.Task == "" && o.TaskFile == "" {
		return fmt.Errorf("--task or --task-file is required")
	}
	if o.Task != "" && o.TaskFile != "" {
		return fmt.Errorf("specify either --task or --task-file, not both")
	}
	if o.Repo != "" && !repoRe.MatchString(o.Repo) {
		return fmt.Errorf("--repo must be owner/name: %q", o.Repo)
	}
	if !filepath.IsAbs(o.Cwd) {
		// remotex exec's argv without a shell, so no tilde expansion --
		// every remote path in argv must be absolute (see
		// validateFleetWatchOpts's identical --log check).
		return fmt.Errorf("--cwd must be an absolute path (no tilde expansion on the remote argv): %q", o.Cwd)
	}
	if o.KeepOpen < 0 {
		return fmt.Errorf("--keep-open must be >= 0")
	}
	return runFleetStart(*o)
}

// loadFleetTask returns the task text: --task-file's contents (minus a
// single trailing newline, the common "file ends with a newline"
// convention) when set, else --task verbatim.
func loadFleetTask(o fleetStartOpts) (string, error) {
	if o.TaskFile == "" {
		return o.Task, nil
	}
	b, err := os.ReadFile(o.TaskFile)
	if err != nil {
		return "", fmt.Errorf("read --task-file %s: %w", o.TaskFile, err)
	}
	return strings.TrimSuffix(string(b), "\n"), nil
}

// runFleetStart is the command body: enqueue the job row, write the
// task file, run the herdr-side start sequence (fleet.StartAgent), and
// -- unless --no-watch -- spawn the detached completion watcher.
func runFleetStart(o fleetStartOpts) error {
	task, err := loadFleetTask(o)
	if err != nil {
		return err
	}
	question := o.Question
	if question == "" {
		question = fleet.DefaultQuestion(task)
	}

	profile, err := resolveProfile()
	if err != nil {
		return err
	}
	store, err := jobs.Open(profile)
	if err != nil {
		return err
	}
	defer store.Close()

	sourceRef := fmt.Sprintf("fleet:%s:%s:%s", o.Host, o.User, o.Name)
	job, err := store.Enqueue("fleet", "", "", question, sourceRef)
	if err != nil {
		return fmt.Errorf("enqueue: %w", err)
	}
	runID := job.RunID

	localHost, _ := os.Hostname()
	if err := store.MarkRunning(runID, localHost, os.Getpid()); err != nil {
		return fmt.Errorf("mark running: %w", err)
	}
	// Best-effort: the job is still useful even if this write fails --
	// the row existing and the herdr-side start are what matter most.
	if err := store.SetAgentModel(runID, o.Name, o.Model); err != nil {
		ui.PrintVerbose("fleet start", "set agent/model failed: "+err.Error())
	}
	fmt.Printf("Enqueued job %s (kind=fleet, %s)\n", runID, sourceRef)

	ctx, cancel := context.WithTimeout(context.Background(), fleetStartTimeout)
	defer cancel()
	runner := remotex.ExecRunner{}

	taskPath := fleet.TaskPath(o.User, o.Name)
	logPath := fleet.LogPath(o.User, o.Name)

	if err := fleet.WriteTask(ctx, runner, o.Host, o.User, taskPath, task, fleetStartTimeout); err != nil {
		_ = store.Fail(runID, "write task: "+err.Error())
		return fmt.Errorf("fleet start %s: write task: %w", runID, err)
	}

	launchArgv := fleet.LauncherArgv(o.User, o.Name, o.Agent, o.Cwd, o.Effort, o.Model, o.KeepOpen)
	result, err := fleet.StartAgent(ctx, runner, o.Host, o.User, o.Cwd, o.Name, launchArgv, fleetStartTimeout)
	if err != nil {
		_ = store.Fail(runID, "start agent: "+err.Error())
		return fmt.Errorf("fleet start %s: start agent: %w", runID, err)
	}

	fmt.Printf("%s: herdr agent %s started (workspace %s) on %s@%s\n", runID, o.Name, result.WorkspaceID, o.User, o.Host)
	fmt.Printf("  log: %s\n", logPath)
	fmt.Printf("  reattach: ssh -t %s /opt/herdr/bin/herdr, then attach %s\n", o.Host, o.Name)

	if o.NoWatch {
		return nil
	}
	if err := spawnFleetWatch(profile, runID, o); err != nil {
		fmt.Fprintf(os.Stderr, "fleet start %s: spawn watch failed, start it by hand: %v\n", runID, err)
		fmt.Fprintf(os.Stderr, "  aida fleet watch --agent %s --log %s --host %s --user %s --run-id %s --model %s\n",
			o.Name, logPath, o.Host, o.User, runID, o.Model)
	}
	return nil
}

// buildFleetWatchCmd constructs (but does not start) the detached
// `aida fleet watch` process that completes the job runFleetStart just
// enqueued. Pure aside from exec.Command's own allocation, so a test
// can assert on Args without ever starting a process -- mirrors
// internal/roster/job.go's buildJobCmd.
func buildFleetWatchCmd(aidaBin string, o fleetStartOpts, runID string) *exec.Cmd {
	args := []string{
		"fleet", "watch",
		"--agent", o.Name,
		"--log", fleet.LogPath(o.User, o.Name),
		"--host", o.Host,
		"--user", o.User,
		"--run-id", runID,
	}
	if o.Repo != "" {
		args = append(args, "--repo", o.Repo)
	}
	if o.Model != "" {
		args = append(args, "--model", o.Model)
	}
	cmd := exec.Command(aidaBin, args...)
	// setsid isn't available on macOS; Setpgid puts the child in its own
	// process group so it survives this (short-lived CLI) process's own
	// exit, same recipe as internal/roster/job.go's buildJobCmd and
	// internal/cli/query.go's spawnDetachedAida.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

// spawnFleetWatch resolves the currently-running aida binary (not
// whatever "aida" happens to be on $PATH -- see os.Executable's use in
// internal/cli/query.go's spawnDetachedAida -- a stale globally
// installed binary might not even have the `fleet watch` subcommand
// yet), builds the watch command, redirects its stdio to a log file
// under the job's run dir, starts it detached, and returns without
// waiting on it: this is a one-shot CLI invocation, not a daemon, so
// there is no long-lived goroutine to reap it with.
func spawnFleetWatch(profile, runID string, o fleetStartOpts) error {
	bin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve aida binary: %w", err)
	}

	runDir := jobs.RunDir(profile, runID)
	if err := os.MkdirAll(runDir, 0755); err != nil {
		return fmt.Errorf("mkdir run dir: %w", err)
	}
	watchLogPath := filepath.Join(runDir, "fleet-watch.log")
	logFile, err := os.OpenFile(watchLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("open %s: %w", watchLogPath, err)
	}
	defer logFile.Close()

	cmd := buildFleetWatchCmd(bin, o, runID)
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start: %w", err)
	}

	fmt.Printf("  watch: pid %d, log %s\n", cmd.Process.Pid, watchLogPath)
	return nil
}

// defaultHerdrHost resolves --host when it isn't passed explicitly: the
// devices: entry with herdr: true, the same lookup newBifrostClients (see
// dashboard_web.go) uses for the /bifrost page's default lane. A machine
// with no devices: block, or one where no device sets herdr: true, has
// nothing to launch a lane 4 agent on, so this errors out with a pointer
// to the fix rather than silently falling back to a guessed hostname.
func defaultHerdrHost() (string, error) {
	cfg, err := config.LoadConfig()
	if err != nil {
		return "", fmt.Errorf("--host not set and loading config failed: %w", err)
	}
	for _, d := range cfg.ResolveDevices(audio.MachineName()) {
		if d.Herdr {
			return d.SSHTarget(), nil
		}
	}
	return "", fmt.Errorf("--host not set and no devices: entry in config.yaml has herdr: true -- set one (see internal/config/devices.go) or pass --host explicitly")
}
