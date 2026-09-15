package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDetectChecks(t *testing.T) {
	// Makefile with a test: target wins.
	mk := t.TempDir()
	if err := os.WriteFile(filepath.Join(mk, "Makefile"), []byte("build:\n\tgo build\ntest:\n\tgo test ./...\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := detectChecks(mk); !reflect.DeepEqual(got, []string{"make test"}) {
		t.Errorf("Makefile test target: got %v, want [make test]", got)
	}

	// go.mod only → go build fallback.
	gm := t.TempDir()
	if err := os.WriteFile(filepath.Join(gm, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := detectChecks(gm); !reflect.DeepEqual(got, []string{"go build ./..."}) {
		t.Errorf("go.mod only: got %v, want [go build ./...]", got)
	}

	// Makefile without a test: target → no gate (not a false make test).
	noTest := t.TempDir()
	if err := os.WriteFile(filepath.Join(noTest, "Makefile"), []byte("build:\n\tgo build\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := detectChecks(noTest); got != nil {
		t.Errorf("Makefile without test target: got %v, want nil", got)
	}

	// Empty dir → no gate.
	if got := detectChecks(t.TempDir()); got != nil {
		t.Errorf("empty dir: got %v, want nil", got)
	}
}

func TestRecognizeSwarm(t *testing.T) {
	cases := []struct {
		q    string
		want bool
	}{
		{"swarm on PR 456", true},
		{"swarm PR 456", true},
		{"work through my prio tasks", true},
		{"loop over my tasks", true},
		{"loop on the cleanup tasks", true},
		{"grind through the backlog", true},
		{"churn through my tasks", true},
		{"please swarm PR 12", true},
		{"can you swarm the prio tasks", true},
		{"ok jarvis, swarm on PR 5", true},
		{"go swarm everything", true},

		// Must NOT fire on ordinary questions.
		{"how does the swarm scheduler work?", false},
		{"what is a loop in go", false},
		{"show me PR 456", false},
		{"search the codebase for swarm", false},
		{"explain the work queue", false},
		{"", false},
	}
	for _, c := range cases {
		if got := recognizeSwarm(c.q); got != c.want {
			t.Errorf("recognizeSwarm(%q) = %v, want %v", c.q, got, c.want)
		}
	}
}

func TestResolveSwarmTarget(t *testing.T) {
	cases := []struct {
		q        string
		wantKind swarmTargetKind
		wantPR   string
		wantTag  string
	}{
		{"swarm on PR 456", targetPR, "456", ""},
		{"swarm pull request #99", targetPR, "99", ""},
		{"work on pr 7", targetPR, "7", ""},
		{"work through my prio tasks", targetTag, "", "prio"},
		{"swarm tasks tagged csv", targetTag, "", "csv"},
		{"grind through the cleanup tasks", targetTag, "", "cleanup"},
		{"swarm my prio tasks", targetTag, "", "prio"},
		{"swarm my tasks", targetAllTasks, "", ""},
		{"loop over all open tasks", targetAllTasks, "", ""},
		{"swarm everything", targetAllTasks, "", ""},
		{"work through the backlog", targetAllTasks, "", ""},
		{"swarm", targetNone, "", ""},
	}
	for _, c := range cases {
		kind, pr, tag := resolveSwarmTarget(c.q)
		if kind != c.wantKind || pr != c.wantPR || tag != c.wantTag {
			t.Errorf("resolveSwarmTarget(%q) = (%v,%q,%q), want (%v,%q,%q)",
				c.q, kind, pr, tag, c.wantKind, c.wantPR, c.wantTag)
		}
	}
}
