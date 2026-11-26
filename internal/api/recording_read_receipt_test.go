// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ownerReadFailStore fails GetRun for one run, as a store fault does, and passes every other call through.
type ownerReadFailStore struct {
	store.Store
	fail uuid.UUID
}

func (s *ownerReadFailStore) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	if id == s.fail {
		return types.AgentRun{}, errors.New("store unavailable")
	}
	return s.Store.GetRun(ctx, id)
}

// replayAuditServer is a stopped run owned by owner, with its recording stored when cast is set, and the path of
// its replay.
func replayAuditServer(t *testing.T, owner string, cast bool) (*Server, *recRecorder, uuid.UUID, string) {
	t.Helper()
	srv, ast, _, rs := newAuthzMatrixServer(t)
	srv.cfg.Store = &envelopeStore{Store: srv.cfg.Store}
	id := uuid.New()
	ast.mu.Lock()
	ast.runs[id] = types.AgentRun{ID: id, CreatedBy: owner, State: types.RunStopped, Agent: "claude-code"}
	ast.mu.Unlock()
	if cast {
		if err := rs.SaveCast(context.Background(), id.String(), strings.NewReader(`{"version":2}`+"\n")); err != nil {
			t.Fatal(err)
		}
	}
	return srv, srv.cfg.Audit.(*recRecorder), id, "/api/v1/runs/" + id.String() + "/recording/" + id.String()
}

func replayAdmin(t *testing.T) *http.Cookie {
	return ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)
}

func recordingReadRows(rec *recRecorder, id uuid.UUID) []types.AuditEvent {
	var rows []types.AuditEvent
	for _, ev := range rec.snapshot() {
		if ev.Action == auditRecordingRead && ev.RunID != nil && *ev.RunID == id {
			rows = append(rows, ev)
		}
	}
	return rows
}

// TestRecordingReadAudit_MissingRecordingWritesNoRow: an admin's replay of another person's run that has no
// recording answers 404 and leaves no recording.read row: the row records a replay that happened.
func TestRecordingReadAudit_MissingRecordingWritesNoRow(t *testing.T) {
	srv, rec, id, path := replayAuditServer(t, "sub-member", false)
	if w := doSSO(t, srv, http.MethodGet, path, replayAdmin(t), ""); w.Code != http.StatusNotFound {
		t.Fatalf("replay of a missing recording = %d %s, want 404", w.Code, w.Body.String())
	}
	if rows := recordingReadRows(rec, id); len(rows) != 0 {
		t.Fatalf("%d recording.read rows for a replay that found nothing: %+v", len(rows), rows)
	}
}

// TestRecordingReadAudit_UnreadableOwnerIsAudited: when the run's owner cannot be read, an admin's replay is
// served (an operator reads every recording) and audited, marked owner_unknown.
func TestRecordingReadAudit_UnreadableOwnerIsAudited(t *testing.T) {
	srv, rec, id, path := replayAuditServer(t, "sub-member", true)
	srv.cfg.Store = &ownerReadFailStore{Store: srv.cfg.Store, fail: id}
	if w := doSSO(t, srv, http.MethodGet, path, replayAdmin(t), ""); w.Code != http.StatusOK {
		t.Fatalf("replay with an unreadable owner = %d %s, want 200", w.Code, w.Body.String())
	}
	rows := recordingReadRows(rec, id)
	if len(rows) != 1 || rows[0].Outcome != "success" || !strings.Contains(string(rows[0].Data), `"owner_unknown":true`) {
		t.Fatalf("recording.read rows %+v, want one success row with owner_unknown true", rows)
	}
}

// TestRecordingReadAudit_ServedReplays: an admin's replay of another person's run writes one row with no
// owner_unknown; the owner's own replay and an admin's replay of their own run write none.
func TestRecordingReadAudit_ServedReplays(t *testing.T) {
	srv, rec, id, path := replayAuditServer(t, "sub-member", true)
	if w := doSSO(t, srv, http.MethodGet, path, replayAdmin(t), ""); w.Code != http.StatusOK {
		t.Fatalf("admin replay = %d %s, want 200", w.Code, w.Body.String())
	}
	if w := doSSO(t, srv, http.MethodGet, path, outputOwnerCookie(t), ""); w.Code != http.StatusOK {
		t.Fatalf("owner replay = %d %s, want 200", w.Code, w.Body.String())
	}
	if rows := recordingReadRows(rec, id); len(rows) != 1 || strings.Contains(string(rows[0].Data), "owner_unknown") {
		t.Fatalf("recording.read rows %+v, want exactly the admin's, without owner_unknown", rows)
	}
	own, ownRec, ownID, ownPath := replayAuditServer(t, "sub-admin", true)
	if w := doSSO(t, own, http.MethodGet, ownPath, replayAdmin(t), ""); w.Code != http.StatusOK {
		t.Fatalf("admin replay of their own run = %d %s, want 200", w.Code, w.Body.String())
	}
	if rows := recordingReadRows(ownRec, ownID); len(rows) != 0 {
		t.Fatalf("recording.read rows %+v for the admin's own run, want none", rows)
	}
}
