//go:build unix

package execx

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// applyProcessGroup puts the child in its own process group so we can
// kill the group (and all its descendants) in one shot on timeout.
func applyProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killGroup is wired into cmd.Cancel. When ctx cancels, this sends
// SIGTERM to the child's entire process group so grandchildren die too,
// not just the direct child.
//
// Returns os.ErrProcessDone if the child has already exited (the exec
// package recognizes this and treats the cancel as a no-op).
func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return os.ErrProcessDone
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		// Child likely already exited; fall back to direct signal.
		if signalErr := cmd.Process.Signal(syscall.SIGTERM); signalErr != nil {
			if errors.Is(signalErr, os.ErrProcessDone) {
				return os.ErrProcessDone
			}
		}
		return os.ErrProcessDone
	}
	if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

// killGroupFinal sends SIGKILL to the child's process group after Wait
// has returned. Best-effort cleanup for anything that ignored SIGTERM.
// Returns 1 if the signal landed on at least one process in the group,
// 0 otherwise. We can't easily count exact descendants portably.
func killGroupFinal(cmd *exec.Cmd) int {
	if cmd.Process == nil {
		return 0
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		return 0
	}
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil {
		return 0
	}
	return 1
}
