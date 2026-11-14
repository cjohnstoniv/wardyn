// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A Put's clean-up runs on its own deadline, six per-call timeouts, not on the
// caller's context: ending the caller's context does not stop it, and a
// clean-up that stalls ends by that deadline.
func TestAfterPut_EndsByItsOwnBound(t *testing.T) {
	s := &Store{extTimeout: 50 * time.Millisecond}
	caller, cancel := context.WithCancel(t.Context())
	start := time.Now()
	var took time.Duration
	var cause error
	s.afterPut(caller, func(ctx context.Context) {
		<-ctx.Done()
		took, cause = time.Since(start), ctx.Err()
	})
	cancel()
	WaitForCleanUps()
	if !errors.Is(cause, context.DeadlineExceeded) || took < 300*time.Millisecond || took > 20*time.Second {
		t.Fatalf("clean-up ended after %v with %v; want its own 300ms deadline, not the caller's cancellation", took, cause)
	}
}

// A clean-up that panics ends alone: it runs on a goroutine no caller's
// recovery covers, so an uncontained panic there would end the process.
func TestAfterPut_ContainsAPanic(t *testing.T) {
	s := &Store{extTimeout: 50 * time.Millisecond}
	s.afterPut(t.Context(), func(context.Context) { panic("the store client broke") })
	WaitForCleanUps()
}
