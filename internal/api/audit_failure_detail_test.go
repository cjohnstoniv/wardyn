// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

// A failed run.revoke row names each failing step and its reason beside the per-step keys, and the
// keys stay (additive).
func TestRevokeRunCascade_FailureRowNamesTheStepAndReason(t *testing.T) {
	srv, _, run := dispatchWithProxyConfigStore(t, &killRaceRunner{fakeRunner: &fakeRunner{}})
	audit := &recRecorder{}
	srv.cfg.Audit = audit
	srv.cfg.Broker = revokeFailBroker{&fakeBroker{}}

	if err := srv.revokeRunCascade(context.Background(), run.ID); err == nil {
		t.Fatal("revokeRunCascade = nil; want the broker failure")
	}
	ev := findAudit(audit.snapshot(), run.ID, "run.revoke", "failure")
	if ev == nil {
		t.Fatal("no run.revoke failure row")
	}
	var d struct {
		BrokerError string              `json:"broker_error"`
		Failed      []map[string]string `json:"failed_steps"`
	}
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		t.Fatal(err)
	}
	want := []map[string]string{{"step": "broker_revoke", "reason": "injected: the broker could not revoke"}}
	if d.BrokerError == "" || !reflect.DeepEqual(d.Failed, want) {
		t.Errorf("run.revoke data = %s; want broker_error kept and failed_steps %v", ev.Data, want)
	}
}

// A run.output.finalize failure row names the step that failed and why; a gap keeps its own reason.
func TestWithFinalizeStepNamesTheFailingStep(t *testing.T) {
	for _, tc := range []struct {
		name      string
		in        map[string]any
		step, why string
	}{
		{"drain", map[string]any{"drain": true, "incomplete": true}, "drain", "a driver's output copy did not end inside the drain barrier"},
		{"persist outranks drain", map[string]any{"drain": true, "persist_failing": true}, "persist", "the final output row could not be written to the store after its inline retries; the retry loop carries on"},
		{"late write", map[string]any{"incomplete": true, "late_write": true}, "late_write", "output arrived after the final row was committed, so the row was marked incomplete"},
		{"gap keeps its reason", map[string]any{"capture_gap": true, "reason": "sandbox_gone"}, "capture", "sandbox_gone"},
	} {
		out := withFinalizeStep(tc.in)
		if out["step"] != tc.step || out["reason"] != tc.why {
			t.Errorf("%s: %v; want step %q reason %q", tc.name, out, tc.step, tc.why)
		}
		for k, v := range tc.in {
			if out[k] != v {
				t.Errorf("%s: flag %q changed from %v to %v", tc.name, k, v, out[k])
			}
		}
	}
	if out := withFinalizeStep(map[string]any{"note": "x"}); out["step"] != nil {
		t.Errorf("a row with no failure flag got a step: %v", out)
	}
}

func TestAuditOutputFinalizeWritesTheStepIntoTheRow(t *testing.T) {
	srv, _, run := dispatchWithProxyConfigStore(t, &killRaceRunner{fakeRunner: &fakeRunner{}})
	audit := &recRecorder{}
	srv.cfg.Audit = audit

	srv.auditOutputFinalize(context.Background(), run.ID, map[string]any{"drain": true, "incomplete": true})

	ev := findAudit(audit.snapshot(), run.ID, "run.output.finalize", "failure")
	if ev == nil {
		t.Fatal("no run.output.finalize failure row")
	}
	var d map[string]any
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		t.Fatal(err)
	}
	if d["step"] != "drain" || d["reason"] == nil || d["drain"] != true {
		t.Errorf("run.output.finalize data = %s; want step drain, a reason and the drain flag kept", ev.Data)
	}
}
