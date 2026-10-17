// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// sharedMintStore is a finished run that minted one `shared` grant and one of
// its owner's own.
func sharedMintStore(run types.AgentRun, ws types.Workspace) (*recordStore, uuid.UUID) {
	shared, own := uuid.New(), uuid.New()
	mint := func(id uuid.UUID) types.AuditEvent {
		return types.AuditEvent{RunID: &run.ID, Action: "credential.mint", Outcome: "success",
			Data: mustJSON(map[string]any{"grant_id": id.String()})}
	}
	return &recordStore{
		run:             run,
		importStateFake: importStateFake{ws: ws},
		events:          []types.AuditEvent{egressAllowEvent(run.ID, "org-api.example"), mint(shared), mint(own)},
		grants: []types.CredentialGrant{
			{ID: shared, RunID: run.ID, Spec: types.GrantSpec{Kind: types.GrantAPIKey, Scope: json.RawMessage(sharedScope), TTLSeconds: 3600}},
			{ID: own, RunID: run.ID, Spec: types.GrantSpec{Kind: types.GrantAPIKey, OwnerOnly: true, TTLSeconds: 3600,
				Scope: json.RawMessage(`{"host":"person-api.example","require_tls":true,"secret_name":"person-secret"}`)}},
		},
	}, shared
}

// A record result lists the secrets a session was proven to use, and the
// workspace's owner reads it. A `shared` grant is listed by its kind, never by
// what the organisation's secret is called; a person's own is named as before.
func TestReconcileRecordRun_ASharedGrantIsListedByKind(t *testing.T) {
	runID, wsID := uuid.New(), uuid.New()
	fake, _ := sharedMintStore(types.AgentRun{ID: runID, WorkspaceID: &wsID, Task: "workspace record",
		State: types.RunCompleted, ConfinementClass: types.CC3}, recordingWorkspace(wsID, runID, "build"))
	srv := New(baseTestConfig(newHarness(t), fake))

	srv.reconcileRecordRun(context.Background(), runID)

	res := fake.savedResult(t, "build")
	if res.Status != recordStatusRecorded {
		t.Fatalf("status = %q, want recorded (hint=%s)", res.Status, res.FailureHint)
	}
	if !slices.Equal(res.SecretNamesMinted, []string{"api_key", "person-secret"}) {
		t.Errorf("secret names = %v, want the shared grant by kind and the person's own by name", res.SecretNamesMinted)
	}
	if strings.Contains(string(fake.saved), sharedSecretName) {
		t.Errorf("the stored record result names the organisation's secret: %s", fake.saved)
	}
}
