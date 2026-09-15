//go:build !unix

package execx

import "os/exec"

// Non-unix stub: process groups + group-signaling aren't portable. We
// still get the WaitDelay defense, just without grandchild reap.

func applyProcessGroup(cmd *exec.Cmd) {}

func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}

func killGroupFinal(cmd *exec.Cmd) int { return 0 }
