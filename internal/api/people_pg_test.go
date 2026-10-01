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

// seedLegacyToken writes, at the store, the row the removed mint route used to
// create (#1477): owned by principal, minted_by someone else, role from the
// person's email, groups unknown. It is the fixture for every token that
// outlives the route, so the sign-in arm keeps its coverage.
func (e peoplePG) seedLegacyToken(t *testing.T, principal, role, mintedBy string) types.APIToken {
	t.Helper()
	unknown := true
	raw := apiTokenPrefix + uuid.NewString()
	row, err := e.st.CreateAPIToken(context.Background(), types.APIToken{
		ID: uuid.New(), Principal: principal, Role: role, UserType: types.UserTypeStandard,
		GroupsTruncated: &unknown, Name: "ci", CreatedAt: time.Now().UTC(), MintedBy: mintedBy,
	}, raw)
	if err != nil {
		t.Fatal(err)
	}
	row.Token = raw
	return row
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

// TestPeople_LegacyMintedTokenActsAsTheNeverSignedInPerson is #1157's done-when
// end to end for a token the removed mint route created (#1477), seeded at the
// store: a security admin pre-creates a person; the legacy token writes the
// person's own secret, launches a run owned and audited as them with their
// drive, and that run mints from their own row; their first sign-in attaches to
// the same principal and sees the run and the token; and DELETE of the token
// ends it at once.
func TestPeople_LegacyMintedTokenActsAsTheNeverSignedInPerson(t *testing.T) {
	e := newPeoplePG(t)
	ctx := context.Background()

	if w := e.createPerson(t, e.sec, personSub, personEmail); w.Code != http.StatusCreated {
		t.Fatalf("create person: %d %s, want 201", w.Code, w.Body.String())
	}
	if w := e.createPerson(t, e.sec, personSub, "PAT@corp.example"); w.Code != http.StatusOK {
		t.Fatalf("confirm person: %d %s, want 200 (same subject, same email)", w.Code, w.Body.String())
	}
	tok := e.seedLegacyToken(t, personSub, oidc.RoleUser, "sec")

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
	tok := e.seedLegacyToken(t, personSub, oidc.RoleUser, "sec")
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
	// "local:alice" and "Admin-Token" are reserved by the shared
	// isReservedPrincipal (#1162): the local seat's prefix and a case fold.
	for _, reserved := range []string{adminTokenPrincipal, "device:laptop", "local:alice", "Admin-Token"} {
		if w := e.createPerson(t, e.sec, reserved, ""); w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("create reserved %q: %d, want 422", reserved, w.Code)
		}
	}
}

// TestPeople_MintGuards is the guard-rail list after #1477: a member cannot
// reach the routes, no admin tier can mint for anyone (a super admin included,
// and a subject with no person record answers the same), a token or the shared
// admin token keep their own refusals, and a legacy token's re-stamp at a
// role-changing sign-in still revokes it instead of raising it.
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
	if w := e.mintFor(t, e.mem, personSub); w.Code != http.StatusForbidden || errorReason(w) == "person_token_mint_removed" {
		t.Errorf("member mints for a person: %d %s, want the tier's 403", w.Code, w.Body.String())
	}
	if w := doSSO(t, e.h.srv, http.MethodGet, "/api/v1/people/"+personSub+"/tokens", e.mem, ""); w.Code != http.StatusForbidden {
		t.Errorf("member lists a person's tokens: %d, want 403", w.Code)
	}
	for _, c := range []struct {
		as     *http.Cookie
		target string
	}{
		{e.sec, personSub}, {e.sec, "boss-sub"}, {e.sec, "sec2-sub"}, {e.super, personSub}, {e.super, "boss-sub"},
		{e.super, "nobody-" + uuid.NewString()},
	} {
		if w := e.mintFor(t, c.as, c.target); w.Code != http.StatusForbidden || errorReason(w) != "person_token_mint_removed" {
			t.Errorf("mint for %s: %d %s, want 403 person_token_mint_removed", c.target, w.Code, w.Body.String())
		}
	}
	if d := e.auditRows("person.token.create"); len(d) != 6 {
		t.Errorf("refused mints audited as %d rows, want 6 denied", len(d))
	} else {
		for _, r := range d {
			if r.Outcome != "denied" {
				t.Errorf("refused mint audited as %+v, want denied", r)
			}
		}
	}
	if n := e.apiTokenCount(t); n != 0 {
		t.Errorf("%d api_tokens rows after the refusals, want none", n)
	}

	superTok, _ := mintToken(t, e.h.srv, e.super, "root-ci")
	if w := do(t, e.h.srv, http.MethodPost, "/api/v1/people/"+personSub+"/tokens", superTok, `{"name":"ci"}`); w.Code != http.StatusForbidden || errorReason(w) != "api_token_from_api_token" {
		t.Errorf("a token mints for a person: %d %s, want 403 api_token_from_api_token", w.Code, w.Body.String())
	}
	if w := do(t, e.h.srv, http.MethodPost, "/api/v1/people/"+personSub+"/tokens", adminToken, `{"name":"ci"}`); w.Code != http.StatusForbidden || errorReason(w) != "mint_no_human" {
		t.Errorf("the admin token mints for a person: %d %s, want 403 mint_no_human", w.Code, w.Body.String())
	}
	if w := doSSO(t, e.h.srv, http.MethodPost, "/api/v1/me/tokens", e.mem, `{"name":"own"}`); w.Code != http.StatusCreated {
		t.Errorf("a person mints their own: %d %s, want 201", w.Code, w.Body.String())
	}

	// The sign-in arm, over a store-seeded legacy token: a role-changing
	// sign-in revokes it; a sign-in under the same role keeps it working and
	// re-stamps it with the person's groups.
	tok := e.seedLegacyToken(t, personSub, oidc.RoleUser, "sec")
	e.login(t, personSub, personEmail, oidc.RoleUser)
	if w := do(t, e.h.srv, http.MethodGet, "/api/v1/me/tokens", tok.Token, ""); w.Code != http.StatusOK {
		t.Errorf("legacy token after a same-role sign-in: %d, want 200 (kept)", w.Code)
	}
	e.login(t, personSub, personEmail, oidc.RoleAdmin)
	if w := do(t, e.h.srv, http.MethodGet, "/api/v1/me/tokens", tok.Token, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("legacy token after an upward re-stamp: %d, want 401 (revoked, never raised)", w.Code)
	}
}

func (e peoplePG) apiTokenCount(t *testing.T) (n int) {
	t.Helper()
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM api_tokens WHERE minted_by IS NOT NULL`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
