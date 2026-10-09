// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnertest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Rig is a remote.Substrate wired to a served runner over the in-memory
// loopback: the whole stream, without a wardyn-runnerd.
type Rig struct {
	RunnerID string
	Sub      *remote.Substrate
	Link     *remote.Link
	Server   *Server
	Fake     *Fake
	Org      *runnerwire.LoopConn // fault injection: Org.Drop(), SetDelay, SeverMidFrame
	Run      *runnerwire.LoopConn

	mu     sync.Mutex
	queued []string // "kind ref", in order
	events []runnerwire.Event
}

// NewRig connects a fresh fake runner for runnerID and waits for its `caps`.
func NewRig(tb testing.TB, runnerID string) *Rig {
	tb.Helper()
	r := &Rig{RunnerID: runnerID, Fake: NewFake(), Link: &remote.Link{}}
	r.Sub = remote.New(runnerID, r.Link, remote.Options{
		Queue: func(_ context.Context, kind types.RunnerActionKind, ref string) error {
			r.mu.Lock()
			r.queued = append(r.queued, string(kind)+" "+ref)
			r.mu.Unlock()
			return nil
		},
		OnEvent: func(ev runnerwire.Event) {
			r.mu.Lock()
			r.events = append(r.events, ev)
			r.mu.Unlock()
		},
	})
	r.Connect(tb)
	return r
}

// Connect opens a new session (a reconnect after a Drop) and publishes caps.
func (r *Rig) Connect(tb testing.TB) {
	tb.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	tb.Cleanup(cancel)
	org, run := runnerwire.Loopback()
	r.Org, r.Run = org, run
	peer := r.Link.Attach(ctx, org)
	r.Server = Serve(ctx, run, r.RunnerID, r.Fake)
	tb.Cleanup(func() { peer.Close(); r.Server.Peer.Close() })
	if err := r.Server.PublishCaps(ctx, placement.Capacity{CPUMillisMax: 4000, MemoryMiBMax: 8192}, []string{"/work"}, "test"); err != nil {
		tb.Fatalf("publish caps: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok := r.Sub.Caps(); ok {
			return
		}
		if time.Now().After(deadline) {
			tb.Fatal("the runner's caps never arrived")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Queued is the pending actions the substrate queued, "kind ref", in order.
func (r *Rig) Queued() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.queued...)
}

// Events is every non-caps, non-waiting event the org received.
func (r *Rig) Events() []runnerwire.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]runnerwire.Event(nil), r.events...)
}
