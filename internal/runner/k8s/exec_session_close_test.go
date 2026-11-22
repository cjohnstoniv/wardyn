// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// A Stdin.Write blocked on an exec stream that reads nothing is woken by ExecSession.Close, which
// cancels the stream and closes the stdin pipe and returns without waiting on the write.
func TestExecSessionClose_WakesBlockedStdinWrite(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	ref := createAgentPodFixture(t, cs, uuid.New(), "wardyn/agent-claude:local", nil)
	fe := newFakeExecutor() // its stream never reads opts.Stdin
	fe.silent = true
	d.execFactory = fe.execFactory
	sess, err := d.ExecStream(context.Background(), ref, runner.ExecSpec{Argv: []string{"cat"}, TTY: true})
	if err != nil {
		t.Fatalf("ExecStream: %v", err)
	}
	fe.waitStarted(t)
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
