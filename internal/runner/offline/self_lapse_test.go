// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// observedSub is a substrate whose proxies lapse themselves and are listed by ObserveProxies.
type observedSub struct {
	*localSub
	seen   map[uuid.UUID]runner.ProxyObservation
	asked  []uuid.UUID
	record func(runner.NodeRecord) error
	got    *runner.NodeRecord
}

func (s *observedSub) ObserveProxies(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]runner.ProxyObservation, error) {
	out := map[uuid.UUID]runner.ProxyObservation{}
	s.asked = append(s.asked[:0], ids...)
	for _, id := range ids {
		if o, ok := s.seen[id]; ok {
			out[id] = o
		}
	}
	return out, nil
}

func (s *observedSub) AcceptNodeRecord(_ context.Context, _ string, rec runner.NodeRecord) error {
	s.got = &rec
	return s.record(rec)
}

// TestContainmentPendingHeldNotStamped: a typed pending answer is held as containment pending,
// contained for READY only, with no ProxyStoppedAt, and retried until completion evidence.
func TestContainmentPendingHeldNotStamped(t *testing.T) {
	m, sub, _, org := managerFixture(t)
	id := createLocal(t, m, org)
	ctx := context.Background()
	m.state.Revoked = true // every run must be contained now
	sub.stopErr = &runner.ContainmentPendingError{Kind: runner.ContainmentPendingDelete, NameReused: true, Err: errors.New("termination unknown")}
	if err := m.CheckLapses(ctx); err != nil {
		t.Fatalf("a pending run failed the lapse pass: %v", err)
	}
	r := m.state.Runs[id]
	if r.ContainmentPending == nil || r.ContainmentPending.Kind != runner.ContainmentPendingDelete || !r.ContainmentPending.NameReused || r.ProxyStoppedAt != nil {
		t.Fatalf("run %+v", r)
	}
	since := r.ContainmentPending.Since
	row, err := m.observeRun(ctx, id)
	if err != nil || row == nil || row.ContainmentPending == nil {
		t.Fatalf("STATE row lacks the pending state: %+v %v", row, err)
	}
	if err := m.CheckLapses(ctx); err != nil || m.state.Runs[id].ContainmentPending.Since != since || sub.stops != 2 {
		t.Fatalf("retry: %v stops=%d", err, sub.stops)
	}
	sub.stopErr = nil
	if err := m.CheckLapses(ctx); err != nil {
		t.Fatal(err)
	}
	if r := m.state.Runs[id]; r.ProxyStoppedAt == nil || r.ContainmentPending != nil {
		t.Fatalf("completion evidence did not settle: %+v", r)
	}
}

// TestProxyExitedObligation: an observed exit is a durable obligation a live lease never clears.
func TestProxyExitedObligation(t *testing.T) {
	m, sub, _, org := managerFixture(t)
	obs := &observedSub{localSub: sub, seen: map[uuid.UUID]runner.ProxyObservation{}}
	m.sub = obs
	if err := m.SetOptions(Options{ProxySelfLapses: true}); err != nil {
		t.Fatal(err)
	}
	running, exited := createLocal(t, m, org), createLocal(t, m, org)
	obs.seen[running], obs.seen[exited] = runner.ProxyRunning, runner.ProxyFailed
	ctx := context.Background()
	if err := m.ObserveProxies(ctx); err != nil {
		t.Fatal(err)
	}
	if m.state.Runs[running].ProxyExited != nil || m.state.Runs[exited].ProxyExited == nil {
		t.Fatal("obligations")
	}
	if err := m.CheckLapses(ctx); err != nil {
		t.Fatal(err)
	}
	if sub.stops != 1 || m.state.Runs[exited].ProxyStoppedAt == nil || m.state.Runs[running].ProxyStoppedAt != nil {
		t.Fatalf("stops=%d", sub.stops)
	}
}

func TestSelfLapseNeedsObserver(t *testing.T) {
	m, _, _, _ := managerFixture(t)
	if err := m.SetOptions(Options{ProxySelfLapses: true}); err == nil {
		t.Fatal("self-lapse accepted a substrate that cannot list its proxies")
	}
}

// TestProvisionalDeadlineAfterRestart: with self-lapsing proxies a restart keeps a running proxy on
// a provisional cleanup deadline instead of containing it at once.
func TestProvisionalDeadlineAfterRestart(t *testing.T) {
	m, sub, _, org := managerFixture(t)
	id := createLocal(t, m, org)
	dir := m.dir
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(dir, m.runnerID, sub, uuid.UUID.String, noOutput, noErase)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	restarted.sub = &observedSub{localSub: sub, seen: map[uuid.UUID]runner.ProxyObservation{id: runner.ProxyRunning}}
	if err := restarted.SetOptions(Options{ProxySelfLapses: true}); err != nil {
		t.Fatal(err)
	}
	if err := restarted.ObserveProxies(context.Background()); err != nil {
		t.Fatal(err)
	}
	restarted.ProvisionalDeadlines()
	if err := restarted.CheckLapses(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sub.stops != 0 {
		t.Fatal("a running self-lapsing proxy was contained at restart")
	}
}

// TestProvisionalDeadlineKeepsEndsAtAfterRestart: a restart keeps a sooner ends_at, not restart + RunTokenTTL.
func TestProvisionalDeadlineKeepsEndsAtAfterRestart(t *testing.T) {
	m, sub, elapsed, org := managerFixture(t)
	base, id := m.now(), uuid.New()
	leaseAndCreate(t, m, runnerwire.Lease{RunID: id, TokenRenewedAt: org, EndsAt: org.Add(20 * time.Minute)})
	*elapsed = 10 * time.Minute
	restarted := restartSelfLapsing(t, m, sub, elapsed, base, id, false)
	expectStops(t, restarted, sub, 0)
	*elapsed = 21 * time.Minute
	expectStops(t, restarted, sub, 1)
}

// TestProvisionalDeadlineKeepsTokenLegAfterRestart: a token renewed long ago keeps its sooner lapse across a restart.
func TestProvisionalDeadlineKeepsTokenLegAfterRestart(t *testing.T) {
	m, sub, elapsed, org := managerFixture(t)
	base, id := m.now(), uuid.New()
	// The token leg is RunTokenLapseAfter (1h5m) less the 40m since the renewal: 25m.
	leaseAndCreate(t, m, runnerwire.Lease{RunID: id, TokenRenewedAt: org.Add(-40 * time.Minute), EndsAt: org.Add(3 * time.Hour)})
	*elapsed = 10 * time.Minute
	restarted := restartSelfLapsing(t, m, sub, elapsed, base, id, false)
	expectStops(t, restarted, sub, 0)
	*elapsed = 24 * time.Minute
	expectStops(t, restarted, sub, 0)
	*elapsed = 26 * time.Minute
	expectStops(t, restarted, sub, 1)
}

// TestProvisionalDeadlineKeepsRenewalAfterRestart: a renewal after the create moves the saved bound with it.
func TestProvisionalDeadlineKeepsRenewalAfterRestart(t *testing.T) {
	m, sub, elapsed, org := managerFixture(t)
	base, id := m.now(), uuid.New()
	leaseAndCreate(t, m, runnerwire.Lease{RunID: id, TokenRenewedAt: org, EndsAt: org.Add(3 * time.Hour)})
	*elapsed = 50 * time.Minute
	// The renewal moves the token leg from base+1h5m to base+1h55m.
	if err := m.Lease(context.Background(), runnerwire.Lease{RunID: id, TokenRenewedAt: org.Add(50 * time.Minute), EndsAt: org.Add(3 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	*elapsed = 70 * time.Minute
	restarted := restartSelfLapsing(t, m, sub, elapsed, base, id, false)
	expectStops(t, restarted, sub, 0)
	*elapsed = 114 * time.Minute
	expectStops(t, restarted, sub, 0)
	*elapsed = 116 * time.Minute
	expectStops(t, restarted, sub, 1)
}

// TestProvisionalDeadlinePassedLapseByGetsReadyGrace: a LapseBy that passed during the downtime still
// leaves restartReadyGrace for the org's READY re-lease.
func TestProvisionalDeadlinePassedLapseByGetsReadyGrace(t *testing.T) {
	m, sub, elapsed, org := managerFixture(t)
	base, id := m.now(), uuid.New()
	leaseAndCreate(t, m, runnerwire.Lease{RunID: id, TokenRenewedAt: org, EndsAt: org.Add(20 * time.Minute)})
	*elapsed = 30 * time.Minute
	restarted := restartSelfLapsing(t, m, sub, elapsed, base, id, false)
	*elapsed = 39 * time.Minute
	expectStops(t, restarted, sub, 0)
	*elapsed = 41 * time.Minute
	expectStops(t, restarted, sub, 1)
}

// TestProvisionalDeadlineNoLapseByIsToday: a state file an older runnerd wrote (no lapse_by) still gets
// restart + RunTokenTTL.
func TestProvisionalDeadlineNoLapseByIsToday(t *testing.T) {
	m, sub, elapsed, org := managerFixture(t)
	base := m.now()
	id := createLocal(t, m, org)
	*elapsed = 10 * time.Minute
	restarted := restartSelfLapsing(t, m, sub, elapsed, base, id, true)
	*elapsed = 69 * time.Minute
	expectStops(t, restarted, sub, 0)
	*elapsed = 71 * time.Minute
	expectStops(t, restarted, sub, 1)
}

// leaseAndCreate gives l's run its lease and creates its sandbox.
func leaseAndCreate(t *testing.T, m *Manager, l runnerwire.Lease) {
	t.Helper()
	ctx := context.Background()
	if err := m.Lease(ctx, l); err != nil {
		t.Fatal(err)
	}
	spec := runner.SandboxSpec{RunID: l.RunID}
	if err := m.BeforeCreate(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if err := m.Created(ctx, spec, l.RunID.String()); err != nil {
		t.Fatal(err)
	}
}

// restartSelfLapsing closes m and reloads its state on the same fake clock, with id's proxy running and
// given its provisional deadline. dropLapseBy first removes lapse_by from the state file, as an older
// runnerd wrote it.
func restartSelfLapsing(t *testing.T, m *Manager, sub *localSub, elapsed *time.Duration, base time.Time, id uuid.UUID, dropLapseBy bool) *Manager {
	t.Helper()
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if dropLapseBy {
		dropStateLapseBy(t, m.dir, id)
	}
	restarted, err := New(m.dir, m.runnerID, sub, uuid.UUID.String, noOutput, noErase)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	restarted.now = func() time.Time { return base.Add(*elapsed) }
	restarted.since = func(at time.Time) time.Duration { return base.Add(*elapsed).Sub(at) }
	restarted.sub = &observedSub{localSub: sub, seen: map[uuid.UUID]runner.ProxyObservation{id: runner.ProxyRunning}}
	if err := restarted.SetOptions(Options{ProxySelfLapses: true}); err != nil {
		t.Fatal(err)
	}
	if err := restarted.ObserveProxies(context.Background()); err != nil {
		t.Fatal(err)
	}
	restarted.ProvisionalDeadlines()
	return restarted
}

// expectStops runs one lapse check and wants want proxy stops in all.
func expectStops(t *testing.T, m *Manager, sub *localSub, want int) {
	t.Helper()
	if err := m.CheckLapses(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sub.stops != want {
		t.Fatalf("stops=%d, want %d", sub.stops, want)
	}
}

// TestNodeRecordRidesStopProxy: a node record on a stop_proxy action reaches the substrate; a
// refused record answers refused; a substrate that takes none refuses it.
func TestNodeRecordRidesStopProxy(t *testing.T) {
	rec := runner.NodeRecord{ID: uuid.New(), Attestation: runner.AttestNodePoweredOff}
	t.Run("refused by the substrate", func(t *testing.T) {
		m, sub, _, org := managerFixture(t)
		obs := &observedSub{localSub: sub, record: func(runner.NodeRecord) error { return runner.ErrNodeRecordRefused }}
		m.sub = obs
		id := createLocal(t, m, org)
		a := pendingContainment(m, id, types.RunnerActionStopProxy)
		a.NodeRecord = &rec
		result, err := m.apply(t.Context(), a)
		if err != nil || result.Outcome != types.RunnerActionRefused || obs.got == nil || sub.stops != 0 {
			t.Fatalf("result %+v err %v stops %d", result, err, sub.stops)
		}
	})
	t.Run("accepted", func(t *testing.T) {
		m, sub, _, org := managerFixture(t)
		obs := &observedSub{localSub: sub, record: func(runner.NodeRecord) error { return nil }}
		m.sub = obs
		id := createLocal(t, m, org)
		a := pendingContainment(m, id, types.RunnerActionStopProxy)
		a.NodeRecord = &rec
		_, err := m.apply(t.Context(), a)
		if err != nil || obs.got == nil || obs.got.ID != rec.ID || sub.stops != 1 {
			t.Fatalf("err %v stops %d", err, sub.stops)
		}
	})
	t.Run("a substrate that takes none", func(t *testing.T) {
		m, sub, _, org := managerFixture(t)
		id := createLocal(t, m, org)
		a := pendingContainment(m, id, types.RunnerActionStopProxy)
		a.NodeRecord = &rec
		result, err := m.apply(t.Context(), a)
		if err != nil || result.Outcome != types.RunnerActionRefused || sub.stops != 0 {
			t.Fatalf("result %+v err %v", result, err)
		}
	})
}

// TestEvidenceStoreAtomic: substrate evidence lives in the run's record; a failed save leaves the
// previous record.
func TestEvidenceStoreAtomic(t *testing.T) {
	m, _, _, org := managerFixture(t)
	id := createLocal(t, m, org)
	if err := m.SaveEvidence(id, []byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	restore := failStateSave(t, m)
	if err := m.SaveEvidence(id, []byte(`{"a":2}`)); err == nil {
		t.Fatal("a failed save reported success")
	}
	restore()
	if b, _ := m.LoadEvidence(id); string(b) != `{"a":1}` {
		t.Fatalf("evidence after a failed save: %s", b)
	}
	if ids := m.EvidenceRunIDs(); len(ids) != 1 || ids[0] != id {
		t.Fatalf("EvidenceRunIDs %v", ids)
	}
}

// TestSubstrateTickRunsEachTick: Options.Tick runs on the containment tick.
func TestSubstrateTickRunsEachTick(t *testing.T) {
	m, _, _, _ := managerFixture(t)
	ticked := make(chan struct{}, 1)
	if err := m.SetOptions(Options{Tick: func(context.Context) {
		select {
		case ticked <- struct{}{}:
		default:
		}
	}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	<-ticked
	cancel()
	<-done
}

// TestSinkRunIDsSkipRetiredAndErased: startup re-registers the sink only for live evidence, never
// for a retired or erased run whose spool is gone; the fence replay skips only the retired run.
func TestSinkRunIDsSkipRetiredAndErased(t *testing.T) {
	m, _, _, org := managerFixture(t)
	live, retired, erased := createLocal(t, m, org), createLocal(t, m, org), createLocal(t, m, org)
	for _, id := range []uuid.UUID{live, retired, erased} {
		if err := m.SaveEvidence(id, []byte(`{"a":1}`)); err != nil {
			t.Fatal(err)
		}
	}
	m.mu.Lock()
	r := m.state.Runs[retired]
	r.Retired = true
	m.state.Runs[retired] = r
	r = m.state.Runs[erased]
	r.EvidenceErased = true
	m.state.Runs[erased] = r
	m.mu.Unlock()
	if got := m.SinkRunIDs(); len(got) != 1 || got[0] != live {
		t.Fatalf("sink runs %v, want only %v", got, live)
	}
	if got := m.EvidenceRunIDs(); len(got) != 2 || slices.Contains(got, retired) {
		t.Fatalf("fence runs %v", got)
	}
}

// TestHeldRunsStayOnTheProxyTick: a run already holding an obligation, or stamped stopped, is still
// passed to the substrate each tick (its name-reuse list and identity sampling), and an observation
// of it records nothing new.
func TestHeldRunsStayOnTheProxyTick(t *testing.T) {
	m, sub, _, org := managerFixture(t)
	obs := &observedSub{localSub: sub, seen: map[uuid.UUID]runner.ProxyObservation{}}
	m.sub = obs
	if err := m.SetOptions(Options{ProxySelfLapses: true}); err != nil {
		t.Fatal(err)
	}
	held, stopped := createLocal(t, m, org), createLocal(t, m, org)
	if err := m.ProxyExited(held, "failed"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	r := m.state.Runs[stopped]
	r.ProxyStoppedAt = &now
	m.state.Runs[stopped] = r
	obs.seen[held], obs.seen[stopped] = runner.ProxyNotFound, runner.ProxyNotFound
	if err := m.ObserveProxies(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(obs.asked) != 2 || !slices.Contains(obs.asked, held) || !slices.Contains(obs.asked, stopped) {
		t.Fatalf("asked %v", obs.asked)
	}
	if m.state.Runs[held].ProxyExited.Evidence != "failed" || m.state.Runs[stopped].ProxyExited != nil {
		t.Fatalf("held %+v stopped %+v", m.state.Runs[held].ProxyExited, m.state.Runs[stopped].ProxyExited)
	}
}
