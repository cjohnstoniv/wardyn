// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// TestRedactWorkloadIntentForReadFailsClosed: a policy that does not decode is withheld whole, and the
// full tier reads the stored bytes as they are.
func TestRedactWorkloadIntentForReadFailsClosed(t *testing.T) {
	raw := json.RawMessage(`{"task":"t","inline_policy":"not an object","image":"r/x:1"}`)
	if got := redactWorkloadIntentForRead(raw, workspaceReadFull); string(got) != string(raw) {
		t.Fatalf("full tier = %s, want the stored bytes", got)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(redactWorkloadIntentForRead(raw, workspaceReadSecurity), &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["inline_policy"]; ok || string(got["image"]) != `""` || string(got["task"]) != `"t"` {
		t.Fatalf("security tier = %v, want inline_policy withheld, image blanked, task kept", got)
	}
	if redactWorkloadIntentForRead(json.RawMessage(`[1]`), workspaceReadSecurity) != nil {
		t.Fatal("an intent that is not an object was not withheld")
	}
}

// TestWorkloadOutDigest: the digest is reported in full to a reader who sees the whole intent, and withheld
// from the security tier reading another owner's, whose intent has its host axis withheld.
func TestWorkloadOutDigest(t *testing.T) {
	v := store.WorkloadView{Workload: store.Workload{Name: "web", Owner: "alice", Digest: "9f1c4e", Intent: json.RawMessage(`{"task":"t"}`)}}
	now := time.Now()
	if got := workloadOut(v, workspaceReadFull, now).Digest; got != "9f1c4e" {
		t.Fatalf("full tier digest = %q, want 9f1c4e", got)
	}
	if got := workloadOut(v, workspaceReadSecurity, now).Digest; got != "" {
		t.Fatalf("security tier digest = %q, want it withheld", got)
	}
}

// TestWorkloadEventOf: a history row keeps its time, short action, generation and runs, and not the actor or
// the hashes: a replace names the run that replaced and the one it ended, a delete the run it ended.
func TestWorkloadEventOf(t *testing.T) {
	oldRun, newRun := uuid.New(), uuid.New()
	at := time.Now()
	for _, tc := range []struct {
		name, action, data string
		want               client.WorkloadEvent
	}{
		{"launch", "workload.launch", `{"generation":4,"run_id":"` + newRun.String() + `"}`,
			client.WorkloadEvent{Action: "launch", Generation: 4, RunID: &newRun}},
		{"replace", "workload.replace", `{"generation":4,"old_run_id":"` + oldRun.String() + `","new_run_id":"` + newRun.String() + `"}`,
			client.WorkloadEvent{Action: "replace", Generation: 4, RunID: &newRun, EndedRunID: &oldRun}},
		{"refused", "workload.launch_refused", `{"generation":3,"reason":"pool_lifetime_exceeded"}`,
			client.WorkloadEvent{Action: "launch_refused", Generation: 3, Reason: "pool_lifetime_exceeded"}},
		{"delete", "workload.delete", `{"ended_run_id":"` + oldRun.String() + `"}`,
			client.WorkloadEvent{Action: "delete", EndedRunID: &oldRun}},
		{"no data", "workload.restart", ``, client.WorkloadEvent{Action: "restart"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := workloadEventOf(types.AuditEvent{Time: at, Action: tc.action, Actor: "someone", RowHash: "h", Data: json.RawMessage(tc.data)})
			tc.want.Time = at
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("workloadEventOf = %+v, want %+v", got, tc.want)
			}
		})
	}
}
