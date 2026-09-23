// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestResolveRunLLMLanes_HonoursTheRunsOwnModelRunAnswer is B2-F7, later
// corrected by #767: the workspace_id term this door once passed to isModelRun
// was itself the bug. This create door (POST /runs, /runs/preflight) can never
// produce a scan run — seedRequestWorkspace (runs_create.go) never sets
// run.WorkspaceID from req.WorkspaceID, so a non-interactive `--workspace`
// launch (the ordinary CLI/console shape) reads as a scan HERE while dispatch,
// reading the run's own never-set WorkspaceID, credentials it as a model run
// anyway. So a create-time request carrying workspace_id must answer the SAME
// as one that does not: only task_mode=exec is excluded at this door.
func TestResolveRunLLMLanes_HonoursTheRunsOwnModelRunAnswer(t *testing.T) {
	h := newHarness(t)
	cfg := bedrockBearerCfg()
	cfg.Identity, cfg.Audit = h.idp, h.audit
	srv := New(cfg)
	wsID := uuid.New()

	for _, tc := range []struct {
		name      string
		req       createRunRequest
		wantReady bool
	}{
		{
			name:      "a non-interactive workspace launch IS a model run at this door (#767)",
			req:       createRunRequest{Agent: "claude-code", WorkspaceID: &wsID, Interactive: false},
			wantReady: true,
		},
		{
			name:      "an exec run runs a plain shell command",
			req:       createRunRequest{Agent: "claude-code", TaskMode: "exec", Task: "ls"},
			wantReady: false,
		},
		{
			name:      "an interactive workspace run is human-driven, so it IS a model run",
			req:       createRunRequest{Agent: "claude-code", WorkspaceID: &wsID, Interactive: true},
			wantReady: true,
		},
		{
			name:      "an ordinary agent run",
			req:       createRunRequest{Agent: "claude-code", Task: "fix it"},
			wantReady: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := types.RunPolicySpec{}
			lanes := srv.resolveRunLLMLanes(context.Background(), tc.req, &spec, nil, awsSSOScope{}, false)
			if lanes.bedrock.ready != tc.wantReady {
				t.Errorf("bedrock.ready = %v, want %v — create must answer the same modelRun question dispatch does",
					lanes.bedrock.ready, tc.wantReady)
			}
		})
	}
}

// TestPreflight_ExecRunDoesNotClaimBedrock is the user-visible half: the
// checklist row a Review renders for a non-model run must not promise a
// credential dispatch will not hand it. task_mode=exec is the only non-model
// shape this create door can actually receive (#767: a workspace_id request
// here is an ordinary launch, never a scan — seedRequestWorkspace never sets
// run.WorkspaceID from it, so there is no create-request shape that reads as a
// scan at dispatch).
func TestPreflight_ExecRunDoesNotClaimBedrock(t *testing.T) {
	h := newHarness(t)
	cfg := bedrockBearerCfg()
	cfg.Identity, cfg.Audit, cfg.AdminToken = h.idp, h.audit, adminToken
	cfg.TrustDomain, cfg.ControlPlaneURL = "wardyn.local", "http://wardynd:8080"
	srv := New(cfg)

	access := srv.resolveRunLLMAccess(context.Background(),
		createRunRequest{Agent: "claude-code", TaskMode: "exec", Task: "ls"},
		types.RunPolicySpec{}, map[string]bool{}, nil, "")
	if access != nil && access.Provisioned && strings.Contains(access.Note, "Bedrock") {
		t.Errorf("model-access note = %q; an exec run gets no Bedrock credential at dispatch", access.Note)
	}
}

// TestAWSSSORefresh_ZeroRegistrationExpiryIsOmittedFromTheAuditRow is B2-F8.
//
// The harness.credential.refresh row formatted a ZERO RegistrationExpiresAt as
// 0001-01-01T00:00:00Z, which reads as "the client registration lapsed long
// ago" — the exact opposite of registrationLapsed's rule that a zero value
// counts as LIVE. awsSSOCacheFileContents already omits the key; this row now
// does too.
func TestAWSSSORefresh_ZeroRegistrationExpiryIsOmittedFromTheAuditRow(t *testing.T) {
	s, audit, blob := ssoRefreshServer(t)
	blob.RegistrationExpiresAt = time.Time{}
	storeSSOBlob(t, s, blob)
	fakeOIDC(t, func(w http.ResponseWriter, _ map[string]string, _ int) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accessToken": "fresh-access-token-abcdefghij", "expiresIn": 3600,
		})
	})

	_ = s.resolveBedrockAuth(context.Background(), "claude-code", false, true, true, nil, awsSSOScope{})

	rows := audit.find("harness.credential.refresh")
	if len(rows) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(rows))
	}
	var data map[string]any
	if err := json.Unmarshal(rows[0].Data, &data); err != nil {
		t.Fatalf("audit data: %v", err)
	}
	if v, ok := data["registration_expires_at"]; ok {
		t.Errorf("registration_expires_at = %v; a zero registration expiry means UNKNOWN and must be absent, "+
			"not rendered as a date in the year 1", v)
	}
}

// TestAWSSSORefresh_RegistrationExpiryStillRecordedWhenKnown is B2-F8's
// negative control.
func TestAWSSSORefresh_RegistrationExpiryStillRecordedWhenKnown(t *testing.T) {
	s, audit, _ := ssoRefreshServer(t)
	fakeOIDC(t, func(w http.ResponseWriter, _ map[string]string, _ int) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accessToken": "fresh-access-token-abcdefghij", "expiresIn": 3600,
		})
	})

	_ = s.resolveBedrockAuth(context.Background(), "claude-code", false, true, true, nil, awsSSOScope{})

	rows := audit.find("harness.credential.refresh")
	if len(rows) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(rows))
	}
	var data map[string]any
	if err := json.Unmarshal(rows[0].Data, &data); err != nil {
		t.Fatalf("audit data: %v", err)
	}
	if _, ok := data["registration_expires_at"]; !ok {
		t.Error("registration_expires_at missing for a blob that carries one")
	}
}
