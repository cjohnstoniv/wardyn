// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// hungAudit is an audit store that never answers: Record returns only when its context ends, and reports how that
// context ended on ended.
type hungAudit struct{ ended chan error }

func (a *hungAudit) Record(ctx context.Context, _ types.AuditEvent) error {
	<-ctx.Done()
	a.ended <- ctx.Err()
	return ctx.Err()
}

// A stream's start-failure row, written on a channel context that has already ended, ends at one audit row's bound
// (heldStoreCallBound) when the store never answers, and the store sees that bound's deadline. Catches: the row
// written on a context with no end, which holds the door, and a daemon stop behind it, as long as the store hangs.
func TestRecordStreamAuditDetached_AWriteThatNeverAnswersEndsAtItsBound(t *testing.T) {
	old := heldStoreCallBound
	heldStoreCallBound = 50 * time.Millisecond
	t.Cleanup(func() { heldStoreCallBound = old })
	a := &hungAudit{ended: make(chan error, 1)}
	s := &Server{cfg: Config{Audit: a, Now: time.Now}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // the client's channel is gone
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.recordStreamAuditDetached(ctx, "container", types.AuditEvent{ActorType: types.ActorHuman, Actor: "alice", Action: "ssh.exec", Outcome: "failure"})
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a start-failure row on a store that never answers held its door for 10s: the write has no bound")
	}
	select {
	case err := <-a.ended:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("the store's call ended with %v, want its bound's deadline", err)
		}
	default:
		t.Fatal("the row never reached the store")
	}
}
