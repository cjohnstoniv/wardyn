// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// actionRows is every row of action, oldest first, with its data decoded.
func actionRows(t *testing.T, events []types.AuditEvent, action string) (rows []types.AuditEvent, data []map[string]any) {
	t.Helper()
	for _, ev := range events {
		if ev.Action != action {
			continue
		}
		var d map[string]any
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatalf("%s data %s: %v", action, ev.Data, err)
		}
		rows, data = append(rows, ev), append(data, d)
	}
	return rows, data
}

// A row the sweep kept is audited as a failure every sweep, naming the row
// and never its value, and a sweep that failed as a whole writes one row too.
func TestSweepExpiredCredentials_AuditsFailures(t *testing.T) {
	h := newHarness(t)
	at := time.Now().Add(-time.Hour).UTC()
	h.srv.cfg.Secrets = &sweepSecrets{memSecrets: &memSecrets{m: map[string][]byte{}},
		gone: []secretstore.Expired{{Owner: "bob", Name: "wardyn-harness-aws-oauth", ExpiresAt: at}},
		err: errors.Join(
			&secretstore.ExpiredKept{Owner: "carol", Name: "wardyn-harness-aws-oauth", Err: errors.New("vault: 403 permission denied")},
			errors.New("pg secretstore: expired select: connection reset"),
		)}
	if n := h.srv.SweepExpiredCredentials(context.Background()); n != 1 {
		t.Fatalf("swept %d, want 1", n)
	}
	rows, data := actionRows(t, h.audit.events, "credential.expired.delete")
	if len(rows) != 3 {
		t.Fatalf("credential.expired.delete rows = %d, want 3 (one deleted, one kept, one failed scan)", len(rows))
	}
	var kept, scan bool
	for i, ev := range rows {
		switch {
		case ev.Outcome == "success":
		case ev.Target == "wardyn-harness-aws-oauth" && data[i]["secret_owner"] == "carol" &&
			data[i]["reason"] == "expired" && data[i]["error"] == "vault: 403 permission denied":
			kept = ev.Outcome == "failure" && ev.ActorType == types.ActorSystem
		case ev.Target == "" && data[i]["secret_owner"] == nil && data[i]["reason"] == "expired":
			scan = ev.Outcome == "failure"
		}
	}
	if !kept || !scan {
		t.Fatalf("kept row audited = %v, failed scan audited = %v; rows %+v", kept, scan, data)
	}
}

// eraseRefusalFixture is a directory in which "bob@corp.example" is bob and
// "Bob" folds onto two people.
func eraseRefusalFixture(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.srv.cfg.OIDC = &oidc.Authenticator{}
	h.srv.cfg.Secrets = &memSecrets{m: map[string][]byte{}}
	h.srv.cfg.Store = secretOwnerDirectory{toks: []types.APIToken{
		{ID: uuid.New(), Principal: "bob", Email: "bob@corp.example"},
		{ID: uuid.New(), Principal: "BOB"},
	}}
	h.srv.router = h.srv.routes()
	return h
}

// An erase refused before it reached a namespace keeps its status and is on
// the record: a blank principal (400), one naming nobody or several (422).
func TestErasePersonCredentials_AuditsInvalidPrincipal(t *testing.T) {
	for _, c := range []struct {
		principal, target, reason, wantBody string
		status                              int
	}{
		{"%20", "", "blank_principal", "", http.StatusBadRequest},
		{"nobody@corp.example", "nobody@corp.example", "owner_unresolved", eraseUnresolvedMsg, http.StatusUnprocessableEntity},
		{"Bob", "Bob", "owner_ambiguous", eraseAmbiguousMsg, http.StatusUnprocessableEntity},
	} {
		t.Run(c.reason, func(t *testing.T) {
			h := eraseRefusalFixture(t)
			admin := ssoSession(t, "admin-1", "admin@corp.example", oidc.RoleAdmin)
			w := doSSO(t, h.srv, http.MethodDelete, "/api/v1/people/"+c.principal+"/credentials", admin, "")
			if w.Code != c.status {
				t.Fatalf("erase %q = %d %s, want %d", c.principal, w.Code, w.Body.String(), c.status)
			}
			// design F-5 finding: the erase route takes no ?owner=, so its 422
			// must be its OWN sentence (eraseUnresolvedMsg/eraseAmbiguousMsg),
			// never resolveSecretOwner's shared "?owner= names…" wording.
			if c.wantBody != "" {
				if got := w.Body.String(); !strings.Contains(got, c.wantBody) {
					t.Fatalf("erase %q body = %s, want it to contain %q", c.principal, got, c.wantBody)
				}
				if strings.Contains(w.Body.String(), "?owner=") {
					t.Fatalf("erase %q body leaked the ?owner= wording: %s", c.principal, w.Body.String())
				}
			}
			rows, data := actionRows(t, h.audit.events, "credential.erase")
			if len(rows) != 1 || rows[0].Outcome != "denied" || rows[0].Target != c.target ||
				len(data[0]) != 1 || data[0]["reason"] != c.reason {
				t.Fatalf("credential.erase rows = %+v %v, want one denied row, target %q, reason %s only", rows, data, c.target, c.reason)
			}
		})
	}
}

// A DELETE ?owner= refused as naming nobody or several people keeps its 422
// and is audited; the name list (a read) refuses the same way, unaudited.
func TestDeleteSecret_AuditsUnresolvedOwner(t *testing.T) {
	for _, c := range []struct{ owner, reason string }{
		{"nobody@corp.example", "owner_unresolved"},
		{"Bob", "owner_ambiguous"},
	} {
		t.Run(c.reason, func(t *testing.T) {
			h := eraseRefusalFixture(t)
			admin := ssoSession(t, "admin-1", "admin@corp.example", oidc.RoleAdmin)
			w := doSSO(t, h.srv, http.MethodDelete, "/api/v1/secrets/npm-token?owner="+c.owner, admin, "")
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("DELETE ?owner=%s = %d %s, want 422", c.owner, w.Code, w.Body.String())
			}
			rows, data := actionRows(t, h.audit.events, "secret.delete")
			if len(rows) != 1 || rows[0].Outcome != "denied" || rows[0].Target != "npm-token" ||
				len(data[0]) != 1 || data[0]["reason"] != c.reason {
				t.Fatalf("secret.delete rows = %+v %v, want one denied row for npm-token, reason %s only", rows, data, c.reason)
			}
			if w := doSSO(t, h.srv, http.MethodGet, "/api/v1/secrets?owner="+c.owner, admin, ""); w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("GET ?owner=%s = %d, want 422", c.owner, w.Code)
			}
			if rows, _ := actionRows(t, h.audit.events, "secret.delete"); len(rows) != 1 {
				t.Fatalf("the name list wrote a secret.delete row: %+v", rows)
			}
		})
	}
}
