// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestCreateDoorIsModelRun pins the ONE predicate the create door asks of a
// run request (#767 step 2), so it and dispatch can never ask a different
// question.
func TestCreateDoorIsModelRun(t *testing.T) {
	wsID := uuid.New()
	for _, tc := range []struct {
		name string
		req  createRunRequest
		want bool
	}{
		{name: "a non-interactive workspace launch IS a model run at this door (#767)",
			req: createRunRequest{Agent: "claude-code", WorkspaceID: &wsID, Interactive: false}, want: true},
		{name: "an interactive workspace run is human-driven, so it IS a model run",
			req: createRunRequest{Agent: "claude-code", WorkspaceID: &wsID, Interactive: true}, want: true},
		{name: "an ordinary agent run", req: createRunRequest{Agent: "claude-code", Task: "fix it"}, want: true},
		{name: "an exec run runs a plain shell command",
			req: createRunRequest{Agent: "claude-code", TaskMode: "exec", Task: "ls"}, want: false},
		{name: "a harness login run is never a model run, whatever isModelRun would answer",
			req: createRunRequest{Agent: "claude-code", Task: harnessLoginTask, WorkspaceID: &wsID, Interactive: false}, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := createDoorIsModelRun(tc.req); got != tc.want {
				t.Errorf("createDoorIsModelRun(%+v) = %v, want %v", tc.req, got, tc.want)
			}
		})
	}
}

// TestCreateRunAuditData_CarriesClampWarnings: the run-bound run.create success
// datum is where the tightening list lives (the run-detail "Effective policy"
// widget's only join key), and it is absent — not empty — when nothing narrowed.
func TestCreateRunAuditData_CarriesClampWarnings(t *testing.T) {
	warns := []string{"resources capped to operator maximum", `dropped 1 egress domain(s) not in operator allowlist: ["evil.example"]`}
	data := createRunAuditData(createRunRequest{Agent: "claude-code"}, nil, types.ConfinementClass("CC2"), types.ConfinementClass("CC2"), "jti", warns, types.AutonomyResolution{}, false, runProviderChoice{})
	got, ok := data["clamp_warnings"].([]string)
	if !ok || len(got) != len(warns) || got[0] != warns[0] {
		t.Fatalf("clamp_warnings = %#v, want %#v", data["clamp_warnings"], warns)
	}
	if _, present := createRunAuditData(createRunRequest{Agent: "claude-code"}, nil, types.ConfinementClass("CC2"), types.ConfinementClass("CC2"), "jti", nil, types.AutonomyResolution{}, false, runProviderChoice{})["clamp_warnings"]; present {
		t.Error("clamp_warnings must be absent when launch narrowed nothing")
	}
}

// TestCreateRunAuditData_CredentialConfinement is #150: the closed-vocabulary
// credential_confinement field is present, with the ONE value it carries
// today, exactly when the caller says this run's SSO-delivered credential is
// below the confinement floor — and absent otherwise, never published as a
// false negative.
func TestCreateRunAuditData_CredentialConfinement(t *testing.T) {
	req := createRunRequest{Agent: "claude-code"}
	data := createRunAuditData(req, nil, types.CC1, "", "jti", nil, types.AutonomyResolution{}, true, runProviderChoice{})
	if got := data["credential_confinement"]; got != credentialConfinementBelowFloor {
		t.Errorf("credential_confinement = %v, want %q", got, credentialConfinementBelowFloor)
	}
	if _, present := createRunAuditData(req, nil, types.CC3, "", "jti", nil, types.AutonomyResolution{}, false, runProviderChoice{})["credential_confinement"]; present {
		t.Error("credential_confinement must be absent when the caller reports no below-floor advisory")
	}
}

// TestCreateRunAuditData_ConfinementSource is 0.7.8: reqCC ("" for an
// unspecified request) is what decides confinement_source, never enforced —
// enforced alone cannot tell a caller who asked for CC1 apart from a runner
// that only had CC1 to offer, and the audit row is the one place that
// distinction survives (docs/AUDIT-ACTIONS.md's run.create row).
func TestCreateRunAuditData_ConfinementSource(t *testing.T) {
	req := createRunRequest{Agent: "claude-code"}
	if got := createRunAuditData(req, nil, types.CC1, "", "jti", nil, types.AutonomyResolution{}, false, runProviderChoice{})["confinement_source"]; got != "defaulted" {
		t.Errorf("confinement_source = %v, want \"defaulted\" for an empty reqCC", got)
	}
	if got := createRunAuditData(req, nil, types.CC1, types.CC1, "jti", nil, types.AutonomyResolution{}, false, runProviderChoice{})["confinement_source"]; got != "requested" {
		t.Errorf("confinement_source = %v, want \"requested\" when the caller named CC1 explicitly", got)
	}
}
