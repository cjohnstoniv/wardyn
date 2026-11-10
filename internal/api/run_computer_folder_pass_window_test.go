// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// passWindowStore is refStore that lists its run, as the orphan passes read it.
type passWindowStore struct{ refStore }

func (s passWindowStore) ListRuns(ctx context.Context) ([]types.AgentRun, error) {
	run, err := s.GetRun(ctx, uuid.Nil)
	return []types.AgentRun{run}, err
}

func (passWindowStore) LiveImageCheckAttempts(context.Context) (map[uuid.UUID]bool, error) {
	return nil, nil
}

// passWindowSandbox is agentSandbox whose substrate also lists its one sandbox, run's, for the orphan sweep,
// recording that sweep's verdict.
type passWindowSandbox struct {
	*agentSandbox
	run    uuid.UUID
	orphan bool
}

func (r *passWindowSandbox) SweepOrphanedSandboxes(_ context.Context, _ time.Duration, isOrphan func(uuid.UUID) bool) (int, error) {
	r.orphan = isOrphan(r.run)
	return 0, nil
}

// An end holds the run's lock from its terminal write through its computer folder's pass back: while that pass
// runs, the run is terminal with its sandbox ref set and no resume recorded, and neither the terminal-sandbox
// sweep nor either orphan pass stops the sandbox under it. Only the end's own stop, after the pass, does.
func TestBringComputerFolderBack_NoSweepStopsTheSandboxDuringItsPass(t *testing.T) {
	s, cs, _, run, sb, _ := bringBackFixture(t)
	withoutRunner(cs)
	rn := &passWindowSandbox{agentSandbox: sb, run: run.ID}
	s.cfg.Runner, s.cfg.Store = rn, passWindowStore{refStore{cs}}
	var ran, terminal, orphan bool
	var stops int
	sb.onPass = func() {
		if ran {
			return
		}
		ran = true
		ctx := context.Background()
		cur, err := s.cfg.Store.GetRun(ctx, run.ID)
		if err != nil {
			t.Error(err)
			return
		}
		terminal = isTerminalRunState(cur.State) && cur.SandboxRef != "" && cs.folderStatus().Resume == nil
		before := sb.stopCount()
		s.sweepTerminalSandboxRun(ctx, cur)
		if err := s.reconcileOrphanedSandbox(ctx); err != nil {
			t.Error(err)
		}
		if err := s.sweepOrphanedSandboxes(ctx); err != nil {
			t.Error(err)
		}
		stops, orphan = sb.stopCount()-before, rn.orphan
	}
	if res := bringBack(t, s, bringBackOwnerSession(t), run); res.code != http.StatusOK {
		t.Fatalf("bring-back = %d %s", res.code, res.body)
	}
	if !ran || !terminal {
		t.Fatalf("the sweeps ran during the pass %v, with the run terminal, its ref set and no resume %v; want both", ran, terminal)
	}
	if stops != 0 || orphan {
		t.Errorf("during the pass back: %d sandbox stops, orphan verdict %v; want none and false", stops, orphan)
	}
	if n := sb.stopCount(); n != 1 {
		t.Errorf("StopSandbox succeeded %d times, want 1: the end's own, after its pass", n)
	}
}

// keptByACutPass is a real end whose pass back is cut after one of two changed files, so it keeps the sandbox
// for a resume (bringComputerFolderBack), its proxy stopped by the quiesce; the store then has the run ended with
// its sandbox ref still set. It answers the run as the store has it.
func keptByACutPass(t *testing.T) (*Server, *computerFolderStore, types.AgentRun, *folderSandboxRunner) {
	t.Helper()
	s, cs, _, run, sb, _ := computerFolderFixture(t, types.ComputerFolderBoth)
	if !s.holdForLocalFolder(context.Background(), run, "sbx") {
		t.Fatal(cs.FailureHint())
	}
	agentWrites(t, sb, "a.txt", "z.txt")
	sb.block = "z.txt"
	_, err := boundedFinalize(t, s, run, time.Second)
	assertFolderBackKept(t, err, cs, sb, types.FolderResume{Unsynced: 1})
	cs.dispatchTestStore.mu.Lock()
	cs.state, cs.run.SandboxRef = types.RunFailed, "sbx"
	cs.dispatchTestStore.mu.Unlock()
	cur, err := cs.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return s, cs, cur, sb
}

// A containment retry on a run an end kept for its computer folder's pass back confirms by a proxy stop alone:
// the sandbox, and the files it is kept for, are not stopped.
func TestStopUnconfirmedProxy_AnEndThatKeptTheSandboxIsConfirmedByAProxyStop(t *testing.T) {
	s, _, cur, sb := keptByACutPass(t)
	sb.proxyStopped.Store(false)
	if err := s.stopUnconfirmedProxy(context.Background(), cur); err != nil {
		t.Fatalf("containment = %v, want confirmed by the proxy stop", err)
	}
	if !sb.proxyStopped.Load() {
		t.Error("the kept sandbox's proxy was not stopped")
	}
	if n := sb.stopCount(); n != 0 {
		t.Errorf("StopSandbox succeeded %d times, want the sandbox kept", n)
	}
}

// A revive's end read back on a run kept for its computer folder's pass back is ended with its stop confirmed:
// the quiesce stopped its proxy. The same run with no resume recorded is ended with its stop unconfirmed.
func TestReloseEnded_ASandboxKeptForItsPassBackIsConfirmed(t *testing.T) {
	s, cs, cur, _ := keptByACutPass(t)
	if fate, err := s.reloseEnded(context.Background(), cur.ID); fate != relostEnded || err != nil {
		t.Errorf("kept: fate %v, err %v; want ended and confirmed", fate, err)
	}
	cs.smu.Lock()
	cs.status.Resume = nil
	cs.smu.Unlock()
	if fate, err := s.reloseEnded(context.Background(), cur.ID); fate != relostEnded || err == nil {
		t.Errorf("not kept: fate %v, err %v; want ended with its stop unconfirmed", fate, err)
	}
}
