// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"reflect"
	"testing"

	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// TestCreateRunRequest_IsClientDTOAlias pins that the server's create-run body
// (createRunRequest) is the SAME type as the public SDK's client.CreateRunRequest,
// not a hand-maintained copy that can drift. The declaration in runs.go is a type
// alias (`type createRunRequest = client.CreateRunRequest`), so this assignment
// and the reflect identity below both hold at compile time — reverting the alias
// to an independent struct fails to compile HERE (the assignment) rather than
// silently re-opening the three-way drift the alias closed. Its counterpart on
// the SDK side, pkg/client/dto_parity_test.go, guards the wire tags structurally
// for external readers who cannot name the unexported alias target.
func TestCreateRunRequest_IsClientDTOAlias(t *testing.T) {
	// Compile-time identity: only legal because createRunRequest IS
	// client.CreateRunRequest. A struct copy would not assign either direction.
	var _ createRunRequest = client.CreateRunRequest{}
	var _ client.CreateRunRequest = createRunRequest{}

	if got, want := reflect.TypeOf(createRunRequest{}), reflect.TypeOf(client.CreateRunRequest{}); got != want {
		t.Fatalf("createRunRequest is %v, want the identical type %v — the alias was replaced by a copy", got, want)
	}
}
