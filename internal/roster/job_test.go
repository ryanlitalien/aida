package roster

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/ryanlitalien/aida/internal/jobs"
)

func TestNewJobBackend_RequiresJobsStore(t *testing.T) {
	if _, err := newJobBackend(&Entry{Name: "x", Kind: KindJob}, Deps{}); err == nil {
		t.Fatal("expected an error when deps.Jobs is nil")
	}
}

func TestNewJobBackend_KindDefaulting(t *testing.T) {
	store := openTempJobsStore(t, "kind-default")

	b, err := newJobBackend(&Entry{Name: "researcher", Kind: KindJob}, Deps{Jobs: store})
	if err != nil {
		t.Fatalf("newJobBackend: %v", err)
	}
	jb := b.(*jobBackend)
	if jb.kind != defaultJobKind {
		t.Errorf("kind = %q, want default %q", jb.kind, defaultJobKind)
	}
	if jb.Kind() != KindJob {
		t.Errorf("Kind() = %q, want %q", jb.Kind(), KindJob)
	}
}

func TestNewJobBackend_KindOverride(t *testing.T) {
	store := openTempJobsStore(t, "kind-override")

	e := &Entry{Name: "researcher", Kind: KindJob, Job: &JobSpec{Kind: "investigate"}}
	b, err := newJobBackend(e, Deps{Jobs: store})
	if err != nil {
		t.Fatalf("newJobBackend: %v", err)
	}
	jb := b.(*jobBackend)
	if jb.kind != "investigate" {
		t.Errorf("kind = %q, want %q", jb.kind, "investigate")
	}
}

func TestNewJobBackend_ExpandsCwd(t *testing.T) {
	store := openTempJobsStore(t, "cwd-expand")

	e := &Entry{Name: "researcher", Kind: KindJob, Job: &JobSpec{Cwd: "~/dev/somewhere"}}
	b, err := newJobBackend(e, Deps{Jobs: store})
	if err != nil {
		t.Fatalf("newJobBackend: %v", err)
	}
	jb := b.(*jobBackend)
	if strings.HasPrefix(jb.cwd, "~") {
		t.Errorf("cwd = %q, want ~ expanded", jb.cwd)
	}
	if !strings.HasSuffix(jb.cwd, "/dev/somewhere") {
		t.Errorf("cwd = %q, want to end with /dev/somewhere", jb.cwd)
	}
}

func TestNewJobBackend_NoCwdByDefault(t *testing.T) {
	store := openTempJobsStore(t, "no-cwd")

	b, err := newJobBackend(&Entry{Name: "researcher", Kind: KindJob}, Deps{Jobs: store})
	if err != nil {
		t.Fatalf("newJobBackend: %v", err)
	}
	if jb := b.(*jobBackend); jb.cwd != "" {
		t.Errorf("cwd = %q, want empty", jb.cwd)
	}
}

// TestBuildJobCmd asserts the argv/env/dir shape of the spawned process
// WITHOUT ever starting it -- exec.Command only builds the *exec.Cmd
// struct, it doesn't run anything.
func TestBuildJobCmd(t *testing.T) {
	cmd := buildJobCmd("/usr/local/bin/aida", "/run/dir/20260722-abc", "work", "/some/cwd", "investigate the outage")

	wantArgs := []string{"/usr/local/bin/aida", "--agent", "--run-dir", "/run/dir/20260722-abc", "investigate the outage"}
	if len(cmd.Args) != len(wantArgs) {
		t.Fatalf("Args = %v, want %v", cmd.Args, wantArgs)
	}
	for i, want := range wantArgs {
		if cmd.Args[i] != want {
			t.Errorf("Args[%d] = %q, want %q", i, cmd.Args[i], want)
		}
	}
	if cmd.Path != "/usr/local/bin/aida" {
		t.Errorf("Path = %q, want %q", cmd.Path, "/usr/local/bin/aida")
	}
	if cmd.Dir != "/some/cwd" {
		t.Errorf("Dir = %q, want %q", cmd.Dir, "/some/cwd")
	}

	wantEnv := map[string]bool{"NO_COLOR=1": true, "CLICOLOR=0": true, "TERM=dumb": true, "AIDA_PROFILE=work": true}
	for want := range wantEnv {
		found := false
		for _, e := range cmd.Env {
			if e == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("Env missing %q (got %v)", want, cmd.Env)
		}
	}
	if cmd.SysProcAttr == nil {
		t.Error("SysProcAttr not set; job would not be detached into its own process group")
	}
}

func TestBuildJobCmd_NoCwdLeavesDirUnset(t *testing.T) {
	cmd := buildJobCmd("/usr/local/bin/aida", "/run/dir/x", "home", "", "do a thing")
	if cmd.Dir != "" {
		t.Errorf("Dir = %q, want empty (inherit the current process's cwd)", cmd.Dir)
	}
}

// TestJobBackend_EnqueueAgainstTempStore exercises the same store.Enqueue
// call Ask() makes, against a real temp jobs.Store, WITHOUT ever exercising
// Ask() itself (which would exec.LookPath("aida") and spawn a real,
// network-hitting agent process -- forbidden in this test suite).
func TestJobBackend_EnqueueAgainstTempStore(t *testing.T) {
	store := openTempJobsStore(t, "enqueue-wiring")

	b, err := newJobBackend(&Entry{Name: "researcher", Kind: KindJob}, Deps{Jobs: store})
	if err != nil {
		t.Fatalf("newJobBackend: %v", err)
	}
	jb := b.(*jobBackend)
	if jb.store != store {
		t.Fatal("jobBackend.store is not the injected Deps.Jobs store")
	}

	job, err := jb.store.Enqueue(jb.kind, "", "", "investigate the flaky test", "aida:roster")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if job.RunID == "" {
		t.Fatal("expected a non-empty run id")
	}

	got, err := store.Get(job.RunID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Question != "investigate the flaky test" {
		t.Errorf("Question = %q, want %q", got.Question, "investigate the flaky test")
	}
	if got.State != jobs.StateQueued {
		t.Errorf("State = %q, want %q", got.State, jobs.StateQueued)
	}
	if got.SourceRef != "aida:roster" {
		t.Errorf("SourceRef = %q, want %q", got.SourceRef, "aida:roster")
	}
}

// TestReapJobExit_FailsJobNotReachedTerminal exercises reapJobExit
// directly against a harmless, already-started local process ("true" --
// nothing to do with `aida` or the network) that exits 0 without ever
// calling store.Complete/store.Fail itself, mirroring a process that died
// mid-flight. reapJobExit's backstop must then fail the job.
func TestReapJobExit_FailsJobNotReachedTerminal(t *testing.T) {
	trueBin, err := exec.LookPath("true")
	if err != nil {
		t.Skip("no `true` binary on PATH")
	}
	store := openTempJobsStore(t, "reaper-backstop")

	job, err := store.Enqueue("agent", "", "", "reaper test", "aida:roster")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	cmd := exec.Command(trueBin)
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	reapJobExit(store, cmd, job.RunID) // called synchronously (not `go`) for a deterministic test

	got, err := store.Get(job.RunID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != jobs.StateFailed {
		t.Errorf("State = %q, want %q (reaper backstop)", got.State, jobs.StateFailed)
	}
}

// openTempJobsStore opens a fresh jobs.Store rooted at a per-test temp
// HOME, so tests never touch the real ~/.aida/jobs/ queue.
func openTempJobsStore(t *testing.T, profile string) *jobs.Store {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	store, err := jobs.Open(profile)
	if err != nil {
		t.Fatalf("jobs.Open(%q): %v", profile, err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}
