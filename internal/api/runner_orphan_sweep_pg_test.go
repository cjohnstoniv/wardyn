// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// readySeen is one READY the runner side saw: its peer, and whether the session was resumed.
type readySeen struct {
	peer    *runnerwire.Peer
	resumed bool
}

// A runner's reconnect runs the READY sweep on both kinds of session the production Hub admits, a fresh one and
// then, on the same client, a resumed one, and neither removes an ended run's sandbox kept for its copy back:
// each pass lists the kept run and a run with no row, and asks the runner to remove only the latter. The keep is
// decided here as a runner that is not here (it sent no caps), a keep all the same; the connected
// and fenced keep is TestReadySweep_KeepsAnOwedSandboxOnAFencedRunner. Catches: the READY sweep calling every ended
// run an orphan, on either session, and either Hub path no longer telling the server READY.
func TestReadySweepPG_FreshAndResumedSessionsKeepAKeptSandbox(t *testing.T) {
	f := newAdmissionFixture(t)
	kept := newFinalizeRun()
	kept.CreatedBy, kept.Placement, kept.RunnerID, kept.State = f.run.CreatedBy, types.PlacementLocal, &f.rn.ID, types.RunCompleted
	kept.SandboxRef = "runner:" + f.rn.ID.String() + "/kept"
	kept.LocalFolder = &types.RunLocalFolder{Name: "my-app", RemoteDir: "/home/agent/work", RunnerID: &f.rn.ID,
		Path: "/home/p/my-app", Direction: types.ComputerFolderBoth}
	if _, err := f.st.CreateRun(f.ctx, kept); err != nil {
		t.Fatal(err)
	}
	if err := f.st.RaiseRunDataOut(f.ctx, kept.ID, types.TransferAllowed); err != nil { // a dispatched run's rule (keepRunDataOut)
		t.Fatal(err)
	}
	if ok, err := f.st.SetRunLocalFolderStatusIf(f.ctx, kept.ID, store.FolderStatusGuard{},
		types.RunLocalFolderStatus{Resume: &types.FolderResume{Unsynced: 1}}); !ok || err != nil {
		t.Fatalf("kept record = %v, %v; want it written", ok, err)
	}
	rowless := uuid.New()
	removals, ready := make(chan []uuid.UUID, 4), make(chan readySeen, 2)
	ctx, cancel := context.WithCancel(f.ctx)
	t.Cleanup(cancel)
	hub := f.srv.runnerHub(f.st)
	var serves sync.WaitGroup
	hooks := runnerwire.RunnerHooks{
		Call: func(_ context.Context, _ *runnerwire.Peer, _ uint32, method string, raw json.RawMessage) (any, error) {
			if method != runnerwire.MethodSweep {
				return nil, errors.New("only the orphan sweep is answered here")
			}
			var args runnerwire.SweepArgs
			if err := json.Unmarshal(raw, &args); err != nil {
				return nil, err
			}
			if len(args.Remove) == 0 {
				return runnerwire.SweepResult{Candidates: []uuid.UUID{kept.ID, rowless}}, nil
			}
			removals <- args.Remove
			return runnerwire.SweepResult{Removed: len(args.Remove)}, nil
		},
		Open:    func(*runnerwire.Peer, *runnerwire.Stream, runnerwire.Open) error { return runnerwire.ErrNotReady },
		Clock:   func(runnerwire.Hello, time.Time) error { return nil },
		Pending: func(context.Context, *runnerwire.Peer, []runnerwire.PendingAction) error { return nil },
		StatePages: func(_ context.Context, send func(runnerwire.StatePage) error) error {
			return send(runnerwire.StatePage{InventoryID: uuid.New(), Runs: []runnerwire.RunState{f.liveRow()}, Final: true})
		},
		Replay:      func(context.Context, *runnerwire.Peer) error { return nil },
		BeforeReady: func(context.Context, *runnerwire.Peer, bool) error { return nil },
		Lease:       func(context.Context, runnerwire.Lease) error { return nil },
		Revoked:     func(context.Context) error { return nil },
		Offline:     func(context.Context) {},
		Online:      func(p *runnerwire.Peer, resumed bool) { ready <- readySeen{p, resumed} },
	}
	client, err := runnerwire.NewClient(runnerwire.ClientConfig{OrgURL: admissionOrgURL, RunnerID: f.rn.ID.String(), PrivateKey: f.key, Hooks: hooks,
		Dial: func(context.Context) (runnerwire.Conn, error) {
			org, run := runnerwire.Loopback()
			serves.Add(1)
			go func() {
				defer serves.Done()
				_ = hub.Serve(ctx, org, f.rn.ID)
			}()
			return run, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	ended := make(chan error, 1)
	t.Cleanup(func() {
		cancel()
		if !joinedWithin(serves.Wait, 10*time.Second) {
			t.Errorf("the hub's side of the sessions had not ended 10s after they were cancelled")
		}
	})
	for _, wantResumed := range []bool{false, true} {
		go func() { ended <- client.Connect(ctx) }()
		var on readySeen
		select {
		case on = <-ready:
		case err := <-ended:
			t.Fatalf("resumed=%v: the session ended before READY: %v", wantResumed, err)
		case <-time.After(10 * time.Second):
			t.Fatalf("resumed=%v: the session never reached READY", wantResumed)
		}
		if on.resumed != wantResumed {
			t.Fatalf("the session's resumed = %v; want %v", on.resumed, wantResumed)
		}
		select {
		case removed := <-removals:
			if !slices.Equal(removed, []uuid.UUID{rowless}) {
				t.Errorf("resumed=%v: the READY sweep removed %v; want only the run with no row, %v (kept %v)", wantResumed, removed, rowless, kept.ID)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("resumed=%v: the READY sweep never asked the runner to remove anything", wantResumed)
		}
		on.peer.Close()
		select {
		case <-ended:
		case <-time.After(10 * time.Second):
			t.Fatalf("resumed=%v: the runner's connection had not ended 10s after its peer closed", wantResumed)
		}
	}
}
