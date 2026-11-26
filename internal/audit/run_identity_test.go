// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func identityData(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("data %s: %v", raw, err)
	}
	return m
}

// TestRunIdentity_StampsAndOverwrites: the three keys are set from the identity, overwriting what a caller posted, and every
// other key (including "owner" and "workload") is untouched. A sponsor is kept only for a service owner.
func TestRunIdentity_StampsAndOverwrites(t *testing.T) {
	person := StampRunIdentity(json.RawMessage(`{"owner":"x","workload":"w","run_owner":"forged","sponsor":"forged","k":1}`),
		RunIdentity{Owner: "alice@example.com", Sponsor: "boss", Workload: "spiffe://t/agent-run/1"})
	m := identityData(t, person)
	if m["run_owner"] != "alice@example.com" || m["run_workload"] != "spiffe://t/agent-run/1" || m["owner"] != "x" || m["workload"] != "w" || m["k"] != float64(1) {
		t.Errorf("person-owned run: data = %s", person)
	}
	if _, ok := m["sponsor"]; ok {
		t.Errorf("person-owned run keeps a sponsor: %s", person)
	}
	svc := identityData(t, StampRunIdentity(nil, RunIdentity{Owner: "service:ci", Sponsor: "boss@example.com", Workload: "w1"}))
	if svc["run_owner"] != "service:ci" || svc["sponsor"] != "boss@example.com" || svc["run_workload"] != "w1" {
		t.Errorf("service-owned run: data = %v", svc)
	}
	// Empty values never leave a key behind, and a non-object body is kept whole.
	bare := identityData(t, StampRunIdentity(json.RawMessage(`[1,2]`), RunIdentity{}))
	if _, ok := bare["run_owner"]; ok || bare["data"] == nil {
		t.Errorf("empty identity over array data: %v", bare)
	}
}

// TestRunIdentity_PersonOwnerDropsPostedSponsor: a sponsor offered with a person owner is dropped at the context.
func TestRunIdentity_PersonOwnerDropsPostedSponsor(t *testing.T) {
	for owner, want := range map[string]string{"alice@example.com": "", "": "", "service:ci": "s"} {
		id, ok := RunIdentityFrom(WithRunIdentity(context.Background(), owner, "s", "w"))
		if !ok || id.Sponsor != want || id.Owner != owner {
			t.Errorf("owner %q: %+v ok=%v, want sponsor %q", owner, id, ok, want)
		}
	}
	if _, ok := RunIdentityFrom(context.Background()); ok {
		t.Error("a bare context reports a run identity")
	}
}

type identityCapture struct{ got []types.AuditEvent }

func (c *identityCapture) Record(_ context.Context, ev types.AuditEvent) error {
	c.got = append(c.got, ev)
	return nil
}

// TestRunIdentityRecorder_RecordBatchStampsEachRow: under a run identity every event is stamped, single or batched; under none, data
// is the exact bytes it was.
func TestRunIdentityRecorder_RecordBatchStampsEachRow(t *testing.T) {
	in := types.AuditEvent{Data: json.RawMessage(`{"a":  1}`)}
	cap := &identityCapture{}
	rec := RunIdentityRecorder{Inner: cap}
	if err := rec.Record(context.Background(), in); err != nil || string(cap.got[0].Data) != `{"a":  1}` {
		t.Fatalf("no identity: %v %s", err, cap.got[0].Data)
	}
	ctx := WithRunIdentity(context.Background(), "alice@example.com", "", "w")
	cap.got = nil
	if err := rec.Record(ctx, in); err != nil {
		t.Fatal(err)
	}
	if err := rec.RecordBatch(ctx, []types.AuditEvent{in, in}); err != nil {
		t.Fatal(err)
	}
	if len(cap.got) != 3 {
		t.Fatalf("recorded %d, want 3", len(cap.got))
	}
	for _, ev := range cap.got {
		if m := identityData(t, ev.Data); m["run_owner"] != "alice@example.com" || m["run_workload"] != "w" || m["a"] != float64(1) {
			t.Errorf("data = %s", ev.Data)
		}
	}
}

// TestRunIdentity_ExistingOwnerKeysUnchanged: over a secret.read-shaped and a principal_key.destroyed-shaped
// object, owner keeps its stored value and only the run keys are added.
func TestRunIdentity_ExistingOwnerKeysUnchanged(t *testing.T) {
	id := RunIdentity{Owner: "bob@example.com", Workload: "spiffe://t/agent-run/2"}
	for name, in := range map[string]string{
		"secret.read":             `{"purpose":"run","owner":"team-a","name":"x"}`,
		"principal_key.destroyed": `{"owner":"alice@example.com","purpose":"audit","generations":[1]}`,
	} {
		before, after := identityData(t, json.RawMessage(in)), identityData(t, StampRunIdentity(json.RawMessage(in), id))
		for k, v := range before {
			if want, _ := json.Marshal(v); true {
				if got, _ := json.Marshal(after[k]); string(got) != string(want) {
					t.Errorf("%s: %s = %s, want %s", name, k, got, want)
				}
			}
		}
		if len(after) != len(before)+2 || after[RunOwnerKey] != "bob@example.com" || after[RunWorkloadKey] != "spiffe://t/agent-run/2" {
			t.Errorf("%s: after = %v, want the stored keys plus the two run keys", name, after)
		}
	}
}

// TestRunIdentity_NoContextLeavesData: a context with no run identity leaves the data as the same bytes.
func TestRunIdentity_NoContextLeavesData(t *testing.T) {
	in := json.RawMessage(`{"owner": "x",  "sponsor":"kept"}`)
	if got := (RunIdentityRecorder{}).stamp(context.Background(), in); string(got) != string(in) {
		t.Errorf("data = %s, want %s", got, in)
	}
}
