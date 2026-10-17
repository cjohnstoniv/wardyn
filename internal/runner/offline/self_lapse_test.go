// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// observedSub is a substrate whose proxies lapse themselves and are listed by ObserveProxies.
type observedSub struct {
	*localSub
	seen   map[uuid.UUID]runner.ProxyObservation
	record func(runner.NodeRecord) error
	got    *runner.NodeRecord
}

func (s *observedSub) ObserveProxies(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]runner.ProxyObservation, error) {
	out := map[uuid.UUID]runner.ProxyObservation{}
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
