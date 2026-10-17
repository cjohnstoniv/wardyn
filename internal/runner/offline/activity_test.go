// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"context"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runneractivity"
	"github.com/cjohnstoniv/wardyn/internal/runnerspool"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

func TestActivityUsesStartedPeerAndNeverReplaysOldMarker(t *testing.T) {
	for _, wire := range []bool{false, true} {
		t.Run(map[bool]string{false: "loopback", true: "websocket"}[wire], func(t *testing.T) {
			sub := &localSub{status: runner.Status{State: types.RunRunning}}
			f := connectWireManager(t, t.TempDir(), sub, uuid.UUID.String, wire)
			m := f.manager
			id := createLocal(t, m, f.orgTime)
			spool, err := runnerspool.Open(m.activityDir(id), id)
			if err != nil {
				t.Fatal(err)
			}
			defer spool.Close()
			m.mu.Lock()
			m.publishActivity()
			epoch := m.activityEpoch
			m.mu.Unlock()
			ctx, cancel := context.WithCancel(f.ctx)
			defer cancel()
			done := make(chan struct{})
			go func() { defer close(done); m.activityLoop(ctx) }()
			defer func() { cancel(); <-done }()
			if err := runneractivity.Record(m.activityDir(id), epoch); err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-f.activity:
				if got != id {
					t.Fatal("wrong run")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("live activity did not reach org")
			}
			// Same marker is not another action. Changing to a previous connection's
			// epoch is also not an action, even if a durable file remains on disk.
			select {
			case <-f.activity:
				t.Fatal("duplicate marker replayed")
			case <-time.After(1100 * time.Millisecond):
			}
			if err := runneractivity.Record(m.activityDir(id), uuid.New()); err != nil {
				t.Fatal(err)
			}
			select {
			case <-f.activity:
				t.Fatal("old epoch replayed")
			case <-time.After(1100 * time.Millisecond):
			}
			m.Offline(ctx)
			if runneractivity.Capture(m.activityDir(id)) != uuid.Nil {
				t.Fatal("offline proxy retained activity epoch")
			}
		})
	}
}

// A retired run is not published to or scanned for activity: it has none, and the per-second loop
// would otherwise walk every run the runner ever held.
func TestActivitySkipsRetiredRuns(t *testing.T) {
	sub := &localSub{status: runner.Status{State: types.RunRunning}}
	f := connectWireManager(t, t.TempDir(), sub, uuid.UUID.String, false)
	m := f.manager
	live, retired := createLocal(t, m, f.orgTime), createLocal(t, m, f.orgTime)
	for _, id := range []uuid.UUID{live, retired} {
		spool, err := runnerspool.Open(m.activityDir(id), id)
		if err != nil {
			t.Fatal(err)
		}
		defer spool.Close()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.state.Runs[retired]
	r.Retired = true
	m.state.Runs[retired] = r
	m.publishActivity()
	if runneractivity.Capture(m.activityDir(live)) == uuid.Nil {
		t.Error("a live run was not published to")
	}
	if runneractivity.Capture(m.activityDir(retired)) != uuid.Nil {
		t.Error("a retired run was published to")
	}
	if ids := m.liveRunIDs(); len(ids) != 1 || ids[0] != live {
		t.Errorf("liveRunIDs = %v, want only %v", ids, live)
	}
}
