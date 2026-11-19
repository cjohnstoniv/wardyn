// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"

	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// A new kept machine is audited as created when its row is inserted. When the runner then fails to create the
// sandbox, the row is dropped and the audit trail says the create failed; a run whose sandbox was created
// writes no such row.
func TestKeptMachineCreateFailedAtRunnerAuditsFailure(t *testing.T) {
	r := newKeptRig(t, keepingCaps())
	plan, rf := r.decide(r.ask(client.RunMachine{Keep: true}))
	if rf != nil {
		t.Fatal(rf.body.Error)
	}
	run := r.startingRun()
	kd, ok := r.srv.keptMachineAtDispatch(context.Background(), run, plan)
	if !ok {
		t.Fatal("dispatch did not insert the machine")
	}
	wantKeptAudit(t, r.audit, "machine.create", "success", "")

	created := true
	r.srv.releaseUnstartedKeptMachine(context.Background(), run, kd, &created)
	for _, ev := range r.audit.snapshot() {
		if ev.Action == "machine.create" && ev.Outcome == "failure" {
			t.Fatal("a created sandbox wrote a failed machine.create")
		}
	}

	created = false
	r.srv.releaseUnstartedKeptMachine(context.Background(), run, kd, &created)
	data := wantKeptAudit(t, r.audit, "machine.create", "failure", "sandbox_not_created")
	if data["machine_id"] != kd.machine.ID.String() || data["run_id"] != run.ID.String() {
		t.Fatalf("failed machine.create = %v", data)
	}
}
