// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"errors"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

func orphanAction(m *Manager, kind types.RunnerActionKind) runnerwire.PendingAction {
	id := uuid.New()
	return runnerwire.PendingAction{ID: uuid.New(), RunID: id, Ref: placement.RunnerRefPrefix(m.runnerID.String()) + id.String(), Kind: kind}
}

func TestMissingIdentityContainmentHasNoExecutionAuthority(t *testing.T) {
	for _, kind := range []types.RunnerActionKind{types.RunnerActionKill, types.RunnerActionStopProxy} {
		t.Run(string(kind), func(t *testing.T) {
			m, sub, _, org := managerFixture(t)
			a := orphanAction(m, kind)
			result, err := m.apply(t.Context(), a)
			if err != nil || result.Outcome != "success" || sub.kills+sub.stops != 1 {
				t.Fatalf("orphan containment: %+v %v", result, err)
			}
			r := m.state.Runs[a.RunID]
			if !r.ContainmentOnly || r.Generation != uuid.Nil || r.ExecID != "" || len(r.Executions) != 0 {
				t.Fatalf("invented execution identity: %+v", r)
			}
			if err := m.Lease(t.Context(), runnerwire.Lease{RunID: a.RunID, TokenRenewedAt: org}); err == nil {
				t.Fatal("orphan accepted lease")
			}
			if err := m.BeforeCreate(t.Context(), runner.SandboxSpec{RunID: a.RunID}); err == nil {
				t.Fatal("orphan reused creation identity")
			}
			for _, method := range []string{runnerwire.MethodExec, runnerwire.MethodStart, runnerwire.MethodReplaceProxy} {
				if err := m.BeforeRefCall(t.Context(), method, r.Ref, uuid.New()); err == nil {
					t.Fatalf("orphan allowed %s", method)
				}
			}
			if err := m.Created(t.Context(), runner.SandboxSpec{RunID: a.RunID}, r.Ref); err == nil {
				t.Fatal("orphan converted to created identity")
			}
			// An unrelated valid run still creates while the orphan remains fenced.
			createLocal(t, m, org)
			if err := m.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := New(m.dir, m.runnerID, sub, uuid.UUID.String, noOutput, noErase)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			replay, err := reopened.apply(t.Context(), a)
			if err != nil || replay != result || sub.kills+sub.stops != 1 {
				t.Fatalf("lost ACK replay: %+v %v", replay, err)
			}
		})
	}
}

func TestMissingIdentityPersistsExactIntentBeforeContainment(t *testing.T) {
	m, sub, _, _ := managerFixture(t)
	a := orphanAction(m, types.RunnerActionStopProxy)
	restore := failStateSave(t, m)
	if _, err := m.apply(t.Context(), a); err == nil || sub.stops != 0 {
		t.Fatal("containment ran before durable intent")
	}
	restore()
	changed := a
	changed.Kind = types.RunnerActionKill
	if _, err := m.apply(t.Context(), changed); err == nil || sub.kills != 0 {
		t.Fatal("pending intent identity changed")
	}
	sub.stopErr = errors.New("transient stop failure")
	if _, err := m.apply(t.Context(), a); !errors.Is(err, sub.stopErr) {
		t.Fatalf("false orphan containment success: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(m.dir, m.runnerID, sub, uuid.UUID.String, noOutput, noErase)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	sub.stopErr = nil
	result, err := reopened.apply(t.Context(), a)
	if err != nil || result.Outcome != "success" || sub.stops != 2 {
		t.Fatalf("restart did not retry exact obligation: %+v %v", result, err)
	}
}

func TestMissingIdentityRejectsForeignOrExecutableAction(t *testing.T) {
	for _, variant := range []string{"prefix", "reference", "nil-run", "end", "recover"} {
		t.Run(variant, func(t *testing.T) {
			m, sub, _, _ := managerFixture(t)
			a := orphanAction(m, types.RunnerActionStopProxy)
			switch variant {
			case "prefix":
				a.Ref = placement.RunnerRefPrefix(uuid.New().String()) + a.RunID.String()
			case "reference":
				a.Ref = placement.RunnerRefPrefix(m.runnerID.String()) + uuid.New().String()
			case "nil-run":
				a.RunID = uuid.Nil
			case "end":
				a.Kind = types.RunnerActionEnd
			case "recover":
				a.Kind = types.RunnerActionRecoverMutation
			}
			result, err := m.apply(t.Context(), a)
			if len(m.state.Runs) != 0 || sub.stops+sub.kills != 0 {
				t.Fatal("untrusted identity/action acquired local authority")
			}
			if variant == "nil-run" {
				if err == nil {
					t.Fatal("an action with no run was answered")
				}
				return
			}
			// Every other action is one this runner can never apply: it ends refused, durably.
			if err != nil || result.Outcome != types.RunnerActionRefused || result.Reason == "" || result.ID != a.ID {
				t.Fatalf("foreign action: %+v %v", result, err)
			}
			if replay, err := m.apply(t.Context(), a); err != nil || replay != result {
				t.Fatalf("refusal replay: %+v %v", replay, err)
			}
		})
	}
}

// TestContainmentOnlyRecordRefusesOtherActionsAcrossARestart: a record kept only for containment
// answers an end (which every control-plane boot queues for a terminal run that kept its sandbox
// ref) refused, with a receipt the org closes, after the runner restarts too, instead of failing
// every session. It stays containment-only.
func TestContainmentOnlyRecordRefusesOtherActionsAcrossARestart(t *testing.T) {
	m, sub, _, org := managerFixture(t)
	kill := orphanAction(m, types.RunnerActionKill)
	if res, err := m.apply(t.Context(), kill); err != nil || res.Outcome != types.RunnerActionSuccess {
		t.Fatalf("orphan kill: %+v %v", res, err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	// The substrate can end a sandbox, so an end that got through would run (as a kill here).
	reopened, err := New(m.dir, m.runnerID, endingSub{sub}, uuid.UUID.String, noOutput, noErase)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.Clock(runnerwire.Hello{ServerTime: org}, reopened.now()); err != nil {
		t.Fatal(err)
	}
	if err := reopened.BeforeReady(t.Context(), &runnerwire.Peer{}, false); err != nil {
		t.Fatal(err)
	}
	end := kill
	end.ID, end.Kind = uuid.New(), types.RunnerActionEnd
	res, err := reopened.apply(t.Context(), end)
	if sub.kills != 1 || !reopened.state.Runs[kill.RunID].ContainmentOnly {
		t.Fatalf("an end ran on a containment-only record: kills %d, record %+v", sub.kills, reopened.state.Runs[kill.RunID])
	}
	if err != nil || res.ID != end.ID || res.Outcome != types.RunnerActionRefused || res.Reason != "containment-only identity refuses this action" {
		t.Fatalf("end on a containment-only record: %+v %v", res, err)
	}
	if replay, err := reopened.apply(t.Context(), end); err != nil || replay != res {
		t.Fatalf("refusal replay: %+v %v", replay, err)
	}
	if err := reopened.Lease(t.Context(), runnerwire.Lease{RunID: kill.RunID, TokenRenewedAt: org}); err == nil {
		t.Fatal("a containment-only record took a lease")
	}
}
