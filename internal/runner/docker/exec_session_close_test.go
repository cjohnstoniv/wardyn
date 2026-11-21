// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// A Stdin.Write blocked on a daemon that reads nothing is woken by ExecSession.Close, which closes the
// hijacked connection and returns without waiting on the write.
func TestExecSessionClose_WakesBlockedStdinWrite(t *testing.T) {
	f := newFakeDocker()
	d, ref := execStreamSandbox(t, f)
	conn, far := net.Pipe() // far reads nothing, so a write on conn blocks
	defer far.Close()
	f.execAttachConn = conn
	sess, err := d.ExecStream(context.Background(), ref, runner.ExecSpec{Argv: []string{"cat"}, TTY: true})
	if err != nil {
		t.Fatalf("ExecStream: %v", err)
	}
	type result struct {
		at  time.Time
		err error
	}
	wrote := make(chan result, 1)
	go func() { _, err := sess.Stdin.Write([]byte("blocked")); wrote <- result{time.Now(), err} }()
	select {
	case w := <-wrote:
		t.Fatalf("Stdin.Write returned %v with nothing read", w.err)
	case <-time.After(200 * time.Millisecond):
	}
	start := time.Now()
	closed := make(chan struct{})
	go func() { _ = sess.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("ExecSession.Close did not return within 1s of a blocked Stdin.Write")
	}
	select {
	case w := <-wrote:
		if w.err == nil {
			t.Fatal("the blocked Stdin.Write reported success after Close")
		}
		if d := w.at.Sub(start); d > time.Second {
			t.Fatalf("the blocked Stdin.Write returned %s after Close, want within 1s", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ExecSession.Close did not wake the blocked Stdin.Write")
	}
}
