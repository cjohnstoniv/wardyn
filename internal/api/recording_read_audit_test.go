// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestRecordingReplayOfAnotherPersonsRunIsAudited pins #2304: an admin replaying a recording of a run
// someone else created writes a run-scoped recording.read row naming the cast; a replay of their own
// run writes none.
func TestRecordingReplayOfAnotherPersonsRunIsAudited(t *testing.T) {
	srv, ast, _, rs := newAuthzMatrixServer(t)
	rec := srv.cfg.Audit.(*recRecorder)
	adminSess := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)
	seed := func(createdBy string) uuid.UUID {
		id := uuid.New()
		ast.mu.Lock()
		ast.runs[id] = types.AgentRun{ID: id, CreatedBy: createdBy, State: types.RunStopped, Agent: "claude-code"}
		ast.mu.Unlock()
		if err := rs.SaveCast(context.Background(), id.String(), strings.NewReader(`{"version":2}`+"\n")); err != nil {
			t.Fatal(err)
		}
		return id
	}
	reads := func(runID uuid.UUID) []types.AuditEvent {
		var out []types.AuditEvent
		for _, ev := range rec.snapshot() {
			if ev.Action == auditRecordingRead && ev.RunID != nil && *ev.RunID == runID {
				out = append(out, ev)
			}
		}
		return out
	}

	foreign := seed("sub-member")
	if w := doSSO(t, srv, http.MethodGet, "/api/v1/runs/"+foreign.String()+"/recording/"+foreign.String(), adminSess, ""); w.Code != http.StatusOK {
		t.Fatalf("admin replay of a member's run = %d %s, want 200", w.Code, w.Body.String())
	}
	got := reads(foreign)
	if len(got) != 1 || got[0].Target != foreign.String() || got[0].Outcome != "success" || got[0].Actor == "" {
		t.Fatalf("recording.read rows = %+v, want one success row targeting the cast key", got)
	}

	own := seed(got[0].Actor)
	if w := doSSO(t, srv, http.MethodGet, "/api/v1/runs/"+own.String()+"/recording/"+own.String(), adminSess, ""); w.Code != http.StatusOK {
		t.Fatalf("admin replay of their own run = %d %s, want 200", w.Code, w.Body.String())
	}
	if got := reads(own); len(got) != 0 {
		t.Fatalf("an owner's own replay was audited: %+v", got)
	}
}
