// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package cli

import (
	"os/exec"
	"syscall"
)

// setProcGroup puts the child in its own process group so a wrapper shell's
// grandchildren can be reaped together on cancel/timeout.
func setProcGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killProcGroup SIGKILLs the child's entire process group (negative PID), then
// the child itself as a fallback if the group could not be addressed.
func killProcGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	pid := cmd.Process.Pid
	if err := syscall.Kill(-pid, syscall.SIGKILL); err == nil {
		return nil
	}
	return cmd.Process.Kill()
}
