// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"errors"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/runnerspool"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/google/uuid"
)

func TestPendingRetirementQuarantineSurvivesRestartWithoutClearingFence(t *testing.T) {
	for _, sealed := range []bool{false, true} {
		name := "unacknowledged"
		if sealed {
			name = "sealed-before-receipt"
		}
		t.Run(name, func(t *testing.T) {
			m, sub, id, spool, a := retirementFixture(t)
			org := m.state.Runs[id].Lease.TokenRenewedAt
			other := createLocal(t, m, org)
			if sealed {
				r := m.state.Runs[id]
				r.Retirement = a.Retirement
				m.state.Runs[id] = r
				if err := m.save(); err != nil {
					t.Fatal(err)
				}
				if ok, err := spool.Seal(t.Context(), a.Retirement.Spool); err != nil || !ok {
					t.Fatal(err)
				}
			} else {
				if err := spool.Append(t.Context(), egress.DecisionLog{Request: egress.Request{RunID: id}}); err != nil {
					t.Fatal(err)
				}
				if result, err := m.apply(t.Context(), a); err != nil || result.Outcome != "deferred" {
					t.Fatalf("pending retirement: %+v %v", result, err)
				}
			}
			before := m.state.Runs[id]
			if before.Retirement == nil {
				t.Fatal("fixture lacks pending producer fence")
			}
			quarantine := quarantineAction(m, id)
			if result, err := m.apply(t.Context(), quarantine); err != nil || result.Outcome != "success" {
				t.Fatalf("quarantine: %+v %v", result, err)
			}
			if err := m.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := New(m.dir, m.runnerID, sub, uuid.UUID.String, noOutput, noErase)
			if err != nil {
				t.Fatalf("quarantine poisoned runner restart: %v", err)
			}
			defer reopened.Close()
			retained := reopened.state.Runs[id]
			if retained.Retirement == nil || *retained.Retirement != *before.Retirement || retained.Quarantine == nil || retained.Retired || *retained.Containment != *before.Containment {
				t.Fatal("quarantine cleared settlement/producer fence or falsely retired")
			}
			if rows, err := allState(t.Context(), reopened); err != nil || len(rows) != 1 || rows[0].RunID != other {
				t.Fatalf("other run stranded: %+v %v", rows, err)
			}
			if err := reopened.Replay(t.Context(), nil); err != nil {
				t.Fatal(err)
			}
			if err := reopened.BeforeReady(t.Context(), &runnerwire.Peer{}, false); err != nil {
				t.Fatal(err)
			}
			if err := reopened.Lease(t.Context(), runnerwire.Lease{RunID: id, TokenRenewedAt: org}); err == nil {
				t.Fatal("quarantined retirement granted renewal")
			}
			if err := reopened.BeforeRefCall(t.Context(), runnerwire.MethodReplaceProxy, id.String(), uuid.Nil); err == nil {
				t.Fatal("quarantined retirement restarted producer")
			}
			if sealed {
				if err := spool.CanRecord(t.Context()); !errors.Is(err, runnerspool.ErrSealed) {
					t.Fatal("quarantine unsealed spool")
				}
			} else {
				position, err := spool.Get(t.Context())
				depth, _, _, statsErr := spool.Stats(t.Context())
				if err != nil || statsErr != nil || position.Seq != 0 || depth != 1 {
					t.Fatal("quarantine advanced/dropped pending evidence")
				}
			}
			if result, err := reopened.apply(t.Context(), quarantine); err != nil || result.Outcome != "success" {
				t.Fatal("quarantine receipt lost on restart")
			}
		})
	}
}
