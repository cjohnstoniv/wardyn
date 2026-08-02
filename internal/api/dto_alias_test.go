// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// TestCreateRunRequest_IsClientDTOAlias pins that the server's create-run body
// (createRunRequest) is the SAME type as the public SDK's client.CreateRunRequest,
// not a hand-maintained copy that can drift. The declaration in runs.go is a type
// alias (`type createRunRequest = client.CreateRunRequest`), so the two
// assignments below hold at COMPILE time — reverting the alias to an independent
// struct fails to build HERE rather than silently re-opening the three-way drift
// the alias closed. Its counterpart on the SDK side,
// pkg/client/dto_parity_test.go, pins the wire tag OPTIONS, which type identity
// does not cover.
func TestCreateRunRequest_IsClientDTOAlias(t *testing.T) {
	// Compile-time identity: only legal because createRunRequest IS
	// client.CreateRunRequest. A struct copy would not assign either direction.
	var _ createRunRequest = client.CreateRunRequest{}
	var _ client.CreateRunRequest = createRunRequest{}
}

// TestRequestDTOs_AreClientDTOAliases pins the same property for the other two
// server bodies whose SDK twin is exported. workspaceRequest is the one that
// bit: as a copy it lacked llm_cred, and decodeWorkspaceRequest rejects unknown
// fields, so an SDK caller could neither set the model binding nor smuggle it
// through while the console could.
func TestRequestDTOs_AreClientDTOAliases(t *testing.T) {
	var _ policyRequest = client.PolicyRequest{}
	var _ client.PolicyRequest = policyRequest{}
	var _ workspaceRequest = client.WorkspaceRequest{}
	var _ client.WorkspaceRequest = workspaceRequest{}
}
