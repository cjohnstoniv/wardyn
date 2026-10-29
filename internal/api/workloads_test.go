// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
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
