// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/test/entrafake"
)

func signOutRows(e *scimEnv, n *scimNode, sub string) []types.AuditEvent {
	var out []types.AuditEvent
	for _, ev := range n.h.audit.snapshot() {
		if ev.Action == "session.revoke" && ev.Target == sub {
			out = append(out, ev)
		}
	}
	return out
}

// Signing out ends the person's browser sessions on every instance, keeps their API token, and writes the
// session-only session.revoke row with the person as the actor. A stale copy of another person's cookie, sent
// beside a valid API token, writes no cut and no row, so it cannot end that person's later sessions.
func TestSignOut_CutsTheSessionAndAuditsItOnlyForALiveCookie(t *testing.T) {
	e := newSCIMEnv(t)
	const alice, bob = "sub-alice", "sub-bob"
	cookie, _, token := e.mintTokenAs(e.a, entrafake.Identity{Username: "alice@corp.example", Subject: alice})
	_, _, bobToken := e.mintTokenAs(e.a, entrafake.Identity{Username: "bob@corp.example", Subject: bob})
	time.Sleep(10 * time.Millisecond)

	// Alice's first cookie is ended by an admin; she signs in again, and Bob presents the dead copy with his token.
	admin := accessSession(t, "sec", "sec@corp.example", "security_admin", []string{})
	if w := doSSO(t, e.a.srv, http.MethodPost, "/api/v1/sessions/revoke", admin, `{"sub":"`+alice+`","sessions_only":true}`); w.Code != http.StatusNoContent {
		t.Fatalf("sessions_only revoke = %d %s", w.Code, w.Body.String())
	}
	fresh, _, _ := e.mintTokenAs(e.a, entrafake.Identity{Username: "alice@corp.example", Subject: alice})
	r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	r.AddCookie(cookie)
	r.Header.Set("Authorization", "Bearer "+bobToken)
	w := httptest.NewRecorder()
	panicFails(t, e.a.srv.Handler()).ServeHTTP(w, r)
	if rows := signOutRows(e, e.a, alice); len(rows) != 1 { // the admin's row only
		t.Errorf("a stale cookie with a bearer token wrote a session.revoke row for the victim: %d rows", len(rows))
	}
	for name, n := range map[string]*scimNode{"a": e.a, "b": e.b} {
		if !e.cookieWorks(n, fresh) {
			t.Errorf("instance %s: the victim's later session was ended by someone else's sign-out", name)
		}
	}

	// Alice signs out with the live cookie.
	w = doSSO(t, e.a.srv, http.MethodPost, "/api/v1/auth/logout", fresh, "")
	if w.Code != http.StatusFound {
		t.Fatalf("sign-out = %d %s", w.Code, w.Body.String())
	}
	for name, n := range map[string]*scimNode{"a": e.a, "b": e.b} {
		if e.cookieWorks(n, fresh) {
			t.Errorf("instance %s: the signed-out cookie still works", name)
		}
		if !e.tokenWorks(n, token) {
			t.Errorf("instance %s: sign-out stopped the API token", name)
		}
	}
	rows := signOutRows(e, e.a, alice)
	if len(rows) != 2 {
		t.Fatalf("session.revoke rows for alice = %d, want the admin's and her own", len(rows))
	}
	own := rows[1]
	var data map[string]any
	if err := json.Unmarshal(own.Data, &data); err != nil {
		t.Fatal(err)
	}
	if own.Actor != alice || own.Outcome != "success" || data["scope"] != "sub" || data["sessions_only"] != true ||
		data["tokens_revoked"] != float64(0) || data["ssh_keys_deleted"] != float64(0) {
		t.Errorf("sign-out audit row = actor %q outcome %q data %v", own.Actor, own.Outcome, data)
	}
}
