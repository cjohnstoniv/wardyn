// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package runner

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// runExecCAWait runs ExecCAWait, its paths moved to dir and its bound to tries, ahead of command,
// as sshExecArgv runs a person's command; env is the run's environment.
func runExecCAWait(t *testing.T, dir, tries, command string, env ...string) (string, string, int) {
	t.Helper()
	script := strings.ReplaceAll(ExecCAWait, "/tmp/wardyn/", dir+"/")
	script = strings.Replace(script, "-ge 75 ]", "-ge "+tries+" ]", 1)
	cmd := exec.Command("/bin/sh", "-c", script+`exec /bin/sh -c "$1"`, "sh", command)
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH")}, env...)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		t.Fatalf("run: %v", err)
	}
	return stdout.String(), stderr.String(), cmd.ProcessState.ExitCode()
}

// TestExecCAWait_HoldsFirstExecForCAFiles: a run reads RUNNING before its idle process has written
// the CA files its variables name; the first exec waits for them instead of failing its TLS call.
func TestExecCAWait_HoldsFirstExecForCAFiles(t *testing.T) {
	dir := t.TempDir()
	go func() {
		time.Sleep(time.Second)
		_ = os.WriteFile(filepath.Join(dir, "mitm-ca.pem"), []byte("CA\n"), 0o644)
		_ = os.WriteFile(filepath.Join(dir, "ca-bundle.pem"), []byte("ROOTS\nCA\n"), 0o644)
	}()
	out, errOut, code := runExecCAWait(t, dir, "75", `cat "$SSL_CERT_FILE"`,
		"SSL_CERT_FILE="+dir+"/ca-bundle.pem", "WARDYN_MITM_CA_PEM=CA")
	if code != 0 || out != "ROOTS\nCA\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q; want 0 and the bundle the idle process wrote", code, out, errOut)
	}
}

// TestExecCAWait_NoCANoWait: a run with no CA (no TLS inspection) runs the command at once.
func TestExecCAWait_NoCANoWait(t *testing.T) {
	start := time.Now()
	out, _, code := runExecCAWait(t, t.TempDir(), "75", "echo ran")
	if code != 0 || out != "ran\n" || time.Since(start) > 2*time.Second {
		t.Fatalf("exit %d, stdout %q after %s; want 0, ran, no wait", code, out, time.Since(start))
	}
}

// TestExecCAWait_RefusesWhenFilesNeverCome: past the bound the command does not run, and the sentence says why.
func TestExecCAWait_RefusesWhenFilesNeverCome(t *testing.T) {
	dir := t.TempDir()
	out, errOut, code := runExecCAWait(t, dir, "2", "echo ran",
		"SSL_CERT_FILE="+dir+"/ca-bundle.pem", "WARDYN_MITM_CA_PEM=CA")
	if code != 1 || out != "" || !strings.Contains(errOut, "were not written within 15 seconds, so the command did not run") {
		t.Fatalf("exit %d, stdout %q, stderr %q; want 1, no output, the refusal", code, out, errOut)
	}
}
