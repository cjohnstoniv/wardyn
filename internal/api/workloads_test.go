// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// TestWorkloadOutBlanksTheRefusalMessageForTheSecurityTier: the security tier reads a refusal's reason code
// but not its sentence, which can name the image the intent's read blanks; the full tier reads both.
func TestWorkloadOutBlanksTheRefusalMessageForTheSecurityTier(t *testing.T) {
	v := store.WorkloadView{Workload: store.Workload{Name: "web", Owner: "service:bot", Intent: json.RawMessage(`{"task":"t","image":"r/x:1"}`),
		LastRefusal: &store.WorkloadRefusal{Generation: 2, Reason: "image_not_allowed", Message: "image r/x:1 is not allowed"}}}
	if f := workloadOut(v, workspaceReadFull, time.Now()).LastRefusal; f == nil || f.Reason != "image_not_allowed" || f.Message != "image r/x:1 is not allowed" {
		t.Fatalf("full tier refusal = %+v, want reason and message", f)
	}
	if f := workloadOut(v, workspaceReadSecurity, time.Now()).LastRefusal; f == nil || f.Reason != "image_not_allowed" || f.Message != "" {
		t.Fatalf("security tier refusal = %+v, want the reason and no message", f)
	}
}

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

// A launch refused placement_credential for one of its policy's grants says where such a grant often comes
// from (the default policy) and how to replace it (eligible_grants), within the status message's bound. Any
// other credential refusal, and any other reason, keeps the create path's sentence as it was.
func TestWorkloadRefusalMsg_NamesTheDefaultPolicyForAGrant(t *testing.T) {
	grant := (&placement.Refusal{Reason: placement.ReasonPlacementCredential, Field: "ProxyConfig.Policy.EligibleGrants[0]",
		Detail: "credential provenance does not match its actual grant"}).Error()
	got := workloadRefusalMsg(string(placement.ReasonPlacementCredential), grant)
	if !strings.HasPrefix(got, grant+" ") || !strings.Contains(got, "default policy") || !strings.Contains(got, "eligible_grants") {
		t.Fatalf("grant refusal = %q, want the create path's sentence and the default-policy hint", got)
	}
	long := workloadRefusalMsg(string(placement.ReasonPlacementCredential), grant+strings.Repeat(" é", runnerwire.MaxWorkloadMessage))
	if len(long) > runnerwire.MaxWorkloadMessage || !utf8.ValidString(long) {
		t.Fatalf("a long grant refusal is %d bytes (valid %v), want at most %d", len(long), utf8.ValidString(long), runnerwire.MaxWorkloadMessage)
	}
	for _, tc := range []struct{ reason, msg string }{
		{string(placement.ReasonPlacementCredential), (&placement.Refusal{Reason: placement.ReasonPlacementCredential,
			Field: "upstream_proxy_secret_ref", Detail: "the runner must use its own upstream proxy configuration"}).Error()},
		{string(placement.ReasonPlacementCredential), localAWSCLIRefusal},
		{"policy_ceiling_exceeded", grant},
	} {
		if got := workloadRefusalMsg(tc.reason, tc.msg); got != tc.msg {
			t.Errorf("%s %q = %q, want it unchanged", tc.reason, tc.msg, got)
		}
	}
}
