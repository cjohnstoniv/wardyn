// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

func TestMutationRecoveryExactProofAndLostAckRestart(t *testing.T) {
	for _, method := range []string{runnerwire.MethodExec, runnerwire.MethodReplaceProxy} {
		t.Run(method, func(t *testing.T) {
			m, sub, _, org := managerFixture(t)
			id := createLocal(t, m, org)
			ctx := t.Context()
			generation := uuid.Nil
			if method == runnerwire.MethodExec {
				generation = executionIntent(t, m, id, org, method)
			}
			restore := failStateSave(t, m)
			if err := m.AfterRefCall(ctx, method, id.String(), "handle", generation); err == nil {
				t.Fatal("fixture did not fail persistence")
			}
			restore()
			if err := m.CheckLapses(ctx); err != nil {
				t.Fatal(err)
			}
			r := m.state.Runs[id]
			if r.MutationFailure == nil || !r.MutationFailure.Contained {
				t.Fatal("containment was not durably confirmed")
			}
			if err := m.BeforeRefCall(ctx, runnerwire.MethodReplaceProxy, id.String(), uuid.Nil); err == nil {
				t.Fatal("confirmed containment silently authorized resume")
			}
			proof := *r.MutationFailure
			action := runnerwire.PendingAction{ID: uuid.New(), Kind: types.RunnerActionRecoverMutation, RunID: id, Ref: "runner:" + m.runnerID.String() + "/" + id.String(), Recovery: &proof}
			for _, field := range []string{"marker", "generation", "operation", "contained", "missing", "run", "ref"} {
				t.Run(field, func(t *testing.T) {
					bad := action
					bad.ID = uuid.New() // a refused action ID is answered for good
					copy := proof
					bad.Recovery = &copy
					switch field {
					case "marker":
						copy.ID = uuid.New()
					case "generation":
						copy.Generation = uuid.New()
					case "operation":
						copy.Operation = "unknown"
					case "contained":
						copy.Contained = false
					case "missing":
						bad.Recovery = nil
					case "run":
						bad.RunID = uuid.New()
					case "ref":
						bad.Ref += "other"
					}
					if result, err := m.apply(ctx, bad); err == nil && result.Outcome != types.RunnerActionRefused {
						t.Fatal("mismatched recovery accepted")
					}
				})
			}
			restore = failStateSave(t, m)
			if _, err := m.apply(ctx, action); err == nil {
				t.Fatal("clearance acknowledged without durable receipt")
			}
			if err := m.BeforeRefCall(ctx, runnerwire.MethodReplaceProxy, id.String(), uuid.Nil); err == nil {
				t.Fatal("unpersisted clearance admitted work")
			}
			restore()
			result, err := m.apply(ctx, action)
			if err != nil {
				t.Fatal(err)
			}
			if err := m.Close(); err != nil {
				t.Fatal(err)
			}
			restarted, err := New(m.dir, m.runnerID, sub, uuid.UUID.String, noOutput, noErase)
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.Close()
			// A decoded replay carries a different pointer, but the same action identity.
			copied := proof
			action.Recovery = &copied
			replay, err := restarted.apply(ctx, action)
			if err != nil || replay != result {
				t.Fatalf("lost ACK retry: %+v %v", replay, err)
			}
			if restarted.state.Runs[id].MutationFailure != nil {
				t.Fatal("replay recreated resolved marker")
			}
			if method == runnerwire.MethodExec {
				if err := restarted.Clock(runnerwire.Hello{ServerTime: org}, restarted.now()); err != nil {
					t.Fatal(err)
				}
				if err := restarted.BeforeReady(ctx, &runnerwire.Peer{}, false); err != nil {
					t.Fatal(err)
				}
				if err := restarted.Lease(ctx, restarted.state.Runs[id].Lease); err != nil {
					t.Fatal(err)
				}
				if err := restarted.BeforeRefCall(ctx, runnerwire.MethodExec, id.String(), generation); err == nil {
					t.Fatal("recovery erased attempted-generation fence")
				}
			}
		})
	}
}
