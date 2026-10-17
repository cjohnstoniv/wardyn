// Copyright 2026 The Wardyn Authors
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
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// moveEnv is svcEnv plus one pre-created person, pat, whom admin-minted tokens are seeded for.
type moveEnv struct {
	svcEnv
	pat *http.Cookie
}

func newMoveEnv(t *testing.T) moveEnv {
	t.Helper()
	e := moveEnv{svcEnv: newSvcEnv(t)}
	e.h.srv.router = e.h.srv.routes() // POST /sessions/revoke mounts only once the revocation store is wired
	if w := e.createPerson(t, e.sec, personSub, personEmail); w.Code != http.StatusCreated {
		t.Fatalf("create person: %d %s", w.Code, w.Body.String())
	}
	e.pat = e.login(t, personSub, personEmail, oidc.RoleUser)
	return e
}

// minted seeds an admin-minted token for pat, as the removed mint route left them.
func (e moveEnv) minted(t *testing.T) types.APIToken {
	t.Helper()
	return e.seedLegacyToken(t, personSub, oidc.RoleUser, "sec")
}

// inventory is GET /tokens?minted_for_others=true by id.
func (e moveEnv) inventory(t *testing.T, withRevoked bool) map[uuid.UUID]types.APIToken {
	t.Helper()
	path := "/api/v1/tokens?minted_for_others=true"
	if withRevoked {
		path += "&include_revoked=true"
	}
	w := doSSO(t, e.h.srv, http.MethodGet, path, e.sec, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, w.Code, w.Body.String())
	}
	var rows []types.APIToken
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	out := map[uuid.UUID]types.APIToken{}
	for _, r := range rows {
		out[r.ID] = r
	}
	return out
}

func (e moveEnv) move(t *testing.T, as *http.Cookie, id uuid.UUID, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doSSO(t, e.h.srv, http.MethodPost, "/api/v1/tokens/"+id.String()+"/move", as, body)
}

func (e moveEnv) revokeAsAdmin(t *testing.T, id uuid.UUID) {
	t.Helper()
	if w := doSSO(t, e.h.srv, http.MethodDelete, "/api/v1/tokens/"+id.String(), e.sec, ""); w.Code != http.StatusNoContent {
		t.Fatalf("revoke %s: %d %s", id, w.Code, w.Body.String())
	}
}

func (e moveEnv) invitationReplacing(t *testing.T, id uuid.UUID) types.Invitation {
	t.Helper()
	w := doSSO(t, e.h.srv, http.MethodPost, "/api/v1/people/"+personSub+"/invitations", e.sec, `{"replaces_token_id":"`+id.String()+`"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("invite replacing %s: %d %s", id, w.Code, w.Body.String())
	}
	var inv types.Invitation
	if err := json.Unmarshal(w.Body.Bytes(), &inv); err != nil {
		t.Fatal(err)
	}
	return inv
}

// TestMintedTokenDispositions drives one seeded admin-minted token to each of the six dispositions by the
// route that earns it, and reads each back from GET /tokens.
func TestMintedTokenDispositions(t *testing.T) {
	e := newMoveEnv(t)
	e.mustCreate(t, svcName, svcSponsor)

	needs, awaiting, replaced := e.minted(t), e.minted(t), e.minted(t)
	linked, moved, revoked, withdrawn := e.minted(t), e.minted(t), e.minted(t), e.minted(t)
	both, adminFirst := e.minted(t), e.minted(t)

	e.invitationReplacing(t, awaiting.ID)
	claimInv := e.invitationReplacing(t, replaced.ID)
	if w := doSSO(t, e.h.srv, http.MethodPost, "/api/v1/me/invitations/"+claimInv.ID.String()+"/claim", e.pat, `{}`); w.Code != http.StatusCreated {
		t.Fatalf("claim: %d %s", w.Code, w.Body.String())
	}
	// A revoked invitation decides nothing: the token it named is back to needing a decision.
	if w := doSSO(t, e.h.srv, http.MethodDelete, "/api/v1/invitations/"+e.invitationReplacing(t, withdrawn.ID).ID.String(), e.sec, ""); w.Code != http.StatusNoContent {
		t.Fatalf("revoke invitation: %d", w.Code)
	}
	for _, id := range []uuid.UUID{linked.ID, moved.ID} {
		if w := e.move(t, e.sec, id, `{"service_identity":"`+svcName+`"}`); w.Code != http.StatusOK {
			t.Fatalf("move %s: %d %s", id, w.Code, w.Body.String())
		}
	}
	e.revokeAsAdmin(t, moved.ID)
	e.revokeAsAdmin(t, revoked.ID)
	// A token with a pending invitation AND a move row reads linked: the move is the admin's explicit choice.
	e.invitationReplacing(t, both.ID)
	if w := e.move(t, e.sec, both.ID, `{"service_identity":"`+svcName+`"}`); w.Code != http.StatusOK {
		t.Fatalf("move %s: %d %s", both.ID, w.Code, w.Body.String())
	}
	// An admin revokes the token after the invitation named it; the owner's later claim retires nothing, so the
	// token reads revoked, not replaced_by_owner.
	adminInv := e.invitationReplacing(t, adminFirst.ID)
	e.revokeAsAdmin(t, adminFirst.ID)
	if w := doSSO(t, e.h.srv, http.MethodPost, "/api/v1/me/invitations/"+adminInv.ID.String()+"/claim", e.pat, `{}`); w.Code != http.StatusCreated {
		t.Fatalf("claim after the admin revoke: %d %s", w.Code, w.Body.String())
	}

	want := map[uuid.UUID]types.TokenDisposition{
		needs.ID:      types.TokenNeedsDecision,
		awaiting.ID:   types.TokenAwaitingOwner,
		replaced.ID:   types.TokenReplacedByOwner,
		linked.ID:     types.TokenLinkedToService,
		moved.ID:      types.TokenMovedToService,
		revoked.ID:    types.TokenRevoked,
		withdrawn.ID:  types.TokenNeedsDecision,
		both.ID:       types.TokenLinkedToService,
		adminFirst.ID: types.TokenRevoked,
	}
	live := e.inventory(t, false)
	all := e.inventory(t, true)
	for id, d := range want {
		if got := all[id].Disposition; got != d {
			t.Errorf("include_revoked=true: token %s is %q, want %q", id, got, d)
		}
		_, isLive := live[id]
		if wantLive := d == types.TokenNeedsDecision || d == types.TokenAwaitingOwner || d == types.TokenLinkedToService; isLive != wantLive {
			t.Errorf("default inventory holds token %s (%s) = %v, want %v", id, d, isLive, wantLive)
		}
		if isLive && live[id].Disposition != d {
			t.Errorf("default inventory: token %s is %q, want %q", id, live[id].Disposition, d)
		}
	}
	// The plain deployment list carries the same dispositions, and only on admin-minted rows.
	w := doSSO(t, e.h.srv, http.MethodGet, "/api/v1/tokens", e.sec, "")
	var rows []types.APIToken
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.MintedBy == "" && r.Disposition != "" {
			t.Errorf("a token nobody minted for someone else carries disposition %q", r.Disposition)
		}
		if d, ok := want[r.ID]; ok && r.Disposition != d {
			t.Errorf("GET /tokens: token %s is %q, want %q", r.ID, r.Disposition, d)
		}
	}
}

// TestTokenMove_NeverRevokesAndRefusesInvalid: a move links and answers the disposition but revokes
// nothing; every invalid token or identity is 422 token_move_invalid and writes no row and no audit.
func TestTokenMove_NeverRevokesAndRefusesInvalid(t *testing.T) {
	e := newMoveEnv(t)
	e.mustCreate(t, svcName, svcSponsor)
	e.mustCreate(t, "second-bot", svcSponsor2)
	tok := e.minted(t)

	w := e.move(t, e.sec, tok.ID, `{"service_identity":"`+svcName+`"}`)
	var got types.TokenMoved
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != http.StatusOK ||
		got.Disposition != types.TokenLinkedToService || got.ServiceIdentity != svcName || got.TokenID != tok.ID {
		t.Fatalf("move: %d %s, want 200 linked_to_service_identity to %s", w.Code, w.Body.String(), svcName)
	}
	if w := do(t, e.h.srv, http.MethodGet, "/api/v1/me/tokens", tok.Token, ""); w.Code != http.StatusOK {
		t.Errorf("the moved token after the move: %d, want 200 (a move never revokes)", w.Code)
	}
	if rows := e.auditRows("token.revoke"); len(rows) != 0 {
		t.Errorf("a move wrote token.revoke rows: %+v", rows)
	}
	rows := e.auditRows("token.update")
	if len(rows) != 1 || rows[0].Target != tok.ID.String() ||
		!strings.Contains(string(rows[0].Data), `"service_identity":"`+svcName+`"`) ||
		!strings.Contains(string(rows[0].Data), `"disposition":"linked_to_service_identity"`) {
		t.Errorf("token.update rows = %+v, want one naming the token, disposition and identity", rows)
	}
	// The admin may correct the choice; the row follows.
	if w := e.move(t, e.sec, tok.ID, `{"service_identity":"second-bot"}`); w.Code != http.StatusOK {
		t.Fatalf("re-move: %d %s", w.Code, w.Body.String())
	}
	var to string
	if err := e.pool.QueryRow(context.Background(), `SELECT service_identity FROM api_token_moves WHERE token_id = $1`, tok.ID).Scan(&to); err != nil || to != "second-bot" {
		t.Errorf("api_token_moves names %q (%v), want second-bot", to, err)
	}

	moveRows := func() int {
		var n int
		if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM api_token_moves`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before, updates := moveRows(), len(e.auditRows("token.update"))

	dead := e.minted(t)
	e.revokeAsAdmin(t, dead.ID)
	own := e.seedLegacyToken(t, personSub, oidc.RoleUser, personSub) // minter is the owner: not admin-minted
	retired := e.mustCreate(t, "old-bot", svcSponsor)
	if w := doSSO(t, e.h.srv, http.MethodDelete, "/api/v1/service-identities/"+retired.Name, e.sec, ""); w.Code != http.StatusOK {
		t.Fatalf("retire: %d %s", w.Code, w.Body.String())
	}
	svcTok := e.mustMint(t, e.sponsor, svcName)
	live := e.minted(t)
	for _, c := range []struct{ what, id, body string }{
		{"a revoked token", dead.ID.String(), `{"service_identity":"` + svcName + `"}`},
		{"a token its owner minted", own.ID.String(), `{"service_identity":"` + svcName + `"}`},
		{"a service identity's token", svcTok.ID.String(), `{"service_identity":"second-bot"}`},
		{"an unknown identity", live.ID.String(), `{"service_identity":"no-such-bot"}`},
		{"a retired identity", live.ID.String(), `{"service_identity":"old-bot"}`},
		{"no identity", live.ID.String(), `{}`},
	} {
		w := doSSO(t, e.h.srv, http.MethodPost, "/api/v1/tokens/"+c.id+"/move", e.sec, c.body)
		if w.Code != http.StatusUnprocessableEntity || respReason(w) != "token_move_invalid" {
			t.Errorf("%s: %d %s, want 422 token_move_invalid", c.what, w.Code, w.Body.String())
		}
	}
	if w := e.move(t, e.sec, uuid.New(), `{"service_identity":"`+svcName+`"}`); w.Code != http.StatusNotFound {
		t.Errorf("an unknown token: %d, want 404", w.Code)
	}
	if w := e.move(t, e.sec, live.ID, `{"service_identity":"`+svcName+`","extra":1}`); w.Code != http.StatusBadRequest {
		t.Errorf("an unknown body field: %d, want 400", w.Code)
	}
	if w := e.move(t, e.mem, live.ID, `{"service_identity":"`+svcName+`"}`); w.Code != http.StatusForbidden {
		t.Errorf("a member: %d, want 403", w.Code)
	}
	if after := moveRows(); after != before {
		t.Errorf("api_token_moves has %d rows after the refusals, want %d", after, before)
	}
	if after := len(e.auditRows("token.update")); after != updates {
		t.Errorf("%d token.update rows after the refusals, want %d", after, updates)
	}
	if d := e.inventory(t, true)[live.ID].Disposition; d != types.TokenNeedsDecision {
		t.Errorf("a refused move left the token %q, want needs_decision", d)
	}
}

// TestTokenMove_ARetiredIdentityHandsTheTokenBack: a live token moved to a service identity that is then
// retired is no longer handled; it needs a decision again and names no identity.
func TestTokenMove_ARetiredIdentityHandsTheTokenBack(t *testing.T) {
	e := newMoveEnv(t)
	e.mustCreate(t, svcName, svcSponsor)
	tok := e.minted(t)
	if w := e.move(t, e.sec, tok.ID, `{"service_identity":"`+svcName+`"}`); w.Code != http.StatusOK {
		t.Fatalf("move: %d %s", w.Code, w.Body.String())
	}
	if w := doSSO(t, e.h.srv, http.MethodDelete, "/api/v1/service-identities/"+svcName, e.sec, ""); w.Code != http.StatusOK {
		t.Fatalf("retire: %d %s", w.Code, w.Body.String())
	}
	if got := e.inventory(t, true)[tok.ID]; got.Disposition != types.TokenNeedsDecision || got.ServiceIdentity != "" {
		t.Errorf("a live token moved to a retired identity reads %q (%q), want needs_decision and no identity", got.Disposition, got.ServiceIdentity)
	}
}

// TestRevokePersonCredentials_SweepsSponsoredTokens: a sponsor's full session revoke leaves the tokens of the
// identities they sponsor revoked in GET /tokens, and leaves another sponsor's alone. A session-only cut
// revokes none.
func TestRevokePersonCredentials_SweepsSponsoredTokens(t *testing.T) {
	e := newMoveEnv(t)
	e.mustCreate(t, svcName, svcSponsor)
	e.mustCreate(t, "second-bot", svcSponsor)
	e.mustCreate(t, "third-bot", svcSponsor2)
	mine1, mine2 := e.mustMint(t, e.sponsor, svcName), e.mustMint(t, e.sponsor, "second-bot")
	theirs := e.mustMint(t, e.sponsor2, "third-bot")

	revokedAt := func() map[uuid.UUID]*time.Time {
		w := doSSO(t, e.h.srv, http.MethodGet, "/api/v1/tokens", e.sec, "")
		var rows []types.APIToken
		if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
			t.Fatal(err)
		}
		out := map[uuid.UUID]*time.Time{}
		for _, r := range rows {
			out[r.ID] = r.RevokedAt
		}
		return out
	}
	if w := doSSO(t, e.h.srv, http.MethodPost, "/api/v1/sessions/revoke", e.sec, `{"sub":"`+svcSponsor+`","sessions_only":true}`); w.Code != http.StatusNoContent {
		t.Fatalf("sessions-only revoke: %d %s", w.Code, w.Body.String())
	}
	for _, id := range []uuid.UUID{mine1.ID, mine2.ID, theirs.ID} {
		if revokedAt()[id] != nil {
			t.Errorf("a session-only cut revoked token %s", id)
		}
	}
	if w := doSSO(t, e.h.srv, http.MethodPost, "/api/v1/sessions/revoke", e.sec, `{"sub":"`+svcSponsor+`"}`); w.Code != http.StatusNoContent {
		t.Fatalf("full revoke: %d %s", w.Code, w.Body.String())
	}
	got := revokedAt()
	for _, id := range []uuid.UUID{mine1.ID, mine2.ID} {
		if got[id] == nil {
			t.Errorf("sponsored token %s is not revoked in GET /tokens after the sponsor's full revoke", id)
		}
	}
	if got[theirs.ID] != nil {
		t.Errorf("another sponsor's token %s was revoked", theirs.ID)
	}
	swept := 0
	for _, r := range e.auditRows("token.revoke") {
		if strings.Contains(string(r.Data), `"scope":"sweep"`) && strings.Contains(string(r.Data), `"principal":"service:`) {
			swept++
		}
	}
	if swept != 2 {
		t.Errorf("%d token.revoke sweep rows name a service principal, want 2", swept)
	}
}

// TestPeople_ExcludesServicePrincipals: GET /people lists no service: principal, even a stray row a table
// holds, while the real people stay.
func TestPeople_ExcludesServicePrincipals(t *testing.T) {
	e := newMoveEnv(t)
	e.mustCreate(t, svcName, svcSponsor)
	e.mustMint(t, e.sponsor, svcName)
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `INSERT INTO people (principal, email, created_by) VALUES ('service:stray', 'stray@corp.example', 'admin')`); err != nil {
		t.Fatal(err)
	}
	w := doSSO(t, e.h.srv, http.MethodGet, "/api/v1/people", e.sec, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /people: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(strings.ToLower(w.Body.String()), "service:") {
		t.Errorf("GET /people lists a service principal: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), personSub) || !strings.Contains(w.Body.String(), svcSponsor) {
		t.Errorf("GET /people lost the real people: %s", w.Body.String())
	}
}

// TestKnownPrincipals_ExcludesServicePrincipals: the directory a name is resolved against holds a person's
// token but neither a service identity's token nor a workspace a service principal owns.
func TestKnownPrincipals_ExcludesServicePrincipals(t *testing.T) {
	e := newMoveEnv(t)
	e.mustCreate(t, svcName, svcSponsor)
	e.mustMint(t, e.sponsor, svcName)
	e.minted(t)
	if _, err := e.st.CreateWorkspace(context.Background(), types.Workspace{
		ID: uuid.New(), Name: "svc-ws", OwnedBy: svcPrin, Status: types.WorkspaceScanned,
		Sources:   []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeEphemeral, Target: "/home/agent/work"}},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	dir, err := e.h.srv.knownPrincipals(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var person bool
	for _, p := range dir {
		if isServicePrincipal(p.principal) {
			t.Errorf("knownPrincipals holds %q", p.principal)
		}
		person = person || p.principal == personSub
	}
	if !person {
		t.Errorf("knownPrincipals lost the person's own token: %+v", dir)
	}
}

// TestRevokePersonCredentials_SweepsSponsoredTokensByEmail: naming the sponsor by email reaches their service
// tokens too, under the subject the key resolver finds, so GET /tokens shows them revoked.
func TestRevokePersonCredentials_SweepsSponsoredTokensByEmail(t *testing.T) {
	e := newMoveEnv(t)
	e.mustCreate(t, svcName, svcSponsor)
	tok := e.mustMint(t, e.sponsor, svcName)
	w := doSSO(t, e.h.srv, http.MethodPost, "/api/v1/sessions/revoke", e.sec, `{"sub":"`+svcSponsor+`@corp.example"}`)
	if w.Code != http.StatusNoContent {
		t.Fatalf("revoke by email: %d %s", w.Code, w.Body.String())
	}
	rows, err := e.st.ListAPITokensByPrincipal(context.Background(), svcPrin)
	if err != nil || len(rows) != 1 || rows[0].ID != tok.ID || rows[0].RevokedAt == nil {
		t.Errorf("the sponsored token after an email-form revoke = %+v (%v), want revoked", rows, err)
	}
}
