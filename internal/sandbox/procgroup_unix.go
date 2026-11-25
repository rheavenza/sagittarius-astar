//go:build !windows

package sandbox

import (
	"os/exec"
	"syscall"
)

// configureProcessGroup starts cmd in a new process group and makes context
// cancellation SIGKILL the entire group (bash plus any children it spawned).
func configureProcessGroup(cmd *exec.Cmd) {
	setProcessGroup(cmd)
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

// killTree signals cmd's whole process group: SIGTERM, or SIGKILL when force.
func killTree(cmd *exec.Cmd, force bool) {
	if cmd.Process == nil {
		return
	}
	sig := syscall.SIGTERM
	if force {
		sig = syscall.SIGKILL
	}
	if err := syscall.Kill(-cmd.Process.Pid, sig); err != nil {
		_ = cmd.Process.Signal(sig)
	}
}

// setProcessGroup starts cmd in a new process group, so killTree reaches
// everything it starts.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
