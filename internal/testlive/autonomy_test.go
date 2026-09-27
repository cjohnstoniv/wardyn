// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package testlive

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestAutonomyL0RefusalOK pins the exact shape TestLive_AutonomyL0Enforced
// checks the live server's 403 body against, hermetically: no live tag, no
// network. It also proves the check is not vacuous — a body that is a 403 for
// some OTHER reason, or that resolves to a different rung, must fail it.
func TestAutonomyL0RefusalOK(t *testing.T) {
	const l0Body = `{"error":"unattended runs are not allowed by your governance profile ` +
		`\"capped-l0\" at this run's posture: it permits autonomy level L0 (bound by egress_sealed), ` +
		`which requires a human at the pane. Launch with --interactive, or narrow the run's egress, ` +
		`secrets or confinement.","reason":"governance_profile"}`
	if err := AutonomyL0RefusalOK(l0Body); err != nil {
		t.Fatalf("a genuine L0 refusal body was rejected: %v", err)
	}

	for _, tc := range []struct {
		name, body string
	}{
		{"unrelated 403", `{"error":"you are not granted integration bedrock-prod — ask an admin to grant it"}`},
		{"resolves to L1, not L0", `{"error":"unattended runs are not allowed by your governance profile ` +
			`\"capped-l1\" at this run's posture: it permits autonomy level L1 (bound by secrets_powerful), ` +
			`which requires a human at the pane."}`},
		{"names the level but not the unattended gate", `{"error":"seed_auto_tools is not allowed by your ` +
			`governance profile \"capped-l0\" at this run's posture: it permits autonomy level L0 (bound by ` +
			`confinement_cc1), and the pre-attach seed runs before any human is at the pane."}`},
		{"empty", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := AutonomyL0RefusalOK(tc.body); err == nil {
				t.Fatalf("body %q was accepted as an L0 unattended-run refusal", tc.body)
			}
		})
	}
}

// agentPolicyEvent builds a run.agent_policy.write row with the given data,
// mirroring auditAgentPolicy's own marshalling.
func agentPolicyEvent(t *testing.T, level string, delivered bool, reason string) types.AuditEvent {
	t.Helper()
	data, err := json.Marshal(agentPolicyWriteData{Level: level, Delivered: delivered, Reason: reason})
	if err != nil {
		t.Fatal(err)
	}
	return types.AuditEvent{ID: uuid.New(), Action: "run.agent_policy.write", Data: data}
}

// TestAgentPolicyDeliveredAndNoExec pins the LL5 k8s/Docker delivery check
// hermetically: delivered=true at the right level with no run.exec row
// passes; a missing row, the wrong level, delivered=false, or a run.exec row
// alongside it must all fail.
func TestAgentPolicyDeliveredAndNoExec(t *testing.T) {
	good := []types.AuditEvent{agentPolicyEvent(t, "L0", true, "")}
	if err := AgentPolicyDeliveredAndNoExec(good, "L0"); err != nil {
		t.Fatalf("a genuine delivered=true L0 row was rejected: %v", err)
	}

	for _, tc := range []struct {
		name   string
		events []types.AuditEvent
	}{
		{"no agent_policy row at all", nil},
		{"delivered=false", []types.AuditEvent{agentPolicyEvent(t, "L0", false, "the sandbox spec did not carry the file")}},
		{"wrong level", []types.AuditEvent{agentPolicyEvent(t, "L1", true, "")}},
		{"a run.exec row is present too", []types.AuditEvent{
			agentPolicyEvent(t, "L0", true, ""),
			{ID: uuid.New(), Action: "run.exec"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := AgentPolicyDeliveredAndNoExec(tc.events, "L0"); err == nil {
				t.Fatalf("events %+v were accepted", tc.events)
			}
		})
	}
}
