//go:build !windows

package sandbox

import (
	"os/exec"
	"syscall"
)

// configureProcessGroup starts cmd in a new process group and makes context
// cancellation SIGKILL the entire group (bash plus any children it spawned).
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Negative PID targets the whole process group.
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
}
