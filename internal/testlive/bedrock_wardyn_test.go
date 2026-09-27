// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package testlive

import (
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestBedrockWardynCredentialRowsOK pins the LL3w audit check hermetically:
// a per-run credential.mint row (actor_type=agent, a run-scoped identity)
// passes; no row, or a row attributed to the shared admin bearer, must fail.
func TestBedrockWardynCredentialRowsOK(t *testing.T) {
	perRun := types.AuditEvent{ID: uuid.New(), ActorType: types.ActorAgent, Actor: "spiffe://wardyn/run/abc123", Action: "credential.mint"}
	if err := BedrockWardynCredentialRowsOK([]types.AuditEvent{perRun}); err != nil {
		t.Fatalf("a genuine per-run credential.mint row was rejected: %v", err)
	}

	for _, tc := range []struct {
		name   string
		events []types.AuditEvent
	}{
		{"no credential row at all", []types.AuditEvent{{ID: uuid.New(), Action: "run.exec"}}},
		{"no rows", nil},
		{"attributed to the admin bearer", []types.AuditEvent{
			{ID: uuid.New(), ActorType: types.ActorSystem, Actor: adminTokenPrincipal, Action: "credential.mint"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := BedrockWardynCredentialRowsOK(tc.events); err == nil {
				t.Fatalf("events %+v were accepted", tc.events)
			}
		})
	}
}

// TestBedrockWardynForcedFaultOK pins the forced-fault check: the run's
// failure_hint must name the exact AWS error class asked for.
func TestBedrockWardynForcedFaultOK(t *testing.T) {
	const denyHint = "Amazon Bedrock refused the model call (AccessDeniedException): a policy denies it — " +
		"an AWS Organizations service control policy or an IAM policy on the role"
	if err := BedrockWardynForcedFaultOK(denyHint, "AccessDeniedException"); err != nil {
		t.Fatalf("a genuine AccessDeniedException hint was rejected: %v", err)
	}

	for _, tc := range []struct{ name, hint, class string }{
		{"empty hint", "", "AccessDeniedException"},
		{"wrong class in hint", "Amazon Bedrock is throttling this model (ThrottlingException)", "AccessDeniedException"},
		{"unknown class asked for", denyHint, "SomeOtherException"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := BedrockWardynForcedFaultOK(tc.hint, tc.class); err == nil {
				t.Fatalf("hint %q against class %q was accepted", tc.hint, tc.class)
			}
		})
	}
}
