// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnertest

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// A TTY exec has no stderr pipe; a plain exec keeps its argv line on stderr; Close ends either, with
// the stderr line unread, and releases the stdout reader.
func TestFakeExecStreamPipesAndClose(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	sb, err := f.CreateSandbox(ctx, runner.SandboxSpec{RunID: uuid.New()})
	if err != nil {
		t.Fatal(err)
	}
	tty, err := f.ExecStream(ctx, sb.Ref, runner.ExecSpec{Argv: []string{"sh"}, TTY: true})
	if err != nil || tty.Stderr != nil {
		t.Fatalf("tty exec: stderr %v, err %v; want no stderr", tty.Stderr, err)
	}
	plain, err := f.ExecStream(ctx, sb.Ref, runner.ExecSpec{Argv: []string{"sh"}})
	if err != nil || plain.Stderr == nil {
		t.Fatalf("plain exec: stderr %v, err %v; want a stderr pipe", plain.Stderr, err)
	}
	if b, _ := io.ReadAll(plain.Stderr); string(b) != "argv:sh\n" {
		t.Errorf("stderr = %q", b)
	}
	for name, s := range map[string]*runner.ExecSession{"tty": tty, "plain": plain} {
		if err := s.Close(); err != nil {
			t.Errorf("%s: close = %v", name, err)
		}
		if _, err := s.Stdout.Read(make([]byte, 1)); err == nil {
			t.Errorf("%s: stdout still readable after Close", name)
		}
	}
	// Closed before stderr is read: the writer goroutine ends with Close, and a TTY exec never had one.
	early, _ := f.ExecStream(ctx, sb.Ref, runner.ExecSpec{Argv: []string{"sh"}})
	ttyEarly, _ := f.ExecStream(ctx, sb.Ref, runner.ExecSpec{Argv: []string{"sh"}, TTY: true})
	for _, s := range []*runner.ExecSession{early, ttyEarly} {
		if err := s.Close(); err != nil {
			t.Errorf("early close = %v", err)
		}
	}
	ended := make(chan struct{})
	go func() { f.stderrWriters.Wait(); close(ended) }()
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("a stderr writer goroutine outlived Close")
	}
}
