package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ryanlitalien/aida/internal/config"
)

// TestValidateFleetStartOpts_RejectsBeforeAnySideEffect exercises every
// validation branch that must fail before runFleetStart ever opens the
// jobs store or touches a remote host -- each of these cases would
// otherwise need a real profile/store/network to reach runFleetStart,
// so a passing test here is proof the guard actually short-circuits.
func TestValidateFleetStartOpts_RejectsBeforeAnySideEffect(t *testing.T) {
	base := func() fleetStartOpts {
		return fleetStartOpts{
			Name: "scarlett-smoke",
			Task: "print hello and exit",
			Host: "minty",
			User: "ryan",
			Cwd:  "/home/ryan/dev/butter_stack",
		}
	}

	tests := []struct {
		name    string
		mutate  func(*fleetStartOpts)
		wantErr string
	}{
		{
			name:    "invalid name",
			mutate:  func(o *fleetStartOpts) { o.Name = "bad name!" },
			wantErr: "invalid name",
		},
		{
			name:    "empty name",
			mutate:  func(o *fleetStartOpts) { o.Name = "" },
			wantErr: "name is required",
		},
		{
			name:    "missing task and task-file",
			mutate:  func(o *fleetStartOpts) { o.Task = "" },
			wantErr: "--task or --task-file is required",
		},
		{
			name: "both task and task-file",
			mutate: func(o *fleetStartOpts) {
				o.TaskFile = "/tmp/does-not-need-to-exist.task"
			},
			wantErr: "not both",
		},
		{
			name:    "malformed repo",
			mutate:  func(o *fleetStartOpts) { o.Repo = "not-owner-slash-name" },
			wantErr: "--repo must be owner/name",
		},
		{
			name:    "relative cwd",
			mutate:  func(o *fleetStartOpts) { o.Cwd = "dev/butter_stack" },
			wantErr: "--cwd must be an absolute path",
		},
		{
			name:    "negative keep-open",
			mutate:  func(o *fleetStartOpts) { o.KeepOpen = -1 },
			wantErr: "--keep-open must be >= 0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := base()
			tt.mutate(&o)
			err := validateFleetStartOpts(&o)
			if err == nil {
				t.Fatalf("validateFleetStartOpts() error = nil, want one containing %q", tt.wantErr)
			}
			if got := err.Error(); !strings.Contains(got, tt.wantErr) {
				t.Errorf("validateFleetStartOpts() error = %q, want it to contain %q", got, tt.wantErr)
			}
		})
	}
}

func TestLoadFleetTask(t *testing.T) {
	t.Run("from --task", func(t *testing.T) {
		got, err := loadFleetTask(fleetStartOpts{Task: "print hello and exit"})
		if err != nil {
			t.Fatalf("loadFleetTask: %v", err)
		}
		if got != "print hello and exit" {
			t.Errorf("loadFleetTask() = %q, want %q", got, "print hello and exit")
		}
	})

	t.Run("from --task-file, trailing newline trimmed", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "task.txt")
		if err := os.WriteFile(path, []byte("do the thing\n"), 0644); err != nil {
			t.Fatalf("write temp task file: %v", err)
		}
		got, err := loadFleetTask(fleetStartOpts{TaskFile: path})
		if err != nil {
			t.Fatalf("loadFleetTask: %v", err)
		}
		if got != "do the thing" {
			t.Errorf("loadFleetTask() = %q, want %q", got, "do the thing")
		}
	})

	t.Run("missing task-file is an error", func(t *testing.T) {
		_, err := loadFleetTask(fleetStartOpts{TaskFile: "/no/such/file/here.task"})
		if err == nil {
			t.Fatal("loadFleetTask() error = nil, want a read error")
		}
	})
}

// TestBuildFleetWatchCmd asserts the argv/SysProcAttr shape of the
// spawned `aida fleet watch` process WITHOUT ever starting it --
// exec.Command only builds the *exec.Cmd struct. Mirrors
// internal/roster/job.go's TestBuildJobCmd.
func TestBuildFleetWatchCmd(t *testing.T) {
	o := fleetStartOpts{
		Name:  "scarlett-978",
		Host:  "minty",
		User:  "ryan",
		Repo:  "ButterStack/butter_stack",
		Model: "claude-opus-4-6",
	}
	cmd := buildFleetWatchCmd("/usr/local/bin/aida", o, "20260902-abc")

	want := []string{
		"/usr/local/bin/aida", "fleet", "watch",
		"--agent", "scarlett-978",
		"--log", "/home/ryan/agents-lane4/scarlett-978.log",
		"--host", "minty",
		"--user", "ryan",
		"--run-id", "20260902-abc",
		"--repo", "ButterStack/butter_stack",
		"--model", "claude-opus-4-6",
	}
	if len(cmd.Args) != len(want) {
		t.Fatalf("Args = %v, want %v", cmd.Args, want)
	}
	for i := range want {
		if cmd.Args[i] != want[i] {
			t.Errorf("Args[%d] = %q, want %q", i, cmd.Args[i], want[i])
		}
	}
	if cmd.SysProcAttr == nil {
		t.Error("SysProcAttr not set; watch would not be detached into its own process group")
	}
}

func TestBuildFleetWatchCmd_OmitsUnsetRepoAndModel(t *testing.T) {
	o := fleetStartOpts{Name: "n", Host: "minty", User: "ryan"}
	cmd := buildFleetWatchCmd("/usr/local/bin/aida", o, "run-1")

	for _, flag := range []string{"--repo", "--model"} {
		for _, arg := range cmd.Args {
			if arg == flag {
				t.Errorf("Args = %v, did not expect %q when unset", cmd.Args, flag)
			}
		}
	}
}

// TestDefaultHerdrHost_NoDevicesConfigured covers a fresh ~/.aida/ with no
// devices: block at all: there is nothing to launch a lane 4 agent on, so
// --host must fail with a pointer to the fix rather than guessing at a
// hostname (the whole point of this checklist item -- no more hardcoded
// "minty" default).
func TestDefaultHerdrHost_NoDevicesConfigured(t *testing.T) {
	withTempHome(t)

	if err := runInitConfigOnly(t); err != nil {
		t.Fatalf("seeding config.yaml: %v", err)
	}

	if _, err := defaultHerdrHost(); err == nil {
		t.Fatal("defaultHerdrHost() error = nil, want an error when no devices: entry has herdr: true")
	}
}

// TestDefaultHerdrHost_ResolvesHerdrDevice mirrors newBifrostClients'
// lookup: the devices: entry with herdr: true wins, using its ssh_host
// override when set.
func TestDefaultHerdrHost_ResolvesHerdrDevice(t *testing.T) {
	withTempHome(t)

	if err := runInitConfigOnly(t); err != nil {
		t.Fatalf("seeding config.yaml: %v", err)
	}
	cfg, err := config.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	cfg.Devices = []config.DeviceConfig{
		{Name: "desk", Role: "primary", Self: true, Probe: config.ProbeLocal},
		{Name: "fleet-box", Role: "gpu", Herdr: true, SSHHost: "fleet-box.example"},
	}
	if err := config.SaveConfig(cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	got, err := defaultHerdrHost()
	if err != nil {
		t.Fatalf("defaultHerdrHost(): %v", err)
	}
	if got != "fleet-box.example" {
		t.Errorf("defaultHerdrHost() = %q, want %q", got, "fleet-box.example")
	}
}

// runInitConfigOnly writes a bare default config.yaml under the test's
// temp HOME without running the full aida init flow (which prompts for
// an API key and probes PATH) -- these tests only need Config.Devices.
func runInitConfigOnly(t *testing.T) error {
	t.Helper()
	if err := config.EnsureDir(); err != nil {
		return err
	}
	return config.SaveConfig(config.DefaultConfig())
}
