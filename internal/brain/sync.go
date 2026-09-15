package brain

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ryanlitalien/aida/internal/ui"
)

// IsGitRepo checks if the brain path is a git repository.
func IsGitRepo(brainPath string) bool {
	_, err := os.Stat(filepath.Join(brainPath, ".git"))
	return err == nil
}

// InitRepo initializes the brain directory as a git repo with an optional remote.
func InitRepo(brainPath, remote string) error {
	if IsGitRepo(brainPath) {
		if remote != "" {
			// Add remote if not already set
			cmd := exec.Command("git", "-C", brainPath, "remote", "get-url", "origin")
			if err := cmd.Run(); err != nil {
				return exec.Command("git", "-C", brainPath, "remote", "add", "origin", remote).Run()
			}
		}
		return nil
	}

	// The brain directory itself may not exist yet -- e.g. a fresh `aida
	// init` on a machine that's never seen ~/.aida/, where nothing has
	// written to brain.db yet to create it as a side effect.
	if err := os.MkdirAll(brainPath, 0755); err != nil {
		return fmt.Errorf("creating brain dir: %w", err)
	}

	if err := exec.Command("git", "-C", brainPath, "init").Run(); err != nil {
		return fmt.Errorf("git init: %w", err)
	}

	// Write .gitignore
	gitignore := filepath.Join(brainPath, ".gitignore")
	if err := os.WriteFile(gitignore, []byte("brain.db\nbrain.db-wal\nbrain.db-shm\n*.embedding.cache\n"), 0644); err != nil {
		return err
	}

	// Write RESOLVER.md
	resolverPath := filepath.Join(brainPath, "RESOLVER.md")
	if _, err := os.Stat(resolverPath); os.IsNotExist(err) {
		resolver := `# Brain Resolver

Decision tree for where information goes in the brain:

1. Is it about a partner/merchant? → entities/partners/{slug}.md
2. Is it about a tool or data source? → entities/tools/{slug}.md
3. Is it about a person? → entities/people/{slug}.md
4. Is it a recurring investigation pattern? → knowledge/patterns/{slug}.md
5. Is it domain knowledge? → knowledge/domains/{topic}.md
6. Is it routing guidance? → knowledge/routing/compiled.md (auto-generated)
7. Is it a raw lesson? → lessons/{profile}/{timestamp}-{hash}.json (auto-generated)
`
		if err := os.WriteFile(resolverPath, []byte(resolver), 0644); err != nil {
			return err
		}
	}

	if remote != "" {
		if err := exec.Command("git", "-C", brainPath, "remote", "add", "origin", remote).Run(); err != nil {
			return fmt.Errorf("add remote: %w", err)
		}
	}

	// Initial commit
	exec.Command("git", "-C", brainPath, "add", "-A").Run()
	exec.Command("git", "-C", brainPath, "commit", "-m", "Initialize aida brain").Run()

	return nil
}

// Pull does a non-blocking git pull --rebase in the brain repo.
// Returns a channel that signals when the pull is done.
func Pull(brainPath string) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		if !IsGitRepo(brainPath) {
			return
		}
		cmd := exec.Command("git", "-C", brainPath, "pull", "--rebase", "--quiet")
		cmd.Stdout = nil
		cmd.Stderr = nil
		if err := cmd.Run(); err != nil {
			ui.PrintVerbose("Brain pull", "failed: "+err.Error())
		}
	}()
	return done
}

// PullAndWaitTimeout bounds how long PullAndWait blocks for a git pull
// before giving up and letting the caller continue anyway.
const PullAndWaitTimeout = 10 * time.Second

// PullAndWait blocks until the git pull started by Pull finishes, or until
// PullAndWaitTimeout elapses, whichever comes first. It exists for callers
// that need the brain repo up to date before doing something id-sensitive
// (e.g. allocating a task id) but must not hang forever on an offline
// machine or a slow/unreachable remote - a git failure inside Pull is
// already logged there; a timeout here logs its own one-line warning and
// returns, leaving the underlying git process to finish in the background.
func PullAndWait(brainPath string) {
	select {
	case <-Pull(brainPath):
	case <-time.After(PullAndWaitTimeout):
		ui.PrintVerbose("Brain pull", fmt.Sprintf("timed out after %s, continuing without waiting", PullAndWaitTimeout))
	}
}

// CommitAndPush commits all changes and pushes via a fire-and-forget subprocess.
// The push process is detached so it outlives the parent aida process.
func CommitAndPush(brainPath, profile string) {
	if !IsGitRepo(brainPath) {
		return
	}

	// Stage all changes
	cmd := exec.Command("git", "-C", brainPath, "add", "-A")
	if err := cmd.Run(); err != nil {
		ui.PrintVerbose("Brain commit", "add failed: "+err.Error())
		return
	}

	// Check if there's anything to commit
	status := exec.Command("git", "-C", brainPath, "status", "--porcelain")
	out, _ := status.Output()
	if len(strings.TrimSpace(string(out))) == 0 {
		return
	}

	// Commit
	msg := fmt.Sprintf("auto: %s %s", profile, time.Now().UTC().Format("2006-01-02T15:04:05"))
	cmd = exec.Command("git", "-C", brainPath, "commit", "-m", msg)
	if err := cmd.Run(); err != nil {
		ui.PrintVerbose("Brain commit", "failed: "+err.Error())
		return
	}

	// Check if remote exists
	remoteCmd := exec.Command("git", "-C", brainPath, "remote", "get-url", "origin")
	if err := remoteCmd.Run(); err != nil {
		return // no remote, skip push
	}

	// Pull-rebase before push so we don't accumulate divergent chains when
	// another machine has pushed in the meantime. Without this, the push
	// fails non-fast-forward, and because the push is detached + silent the
	// failure is invisible - divergence accumulates until manual recovery.
	// --autostash handles any uncommitted edits that snuck in between
	// the commit above and this pull.
	pullCmd := exec.Command("git", "-C", brainPath, "pull", "--rebase", "--autostash", "--quiet")
	if err := pullCmd.Run(); err != nil {
		ui.PrintVerbose("Brain pull-before-push", "failed: "+err.Error()+
			" - skipping push, will retry on next sync")
		return
	}

	// Fire-and-forget push - detached subprocess outlives parent
	pushCmd := exec.Command("git", "-C", brainPath, "push", "--quiet")
	pushCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	pushCmd.Stdout = nil
	pushCmd.Stderr = nil
	if err := pushCmd.Start(); err != nil {
		ui.PrintVerbose("Brain push", "failed to start: "+err.Error())
	}
	// Don't Wait() - let it run after we exit
}

// Sync does a manual full sync: pull then commit and push (blocking).
func Sync(brainPath, profile string) error {
	if !IsGitRepo(brainPath) {
		return fmt.Errorf("brain at %s is not a git repo - run 'aida brain init' first", brainPath)
	}

	// Pull
	cmd := exec.Command("git", "-C", brainPath, "pull", "--rebase")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git pull: %w", err)
	}

	// Stage
	cmd = exec.Command("git", "-C", brainPath, "add", "-A")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git add: %w", err)
	}

	// Check if anything to commit
	status := exec.Command("git", "-C", brainPath, "status", "--porcelain")
	out, _ := status.Output()
	if len(strings.TrimSpace(string(out))) == 0 {
		fmt.Println("Brain is up to date - nothing to commit.")
		return nil
	}

	// Commit
	msg := fmt.Sprintf("auto: %s %s", profile, time.Now().UTC().Format("2006-01-02T15:04:05"))
	cmd = exec.Command("git", "-C", brainPath, "commit", "-m", msg)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git commit: %w", err)
	}

	// Push
	cmd = exec.Command("git", "-C", brainPath, "push")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git push: %w", err)
	}

	return nil
}
