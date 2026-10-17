// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// hungStop answers StopProxy only when its context ends, as a wedged engine would.
type hungStop struct{ *localSub }

func (hungStop) StopProxy(ctx context.Context, _ string) error {
	<-ctx.Done()
	return ctx.Err()
}

// A resumed session runs containment before its peer starts, so no link failure ends it: a stop the
// engine never answers fails at beforeReadyContainBound instead of holding the reconnect for ever.
func TestBeforeReadyGivesUpOnAStopThatNeverAnswers(t *testing.T) {
	prev := beforeReadyContainBound
	beforeReadyContainBound = 50 * time.Millisecond
	t.Cleanup(func() { beforeReadyContainBound = prev })
	m, sub, elapsed, org := managerFixture(t)
	createLocal(t, m, org)
	m.Offline(context.Background())
	*elapsed = identity.RunTokenLapseAfter
	m.stopper = hungStop{sub}
	done := make(chan error, 1)
	go func() { done <- m.BeforeReady(context.Background(), &runnerwire.Peer{}, true) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("BeforeReady = %v, want the containment deadline", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("BeforeReady still waits on a stop that never answers")
	}
}
