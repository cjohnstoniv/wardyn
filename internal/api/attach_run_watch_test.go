// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// runReadCounter is the rig's store, counting the run's half of the attach re-check: run reads and profile reads.
type runReadCounter struct {
	*authorityUnits
	runReads, unitReads atomic.Int64
}

func (s *runReadCounter) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	s.runReads.Add(1)
	return s.authorityUnits.GetRun(ctx, id)
}

func (s *runReadCounter) GetGovernanceUnitChain(ctx context.Context, id uuid.UUID) ([]types.GovernanceUnit, error) {
	s.unitReads.Add(1)
	return s.authorityUnits.GetGovernanceUnitChain(ctx, id)
}

// TestAttachAuthority_OneRunReadPerTickForEverySocket: the open terminals on one run share the run's half of the
// re-check, one run read and one profile read per interval rather than one per socket, and a profile that begins
// denying interactive sessions still ends all of them at the next check.
func TestAttachAuthority_OneRunReadPerTickForEverySocket(t *testing.T) {
	var reads *runReadCounter
	rig := newAuthorityRigWith(t, 30*time.Minute, func(c *Config) {
		c.GovernAdminRuns = true
		reads = &runReadCounter{authorityUnits: c.Store.(*authorityUnits)}
		c.Store = reads
	})
	unit := types.GovernanceUnit{ID: uuid.New(), Name: "walled"}
	rig.units.set(unit)
	rig.ast.mu.Lock()
	rig.run.OperatorOwned, rig.run.UnitID = false, &unit.ID
	rig.ast.runs[rig.run.ID] = rig.run
	rig.ast.mu.Unlock()
	cookie := rig.cookie(t, holderOwner, time.Now().UTC().Add(time.Hour))
	ends := make([]<-chan error, maxAttachSessionsPerRun)
	for i := range ends {
		_, ends[i] = rig.dial(t, cookie, "", i > 0) // the first socket is the writer, the rest observe
	}

	runs, units, begin := reads.runReads.Load(), reads.unitReads.Load(), time.Now()
	time.Sleep(25 * authorityTick)
	runs, units = reads.runReads.Load()-runs, reads.unitReads.Load()-units
	ticks := int64(time.Since(begin)/authorityTick) + 1
	if runs > 2*ticks || units > 2*ticks {
		t.Fatalf("%d sockets read the run %d times and its profile %d times over about %d checks, want at most %d each (one shared read per check)",
			len(ends), runs, units, ticks, 2*ticks)
	}

	unit.Limits.DenyInteractive = true
	rig.units.set(unit)
	for i, end := range ends {
		select {
		case err := <-end:
			if got := websocket.CloseStatus(err); got != attachAuthEndedCode {
				t.Fatalf("socket %d ended with %v (status %d), want close %d", i, err, got, attachAuthEndedCode)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("socket %d is still open after its profile began denying interactive sessions", i)
		}
	}
}

// TestAttachAuthority_OneSocketsCredentialEndsOnlyThatSocket: only the run's half of the re-check is shared. Of two
// terminals on one run opened with two session cookies, the one whose cookie expires ends at the next check and the
// other stays open.
func TestAttachAuthority_OneSocketsCredentialEndsOnlyThatSocket(t *testing.T) {
	rig := newAuthorityRig(t, 0)
	short := rig.cookie(t, holderOwner, time.Now().UTC().Add(10*time.Minute))
	long := rig.cookie(t, holderOwner, time.Now().UTC().Add(time.Hour))
	_, shortEnd := rig.dial(t, short, "", false)
	_, longEnd := rig.dial(t, long, "", true)
	rig.offset.Store(int64(11 * time.Minute))
	select {
	case err := <-shortEnd:
		if got := websocket.CloseStatus(err); got != attachAuthEndedCode {
			t.Fatalf("the socket whose cookie expired ended with %v (status %d), want close %d", err, got, attachAuthEndedCode)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the socket whose cookie expired is still open")
	}
	time.Sleep(10 * authorityTick)
	select {
	case err := <-longEnd:
		t.Fatalf("the socket whose cookie is still live ended: %v", err)
	default:
	}
}

// TestAttachAuthority_SharedReadIsUsedOnlyWhileItsBoundHolds: a socket runs its check on the shared read only while
// that read's lookup bound has not passed and no read of the socket's own began after it. A read whose bound has
// passed (the socket was busy with a retry) is dropped and the check reads the run afresh; a read older than the
// socket's latest fresh read is not applied at all.
func TestAttachAuthority_SharedReadIsUsedOnlyWhileItsBoundHolds(t *testing.T) {
	var reads *runReadCounter
	rig := newAuthorityRigWith(t, 0, func(c *Config) {
		reads = &runReadCounter{authorityUnits: c.Store.(*authorityUnits)}
		c.Store = reads
	})
	srv, runID := rig.srv, rig.run.ID
	bound := srv.attachAuthorityLookupBound()
	now := time.Now()
	exempt := func(types.AgentRun) bool { return true }
	check := func(st attachRunState, fresh time.Time) (ran bool, runReads int64) {
		t.Helper()
		before := reads.runReads.Load()
		cctx, ran := srv.sharedCheck(context.Background(), runID, st, fresh, now)
		if !ran {
			return false, 0
		}
		if err := srv.attachUnitCheck(cctx, runID, nil, exempt); err != nil {
			t.Fatalf("check: %v", err)
		}
		return true, reads.runReads.Load() - before
	}

	// Within its bound and newer than the socket's fresh read: used, so the check reads nothing itself.
	live := attachRunState{at: now.Add(-bound / 2), run: rig.run}
	if ran, n := check(live, now.Add(-bound)); !ran || n != 0 {
		t.Fatalf("a read inside its bound: ran=%v, %d run reads, want ran with 0", ran, n)
	}
	// Its bound has passed: dropped, the check reads the run afresh.
	late := attachRunState{at: now.Add(-bound), run: rig.run}
	if ran, n := check(late, time.Time{}); !ran || n != 1 {
		t.Fatalf("a read past its bound: ran=%v, %d run reads, want ran with 1", ran, n)
	}
	// A fresh read began after the shared one: the shared read is not applied, even though its bound still holds.
	if ran, _ := check(live, now.Add(-bound/4)); ran {
		t.Fatal("a shared read older than the socket's fresh read was applied")
	}
	// The same, but the shared read refuses: a stale refusal still ends the socket, the safe side.
	gone := attachRunState{at: live.at, gone: true}
	if _, ran := srv.sharedCheck(context.Background(), runID, gone, now.Add(-bound/4), now); !ran {
		t.Fatal("a shared refusal older than the socket's fresh read was skipped")
	}
}
