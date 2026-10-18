// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"context"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

func pendingContainment(m *Manager, id uuid.UUID, kind types.RunnerActionKind) runnerwire.PendingAction {
	return runnerwire.PendingAction{ID: uuid.New(), RunID: id, Ref: placement.RunnerRefPrefix(m.runnerID.String()) + id.String(), Kind: kind}
}

func TestPendingKillFencesLateMutationAfterReconnect(t *testing.T) {
	for _, create := range []bool{false, true} {
		t.Run(map[bool]string{false: "replace", true: "create"}[create], func(t *testing.T) {
			m, sub, _, org := managerFixture(t)
			id := createLocal(t, m, org)
			if !create {
				if err := m.MutationFinished(t.Context(), runnerwire.MethodCreateSandbox, "", id, uuid.Nil, nil); err != nil {
					t.Fatal(err)
				}
				if err := m.BeforeRefCall(t.Context(), runnerwire.MethodReplaceProxy, id.String(), uuid.Nil); err != nil {
					t.Fatal(err)
				}
			}
			m.Offline(t.Context())
			a := pendingContainment(m, id, types.RunnerActionKill)
			result, err := m.apply(t.Context(), a)
			if err != nil || result.Outcome != "deferred" {
				t.Fatalf("inflight kill falsely settled: %+v %v", result, err)
			}
			if err := m.BeforeReady(t.Context(), &runnerwire.Peer{}, true); err != nil {
				t.Fatal(err)
			}
			var late, finish error
			if create {
				late = m.Created(t.Context(), runner.SandboxSpec{RunID: id}, id.String())
				finish = m.MutationFinished(t.Context(), runnerwire.MethodCreateSandbox, "", id, uuid.Nil, late)
			} else {
				late = m.AfterRefCall(t.Context(), runnerwire.MethodReplaceProxy, id.String(), "", uuid.Nil)
				finish = m.MutationFinished(t.Context(), runnerwire.MethodReplaceProxy, id.String(), uuid.Nil, uuid.Nil, late)
			}
			if late == nil || finish == nil || m.state.Runs[id].ProxyStoppedAt == nil || !m.mutationsDrained(id) {
				t.Fatal("late mutation bypassed kill disposition")
			}
			if replay, err := m.apply(t.Context(), a); err != nil || replay != result {
				t.Fatal("deferred action changed on retry")
			}
			a.ID = uuid.New()
			if settled, err := m.apply(t.Context(), a); err != nil || settled.Outcome != "success" || sub.kills != 2 {
				t.Fatalf("fresh exact kill not settled: %+v %v", settled, err)
			}
			r := m.state.Runs[id]
			recover := pendingContainment(m, id, types.RunnerActionRecoverContainment)
			recover.Recovery = r.Containment
			if res, err := m.apply(t.Context(), recover); err != nil || res.Outcome != types.RunnerActionRefused {
				t.Fatalf("terminal kill recovery = %+v %v, want refused", res, err)
			}
			if err := m.Lease(t.Context(), runnerwire.Lease{RunID: id, TokenRenewedAt: org.Add(time.Second)}); err == nil {
				t.Fatal("terminal kill admitted fresh lease")
			}
		})
	}
}

func TestRenewableContainmentRequiresExactRecoveryAndFreshLease(t *testing.T) {
	m, sub, _, org := managerFixture(t)
	id := createLocal(t, m, org)
	if err := m.MutationFinished(t.Context(), runnerwire.MethodCreateSandbox, "", id, uuid.Nil, nil); err != nil {
		t.Fatal(err)
	}
	a := pendingContainment(m, id, types.RunnerActionStopProxy)
	restore := failStateSave(t, m)
	if _, err := m.apply(t.Context(), a); err == nil || sub.stops != 0 {
		t.Fatal("containment preceded durable fence")
	}
	restore()
	if result, err := m.apply(t.Context(), a); err != nil || result.Outcome != "success" {
		t.Fatalf("stop not settled: %+v %v", result, err)
	}
	if err := m.Lease(t.Context(), runnerwire.Lease{RunID: id, TokenRenewedAt: org.Add(time.Second)}); err == nil {
		t.Fatal("lease alone cleared disposition")
	}
	recover := pendingContainment(m, id, types.RunnerActionRecoverContainment)
	proof := *m.state.Runs[id].Containment
	recover.Recovery = &proof
	cross := recover
	cross.Kind = types.RunnerActionRecoverMutation
	if _, err := m.apply(t.Context(), cross); err == nil {
		t.Fatal("containment proof used as mutation recovery")
	}
	wrong := recover
	wrong.ID = uuid.New()
	bad := proof
	bad.ID = uuid.New()
	wrong.Recovery = &bad
	if res, err := m.apply(t.Context(), wrong); err != nil || res.Outcome != types.RunnerActionRefused {
		t.Fatalf("wrong containment proof = %+v %v, want refused", res, err)
	}
	result, err := m.apply(t.Context(), recover)
	if err != nil || result.Outcome != "success" {
		t.Fatalf("exact recovery: %+v %v", result, err)
	}
	if err := m.BeforeRefCall(t.Context(), runnerwire.MethodReplaceProxy, id.String(), uuid.Nil); err == nil {
		t.Fatal("old monotonic deadline survived recovery")
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(m.dir, m.runnerID, sub, uuid.UUID.String, noOutput, noErase)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if replay, err := reopened.apply(t.Context(), recover); err != nil || replay != result {
		t.Fatal("recovery receipt lost on restart")
	}
	if err := reopened.Clock(runnerwire.Hello{ServerTime: org}, reopened.now()); err != nil {
		t.Fatal(err)
	}
	if err := reopened.BeforeReady(t.Context(), &runnerwire.Peer{}, false); err != nil {
		t.Fatal(err)
	}
	for _, renewed := range []time.Time{org.Add(-time.Second), org} {
		if err := reopened.Lease(t.Context(), runnerwire.Lease{RunID: id, TokenRenewedAt: renewed}); err == nil {
			t.Fatal("replayed/rollback lease bypassed persisted floor")
		}
	}
	if err := reopened.Lease(t.Context(), runnerwire.Lease{RunID: id, TokenRenewedAt: org.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	if err := reopened.BeforeRefCall(t.Context(), runnerwire.MethodReplaceProxy, id.String(), uuid.Nil); err != nil {
		t.Fatal(err)
	}
}

// A matching recovery met while a mutation of the run is in flight is deferred, not refused: the cause
// passes, and a fresh action for the same marker recovers the run once the mutation finishes.
func TestContainmentRecoveryDefersWhileAMutationIsInFlight(t *testing.T) {
	m, _, _, org := managerFixture(t)
	id := createLocal(t, m, org)
	if err := m.MutationFinished(t.Context(), runnerwire.MethodCreateSandbox, "", id, uuid.Nil, nil); err != nil {
		t.Fatal(err)
	}
	if result, err := m.apply(t.Context(), pendingContainment(m, id, types.RunnerActionStopProxy)); err != nil || result.Outcome != "success" {
		t.Fatalf("stop not settled: %+v %v", result, err)
	}
	key := mutationKey{runnerwire.MethodReplaceProxy, m.expectedRef(id), uuid.Nil}
	m.beginMutation(key.method, key.ref, key.generation)
	recover := pendingContainment(m, id, types.RunnerActionRecoverContainment)
	proof := *m.state.Runs[id].Containment
	recover.Recovery = &proof
	deferred, err := m.apply(t.Context(), recover)
	if err != nil || deferred.Outcome != "deferred" || m.state.Runs[id].Containment == nil {
		t.Fatalf("recovery with a mutation in flight = %+v %v, want deferred with the marker kept", deferred, err)
	}
	if replay, err := m.apply(t.Context(), recover); err != nil || replay != deferred {
		t.Fatalf("deferred recovery replay = %+v %v", replay, err)
	}
	m.finishMutation(key)
	fresh := recover
	fresh.ID = uuid.New()
	if result, err := m.apply(t.Context(), fresh); err != nil || result.Outcome != "success" || m.state.Runs[id].Containment != nil {
		t.Fatalf("fresh recovery after the mutation drained = %+v %v, want success", result, err)
	}
}

type endingSub struct{ *localSub }

func (s endingSub) EndSandbox(ctx context.Context, ref string) error { return s.KillSandbox(ctx, ref) }

func TestContainmentSupersessionIsMonotone(t *testing.T) {
	for _, pair := range [][2]types.RunnerActionKind{
		{types.RunnerActionStopProxy, types.RunnerActionEnd},
		{types.RunnerActionEnd, types.RunnerActionStopProxy},
		{types.RunnerActionKill, types.RunnerActionStopProxy},
		{types.RunnerActionKill, types.RunnerActionEnd},
	} {
		t.Run(string(pair[0])+"-"+string(pair[1]), func(t *testing.T) {
			m, sub, _, org := managerFixture(t)
			m.sub = endingSub{sub}
			id := createLocal(t, m, org)
			if err := m.MutationFinished(t.Context(), runnerwire.MethodCreateSandbox, "", id, uuid.Nil, nil); err != nil {
				t.Fatal(err)
			}
			first := pendingContainment(m, id, pair[0])
			if result, err := m.apply(t.Context(), first); err != nil || result.Outcome != "success" {
				t.Fatalf("first: %+v %v", result, err)
			}
			original := *m.state.Runs[id].Containment
			second := pendingContainment(m, id, pair[1])
			if result, err := m.apply(t.Context(), second); err != nil || result.Outcome != "success" {
				t.Fatalf("second permanently deferred: %+v %v", result, err)
			}
			marker := m.state.Runs[id].Containment
			if containmentRank(pair[1]) > containmentRank(pair[0]) {
				if marker.ID != second.ID || marker.Operation != string(pair[1]) {
					t.Fatal("stronger action did not replace exact marker")
				}
				recover := pendingContainment(m, id, types.RunnerActionRecoverContainment)
				recover.Recovery = &original
				if res, err := m.apply(t.Context(), recover); err != nil || res.Outcome != types.RunnerActionRefused {
					t.Fatalf("superseded recovery marker = %+v %v, want refused", res, err)
				}
			} else if *marker != original {
				t.Fatal("weaker action downgraded stronger containment")
			}
		})
	}
}
