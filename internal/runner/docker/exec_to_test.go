// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"bytes"
	"context"
	"testing"
	"time"
)

var selftestArgv = []string{"/usr/local/bin/agent-run", "--selftest"}

// waitDrains waits until out has begun and ended want drains.
func waitDrains(t *testing.T, out *drainSpyBuffer, want int) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		begun, ended, err := out.counts()
		if begun == want && ended == want && err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("drains begun %d ended %d err %v, want %d clean", begun, ended, err, want)
		}
	}
}

// TestExec_SelftestOutputStillTeesIntoExecOutput: the local path is unchanged.
// Exec copies a BYOI selftest's output into the sandbox's ExecOutput as one
// drain, like any other exec.
func TestExec_SelftestOutputStillTeesIntoExecOutput(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	f.execAttachConn = &bufConn{r: bytes.NewReader([]byte("selftest ok\r\n"))}
	d := newTestDriver(f)
	out := &drainSpyBuffer{}
	spec := testSpec()
	spec.ExecOutput = out
	sb, err := d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if _, err := d.Exec(context.Background(), sb.Ref, selftestArgv); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	out.waitFor(t, "selftest ok\r\n")
	waitDrains(t, out, 1)
}

// closeSpy is an ExecTo writer that records its bytes and its Close.
type closeSpy struct {
	lockedBuffer
	closed chan struct{}
}

func (c *closeSpy) Close() error { close(c.closed); return nil }

// TestExecTo_SendsOneExecsOutputToItsOwnWriter: ExecTo's output goes to the
// writer it is given, which is closed at EOF; the sandbox's ExecOutput sees no
// byte and no drain from it, and Wait still tracks that exec. The next Exec
// copies into ExecOutput as before.
func TestExecTo_SendsOneExecsOutputToItsOwnWriter(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	f.execAttachConn = &bufConn{r: bytes.NewReader([]byte("selftest ok\r\n"))}
	d := newTestDriver(f)
	out := &drainSpyBuffer{}
	spec := testSpec()
	spec.ExecOutput = out
	sb, err := d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	own := &closeSpy{closed: make(chan struct{})}
	id, err := d.ExecTo(context.Background(), sb.Ref, selftestArgv, own)
	if err != nil {
		t.Fatalf("ExecTo: %v", err)
	}
	select {
	case <-own.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("ExecTo's writer was never closed")
	}
	own.waitFor(t, "selftest ok\r\n")
	d.mu.Lock()
	tracked := d.agentExecs[sb.Ref]
	d.mu.Unlock()
	if tracked != id {
		t.Fatalf("tracked exec = %q, want the selftest's %q", tracked, id)
	}
	out.mu.Lock()
	leaked := out.buf.String()
	out.mu.Unlock()
	if begun, _, _ := out.counts(); begun != 0 || leaked != "" {
		t.Fatalf("ExecOutput saw %d drains and %q", begun, leaked)
	}

	f.mu.Lock()
	f.execAttachConn = &bufConn{r: bytes.NewReader([]byte("task out\r\n"))}
	f.mu.Unlock()
	if _, err := d.Exec(context.Background(), sb.Ref, []string{"/usr/local/bin/agent-run", "task"}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	out.waitFor(t, "task out\r\n")
	waitDrains(t, out, 1)
}
