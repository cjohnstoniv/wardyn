// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// liveCtxBroker records whether a revoke reached the broker on a context that had already ended.
type liveCtxBroker struct {
	*flakyBroker
	ended atomic.Bool
}

func (b *liveCtxBroker) RevokeRun(ctx context.Context, id uuid.UUID) error {
	if ctx.Err() != nil {
		b.ended.Store(true)
	}
	return b.flakyBroker.RevokeRun(ctx, id)
}

// TestRevoke_AHungStepLeavesTheStepsAfterItALiveContext: an identity revoke that never answers ends at
// revokeStepBound, and the broker revoke after it still runs; the revoke stays owed for the queue.
func TestRevoke_AHungStepLeavesTheStepsAfterItALiveContext(t *testing.T) {
	old := revokeStepBound
	revokeStepBound = 50 * time.Millisecond
	t.Cleanup(func() { revokeStepBound = old })
	st, brk, id := newOwedStore(), newFlakyBroker(0), uuid.New()
	srv, _ := revokeOwedServer(t, st, brk)
	idp := &hungRevokeIdentity{Provider: srv.cfg.Identity}
	srv.cfg.Identity = idp
	live := &liveCtxBroker{flakyBroker: brk}
	srv.cfg.Broker = live
	done := make(chan error, 1)
	go func() { done <- srv.revokeRunCascade(context.Background(), id) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the revoke reported success; its identity step never answered")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a hung identity revoke held the cascade past its step bound")
	}
	if idp.calls.Load() != 1 || brk.callsFor(id) != 1 || live.ended.Load() || !st.isOwed(id) {
		t.Fatalf("identity calls %d, broker calls %d, broker context ended %v, owed %v; want both steps run, the second on a live context, and the revoke owed",
			idp.calls.Load(), brk.callsFor(id), live.ended.Load(), st.isOwed(id))
	}
}

// TestRevoke_AMarkThatCouldNotBeRecordedIsWrittenAgain: when both of a revoke's owed marks fail, the mark is written
// again in the background until it lands, so the revoke queue still retries the run.
func TestRevoke_AMarkThatCouldNotBeRecordedIsWrittenAgain(t *testing.T) {
	old := revokeRemarkEvery
	revokeRemarkEvery = 20 * time.Millisecond
	t.Cleanup(func() { revokeRemarkEvery = old })
	st, brk, id := newOwedStore(), newFlakyBroker(1), uuid.New()
	srv, _ := revokeOwedServer(t, st, brk)
	st.mu.Lock()
	st.markErrs = 3
	st.mu.Unlock()
	if err := srv.revokeRunCascade(context.Background(), id); err == nil {
		t.Fatal("the revoke succeeded; the broker was set to fail")
	}
	waitFor(t, "the owed mark, written again", func() bool { return st.isOwed(id) })
	waitFor(t, "the re-mark loop to end", func() bool {
		_, busy := revokeRemarks.Load(revokeRemarkKey{srv, id})
		return !busy
	})
	if n := st.markCalls(); n != 4 {
		t.Fatalf("%d mark calls, want 4: two that failed inline, one that failed in the loop, one that landed", n)
	}
}
