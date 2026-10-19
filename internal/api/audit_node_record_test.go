// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A run's owner reads its node record row and the runner's refusal of it, through the query and the
// export, without the source id or the node name; the security tier reads the rows as recorded.
func TestAudit_ANodeRecordsSourceIDIsTheSecurityTiers(t *testing.T) {
	runID, owner := uuid.New(), "sub-run-owner"
	const sourceID, nodeName = "i-0abc123", "worker-node-7"
	rows := []types.AuditEvent{{ID: uuid.New(), RunID: &runID, ActorType: types.ActorHuman, Actor: "sec", Action: auditRunNodeRecord,
		Target: runID.String(), Outcome: "success",
		Data: json.RawMessage(`{"attestation":"node_powered_off","people":1,"source":"provider_console","source_id":"` + sourceID + `"}`)},
		{ID: uuid.New(), RunID: &runID, ActorType: types.ActorSystem, Actor: "runner:r1", Action: auditRunnerActionRefuse,
			Target: uuid.NewString(), Outcome: "failure",
			Data: json.RawMessage(`{"action":"stop_proxy","reason":"a Node still holds proxy's node name ` + nodeName + ` on the acceptance list","run_id":"` + runID.String() + `"}`)}}
	srv, _ := sharedAuditDoors(t, runID, owner, rows)
	paths := []string{"/api/v1/audit?run_id=" + runID.String(), "/api/v1/audit/export?run_id=" + runID.String()}
	for _, path := range paths {
		w := doSSO(t, srv, http.MethodGet, path, ssoSession(t, owner, "owner@corp.example", oidc.RoleUser), "")
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"attestation":"node_powered_off"`) {
			t.Fatalf("the owner's GET %s = %d %s, want 200 with the row", path, w.Code, w.Body)
		}
		if strings.Contains(w.Body.String(), sourceID) || strings.Contains(w.Body.String(), nodeName) {
			t.Errorf("the owner's GET %s serves the source id or the node name: %s", path, w.Body)
		}
		if !strings.Contains(w.Body.String(), nodeRecordReasonWithheld) {
			t.Errorf("the owner's GET %s lost the refusal row's reason sentence: %s", path, w.Body)
		}
		w = doSSO(t, srv, http.MethodGet, path, ssoSession(t, "sec", "sec@corp.example", oidc.RoleSecurityAdmin), "")
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), sourceID) || !strings.Contains(w.Body.String(), nodeName) {
			t.Errorf("the security tier's GET %s = %d %s, want the source id and the node name", path, w.Code, w.Body)
		}
	}
}
