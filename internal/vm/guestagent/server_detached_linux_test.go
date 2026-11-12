// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// finish waits for the detached exec to report done and returns its state, its output and how long
// the wait took.
func finish(t *testing.T, c *Client, id string) (ExecState, string, time.Duration) {
	t.Helper()
	start := time.Now()
	var st ExecState
	var err error
	for st, err = c.State(ctxT(t), id); err == nil && st.Running; st, err = c.State(ctxT(t), id) {
		if time.Since(start) > 15*time.Second {
			t.Fatalf("exec %s still running after 15s", id)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	took := time.Since(start)
	out, err := c.Output(ctxT(t), id, 0)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(out)
	out.Close()
	return st, string(b), took
}

// gone says whether pid no longer runs: it is not there, or it is a zombie waiting for its parent.
func gone(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}
	i := bytes.LastIndexByte(b, ')')
	return i >= 0 && i+2 < len(b) && b[i+2] == 'Z'
}

func TestDetachedExecEndsWhenItsProcessDoes(t *testing.T) {
	c := newRig(t).client(t)
	id, err := c.Start(ctxT(t), ExecRequest{Argv: []string{"sh", "-c", `sleep 30 & echo started $!; exit 7`}})
	if err != nil {
		t.Fatal(err)
	}
	st, out, took := finish(t, c, id)
	if st.ExitCode == nil || *st.ExitCode != 7 {
		t.Fatalf("state = %+v, want exit 7", st)
	}
	if took > 2*time.Second {
		t.Fatalf("the exec reported done after %v: it waited for the sleep that holds its output", took)
	}
	if !strings.HasPrefix(out, "started ") {
		t.Fatalf("output = %q, want the line the process wrote", out)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(out, "started ")))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !gone(pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !gone(pid) {
		t.Fatalf("the background sleep (pid %d) outlived its exec", pid)
	}
}

// A process outside the group (setsid) still holds the output: the exec is done once the drain's wait is
// over, with what it wrote kept.
func TestDetachedExecDoesNotWaitForAProcessOutsideItsGroup(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("no setsid")
	}
	c := newRig(t).client(t)
	pidFile := filepath.Join(t.TempDir(), "pid")
	t.Cleanup(func() { // the sleep has its own session, so the exec's kill does not reach it
		if b, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	id, err := c.Start(ctxT(t), ExecRequest{Argv: []string{"sh", "-c", `setsid sh -c 'echo $$ > "$0"; exec sleep 30' ` + pidFile + ` & until [ -s ` + pidFile + ` ]; do sleep 0.01; done; echo started; exit 3`}})
	if err != nil {
		t.Fatal(err)
	}
	st, out, took := finish(t, c, id)
	if st.ExitCode == nil || *st.ExitCode != 3 || out != "started\n" {
		t.Fatalf("state = %+v, output %q", st, out)
	}
	if took < drainWait/2 || took > drainWait+2*time.Second {
		t.Fatalf("the exec reported done after %v, want about %v (the drain's wait)", took, drainWait)
	}
}

func TestDetachedExecGetsATerminalWhenAskedFor(t *testing.T) {
	if _, err := os.Stat("/dev/ptmx"); err != nil {
		t.Skip("no /dev/ptmx")
	}
	c := newRig(t).client(t)
	for _, tc := range []struct {
		tty  bool
		want string
	}{{true, "tty"}, {false, "notty"}} {
		id, err := c.Start(ctxT(t), ExecRequest{Argv: []string{"sh", "-c", `if test -t 1; then echo tty; else echo notty; fi`}, TTY: tc.tty})
		if err != nil {
			t.Fatal(err)
		}
		st, out, _ := finish(t, c, id)
		if st.ExitCode == nil || *st.ExitCode != 0 || strings.TrimSpace(out) != tc.want {
			t.Errorf("TTY %v: state %+v, output %q, want %q", tc.tty, st, out, tc.want)
		}
	}
}

func TestDetachedExecWithATerminalEndsWithItsProcess(t *testing.T) {
	if _, err := os.Stat("/dev/ptmx"); err != nil {
		t.Skip("no /dev/ptmx")
	}
	c := newRig(t).client(t)
	id, err := c.Start(ctxT(t), ExecRequest{Argv: []string{"sh", "-c", `sleep 30 & echo started; exit 7`}, TTY: true})
	if err != nil {
		t.Fatal(err)
	}
	st, out, took := finish(t, c, id)
	if st.ExitCode == nil || *st.ExitCode != 7 || !strings.Contains(out, "started") || took > 2*time.Second {
		t.Fatalf("state %+v, output %q, took %v", st, out, took)
	}
}

func TestDetachedOutputIsCapped(t *testing.T) {
	for _, tc := range []struct {
		limit int64
		want  string
	}{{1000, "\n[output truncated at 1000 bytes]\n"}, {1 << 20, "\n[output truncated at 1 MiB]\n"}} {
		r := newRig(t)
		r.srv.OutputCap = tc.limit
		c := r.client(t)
		id, err := c.Start(ctxT(t), ExecRequest{Argv: []string{"sh", "-c", `head -c 3000000 /dev/zero | tr '\0' x; echo later`}})
		if err != nil {
			t.Fatal(err)
		}
		st, out, _ := finish(t, c, id)
		if st.ExitCode == nil || *st.ExitCode != 0 {
			t.Fatalf("limit %d: state = %+v: a capped exec still runs to its end", tc.limit, st)
		}
		if want := strings.Repeat("x", int(tc.limit)) + tc.want; out != want {
			t.Errorf("limit %d: output is %d bytes ending %q, want %d bytes ending %q", tc.limit, len(out), out[max(0, len(out)-50):], len(want), tc.want)
		}
	}
}

func TestDetachedOutputUnderTheCapIsWhole(t *testing.T) {
	r := newRig(t)
	r.srv.OutputCap = 1000
	c := r.client(t)
	id, err := c.Start(ctxT(t), ExecRequest{Argv: []string{"echo", "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, out, _ := finish(t, c, id); out != "hi\n" {
		t.Fatalf("output = %q", out)
	}
}

func TestInitWritesTheRunsCAFiles(t *testing.T) {
	r := newRig(t)
	dir := filepath.Join(t.TempDir(), "wardyn")
	roots := filepath.Join(t.TempDir(), "roots.crt")
	if err := os.WriteFile(roots, []byte("SYSTEM ROOTS\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.srv.CADir, r.srv.SystemRoots = dir, roots
	if _, err := r.client(t).Init(ctxT(t), Init{Env: map[string]string{runCAEnv: "-----BEGIN CERTIFICATE-----\nRUNCA\n-----END CERTIFICATE-----"}}); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"mitm-ca.pem":   "-----BEGIN CERTIFICATE-----\nRUNCA\n-----END CERTIFICATE-----\n",
		"ca-bundle.pem": "SYSTEM ROOTS\n-----BEGIN CERTIFICATE-----\nRUNCA\n-----END CERTIFICATE-----\n",
	} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		st, serr := os.Stat(filepath.Join(dir, name))
		if err != nil || serr != nil || string(b) != want || st.Mode().Perm() != 0o644 {
			t.Errorf("%s = %q, %v, mode %v; want %q at 0644", name, b, err, st, want)
		}
	}
	if st, err := os.Stat(dir); err != nil || st.Mode()&os.ModeSticky == 0 || st.Mode().Perm() != 0o777 {
		t.Errorf("CA dir = %v, %v; want sticky 1777", st, err)
	}
}

func TestInitWithoutTheRunsCAWritesNoCAFiles(t *testing.T) {
	r := newRig(t)
	dir := filepath.Join(t.TempDir(), "wardyn")
	r.srv.CADir = dir
	if _, err := r.client(t).Init(ctxT(t), Init{Env: map[string]string{"A": "1"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("CA dir after an Init with no CA: %v", err)
	}
}

func TestExecAfterAnAgentRestartIsRefused(t *testing.T) {
	boot := t.TempDir()
	if _, err := newRigAt(t, boot).client(t).Init(ctxT(t), Init{Env: map[string]string{"A": "1"}}); err != nil {
		t.Fatal(err)
	}
	c := newRigAt(t, boot).client(t)
	_, err := c.Exec(ctxT(t), ExecRequest{Argv: []string{"sh", "-c", `printf %s "$A"`}})
	if err == nil || !strings.Contains(err.Error(), "409 Conflict") || !strings.Contains(err.Error(), "restarted") {
		t.Fatalf("attached exec after a restart = %v, want 409", err)
	}
	if _, err := c.Start(ctxT(t), ExecRequest{Argv: []string{"true"}}); err == nil || !strings.Contains(err.Error(), "409 Conflict") {
		t.Fatalf("detached exec after a restart = %v, want 409", err)
	}
}
