// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"testing"
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
