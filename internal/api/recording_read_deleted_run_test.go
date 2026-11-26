// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestRecordingReplayOfADeletedRunIsAlwaysAudited: a recording outlives a run the run history retention sweep
// deleted. An admin's replay of it, with no run row to name an owner, writes a recording.read row marked
// run_deleted, whoever the reader is.
func TestRecordingReplayOfADeletedRunIsAlwaysAudited(t *testing.T) {
	srv, _, _, rs := newAuthzMatrixServer(t)
	rec := srv.cfg.Audit.(*recRecorder)
	adminSess := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)
	gone := uuid.New() // a cast with no run row
	if err := rs.SaveCast(context.Background(), gone.String(), strings.NewReader(`{"version":2}`+"\n")); err != nil {
		t.Fatal(err)
	}
	if w := doSSO(t, srv, http.MethodGet, "/api/v1/runs/"+gone.String()+"/recording/"+gone.String(), adminSess, ""); w.Code != http.StatusOK {
		t.Fatalf("admin replay of a deleted run's recording = %d %s, want 200", w.Code, w.Body.String())
	}
	var got []types.AuditEvent
	for _, ev := range rec.snapshot() {
		if ev.Action == auditRecordingRead && ev.RunID != nil && *ev.RunID == gone {
			got = append(got, ev)
		}
	}
	if len(got) != 1 || got[0].Target != gone.String() || got[0].Outcome != "success" {
		t.Fatalf("recording.read rows = %+v, want one success row targeting the cast key", got)
	}
	var data struct {
		RunDeleted bool `json:"run_deleted"`
	}
	if err := json.Unmarshal(got[0].Data, &data); err != nil || !data.RunDeleted {
		t.Fatalf("recording.read data = %s (%v), want run_deleted true", got[0].Data, err)
	}
}
