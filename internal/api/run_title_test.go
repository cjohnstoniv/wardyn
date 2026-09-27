// Copyright 2025 The Wardyn Authors
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

const runTitleOwner = "sub-run-title-owner"

// titleStore is dispatchTestStore plus store.RunTitler: SetRunTitle writes
// straight into the shared run (under dispatchTestStore's own mutex) so a
// following GetRun sees the new title, the same shape leaseStore's own
// SetRunEndAndWait double follows for the lease PATCH.
type titleStore struct {
	*dispatchTestStore
}

func (s *titleStore) SetRunTitle(_ context.Context, _ uuid.UUID, title string) error {
	s.dispatchTestStore.mu.Lock()
	defer s.dispatchTestStore.mu.Unlock()
	s.dispatchTestStore.run.Title = title
	return nil
}

func newRunTitleFixture(t *testing.T, state types.RunState) (*Server, *titleStore, *recRecorder) {
	t.Helper()
	run := newFinalizeRun()
	run.CreatedBy = runTitleOwner
	run.Title = "original title"
	h := newHarness(t)
	st := &titleStore{dispatchTestStore: &dispatchTestStore{run: run, state: state}}
	cfg := baseTestConfig(h, st)
	cfg.OIDC = &oidc.Authenticator{}
	return New(cfg), st, h.audit
}

func runTitleOwnerSession(t *testing.T) *http.Cookie {
	return ssoSession(t, runTitleOwner, "owner@corp.example", oidc.RoleUser)
}

func runTitlePatch(t *testing.T, srv *Server, cookie *http.Cookie, id, body string) (int, map[string]any) {
	t.Helper()
	w := doSSO(t, srv, http.MethodPatch, "/api/v1/runs/"+id+"/title", cookie, body)
	var out map[string]any
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return w.Code, out
}

func (s *titleStore) storedTitle() string {
	s.dispatchTestStore.mu.Lock()
	defer s.dispatchTestStore.mu.Unlock()
	return s.dispatchTestStore.run.Title
}

func titleAuditRows(audit *recRecorder) []types.AuditEvent {
	var out []types.AuditEvent
	for _, ev := range audit.snapshot() {
		if ev.Action == "run.title.set" {
			out = append(out, ev)
		}
	}
	return out
}

// TestSetRunTitle_OwnerRenamesRunningRun: the owner renames a RUNNING run —
// 200, the store is written, and the audit row carries the old and new
// titles.
func TestSetRunTitle_OwnerRenamesRunningRun(t *testing.T) {
	srv, st, audit := newRunTitleFixture(t, types.RunRunning)
	id := st.dispatchTestStore.run.ID.String()

	code, out := runTitlePatch(t, srv, runTitleOwnerSession(t), id, `{"title":"Refund flow retry"}`)
	if code != http.StatusOK {
		t.Fatalf("rename a RUNNING run = %d, want 200", code)
	}
	if out["title"] != "Refund flow retry" {
		t.Errorf("response title = %v, want %q", out["title"], "Refund flow retry")
	}
	if got := st.storedTitle(); got != "Refund flow retry" {
		t.Errorf("stored title = %q, want %q", got, "Refund flow retry")
	}

	rows := titleAuditRows(audit)
	if len(rows) != 1 {
		t.Fatalf("run.title.set rows = %d, want 1", len(rows))
	}
	if rows[0].Actor != runTitleOwner || rows[0].Outcome != "success" {
		t.Fatalf("audit row = actor %q outcome %q, want the owner, success", rows[0].Actor, rows[0].Outcome)
	}
	var data map[string]any
	if err := json.Unmarshal(rows[0].Data, &data); err != nil {
		t.Fatalf("decode audit data: %v", err)
	}
	if data["from"] != "original title" || data["to"] != "Refund flow retry" {
		t.Errorf("audit data = %v, want from %q to %q", data, "original title", "Refund flow retry")
	}
}

// TestSetRunTitle_OwnerRenamesEndedRun: unlike PATCH /runs/{id} (end/wait),
// this route is allowed on a TERMINAL run — a title is a display field, not
// part of the lease.
func TestSetRunTitle_OwnerRenamesEndedRun(t *testing.T) {
	srv, st, audit := newRunTitleFixture(t, types.RunArchived)
	id := st.dispatchTestStore.run.ID.String()

	code, out := runTitlePatch(t, srv, runTitleOwnerSession(t), id, `{"title":"Renamed after ending"}`)
	if code != http.StatusOK {
		t.Fatalf("rename an ARCHIVED (ended) run = %d, want 200", code)
	}
	if out["title"] != "Renamed after ending" {
		t.Errorf("response title = %v, want %q", out["title"], "Renamed after ending")
	}
	if got := st.storedTitle(); got != "Renamed after ending" {
		t.Errorf("stored title = %q, want %q", got, "Renamed after ending")
	}
	rows := titleAuditRows(audit)
	if len(rows) != 1 || rows[0].Outcome != "success" {
		t.Fatalf("run.title.set rows = %+v, want exactly one success row", rows)
	}
}

// TestSetRunTitle_MemberOnForeignRunGets404: a member (not the owner, not an
// admin) gets the byte-identical 404 a missing run would — no existence
// oracle (getRunAuthorized).
func TestSetRunTitle_MemberOnForeignRunGets404(t *testing.T) {
	srv, st, audit := newRunTitleFixture(t, types.RunRunning)
	id := st.dispatchTestStore.run.ID.String()
	foreign := ssoSession(t, "sub-someone-else", "someone@corp.example", oidc.RoleUser)

	code, _ := runTitlePatch(t, srv, foreign, id, `{"title":"hijacked"}`)
	if code != http.StatusNotFound {
		t.Fatalf("member renaming a foreign run = %d, want 404", code)
	}
	if got := st.storedTitle(); got != "original title" {
		t.Errorf("stored title changed to %q; a refused rename must write nothing", got)
	}
	if rows := titleAuditRows(audit); len(rows) != 0 {
		t.Errorf("run.title.set rows = %d on a refused rename, want 0", len(rows))
	}
}

// TestSetRunTitle_AdminOnForeignRunGets404: OWNER ONLY (design.md §3.4,
// packet H-5 = A) — unlike every other /runs/{id} route, neither a SUPER
// admin nor a security_admin bypasses ownership here. Both get the same
// byte-identical 404 a non-owner member gets, and the store is untouched.
func TestSetRunTitle_AdminOnForeignRunGets404(t *testing.T) {
	for _, role := range []string{oidc.RoleAdmin, oidc.RoleSecurityAdmin} {
		t.Run(role, func(t *testing.T) {
			srv, st, audit := newRunTitleFixture(t, types.RunRunning)
			id := st.dispatchTestStore.run.ID.String()
			admin := ssoSession(t, "sub-some-admin", "admin@corp.example", role)

			code, _ := runTitlePatch(t, srv, admin, id, `{"title":"admin rename"}`)
			if code != http.StatusNotFound {
				t.Fatalf("%s renaming a foreign run = %d, want 404 (owner only, no admin bypass)", role, code)
			}
			if got := st.storedTitle(); got != "original title" {
				t.Errorf("stored title changed to %q after an %s rename; a refused rename must write nothing", got, role)
			}
			if rows := titleAuditRows(audit); len(rows) != 0 {
				t.Errorf("run.title.set rows = %d on a refused %s rename, want 0", len(rows), role)
			}
		})
	}
}

// TestSetRunTitle_TooLong: 201 characters is refused with the same
// too-long shape create-run's title gets, and the store is untouched.
func TestSetRunTitle_TooLong(t *testing.T) {
	srv, st, _ := newRunTitleFixture(t, types.RunRunning)
	id := st.dispatchTestStore.run.ID.String()
	body := `{"title":"` + strings.Repeat("a", 201) + `"}`

	w := doSSO(t, srv, http.MethodPatch, "/api/v1/runs/"+id+"/title", runTitleOwnerSession(t), body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("201-char title = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "too long") {
		t.Errorf("body = %q, want it to say too long", w.Body.String())
	}
	if got := st.storedTitle(); got != "original title" {
		t.Errorf("stored title changed to %q; a refused rename must write nothing", got)
	}
}

// TestSetRunTitle_ControlCharacterRefused: a control character (a bare NUL,
// same trust boundary as create-run's title) is refused, not stored.
func TestSetRunTitle_ControlCharacterRefused(t *testing.T) {
	srv, st, _ := newRunTitleFixture(t, types.RunRunning)
	id := st.dispatchTestStore.run.ID.String()
	body := `{"title":"bad\u0000title"}`

	w := doSSO(t, srv, http.MethodPatch, "/api/v1/runs/"+id+"/title", runTitleOwnerSession(t), body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("a control character in title = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "control character") {
		t.Errorf("body = %q, want it to name a control character", w.Body.String())
	}
	if got := st.storedTitle(); got != "original title" {
		t.Errorf("stored title changed to %q; a refused rename must write nothing", got)
	}
}

// TestSetRunTitle_UnknownKeyRefused: strict JSON keys — an unknown field in
// the body is refused rather than silently ignored.
func TestSetRunTitle_UnknownKeyRefused(t *testing.T) {
	srv, st, _ := newRunTitleFixture(t, types.RunRunning)
	id := st.dispatchTestStore.run.ID.String()

	w := doSSO(t, srv, http.MethodPatch, "/api/v1/runs/"+id+"/title", runTitleOwnerSession(t),
		`{"title":"ok","task":"sneaking a task change in"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("an unknown key = %d, want 400", w.Code)
	}
	if got := st.storedTitle(); got != "original title" {
		t.Errorf("stored title changed to %q; a refused rename must write nothing", got)
	}
}
