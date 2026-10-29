// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
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
