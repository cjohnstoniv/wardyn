// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// peoplePG is ownerOnlyPG (real Postgres run store, secret store and broker)
// behind a REAL authenticator whose role map keys on email, so a pre-created
// person's role is derived exactly as their sign-in would derive it. Skips
// without WARDYN_TEST_PG.
type peoplePG struct {
	ownerOnlyPG
	st              store.PG
	super, sec, mem *http.Cookie
}

const (
	personSub   = "pat-sub"
	personEmail = "pat@corp.example"
)

func newPeoplePG(t *testing.T) peoplePG {
	t.Helper()
	e := newOwnerOnlyPG(t)
	e.h.srv.cfg.OIDC = newAccessAuth(t, map[string]string{
		"root@corp.example": oidc.RoleAdmin, "boss@corp.example": oidc.RoleAdmin,
		"sec@corp.example": oidc.RoleSecurityAdmin, "sec2@corp.example": oidc.RoleSecurityAdmin,
	}, oidc.RoleUser, nil, nil)
	e.h.srv.cfg.RunnerTarget = "docker"
	e.h.srv.router = e.h.srv.routes()
	e.admin = accessSession(t, "root", "root@corp.example", oidc.RoleAdmin, []string{})
	return peoplePG{
		ownerOnlyPG: e,
		st:          store.NewPG(e.pool),
		super:       e.admin,
		sec:         accessSession(t, "sec", "sec@corp.example", oidc.RoleSecurityAdmin, []string{}),
		mem:         accessSession(t, "mem", "mem@corp.example", oidc.RoleUser, []string{}),
	}
}

func (e peoplePG) createPerson(t *testing.T, as *http.Cookie, principal, email string) *httptest.ResponseRecorder {
	t.Helper()
	return doSSO(t, e.h.srv, http.MethodPost, "/api/v1/people", as, `{"principal":"`+principal+`","email":"`+email+`"}`)
}

func (e peoplePG) mintFor(t *testing.T, as *http.Cookie, principal string) *httptest.ResponseRecorder {
	t.Helper()
	return doSSO(t, e.h.srv, http.MethodPost, "/api/v1/people/"+principal+"/tokens", as, `{"name":"ci"}`)
}

func decodeToken(t *testing.T, w *httptest.ResponseRecorder) types.APIToken {
	t.Helper()
	if w.Code != http.StatusCreated {
		t.Fatalf("mint for person: %d, want 201: %s", w.Code, w.Body.String())
	}
	var tok types.APIToken
	if err := json.Unmarshal(w.Body.Bytes(), &tok); err != nil {
		t.Fatal(err)
	}
	return tok
}

// login is what a sign-in's OnLogin hook does for sub (refreshLoginStamps,
// pinned in cmd/wardynd), followed by the session cookie it issues.
func (e peoplePG) login(t *testing.T, sub, email, role string) *http.Cookie {
	t.Helper()
	ctx := context.Background()
	if err := e.st.RefreshAPITokenIdentity(ctx, sub, role, types.UserTypeStandard, []string{"eng"}, false); err != nil {
		t.Fatal(err)
	}
	if err := e.st.MarkPersonSignedIn(ctx, sub, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	return accessSession(t, sub, email, role, []string{"eng"})
}

func (e peoplePG) auditRows(action string) []types.AuditEvent {
	var out []types.AuditEvent
	for _, ev := range e.h.audit.snapshot() {
		if ev.Action == action {
			out = append(out, ev)
		}
	}
	return out
}

// TestPeople_MintedTokenActsAsTheNeverSignedInPerson is #1157's done-when end
// to end: a security admin pre-creates a person and mints them a token; the
// token writes the person's own secret, launches a run owned and audited as
// them with their drive, and that run mints from their own row; their first
// sign-in attaches to the same principal and sees the run and the token; and
// DELETE of the token ends it at once.
func TestPeople_MintedTokenActsAsTheNeverSignedInPerson(t *testing.T) {
	e := newPeoplePG(t)
	ctx := context.Background()

	if w := e.createPerson(t, e.sec, personSub, personEmail); w.Code != http.StatusCreated {
		t.Fatalf("create person: %d %s, want 201", w.Code, w.Body.String())
	}
	if w := e.createPerson(t, e.sec, personSub, "PAT@corp.example"); w.Code != http.StatusOK {
		t.Fatalf("confirm person: %d %s, want 200 (same subject, same email)", w.Code, w.Body.String())
	}
	tok := decodeToken(t, e.mintFor(t, e.sec, personSub))
	if tok.Principal != personSub || tok.Role != oidc.RoleUser || tok.UserType != types.UserTypeStandard ||
		tok.MintedBy != "sec" || tok.GroupsTruncated == nil || !*tok.GroupsTruncated || !strings.HasPrefix(tok.Token, apiTokenPrefix) {
		t.Fatalf("minted token = %+v, want the person's derived role/type, minted_by the caller, groups unknown", tok)
	}
	rows := e.auditRows("person.token.create")
	if len(rows) != 1 || rows[0].Actor != "sec" || !strings.Contains(string(rows[0].Data), `"principal":"`+personSub+`"`) {
		t.Fatalf("person.token.create rows = %+v, want one naming both parties", rows)
	}

	// Their own secret, their own drive.
	if w := do(t, e.h.srv, http.MethodPut, "/api/v1/secrets/x", tok.Token, `{"value":"pat-own-secret-value"}`); w.Code/100 != 2 {
		t.Fatalf("token writes its own secret: %d %s", w.Code, w.Body.String())
	}
	drive, err := e.st.UpsertUserDrive(ctx, *driveFixture(nil), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.UpsertUserDriveGrant(ctx, *grantFixture(drive.ID, func(g *types.UserDriveGrant) { g.Subject = personSub }), false); err != nil {
		t.Fatal(err)
	}
	strict := e.storePolicy(t, "strict", "x", true)
	created := mustCreate(t, do(t, e.h.srv, http.MethodPost, "/api/v1/runs", tok.Token,
		`{"agent":"claude-code","task":"t","policy_id":"`+strict+`","drive":{"enabled":true}}`))
	var run createRunResponse
	if err := json.Unmarshal(created.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	if run.CreatedBy != personSub {
		t.Fatalf("run created_by = %q, want the person", run.CreatedBy)
	}
	if rc := e.auditRows("run.create"); len(rc) != 1 || rc[0].Actor != personSub {
		t.Fatalf("run.create rows = %+v, want one with the person as actor", rc)
	}
	if got := mintedToken(t, e.mint(t, personSub, created)); got != "pat-own-secret-value" {
		t.Fatalf("run minted %q, want the person's own row", got)
	}

	// First sign-in: same principal, sees the run and the token.
	pat := e.login(t, personSub, personEmail, oidc.RoleUser)
	if w := doSSO(t, e.h.srv, http.MethodGet, "/api/v1/runs/"+run.ID.String(), pat, ""); w.Code != http.StatusOK {
		t.Fatalf("signed-in person reads their run: %d %s", w.Code, w.Body.String())
	}
	if w := doSSO(t, e.h.srv, http.MethodGet, "/api/v1/me/tokens", pat, ""); !strings.Contains(w.Body.String(), tok.ID.String()) {
		t.Fatalf("signed-in person's /me/tokens = %s, want the minted token", w.Body.String())
	}
	if p, err := e.st.GetPerson(ctx, personSub); err != nil || p.FirstSignedInAt == nil {
		t.Fatalf("person after sign-in = %+v (%v), want first_signed_in_at stamped", p, err)
	}
	if w := doSSO(t, e.h.srv, http.MethodGet, "/api/v1/people/"+personSub+"/tokens", e.sec, ""); !strings.Contains(w.Body.String(), `"minted_by":"sec"`) {
		t.Fatalf("admin list of the person's tokens = %d %s, want the token with minted_by", w.Code, w.Body.String())
	}

	// Revocation is immediate.
	if w := do(t, e.h.srv, http.MethodGet, "/api/v1/me/tokens", tok.Token, ""); w.Code != http.StatusOK {
		t.Fatalf("token still live after sign-in: %d %s", w.Code, w.Body.String())
	}
	if w := doSSO(t, e.h.srv, http.MethodDelete, "/api/v1/tokens/"+tok.ID.String(), e.sec, ""); w.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", w.Code, w.Body.String())
	}
	if w := do(t, e.h.srv, http.MethodGet, "/api/v1/me/tokens", tok.Token, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("revoked token: %d, want 401", w.Code)
	}
}

// TestPeople_AnotherLoginNeverAttaches pins the keying rule: a person is their
// IdP subject, so a different subject carrying the same email — a collision,
// or the real person under a subject the admin mistyped — reaches nothing the
// pre-created identity owns, and does not mark it signed in. The same rule
// refuses pre-creating an identity that is ambiguous against the directory.
func TestPeople_AnotherLoginNeverAttaches(t *testing.T) {
	e := newPeoplePG(t)
	ctx := context.Background()
	if w := e.createPerson(t, e.sec, personSub, personEmail); w.Code != http.StatusCreated {
		t.Fatalf("create person: %d %s", w.Code, w.Body.String())
	}
	tok := decodeToken(t, e.mintFor(t, e.sec, personSub))
	run := mustCreate(t, do(t, e.h.srv, http.MethodPost, "/api/v1/runs", tok.Token, `{"agent":"claude-code","task":"t"}`))
	var r createRunResponse
	if err := json.Unmarshal(run.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}

	other := e.login(t, "pat-sub-2", personEmail, oidc.RoleUser)
	if w := doSSO(t, e.h.srv, http.MethodGet, "/api/v1/me/tokens", other, ""); strings.Contains(w.Body.String(), tok.ID.String()) {
		t.Fatalf("same email, different subject sees the token: %s", w.Body.String())
	}
	if w := doSSO(t, e.h.srv, http.MethodGet, "/api/v1/runs/"+r.ID.String(), other, ""); w.Code != http.StatusNotFound {
		t.Fatalf("same email, different subject reads the run: %d, want 404", w.Code)
	}
	if p, err := e.st.GetPerson(ctx, personSub); err != nil || p.FirstSignedInAt != nil {
		t.Fatalf("person after someone else's sign-in = %+v (%v), want still unsigned", p, err)
	}

	for _, tc := range []struct{ name, principal, email string }{
		{"the email already names another subject", "pat-sub-3", "PAT@corp.example"},
		{"the subject differs from a known one only by case", "PAT-SUB", ""},
		{"the subject is known under a different email", personSub, "someone@corp.example"},
		{"the subject is another person's email", personEmail, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if w := e.createPerson(t, e.sec, tc.principal, tc.email); w.Code != http.StatusConflict {
				t.Fatalf("create %q/%q: %d %s, want 409", tc.principal, tc.email, w.Code, w.Body.String())
			}
		})
	}
	for _, reserved := range []string{adminTokenPrincipal, "device:laptop"} {
		if w := e.createPerson(t, e.sec, reserved, ""); w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("create reserved %q: %d, want 422", reserved, w.Code)
		}
	}
}

// TestPeople_MintGuards is the guard-rail list: a member cannot reach the
// routes, a security admin cannot mint for an admin or another security admin
// (a super admin can), no minting by a token or the shared admin token, none
// for a subject with no person record, and a re-stamp at sign-in that would
// RAISE an admin-minted token revokes it instead.
func TestPeople_MintGuards(t *testing.T) {
	e := newPeoplePG(t)
	for _, p := range [][2]string{{personSub, personEmail}, {"boss-sub", "boss@corp.example"}, {"sec2-sub", "sec2@corp.example"}} {
		if w := e.createPerson(t, e.super, p[0], p[1]); w.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", p[0], w.Code, w.Body.String())
		}
	}

	if w := e.createPerson(t, e.mem, "x-sub", ""); w.Code != http.StatusForbidden {
		t.Errorf("member creates a person: %d, want 403", w.Code)
	}
	if w := e.mintFor(t, e.mem, personSub); w.Code != http.StatusForbidden {
		t.Errorf("member mints for a person: %d, want 403", w.Code)
	}
	if w := doSSO(t, e.h.srv, http.MethodGet, "/api/v1/people/"+personSub+"/tokens", e.mem, ""); w.Code != http.StatusForbidden {
		t.Errorf("member lists a person's tokens: %d, want 403", w.Code)
	}
	for _, target := range []string{"boss-sub", "sec2-sub"} {
		if w := e.mintFor(t, e.sec, target); w.Code != http.StatusForbidden {
			t.Errorf("security admin mints for %s: %d %s, want 403", target, w.Code, w.Body.String())
		}
	}
	if d := e.auditRows("person.token.create"); len(d) != 2 || d[0].Outcome != "denied" || d[1].Outcome != "denied" {
		t.Errorf("denied mints audited as %+v, want two denied rows", d)
	}
	if tok := decodeToken(t, e.mintFor(t, e.super, "boss-sub")); tok.Role != oidc.RoleAdmin || tok.MintedBy != "root" {
		t.Errorf("super admin mints for an admin: %+v, want an admin token minted by root", tok)
	}

	superTok, _ := mintToken(t, e.h.srv, e.super, "root-ci")
	if w := do(t, e.h.srv, http.MethodPost, "/api/v1/people/"+personSub+"/tokens", superTok, `{"name":"ci"}`); w.Code != http.StatusForbidden {
		t.Errorf("a token mints for a person: %d, want 403", w.Code)
	}
	if w := do(t, e.h.srv, http.MethodPost, "/api/v1/people/"+personSub+"/tokens", adminToken, `{"name":"ci"}`); w.Code != http.StatusForbidden {
		t.Errorf("the admin token mints for a person: %d, want 403", w.Code)
	}
	if w := e.mintFor(t, e.super, "nobody-"+uuid.NewString()); w.Code != http.StatusNotFound {
		t.Errorf("mint for an unrecorded subject: %d, want 404", w.Code)
	}

	// A security admin mints for pat as a user; pat's real sign-in says admin.
	tok := decodeToken(t, e.mintFor(t, e.sec, personSub))
	e.login(t, personSub, personEmail, oidc.RoleAdmin)
	if w := do(t, e.h.srv, http.MethodGet, "/api/v1/me/tokens", tok.Token, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("admin-minted token after an upward re-stamp: %d, want 401 (revoked, never raised)", w.Code)
	}
}
