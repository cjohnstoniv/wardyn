// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// olBackstop is a default-terms pausing proxy's deadline from its renewal stamp: the token's lifetime, the 24 h in
// which a lapsed token may still be recovered, and the stamp margin.
const olBackstop = 25*time.Hour + 5*time.Minute

// olLeaseAt gives a fresh run the lease l (stamped at org) and creates its sandbox, returning the run's id.
func olLeaseAt(t *testing.T, m *Manager, org time.Time, l runnerwire.Lease) uuid.UUID {
	t.Helper()
	l.RunID, l.TokenRenewedAt = uuid.New(), org
	leaseAndCreate(t, m, l)
	return l.RunID
}

// TestOffline_DefaultTermsPausingProxyGetsBackstop: a run on the default terms (no allowance sent) whose proxy
// pauses and recovers by itself keeps its network to the last moment a recovery could succeed, 25 h 5 min from the
// stamp, not the token's 65 minutes.
func TestOffline_DefaultTermsPausingProxyGetsBackstop(t *testing.T) {
	m, sub, elapsed, org := managerFixture(t)
	id := olLeaseAt(t, m, org, runnerwire.Lease{ProxyPauses: true})
	if got := m.deadlines[id].tokenLeft; got != olBackstop {
		t.Fatalf("deadline %v from the stamp, want %v", got, olBackstop)
	}
	*elapsed = 2 * time.Hour // well past the token's own 65 minutes
	expectStops(t, m, sub, 0)
	*elapsed = 25 * time.Hour
	expectStops(t, m, sub, 0)
	*elapsed = olBackstop + time.Minute
	expectStops(t, m, sub, 1)
}

// TestOffline_OldProxyKeepsTodaysDeadline: a lease from an older server or for an older proxy (ProxyPauses unset)
// keeps the token's 65 minutes whatever terms it carries, as such a proxy cannot recover.
func TestOffline_OldProxyKeepsTodaysDeadline(t *testing.T) {
	for _, tc := range []struct {
		name  string
		lease runnerwire.Lease
	}{
		{"default terms", runnerwire.Lease{}},
		{"an allowance", runnerwire.Lease{OutageAllowanceS: 7200}},
		{"strict", runnerwire.Lease{OutageAllowanceS: 7200, OutageStrict: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, sub, elapsed, org := managerFixture(t)
			id := olLeaseAt(t, m, org, tc.lease)
			if got := m.deadlines[id].tokenLeft; got != 65*time.Minute {
				t.Fatalf("deadline %v from the stamp, want 65m", got)
			}
			*elapsed = 64 * time.Minute
			expectStops(t, m, sub, 0)
			*elapsed = 66 * time.Minute
			expectStops(t, m, sub, 1)
		})
	}
}

// TestOffline_StrictStopsAtAllowance: a pausing proxy under strict terms is stopped at its allowance, counted
// from the stamp; the same lease on non-strict terms keeps the backstop instead. A lease whose allowance is
// outside 30 minutes to 24 hours is refused.
func TestOffline_StrictStopsAtAllowance(t *testing.T) {
	t.Run("from the stamp", func(t *testing.T) {
		m, sub, elapsed, org := managerFixture(t)
		olLeaseAt(t, m, org.Add(-30*time.Minute), runnerwire.Lease{ProxyPauses: true, OutageStrict: true, OutageAllowanceS: 7200})
		*elapsed = 89 * time.Minute // 30 minutes of the two hours were spent before the lease arrived
		expectStops(t, m, sub, 0)
		*elapsed = 91 * time.Minute
		expectStops(t, m, sub, 1)
	})
	t.Run("not strict keeps the backstop", func(t *testing.T) {
		m, sub, elapsed, org := managerFixture(t)
		olLeaseAt(t, m, org, runnerwire.Lease{ProxyPauses: true, OutageAllowanceS: 7200})
		*elapsed = 121 * time.Minute
		expectStops(t, m, sub, 0)
		*elapsed = 24 * time.Hour
		expectStops(t, m, sub, 0)
	})
	t.Run("allowance bounds", func(t *testing.T) {
		for _, tc := range []struct {
			allowanceS int64
			ok         bool
		}{{1799, false}, {1800, true}, {86400, true}, {86401, false}} {
			m, _, _, org := managerFixture(t)
			err := m.Lease(t.Context(), runnerwire.Lease{RunID: uuid.New(), TokenRenewedAt: org, ProxyPauses: true, OutageStrict: true, OutageAllowanceS: tc.allowanceS})
			if (err == nil) != tc.ok {
				t.Fatalf("allowance %d s: error %v, want accepted=%v", tc.allowanceS, err, tc.ok)
			}
		}
	})
}

// TestOffline_KillStopsPausedRun: a run whose proxy pauses and recovers is not contained for being paused, and a
// kill still contains it however far off its backstop is.
func TestOffline_KillStopsPausedRun(t *testing.T) {
	m, sub, elapsed, org := managerFixture(t)
	id := olLeaseAt(t, m, org, runnerwire.Lease{ProxyPauses: true})
	if err := m.MutationFinished(t.Context(), runnerwire.MethodCreateSandbox, "", id, uuid.Nil, nil); err != nil {
		t.Fatal(err)
	}
	*elapsed = 3 * time.Hour // past the token's lifetime: the proxy has paused by itself
	expectStops(t, m, sub, 0)
	if m.state.Runs[id].ProxyStoppedAt != nil {
		t.Fatal("a paused run's proxy was stopped without a kill")
	}
	result, err := m.apply(t.Context(), pendingContainment(m, id, types.RunnerActionKill))
	if err != nil || result.Outcome != "success" {
		t.Fatalf("kill of a paused run: %+v %v", result, err)
	}
	if r := m.state.Runs[id]; sub.kills != 1 || r.ProxyStoppedAt == nil || r.Containment == nil || !r.Containment.Contained {
		t.Fatalf("kills=%d, run %+v: the kill did not contain the paused run", sub.kills, r)
	}
}

// TestOffline_RestartKeepsPausingLeaseDeadline: a restarted runnerd gives a held run whose last lease paused
// its proxy the deadline its lease carried (here 20 h), on either substrate, not the token's hour; a run whose
// last lease did not keeps today's bound.
func TestOffline_RestartKeepsPausingLeaseDeadline(t *testing.T) {
	type restartFn func(*testing.T, *Manager, *localSub, *time.Duration, time.Time, uuid.UUID) *Manager
	selfLapsing := func(t *testing.T, m *Manager, sub *localSub, elapsed *time.Duration, base time.Time, id uuid.UUID) *Manager {
		return restartSelfLapsing(t, m, sub, elapsed, base, id, false)
	}
	docker := func(t *testing.T, m *Manager, sub *localSub, elapsed *time.Duration, base time.Time, _ uuid.UUID) *Manager {
		return restartDocker(t, m, sub, elapsed, base, nil)
	}
	for _, tc := range []struct {
		name    string
		restart restartFn
		pauses  bool
		// kept is how long after the restart's 10 minutes the proxy is kept; one minute either side is probed.
		kept time.Duration
	}{
		{"self-lapsing, pausing lease", selfLapsing, true, 20*time.Hour - 10*time.Minute},
		{"self-lapsing, older lease", selfLapsing, false, time.Hour},
		{"docker, pausing lease", docker, true, 20*time.Hour - 10*time.Minute},
		{"docker, older lease", docker, false, identity.RunTokenLapseAfter},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, sub, elapsed, org := managerFixture(t)
			base := m.now()
			id := olLeaseAt(t, m, org, runnerwire.Lease{ProxyPauses: tc.pauses, OutageStrict: tc.pauses, OutageAllowanceS: 20 * 3600, EndsAt: org.Add(30 * time.Hour)})
			if err := m.Close(); err != nil {
				t.Fatal(err)
			}
			// Both stored leases carry a LapseBy 20 h out, as a pausing lease's own would.
			setStateLapseBy(t, m.dir, id, base.Add(20*time.Hour))
			*elapsed = 10 * time.Minute
			restarted := tc.restart(t, m, sub, elapsed, base, id)
			if got := restarted.state.Runs[id].Lease.ProxyPauses; got != tc.pauses {
				t.Fatalf("the stored lease's ProxyPauses = %v, want %v", got, tc.pauses)
			}
			if got := restarted.deadlines[id].tokenLeft; got != tc.kept {
				t.Fatalf("provisional deadline %v after the restart, want %v", got, tc.kept)
			}
			*elapsed = 10*time.Minute + tc.kept - time.Minute
			expectStops(t, restarted, sub, 0)
			*elapsed = 10*time.Minute + tc.kept + time.Minute
			expectStops(t, restarted, sub, 1)
		})
	}
}
