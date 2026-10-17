// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
)

// DeliverResident reads the Dispatch gate before the call, as CreateSandbox
// does: with runners turned off a resident value never reaches a connected
// runner, and once the gate opens the same delivery goes through.
func TestDeliverResident_DispatchGateRefusesBeforeTheWire(t *testing.T) {
	closed := errors.New("dispatch closed")
	var gateClosed atomic.Bool
	gateClosed.Store(true)
	var reads atomic.Int32
	link := &remote.Link{}
	sub := remote.New(rid, link, remote.Options{Dispatch: func(context.Context) error {
		reads.Add(1)
		if gateClosed.Load() {
			return closed
		}
		return nil
	}})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	org, run := runnerwire.Loopback()
	peer := link.Attach(ctx, org)
	served := runnertest.Serve(ctx, run, rid, runnertest.NewFake())
	t.Cleanup(func() { peer.Close(); served.Peer.Close() })

	a := runnerwire.DeliverResidentArgs{RunID: uuid.New(), GrantID: uuid.New(), Kind: "env_secret", Value: "resident-value", NotAfter: time.Now().Add(time.Hour)}
	if err := sub.DeliverResident(ctxT(t), a); !errors.Is(err, closed) || reads.Load() != 1 {
		t.Fatalf("deliver with the gate closed = %v after %d gate reads, want the gate's refusal after one", err, reads.Load())
	}
	if got := served.Resident[a.RunID]; len(got) != 0 {
		t.Fatalf("a delivery refused by the gate reached the runner: %+v", got)
	}
	gateClosed.Store(false)
	if err := sub.DeliverResident(ctxT(t), a); err != nil {
		t.Fatalf("deliver with the gate open = %v", err)
	}
	if got := served.Resident[a.RunID]; len(got) != 1 || got[0].GrantID != a.GrantID {
		t.Fatalf("the runner holds %+v after an admitted delivery", got)
	}
}
