// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// overTheWire is err as the remote substrate hands a runner's answer on: the REPLY error, after a JSON
// round trip, read on this side (runnerwire.Error.Err).
func overTheWire(t *testing.T, err error) error {
	t.Helper()
	b, merr := json.Marshal(runnerwire.Reply{Error: runnerwire.ErrorFor(err)})
	if merr != nil {
		t.Fatal(merr)
	}
	var rep runnerwire.Reply
	if uerr := json.Unmarshal(b, &rep); uerr != nil {
		t.Fatal(uerr)
	}
	return rep.Error.Err()
}

// TestKillRun_RunnerConfirmingAnswers202ThenConfirms: on runner-mode Kubernetes, a person's kill whose runner
// answers that the pods are still stopping gets the same 202 `teardown: confirming`, and one success row once
// the runner's kill answers nil.
func TestKillRun_RunnerConfirmingAnswers202ThenConfirms(t *testing.T) {
	wire := overTheWire(t, errStillTerminating)
	r := newConfirmRig(t, 5*time.Second, []error{wire, wire}, nil)

	if body := r.kill(t); body["teardown"] != "confirming" {
		t.Fatalf("kill body = %v, want teardown confirming", body)
	}
	rows := r.settle(t)
	if len(rows) != 1 || rows[0].Outcome != "success" {
		t.Fatalf("run.kill.confirm rows = %+v, want exactly one success row", rows)
	}
	if got := r.st.ref(); got != "" {
		t.Errorf("sandbox ref after a confirmed stop = %q, want cleared", got)
	}
	kills := r.rows("run.kill")
	if len(kills) != 1 || kills[0].Outcome != "success" || !confirmDataHas(kills[0].Data, "sandbox_stop", "confirming") {
		t.Errorf("run.kill rows = %+v, want one success row with sandbox_stop confirming", kills)
	}
}

// TestKillRun_RunnerConfirmingSecondKillAddsNoRow: a second kill while the runner-mode confirm runs answers the
// same 202 and adds no row.
func TestKillRun_RunnerConfirmingSecondKillAddsNoRow(t *testing.T) {
	r := newConfirmRig(t, 30*time.Second, nil, overTheWire(t, errStillTerminating))

	first, second := r.kill(t), r.kill(t)
	if first["teardown"] != "confirming" || second["teardown"] != "confirming" {
		t.Fatalf("kill bodies = %v and %v, want both teardown confirming", first, second)
	}
	r.rr.release()
	r.settle(t)
	r.srv.WaitBackground()
	if rows := r.rows("run.kill.confirm"); len(rows) != 1 {
		t.Errorf("run.kill.confirm rows = %d, want 1", len(rows))
	}
}

// TestKillTeardownTail_RunnerAnswersKeepTheFailure: a kill with no confirm carrier keeps runner_error for the
// runner's still-stopping answer, and a person's kill keeps it for an older runner's plain answer, which
// arrives as internal.
func TestKillTeardownTail_RunnerAnswersKeepTheFailure(t *testing.T) {
	older := overTheWire(t, errors.New("k8s: teardown of run held, NetworkPolicies kept: no completion evidence yet"))
	for name, tc := range map[string]struct {
		answer  error
		carrier bool
	}{
		"no carrier":   {overTheWire(t, errStillTerminating), false},
		"older runner": {older, true},
	} {
		t.Run(name, func(t *testing.T) {
			r := newConfirmRig(t, 5*time.Second, nil, tc.answer)
			ctx := context.Background()
			if tc.carrier {
				ctx = withKillConfirm(ctx)
			}
			data := r.srv.killTeardownTail(ctx, r.st.run, types.ActorSystem, "test", nil, true)
			if data["runner_error"] == nil {
				t.Errorf("killData = %v, want the runner_error failure", data)
			}
			if kc, ok := ctx.Value(killConfirmKey{}).(*killConfirm); ok && kc.confirming.Load() {
				t.Error("the kill was marked confirming")
			}
			kills := r.rows("run.kill")
			if len(kills) != 1 || kills[0].Outcome != "failure" || confirmDataHas(kills[0].Data, "sandbox_stop", "confirming") {
				t.Errorf("run.kill rows = %+v, want one failure row with no sandbox_stop confirming", kills)
			}
		})
	}
}
