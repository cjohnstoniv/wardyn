// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package runner

import (
	"errors"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// TestAgentIdleScript_ExitsOnSIGTERM pins #468: the idle main process must exit
// promptly on TERM, with the usual signal exit code 143 (not 0 — a downstream
// probe maps ExitCode==0 to RunCompleted, and an out-of-band container/pod
// stop must still read as a kill, not success). `exec sleep infinity` fails
// this (TERM kills it, not a clean exit), and as PID 1 it would ignore TERM
// and sit out the full kill timeout.
func TestAgentIdleScript_ExitsOnSIGTERM(t *testing.T) {
	cmd := exec.Command("sh", "-c", AgentIdleScript)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start idle script: %v", err)
	}
	// The trap exits without reaping the backgrounded sleep; kill the group.
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	time.Sleep(300 * time.Millisecond) // let the trap install
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("idle script on SIGTERM: %v, want *exec.ExitError with code 143", err)
		}
		if code := exitErr.ExitCode(); code != 143 {
			t.Fatalf("idle script on SIGTERM: exit code %d, want 143", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("idle script did not exit within 2s of SIGTERM")
	}
}
