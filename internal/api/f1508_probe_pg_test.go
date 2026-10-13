// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The #1508 probe table (design section 10, slice M2): every route of the main page's section 7.1 against every
// caller the issue names, answering exactly what the design's tables say. It is written from those tables, not
// from the handlers: a cell that disagrees with the code is a defect in a C slice, and the cell stays.
//
// A cell has an exact answer only where a design row gives one. Where the design is silent about a caller (the
// admin token on a listing, a token on a sponsor's list), the cell is an invariant: never a 5xx, and a 2xx may
// show nothing the caller is not owed and no bearer-shaped value.

const (
	cTarget      = "target"
	cOther       = "other person"
	cSponsor     = "sponsor without service_attach"
	cSponsorPlus = "sponsor with service_attach"
	cAdmin       = "admin"
	cSecurity    = "security admin"
	cSuper       = "super admin"
	cAdminToken  = "admin token"
	cWdn         = "wdn_ token"
	cWdg         = "wdg_ token"
	cService     = "service token"
	cExpired     = "expired invitation"
	cClaimed     = "claimed invitation"
)

var probeCallers = []string{cTarget, cOther, cSponsor, cSponsorPlus, cAdmin, cSecurity, cSuper, cAdminToken, cWdn, cWdg, cService, cExpired, cClaimed}

// want is one cell's answer. code 0 is the invariant-only cell; anyOK takes any 2xx; same compares with another
// call: "missing" is the same caller's call on an id or name that does not exist (body for body), "tier" is the
// status and reason POST /people gives that caller (the tier securityOps shares). shows is whether a 2xx may
// carry the fixture's secret.
type want struct {
	code   int
	anyOK  bool
	reason string
	same   string
	shows  bool
}

// probeBlock is one route of section 7.1 with a cell for every caller.
type probeBlock struct {
	ref     string // the design table the rows come from
	route   string // a routeMatrix key
	subject string // whose wdn_ and wdg_ the token callers carry: "target" or "sponsor"
	state   string // the invitation's state at the call, "" is pending (the expired and claimed callers set their own)
	partial bool   // fewer than all callers: extra rows for a state, not a route
	rows    map[string]want
}

// probeFix is one fresh request for a cell.
type probeFix struct {
	path, body string
	missing    string // the same call on something that does not exist
	secret     string // a value only some callers may see in a 2xx body
	after      func(t *testing.T, w *httptest.ResponseRecorder)
}

var (
	refused  = func(code int, reason string) want { return want{code: code, reason: reason} }
	notFound = func(reason string) want { return want{code: http.StatusNotFound, reason: reason, same: "missing"} }
	silent   = want{}
)

// securityTier is the answer set of a securityOps route (design 3.2: "Callers that pass it today pass it here,
// the admin token included"; section 4: a service token is refused off its allow-list; 2: a portal's token is on
// an allow-list of user reach). okCode is the route's own success status.
func securityTier(okCode int) map[string]want {
	tier := want{code: http.StatusForbidden, same: "tier"}
	ok := want{code: okCode}
	return map[string]want{
		cTarget: tier, cOther: tier, cSponsor: tier, cSponsorPlus: tier, cExpired: tier, cClaimed: tier, cWdn: tier,
		cAdmin: ok, cSecurity: ok, cSuper: ok, cAdminToken: ok,
		cWdg:     refused(http.StatusForbidden, "delegation_scope"),
		cService: refused(http.StatusForbidden, "service_scope"),
	}
}

// onlyTheSponsor is the answer set of a route "the sponsor; others 404 service_identity_not_found" (7.1), with the
// service token off its allow-list (service page 4).
func onlyTheSponsor(sponsorWant want) map[string]want {
	other := notFound("service_identity_not_found")
	return map[string]want{
		cSponsor: sponsorWant, cSponsorPlus: sponsorWant,
		cTarget: other, cOther: other, cAdmin: other, cSecurity: other, cSuper: other, cExpired: other, cClaimed: other,
		cService:    refused(http.StatusForbidden, "service_scope"),
		cAdminToken: silent, cWdn: {shows: true}, cWdg: {shows: true}, // the sponsor's own bearers act as the sponsor
	}
}

func probeBlocks() []probeBlock {
	const (
		claim     = "POST /api/v1/me/invitations/{id}/claim"
		mint      = "POST /api/v1/me/service-identities/{name}/tokens"
		invite    = "POST /api/v1/people/{principal}/invitations"
		revokeInv = "DELETE /api/v1/invitations/{id}"
	)
	missingClaim := notFound("invitation_not_found")
	notPending := refused(http.StatusConflict, "invitation_not_pending")
	blocks := []probeBlock{
		{ref: "main 3.4 (who gets what)", route: claim, subject: "target", rows: map[string]want{
			cTarget: {code: http.StatusCreated}, cExpired: notPending, cClaimed: notPending,
			cOther: missingClaim, cSponsor: missingClaim, cSponsorPlus: missingClaim,
			cAdmin: missingClaim, cSecurity: missingClaim, cSuper: missingClaim,
			cAdminToken: refused(http.StatusForbidden, "api_token_no_human"),
			cWdn:        refused(http.StatusForbidden, "api_token_from_api_token"),
			cWdg:        refused(http.StatusForbidden, "delegation_scope"),
			cService:    refused(http.StatusForbidden, "service_scope"),
		}},
		{ref: "service 6 (sponsor mint caller table)", route: mint, subject: "sponsor", rows: map[string]want{
			cSponsor: {code: http.StatusCreated}, cSponsorPlus: {code: http.StatusCreated},
			cTarget: notFound("service_identity_not_found"), cOther: notFound("service_identity_not_found"),
			cAdmin: notFound("service_identity_not_found"), cSecurity: notFound("service_identity_not_found"),
			cSuper: notFound("service_identity_not_found"), cExpired: notFound("service_identity_not_found"), cClaimed: notFound("service_identity_not_found"),
			cAdminToken: refused(http.StatusForbidden, "api_token_no_human"),
			cWdn:        refused(http.StatusForbidden, "api_token_from_api_token"),
			cWdg:        refused(http.StatusForbidden, "delegation_scope"),
			cService:    refused(http.StatusForbidden, "service_scope"),
		}},
		{ref: "main 7.1 (the sponsor; others 404) and service 4", route: "GET /api/v1/me/service-identities/{name}/tokens", subject: "sponsor",
			rows: onlyTheSponsor(want{code: http.StatusOK})},
		{ref: "main 7.1 (the sponsor; others 404) and service 4", route: "DELETE /api/v1/me/service-identities/{name}/tokens/{id}", subject: "sponsor",
			rows: onlyTheSponsor(want{code: http.StatusNoContent})},
		{ref: "main 7.1 (signed-in human: the identities they sponsor) and service 4", route: "GET /api/v1/me/service-identities", subject: "sponsor", rows: map[string]want{
			cSponsor: {code: http.StatusOK, shows: true}, cSponsorPlus: {code: http.StatusOK},
			cTarget: {code: http.StatusOK}, cOther: {code: http.StatusOK}, cAdmin: {code: http.StatusOK}, cSecurity: {code: http.StatusOK},
			cSuper: {code: http.StatusOK}, cExpired: {code: http.StatusOK}, cClaimed: {code: http.StatusOK},
			cService:    refused(http.StatusForbidden, "service_scope"),
			cAdminToken: silent, cWdn: {shows: true}, cWdg: {shows: true},
		}},
		{ref: "main 7.1 (signed-in human: own pending) and service 4", route: "GET /api/v1/me/invitations", subject: "target", rows: map[string]want{
			cTarget: {code: http.StatusOK, shows: true}, cOther: {code: http.StatusOK}, cSponsor: {code: http.StatusOK}, cSponsorPlus: {code: http.StatusOK},
			cAdmin: {code: http.StatusOK}, cSecurity: {code: http.StatusOK}, cSuper: {code: http.StatusOK},
			cService:    refused(http.StatusForbidden, "service_scope"),
			cAdminToken: silent, cWdn: {shows: true}, cWdg: {shows: true},
			cExpired: {code: http.StatusOK}, cClaimed: {code: http.StatusOK}, // their invitation is no longer pending, so it is not listed
		}},
		{ref: "main 3.2 and 7.1 (securityOps; 201)", route: invite, subject: "target", rows: securityTier(http.StatusCreated)},
		{ref: "main 3.4 trailer and 7.1 (securityOps; metadata)", route: "GET /api/v1/invitations", subject: "target", rows: securityTier(http.StatusOK)},
		{ref: "main 3.4 trailer and 7.1 (securityOps; metadata)", route: "GET /api/v1/people/{principal}/invitations", subject: "target", rows: securityTier(http.StatusOK)},
		{ref: "main 3.4 trailer and 7.1 (securityOps; 204)", route: revokeInv, subject: "target", rows: securityTier(http.StatusNoContent)},
		{ref: "main 7.1 (securityOps; record with derived state)", route: "POST /api/v1/service-identities", subject: "target", rows: anyOKTier(securityTier(http.StatusCreated))},
		{ref: "main 7.1 (securityOps; record with derived state)", route: "GET /api/v1/service-identities", subject: "target", rows: anyOKTier(securityTier(http.StatusOK))},
		{ref: "main 7.1 (securityOps; record with derived state)", route: "GET /api/v1/service-identities/{name}", subject: "target", rows: anyOKTier(securityTier(http.StatusOK))},
		{ref: "main 7.1 (securityOps; record with derived state)", route: "PATCH /api/v1/service-identities/{name}", subject: "target", rows: anyOKTier(securityTier(http.StatusOK))},
		{ref: "main 7.1 (securityOps; record with derived state)", route: "DELETE /api/v1/service-identities/{name}", subject: "target", rows: anyOKTier(securityTier(http.StatusOK))},
		{ref: "main 6.2 and 7.1 (securityOps; 200 with disposition)", route: "POST /api/v1/tokens/{id}/move", subject: "target", rows: securityTier(http.StatusOK)},
	}
	// 3.2: an admin revokes a pending invitation; "otherwise 404" is every state that is not pending.
	for _, state := range []string{"expired", "claimed", "revoked"} {
		rows := map[string]want{}
		for _, c := range []string{cAdmin, cSecurity, cSuper, cAdminToken} {
			rows[c] = want{code: http.StatusNotFound}
		}
		blocks = append(blocks, probeBlock{ref: "main 3.4 trailer (DELETE revokes a pending one; otherwise 404)", route: revokeInv,
			subject: "target", state: state, partial: true, rows: rows})
	}
	return blocks
}

// anyOKTier is securityTier for a route whose success status the design leaves at "a record".
func anyOKTier(rows map[string]want) map[string]want {
	for c, w := range rows {
		if w.code >= 200 && w.code < 300 {
			rows[c] = want{anyOK: true}
		}
	}
	return rows
}

// callAs sends one request as a caller. subject picks whose bearers the token callers carry.
func (e probeEnv) callAs(t *testing.T, caller, subject, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	patSide := subject == "target"
	switch caller {
	case cTarget, cExpired, cClaimed:
		return e.ask(t, e.pat, "", method, path, body)
	case cOther:
		return e.ask(t, e.other, "", method, path, body)
	case cSponsor:
		return e.ask(t, e.sponsor, "", method, path, body)
	case cSponsorPlus:
		return e.ask(t, e.sponsor2, "", method, path, body)
	case cAdmin:
		return e.ask(t, e.boss, "", method, path, body)
	case cSecurity:
		return e.ask(t, e.sec, "", method, path, body)
	case cSuper:
		return e.ask(t, e.super, "", method, path, body)
	case cAdminToken:
		return e.ask(t, nil, e.h.srv.cfg.AdminToken, method, path, body)
	case cWdn:
		if patSide {
			return e.ask(t, nil, e.patWdn, method, path, body)
		}
		return e.ask(t, nil, e.sponsorWdn, method, path, body)
	case cWdg:
		if patSide {
			return e.ask(t, nil, e.patWdg, method, path, body)
		}
		return e.ask(t, nil, e.sponsorWdg, method, path, body)
	case cService:
		return e.ask(t, nil, e.svcTok, method, path, body)
	}
	t.Fatalf("unknown caller %q", caller)
	return nil
}

// newInvitation has the security admin invite the target and returns the invitation, in the state the cell needs.
func (e probeEnv) newInvitation(t *testing.T, state string) types.Invitation {
	t.Helper()
	inv := inviteEnv{peoplePG: e.peoplePG, pat: e.pat, other: e.other}
	rec := inv.mustInvite(t, `{}`)
	switch state {
	case "expired":
		inv.expire(t, rec.ID)
	case "claimed":
		w := inv.claim(t, e.pat, rec.ID)
		if w.Code != http.StatusCreated {
			t.Fatalf("claim to set up a claimed invitation: %d %s", w.Code, w.Body.String())
		}
		e.revokeOwn(t, w) // the token cap is twenty live tokens
	case "revoked":
		if w := e.ask(t, e.sec, "", http.MethodDelete, "/api/v1/invitations/"+rec.ID.String(), ""); w.Code != http.StatusNoContent {
			t.Fatalf("revoke to set up a revoked invitation: %d %s", w.Code, w.Body.String())
		}
	}
	return rec
}

// revokeOwn has the target revoke the token a claim response carries.
func (e probeEnv) revokeOwn(t *testing.T, claim *httptest.ResponseRecorder) {
	t.Helper()
	var tok types.APIToken
	if json.Unmarshal(claim.Body.Bytes(), &tok) != nil {
		return
	}
	e.ask(t, e.pat, "", http.MethodDelete, "/api/v1/me/tokens/"+tok.ID.String(), "")
}

func (e probeEnv) dropInvitation(t *testing.T, id uuid.UUID) {
	t.Helper()
	e.ask(t, e.sec, "", http.MethodDelete, "/api/v1/invitations/"+id.String(), "") // pending ones only: the cap is ten
}

// fixtureArgs is what a fixture knows of its cell: the caller, the invitation state the cell needs ("" is
// pending), and a fresh id for naming what must not exist.
type fixtureArgs struct {
	caller, state, miss string
}

// probeFixtures is the request builder of each route of the table. Every cell gets fresh state, so no cell
// depends on another.
var probeFixtures = map[string]func(e probeEnv, t *testing.T, a fixtureArgs) probeFix{
	"POST /api/v1/me/invitations/{id}/claim":                  probeEnv.claimFixture,
	"POST /api/v1/me/service-identities/{name}/tokens":        probeEnv.mintFixture,
	"GET /api/v1/me/service-identities/{name}/tokens":         probeEnv.tokenListFixture,
	"DELETE /api/v1/me/service-identities/{name}/tokens/{id}": probeEnv.tokenRevokeFixture,
	"GET /api/v1/me/service-identities": func(probeEnv, *testing.T, fixtureArgs) probeFix {
		return probeFix{path: "/api/v1/me/service-identities", secret: svcName}
	},
	"GET /api/v1/me/invitations":                  probeEnv.myInvitationsFixture,
	"POST /api/v1/people/{principal}/invitations": probeEnv.inviteFixture,
	"GET /api/v1/invitations":                     invitationReadFixture("/api/v1/invitations"),
	"GET /api/v1/people/{principal}/invitations":  invitationReadFixture("/api/v1/people/" + personSub + "/invitations"),
	"DELETE /api/v1/invitations/{id}":             probeEnv.revokeInvitationFixture,
	"POST /api/v1/service-identities":             probeEnv.createIdentityFixture,
	"GET /api/v1/service-identities":              func(probeEnv, *testing.T, fixtureArgs) probeFix { return probeFix{path: "/api/v1/service-identities"} },
	"GET /api/v1/service-identities/{name}": func(probeEnv, *testing.T, fixtureArgs) probeFix {
		return probeFix{path: "/api/v1/service-identities/" + svcName}
	},
	"PATCH /api/v1/service-identities/{name}": func(probeEnv, *testing.T, fixtureArgs) probeFix {
		return probeFix{path: "/api/v1/service-identities/" + svcName, body: `{"description":"probed"}`}
	},
	"DELETE /api/v1/service-identities/{name}": probeEnv.retireIdentityFixture,
	"POST /api/v1/tokens/{id}/move":            probeEnv.moveFixture,
}

func (e probeEnv) fixture(t *testing.T, route, caller, state string) probeFix {
	t.Helper()
	st := state
	switch caller {
	case cExpired:
		st = "expired"
	case cClaimed:
		st = "claimed"
	}
	fx, ok := probeFixtures[route]
	if !ok {
		t.Fatalf("no fixture for %s", route)
	}
	return fx(e, t, fixtureArgs{caller: caller, state: st, miss: uuid.NewString()})
}

// pendingFor is a fresh invitation for the target in the state the cell needs, dropped when the cell ends.
func (e probeEnv) pendingFor(t *testing.T, a fixtureArgs) types.Invitation {
	t.Helper()
	inv := e.newInvitation(t, a.state)
	t.Cleanup(func() { e.dropInvitation(t, inv.ID) })
	return inv
}

func (e probeEnv) claimFixture(t *testing.T, a fixtureArgs) probeFix {
	inv := e.pendingFor(t, a)
	return probeFix{path: "/api/v1/me/invitations/" + inv.ID.String() + "/claim", body: `{}`,
		missing: "/api/v1/me/invitations/" + a.miss + "/claim",
		after: func(t *testing.T, w *httptest.ResponseRecorder) {
			t.Helper()
			// A refusal leaves the invitation as it was, and the target with no new token.
			after := inviteEnv{peoplePG: e.peoplePG}.stateOf(t, inv.ID)
			switch {
			case w.Code == http.StatusCreated:
				if after != types.InvitationClaimed {
					t.Errorf("a 201 claim left the invitation %s", after)
				}
				var tok types.APIToken
				if json.Unmarshal(w.Body.Bytes(), &tok) != nil || tok.Principal != personSub || tok.MintedBy != "" {
					t.Errorf("the claimed token = %s, want one for %s with no minter", w.Body.String(), personSub)
				}
				if c := e.ask(t, nil, tok.Token, http.MethodGet, "/api/v1/me", ""); !strings.Contains(c.Body.String(), personSub) {
					t.Errorf("the claimed token authenticates as %s, want %s", c.Body.String(), personSub)
				}
				e.revokeOwn(t, w)
			case a.state == "" && after != types.InvitationPending:
				t.Errorf("a refused claim (%d) left the invitation %s, want pending", w.Code, after)
			}
		}}
}

func (e probeEnv) mintFixture(t *testing.T, a fixtureArgs) probeFix {
	name := e.identityFor(a.caller)
	return probeFix{path: "/api/v1/me/service-identities/" + name + "/tokens", body: `{"ttl_seconds":3600}`,
		missing: "/api/v1/me/service-identities/no-such-" + a.miss[:8] + "/tokens",
		after: func(t *testing.T, w *httptest.ResponseRecorder) {
			t.Helper()
			if w.Code != http.StatusCreated {
				return
			}
			var tok types.APIToken
			if json.Unmarshal(w.Body.Bytes(), &tok) != nil || tok.Principal != "service:"+name {
				t.Errorf("the minted token = %s, want a token of service:%s", w.Body.String(), name)
			}
		}}
}

func (e probeEnv) tokenListFixture(t *testing.T, a fixtureArgs) probeFix {
	return probeFix{path: "/api/v1/me/service-identities/" + e.identityFor(a.caller) + "/tokens",
		missing: "/api/v1/me/service-identities/no-such-" + a.miss[:8] + "/tokens"}
}

func (e probeEnv) tokenRevokeFixture(t *testing.T, a fixtureArgs) probeFix {
	name := e.identityFor(a.caller)
	tok := e.mustMint(t, map[string]*http.Cookie{svcName: e.sponsor, plusBot: e.sponsor2}[name], name)
	return probeFix{path: "/api/v1/me/service-identities/" + name + "/tokens/" + tok.ID.String(),
		missing: "/api/v1/me/service-identities/no-such-" + a.miss[:8] + "/tokens/" + a.miss,
		after: func(t *testing.T, w *httptest.ResponseRecorder) {
			t.Helper()
			// Only an answered 204 may have revoked it.
			var revoked bool
			if err := e.pool.QueryRow(context.Background(), `SELECT revoked_at IS NOT NULL FROM api_tokens WHERE id = $1`, tok.ID).Scan(&revoked); err != nil {
				t.Fatal(err)
			}
			if revoked != (w.Code == http.StatusNoContent) {
				t.Errorf("answered %d and the token's revoked = %v", w.Code, revoked)
			}
		}}
}

func (e probeEnv) myInvitationsFixture(t *testing.T, a fixtureArgs) probeFix {
	return probeFix{path: "/api/v1/me/invitations", secret: e.pendingFor(t, a).ID.String()}
}

func (e probeEnv) inviteFixture(t *testing.T, a fixtureArgs) probeFix {
	return probeFix{path: "/api/v1/people/" + personSub + "/invitations", body: `{}`, after: func(t *testing.T, w *httptest.ResponseRecorder) {
		t.Helper()
		var rec types.Invitation
		if w.Code == http.StatusCreated && json.Unmarshal(w.Body.Bytes(), &rec) == nil {
			e.dropInvitation(t, rec.ID)
		}
	}}
}

// invitationReadFixture is an admin read of invitations, with one in the cell's state to read.
func invitationReadFixture(path string) func(probeEnv, *testing.T, fixtureArgs) probeFix {
	return func(e probeEnv, t *testing.T, a fixtureArgs) probeFix {
		e.pendingFor(t, a)
		return probeFix{path: path}
	}
}

func (e probeEnv) revokeInvitationFixture(t *testing.T, a fixtureArgs) probeFix {
	inv := e.pendingFor(t, a)
	return probeFix{path: "/api/v1/invitations/" + inv.ID.String(),
		after: func(t *testing.T, w *httptest.ResponseRecorder) {
			t.Helper()
			if after := (inviteEnv{peoplePG: e.peoplePG}).stateOf(t, inv.ID); w.Code != http.StatusNoContent && a.state == "" && after != types.InvitationPending {
				t.Errorf("a refused revoke (%d) left the invitation %s", w.Code, after)
			}
		}}
}

func (e probeEnv) createIdentityFixture(t *testing.T, a fixtureArgs) probeFix {
	return probeFix{path: "/api/v1/service-identities", body: fmt.Sprintf(`{"name":%q,"sponsor":%q}`, "probe-"+a.miss[:8], svcSponsor)}
}

func (e probeEnv) retireIdentityFixture(t *testing.T, a fixtureArgs) probeFix {
	name := "retire-" + a.miss[:8]
	e.mustCreate(t, name, svcSponsor)
	return probeFix{path: "/api/v1/service-identities/" + name}
}

func (e probeEnv) moveFixture(t *testing.T, a fixtureArgs) probeFix {
	tok := e.seedLegacyToken(t, personSub, "user", "sec")
	return probeFix{path: "/api/v1/tokens/" + tok.ID.String() + "/move", body: `{"service_identity":"` + svcName + `"}`}
}

// identityFor is the service identity a caller's request names: their own when they sponsor one, else ci-bot.
func (e probeEnv) identityFor(caller string) string {
	if caller == cSponsorPlus {
		return plusBot
	}
	return svcName
}

func method(route string) string { m, _, _ := strings.Cut(route, " "); return m }

// TestF1508Probe_CallersByRoute runs the table: for each route of section 7.1, every caller.
func TestF1508Probe_CallersByRoute(t *testing.T) {
	e := newProbeEnv(t)
	// plus-bot needs a token of its own for the sponsor-with-grant list and revoke cells.
	e.mustMint(t, e.sponsor2, plusBot)
	blocks := probeBlocks()
	seen := map[string]bool{}
	for _, b := range blocks {
		if _, ok := routeMatrix[b.route]; !ok {
			t.Fatalf("%s is not a route of the matrix", b.route)
		}
		seen[b.route] = true
		if !b.partial && len(b.rows) != len(probeCallers) {
			t.Fatalf("%s has %d callers, want all %d", b.route, len(b.rows), len(probeCallers))
		}
		for _, c := range probeCallers {
			w, ok := b.rows[c]
			if !ok {
				if b.partial {
					continue
				}
				t.Fatalf("%s has no row for %s", b.route, c)
			}
			name := b.route + " / " + c
			if b.state != "" {
				name += " / " + b.state
			}
			t.Run(name, func(t *testing.T) { e.runCell(t, b, c, w) })
		}
	}
	// Section 7.1 lists these routes; the table has to cover each one.
	for _, r := range []string{
		"POST /api/v1/people/{principal}/invitations", "GET /api/v1/invitations", "GET /api/v1/people/{principal}/invitations",
		"DELETE /api/v1/invitations/{id}", "GET /api/v1/me/invitations", "POST /api/v1/me/invitations/{id}/claim",
		"POST /api/v1/service-identities", "GET /api/v1/service-identities", "GET /api/v1/service-identities/{name}",
		"PATCH /api/v1/service-identities/{name}", "DELETE /api/v1/service-identities/{name}", "GET /api/v1/me/service-identities",
		"POST /api/v1/me/service-identities/{name}/tokens", "GET /api/v1/me/service-identities/{name}/tokens",
		"DELETE /api/v1/me/service-identities/{name}/tokens/{id}", "POST /api/v1/tokens/{id}/move",
	} {
		if !seen[r] {
			t.Errorf("section 7.1 route %s has no probe block", r)
		}
	}
}

func (e probeEnv) runCell(t *testing.T, b probeBlock, caller string, w want) {
	t.Helper()
	fx := e.fixture(t, b.route, caller, b.state)
	m := method(b.route)
	got := e.callAs(t, caller, b.subject, m, fx.path, fx.body)
	if fx.after != nil {
		defer fx.after(t, got)
	}
	// Nothing but a mint's own 201 may carry a bearer-shaped value.
	mintsOne := got.Code == http.StatusCreated && (b.route == "POST /api/v1/me/invitations/{id}/claim" || strings.HasSuffix(b.route, "/tokens"))
	if got.Code/100 == 2 && !mintsOne && bearerShaped.MatchString(got.Body.String()) {
		t.Errorf("%d carries a bearer-shaped value: %s", got.Code, got.Body.String())
	}
	if got.Code >= 500 {
		t.Errorf("%d %s: the design gives no cell that answers 5xx", got.Code, got.Body.String())
	}
	switch {
	case w.code != 0:
		if got.Code != w.code {
			t.Errorf("%s: %d %s, want %d %s", caller, got.Code, strings.TrimSpace(got.Body.String()), w.code, w.reason)
		}
	case w.anyOK:
		if got.Code/100 != 2 {
			t.Errorf("%s: %d %s, want a 2xx", caller, got.Code, strings.TrimSpace(got.Body.String()))
		}
	}
	if w.reason != "" && respReason(got) != w.reason {
		t.Errorf("%s: reason %q, want %q (%s)", caller, respReason(got), w.reason, strings.TrimSpace(got.Body.String()))
	}
	switch w.same {
	case "missing":
		ref := e.callAs(t, caller, b.subject, m, fx.missing, fx.body)
		if got.Code != ref.Code || got.Body.String() != ref.Body.String() {
			t.Errorf("%s: %d %q differs from the answer for something that does not exist, %d %q: an existence oracle",
				caller, got.Code, got.Body.String(), ref.Code, ref.Body.String())
		}
	case "tier":
		ref := e.callAs(t, caller, b.subject, http.MethodPost, "/api/v1/people", `{}`)
		if got.Code != ref.Code || respReason(got) != respReason(ref) {
			t.Errorf("%s: %d %q, want the %d %q POST /people gives the same caller", caller, got.Code, respReason(got), ref.Code, respReason(ref))
		}
	}
	// The invariant for every cell with no exact answer, and the secret rule for the ones that carry one.
	if fx.secret != "" && got.Code/100 == 2 && strings.Contains(got.Body.String(), fx.secret) != w.shows {
		t.Errorf("%s: %d body shows %q = %v, want %v: %s", caller, got.Code, fx.secret, !w.shows, w.shows, got.Body.String())
	}
	if w.code == 0 && !w.anyOK && !w.shows && got.Code/100 == 2 {
		t.Errorf("%s: a 2xx on a cell the design leaves to the invariant: %d %s", caller, got.Code, got.Body.String())
	}
}
