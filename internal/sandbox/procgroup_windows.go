//go:build windows

package sandbox

import (
	"os/exec"
	"strconv"
)

// configureProcessGroup is a no-op on Windows; exec.CommandContext's default
// Process.Kill is used.
func configureProcessGroup(cmd *exec.Cmd) {}

// killTree ends cmd and its child processes (taskkill /T); force adds /F.
func killTree(cmd *exec.Cmd, force bool) {
	if cmd.Process == nil {
		return
	}
	args := []string{"/T", "/PID", strconv.Itoa(cmd.Process.Pid)}
	if force {
		args = append([]string{"/F"}, args...)
	}
	if err := exec.Command("taskkill", args...).Run(); err != nil && force {
		_ = cmd.Process.Kill()
	}
}

// setProcessGroup is a no-op on Windows: killTree uses taskkill /T.
func setProcessGroup(cmd *exec.Cmd) {}
