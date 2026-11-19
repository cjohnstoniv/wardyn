// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// TestWaitExecWithin_AnExecThatDoesNotEndEndsAtItsContext: an exec whose Wait never returns, as a remote runner's
// can when the runner stops answering, ends at the caller's context; one that ends answers its output and exit code.
func TestWaitExecWithin_AnExecThatDoesNotEndEndsAtItsContext(t *testing.T) {
	t.Run("a wait that never returns", func(t *testing.T) {
		never := make(chan struct{})
		t.Cleanup(func() { close(never) })
		sess := &runner.ExecSession{
			Stdout: strings.NewReader(""),
			Wait:   func() (int, error) { <-never; return 0, nil },
		}
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		done := make(chan error, 1)
		go func() { _, _, err := waitExecWithin(ctx, sess, 8<<10); done <- err }()
		select {
		case err := <-done:
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("err %v, want the context's deadline", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the wait outlived its context")
		}
	})
	t.Run("an exec that ends", func(t *testing.T) {
		sess := &runner.ExecSession{
			Stdout: strings.NewReader("stopped\n"),
			Wait:   func() (int, error) { return 3, nil },
		}
		out, code, err := waitExecWithin(context.Background(), sess, 8<<10)
		if out != "stopped\n" || code != 3 || err != nil {
			t.Fatalf("got %q %d %v, want the exec's output and exit code", out, code, err)
		}
	})
}
