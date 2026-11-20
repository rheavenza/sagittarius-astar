//go:build windows

package sandbox

import "os/exec"

// configureProcessGroup is a no-op on Windows; exec.CommandContext's default
// Process.Kill is used.
func configureProcessGroup(cmd *exec.Cmd) {}
