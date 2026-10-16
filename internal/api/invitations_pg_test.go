// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// inviteEnv is peoplePG with one pre-created person, pat, and the sessions that may and may not claim for them.
type inviteEnv struct {
	peoplePG
	pat, other *http.Cookie
}

func newInviteEnv(t *testing.T) inviteEnv {
	t.Helper()
	e := inviteEnv{peoplePG: newPeoplePG(t)}
	if w := e.createPerson(t, e.sec, personSub, personEmail); w.Code != http.StatusCreated {
		t.Fatalf("create person: %d %s", w.Code, w.Body.String())
	}
	e.pat = e.login(t, personSub, personEmail, oidc.RoleUser)
	e.other = accessSession(t, "other-sub", "other@corp.example", oidc.RoleUser, []string{})
	return e
}

func (e inviteEnv) invite(t *testing.T, as *http.Cookie, principal, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doSSO(t, e.h.srv, http.MethodPost, "/api/v1/people/"+principal+"/invitations", as, body)
}

func (e inviteEnv) mustInvite(t *testing.T, body string) types.Invitation {
	t.Helper()
	w := e.invite(t, e.sec, personSub, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create invitation: %d %s, want 201", w.Code, w.Body.String())
	}
	var inv types.Invitation
	if err := json.Unmarshal(w.Body.Bytes(), &inv); err != nil {
		t.Fatal(err)
	}
	return inv
}

func (e inviteEnv) claim(t *testing.T, as *http.Cookie, id uuid.UUID) *httptest.ResponseRecorder {
	t.Helper()
	return doSSO(t, e.h.srv, http.MethodPost, "/api/v1/me/invitations/"+id.String()+"/claim", as, `{}`)
}

func (e inviteEnv) tokenRows(t *testing.T, principal string) int {
	t.Helper()
	rows, err := e.st.ListAPITokensByPrincipal(context.Background(), principal)
	if err != nil {
		t.Fatal(err)
	}
	return len(rows)
}

func (e inviteEnv) stateOf(t *testing.T, id uuid.UUID) types.InvitationState {
	t.Helper()
	inv, err := e.st.GetInvitation(context.Background(), id, "")
	if err != nil {
		t.Fatal(err)
	}
	return inv.StateAt(time.Now())
}

func (e inviteEnv) expire(t *testing.T, id uuid.UUID) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(), `UPDATE person_invitations SET expires_at = now() - interval '1 minute' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
}

func inviteReason(w *httptest.ResponseRecorder) string {
	var b struct {
		Reason string `json:"reason"`
		State  string `json:"state"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &b)
	return b.Reason
}

// seedPortalToken registers a portal and gives it a live delegated token for principal, the state a
// successful exchange leaves, written at the store.
func (e inviteEnv) seedPortalToken(t *testing.T, principal string) string {
	t.Helper()
	ctx := context.Background()
	d, err := e.st.CreateDelegate(ctx, types.Delegate{ID: uuid.New(), Name: "portal", IdPClientID: "portal-client", Group: "portal-users"},
		newBearer(delegateCredentialPrefix))
	if err != nil {
		t.Fatal(err)
	}
	raw := newBearer(delegatedTokenPrefix)
	tok := types.DelegatedToken{
		ID: uuid.New(), DelegateID: d.ID, Principal: principal, Email: personEmail,
		UserType: types.UserTypeStandard, Groups: []string{"portal-users"},
		CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(delegatedTokenTTL),
	}
	if _, err := e.st.MintDelegatedToken(ctx, tok, raw, time.Now()); err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestInvitation_ClaimOnlyByTheSignedInTarget walks every row of the design's caller table: the target
// mints a token that is theirs, and everyone else is refused with the answer the table names, leaving
// the invitation pending and no token behind.
func TestInvitation_ClaimOnlyByTheSignedInTarget(t *testing.T) {
	e := newInviteEnv(t)
	missing := e.claim(t, e.other, uuid.New())
	if missing.Code != http.StatusNotFound || inviteReason(missing) != reasonInvitationNotFound {
		t.Fatalf("a missing id: %d %s, want 404 invitation_not_found", missing.Code, missing.Body.String())
	}

	// refused runs one non-target row: the answer, and nothing claimed or minted.
	refused := func(name string, w func(inv types.Invitation) *httptest.ResponseRecorder, wantCode int, wantReason string) {
		t.Run(name, func(t *testing.T) {
			inv := e.mustInvite(t, `{}`)
			before := e.tokenRows(t, personSub)
			got := w(inv)
			if got.Code != wantCode || inviteReason(got) != wantReason {
				t.Fatalf("%d %s, want %d %s", got.Code, got.Body.String(), wantCode, wantReason)
			}
			if s := e.stateOf(t, inv.ID); s != types.InvitationPending {
				t.Errorf("invitation is %s after a refused claim, want pending", s)
			}
			if n := e.tokenRows(t, personSub); n != before {
				t.Errorf("target holds %d tokens after a refused claim, had %d", n, before)
			}
		})
	}
	notFound := func(name string, c *http.Cookie) {
		refused(name, func(inv types.Invitation) *httptest.ResponseRecorder {
			w := e.claim(t, c, inv.ID)
			if w.Body.String() != missing.Body.String() {
				t.Errorf("body %q differs from a missing id's %q: the answer is an oracle", w.Body.String(), missing.Body.String())
			}
			return w
		}, http.StatusNotFound, reasonInvitationNotFound)
	}
	notFound("another person", e.other)
	notFound("an admin", e.super)
	notFound("a security admin", e.sec)
	refused("the admin token", func(inv types.Invitation) *httptest.ResponseRecorder {
		return do(t, e.h.srv, http.MethodPost, "/api/v1/me/invitations/"+inv.ID.String()+"/claim", e.h.srv.cfg.AdminToken, `{}`)
	}, http.StatusForbidden, reasonAPITokenNoHuman)
	refused("local mode", func(inv types.Invitation) *httptest.ResponseRecorder {
		cfg := e.h.srv.cfg
		cfg.OIDC, cfg.AdminToken, cfg.LocalMode, cfg.LocalOperator, cfg.LocalLoopback = nil, "", true, "local:test", true
		return do(t, New(cfg), http.MethodPost, "/api/v1/me/invitations/"+inv.ID.String()+"/claim", "", `{}`)
	}, http.StatusForbidden, reasonAPITokenNoHuman)
	tok := e.seedLegacyToken(t, personSub, oidc.RoleUser, "")
	refused("the target's own wdn_ token", func(inv types.Invitation) *httptest.ResponseRecorder {
		return do(t, e.h.srv, http.MethodPost, "/api/v1/me/invitations/"+inv.ID.String()+"/claim", tok.Token, `{}`)
	}, http.StatusForbidden, reasonAPITokenFromAPIToken)
	refused("a portal's wdg_ token", func(inv types.Invitation) *httptest.ResponseRecorder {
		return do(t, e.h.srv, http.MethodPost, "/api/v1/me/invitations/"+inv.ID.String()+"/claim", e.seedPortalToken(t, personSub), `{}`)
	}, http.StatusForbidden, "delegation_scope")
	refused("the target without the api_token feature", func(inv types.Invitation) *httptest.ResponseRecorder {
		if _, err := e.st.PutCapabilityEnforcement(context.Background(), map[string]bool{capFeature: true}); err != nil {
			t.Fatal(err)
		}
		return e.claim(t, e.pat, inv.ID)
	}, http.StatusForbidden, "capability_feature")
	_, _ = e.st.PutCapabilityEnforcement(context.Background(), map[string]bool{})

	t.Run("the target in member mode", func(t *testing.T) {
		srv, _ := memberModeServer(t)
		w := doSSO(t, srv, http.MethodPost, "/api/v1/me/invitations/"+uuid.NewString()+"/claim",
			memberModeSSOSession(t, personSub, personEmail, oidc.RoleUser, true), `{}`)
		if w.Code != http.StatusConflict || inviteReason(w) != reasonAPITokenMemberModeMint {
			t.Fatalf("%d %s, want 409 api_token_member_mode_mint", w.Code, w.Body.String())
		}
	})

	t.Run("the target, pending: 201 with a token that is theirs", func(t *testing.T) {
		inv := e.mustInvite(t, `{"token_name":"ci"}`)
		w := e.claim(t, e.pat, inv.ID)
		tok := decodeToken(t, w)
		if !strings.HasPrefix(tok.Token, apiTokenPrefix) || tok.Principal != personSub || tok.Name != "ci" || tok.MintedBy != "" {
			t.Fatalf("claimed token = %+v, want a wdn_ token for the target, named ci, with no minter", tok)
		}
		if w := do(t, e.h.srv, http.MethodGet, "/api/v1/me/tokens", tok.Token, ""); w.Code != http.StatusOK {
			t.Errorf("the claimed token does not authenticate: %d %s", w.Code, w.Body.String())
		}
		if s := e.stateOf(t, inv.ID); s != types.InvitationClaimed {
			t.Errorf("invitation is %s, want claimed", s)
		}
		for _, action := range []string{"token.create", "invitation.claim"} {
			if len(e.auditRows(action)) == 0 {
				t.Errorf("no %s row", action)
			}
		}
		if rows := e.auditRows("token.create"); !strings.Contains(string(rows[len(rows)-1].Data), inv.ID.String()) {
			t.Errorf("token.create data = %s, want the invitation id", rows[len(rows)-1].Data)
		}
	})

	for _, tc := range []struct {
		name  string
		prep  func(inv types.Invitation)
		state types.InvitationState
	}{
		{"claimed", func(inv types.Invitation) { e.claim(t, e.pat, inv.ID) }, types.InvitationClaimed},
		{"revoked", func(inv types.Invitation) {
			doSSO(t, e.h.srv, http.MethodDelete, "/api/v1/invitations/"+inv.ID.String(), e.sec, "")
		}, types.InvitationRevoked},
		{"expired", func(inv types.Invitation) { e.expire(t, inv.ID) }, types.InvitationExpired},
	} {
		t.Run("the target, "+tc.name+": 409 with the state", func(t *testing.T) {
			inv := e.mustInvite(t, `{}`)
			tc.prep(inv)
			before := e.tokenRows(t, personSub)
			w := e.claim(t, e.pat, inv.ID)
			var body struct{ State types.InvitationState }
			_ = json.Unmarshal(w.Body.Bytes(), &body)
			if w.Code != http.StatusConflict || inviteReason(w) != reasonInvitationNotPending || body.State != tc.state {
				t.Fatalf("%d %s, want 409 invitation_not_pending with state %s", w.Code, w.Body.String(), tc.state)
			}
			if n := e.tokenRows(t, personSub); n != before {
				t.Errorf("a refused claim minted a token (%d -> %d)", before, n)
			}
		})
	}
}

// TestInvitation_AdminSeesMetadataNeverTheToken: every admin read, and every audit row, holds no token.
func TestInvitation_AdminSeesMetadataNeverTheToken(t *testing.T) {
	e := newInviteEnv(t)
	inv := e.mustInvite(t, `{}`)
	claimed := decodeToken(t, e.claim(t, e.pat, inv.ID))
	if !strings.HasPrefix(claimed.Token, apiTokenPrefix) {
		t.Fatalf("claimed token %q", claimed.Token)
	}

	reads := map[string]string{
		"create":  e.invite(t, e.sec, personSub, `{}`).Body.String(),
		"list":    doSSO(t, e.h.srv, http.MethodGet, "/api/v1/invitations", e.sec, "").Body.String(),
		"claimed": doSSO(t, e.h.srv, http.MethodGet, "/api/v1/invitations?state=claimed", e.super, "").Body.String(),
		"person":  doSSO(t, e.h.srv, http.MethodGet, "/api/v1/people/"+personSub+"/invitations", e.sec, "").Body.String(),
	}
	for name, body := range reads {
		if strings.Contains(body, "wdn_") || strings.Contains(body, `"token":`) {
			t.Errorf("admin %s response carries a token: %s", name, body)
		}
	}
	if !strings.Contains(reads["claimed"], `"token_id":"`+claimed.ID.String()+`"`) || !strings.Contains(reads["claimed"], `"state":"claimed"`) {
		t.Errorf("admin list of claimed = %s, want the record with token_id and state", reads["claimed"])
	}
	for _, ev := range e.h.audit.snapshot() {
		if raw, _ := json.Marshal(ev); strings.Contains(string(raw), claimed.Token) || (strings.HasPrefix(ev.Action, "invitation.") && strings.Contains(string(raw), "wdn_")) {
			t.Errorf("audit row %s holds a token: %s", ev.Action, raw)
		}
	}
	if rows := e.auditRows("invitation.create"); len(rows) != 2 {
		t.Errorf("invitation.create rows = %d, want 2", len(rows))
	}
}

// TestInvitation_UnclaimedOrExpiredGrantsNothing: an invitation is not a credential. Its id presented
// as a bearer is a 401, no token row exists before the claim, and an expired or revoked one never gets one.
func TestInvitation_UnclaimedOrExpiredGrantsNothing(t *testing.T) {
	e := newInviteEnv(t)
	pending, expired, revoked := e.mustInvite(t, `{}`), e.mustInvite(t, `{}`), e.mustInvite(t, `{}`)
	e.expire(t, expired.ID)
	if w := doSSO(t, e.h.srv, http.MethodDelete, "/api/v1/invitations/"+revoked.ID.String(), e.sec, ""); w.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", w.Code, w.Body.String())
	}
	for _, inv := range []types.Invitation{pending, expired, revoked} {
		for _, bearer := range []string{inv.ID.String(), apiTokenPrefix + inv.ID.String(), inv.ConsolePath} {
			if w := do(t, e.h.srv, http.MethodGet, "/api/v1/me/tokens", bearer, ""); w.Code != http.StatusUnauthorized {
				t.Errorf("invitation %s as bearer %q: %d, want 401", inv.ID, bearer, w.Code)
			}
		}
	}
	if n := e.tokenRows(t, personSub); n != 0 {
		t.Fatalf("target holds %d tokens before any claim, want none", n)
	}
	for _, inv := range []types.Invitation{expired, revoked} {
		if w := e.claim(t, e.pat, inv.ID); w.Code != http.StatusConflict {
			t.Errorf("claim of %s: %d, want 409", inv.ID, w.Code)
		}
	}
	if n := e.tokenRows(t, personSub); n != 0 {
		t.Errorf("target holds %d tokens after claiming dead invitations, want none", n)
	}
}

// TestInvitation_CreateRefusals walks the create table: each refusal's status and reason, and the 201.
func TestInvitation_CreateRefusals(t *testing.T) {
	e := newInviteEnv(t)
	legacy := e.seedLegacyToken(t, personSub, oidc.RoleUser, "sec")
	own := e.seedLegacyToken(t, personSub, oidc.RoleUser, "")
	others := e.seedLegacyToken(t, "other-sub", oidc.RoleUser, "sec")
	for _, tc := range []struct {
		name, principal, body string
		as                    *http.Cookie
		code                  int
		reason                string
	}{
		{"a member", personSub, `{}`, e.mem, http.StatusForbidden, ""},
		{"a service principal", "service:ci", `{}`, e.sec, http.StatusUnprocessableEntity, reasonInvitationTargetInvalid},
		{"a reserved principal", "local:alice", `{}`, e.sec, http.StatusUnprocessableEntity, reasonInvitationTargetInvalid},
		{"a principal with a space", "a%20b", `{}`, e.sec, http.StatusUnprocessableEntity, reasonInvitationTargetInvalid},
		{"an unknown person", "nobody-sub", `{}`, e.sec, http.StatusUnprocessableEntity, reasonInvitationTargetUnknown},
		{"a token that does not exist", personSub, `{"replaces_token_id":"` + uuid.NewString() + `"}`, e.sec, http.StatusUnprocessableEntity, reasonInvitationReplacesInvalid},
		{"a live admin-minted token of another person", personSub, `{"replaces_token_id":"` + others.ID.String() + `"}`, e.sec, http.StatusUnprocessableEntity, reasonInvitationReplacesInvalid},
		{"a self-minted token", personSub, `{"replaces_token_id":"` + own.ID.String() + `"}`, e.sec, http.StatusUnprocessableEntity, reasonInvitationReplacesInvalid},
		{"a lifetime over 14 days", personSub, `{"expires_in_seconds":1209601}`, e.sec, http.StatusBadRequest, reasonInvitationTTLInvalid},
		{"a negative lifetime", personSub, `{"expires_in_seconds":-1}`, e.sec, http.StatusBadRequest, reasonInvitationTTLInvalid},
		{"a negative token lifetime", personSub, `{"token_ttl_seconds":-1}`, e.sec, http.StatusBadRequest, reasonInvitationTTLInvalid},
		{"an admin-minted token", personSub, `{"replaces_token_id":"` + legacy.ID.String() + `","expires_in_seconds":1209600}`, e.sec, http.StatusCreated, ""},
		{"the default", personSub, `{}`, e.super, http.StatusCreated, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := e.invite(t, tc.as, tc.principal, tc.body)
			if w.Code != tc.code || (tc.reason != "" && inviteReason(w) != tc.reason) {
				t.Fatalf("%d %s, want %d %s", w.Code, w.Body.String(), tc.code, tc.reason)
			}
			if tc.code == http.StatusCreated && !strings.Contains(w.Body.String(), `"console_path":"/account?invitation=`) {
				t.Errorf("201 body %s has no console_path", w.Body.String())
			}
		})
	}
	if w := e.invite(t, e.sec, personSub, `{"unknown":1}`); w.Code != http.StatusBadRequest {
		t.Errorf("an unknown field: %d, want 400", w.Code)
	}
}

// TestInvitation_TenPendingIsTheCap: the eleventh pending invitation for one person is refused, and a
// revoke frees a slot.
func TestInvitation_TenPendingIsTheCap(t *testing.T) {
	e := newInviteEnv(t)
	var first types.Invitation
	for i := 0; i < invitationMaxPending; i++ {
		inv := e.mustInvite(t, `{}`)
		if i == 0 {
			first = inv
		}
	}
	if w := e.invite(t, e.sec, personSub, `{}`); w.Code != http.StatusUnprocessableEntity || inviteReason(w) != reasonInvitationCapReached {
		t.Fatalf("eleventh: %d %s, want 422 invitation_cap_reached", w.Code, w.Body.String())
	}
	if w := doSSO(t, e.h.srv, http.MethodDelete, "/api/v1/invitations/"+first.ID.String(), e.sec, ""); w.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d", w.Code)
	}
	if w := e.invite(t, e.sec, personSub, `{}`); w.Code != http.StatusCreated {
		t.Errorf("after a revoke: %d %s, want 201", w.Code, w.Body.String())
	}
	if w := doSSO(t, e.h.srv, http.MethodDelete, "/api/v1/invitations/"+first.ID.String(), e.sec, ""); w.Code != http.StatusNotFound {
		t.Errorf("revoking a revoked invitation: %d, want 404", w.Code)
	}
}

// TestInvitation_ClaimRetiresTheReplacedToken: a claim revokes the admin-minted token it replaces in the
// same transaction, bounds the new token's lifetime by the invitation, and writes the three rows.
func TestInvitation_ClaimRetiresTheReplacedToken(t *testing.T) {
	e := newInviteEnv(t)
	legacy := e.seedLegacyToken(t, personSub, oidc.RoleUser, "sec")
	inv := e.mustInvite(t, `{"replaces_token_id":"`+legacy.ID.String()+`","token_ttl_seconds":3600}`)
	if w := do(t, e.h.srv, http.MethodGet, "/api/v1/me/tokens", legacy.Token, ""); w.Code != http.StatusOK {
		t.Fatalf("the admin-minted token is live before the claim: %d", w.Code)
	}
	tok := decodeToken(t, doSSO(t, e.h.srv, http.MethodPost, "/api/v1/me/invitations/"+inv.ID.String()+"/claim", e.pat, `{"ttl_seconds":999999}`))
	if tok.ExpiresAt == nil || tok.ExpiresAt.After(time.Now().Add(time.Hour+time.Minute)) {
		t.Errorf("claimed token expires %v, want within the invitation's hour", tok.ExpiresAt)
	}
	if w := do(t, e.h.srv, http.MethodGet, "/api/v1/me/tokens", legacy.Token, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("the replaced token after the claim: %d, want 401", w.Code)
	}
	rows := e.auditRows("token.revoke")
	if len(rows) != 1 || !strings.Contains(string(rows[0].Data), `"reason":"replaced_by_owner"`) || rows[0].Target != legacy.ID.String() {
		t.Errorf("token.revoke rows = %+v, want one replaced_by_owner row for the legacy token", rows)
	}
}

// TestInvitation_ConcurrentClaimsMintOneToken: two claims race for one invitation; exactly one token exists.
func TestInvitation_ConcurrentClaimsMintOneToken(t *testing.T) {
	e := newInviteEnv(t)
	inv := e.mustInvite(t, `{}`)
	var wg sync.WaitGroup
	codes := make([]int, 6)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = e.claim(t, e.pat, inv.ID).Code
		}()
	}
	wg.Wait()
	slices.Sort(codes)
	if codes[0] != http.StatusCreated || codes[1] != http.StatusConflict || codes[len(codes)-1] != http.StatusConflict {
		t.Errorf("claim codes = %v, want one 201 and the rest 409", codes)
	}
	if n := e.tokenRows(t, personSub); n != 1 {
		t.Errorf("target holds %d tokens, want exactly one", n)
	}
}

// TestInvitation_MeListsOnlyOwnPending: GET /me/invitations shows the caller's pending invitations and no one else's.
func TestInvitation_MeListsOnlyOwnPending(t *testing.T) {
	e := newInviteEnv(t)
	mine, done := e.mustInvite(t, `{}`), e.mustInvite(t, `{}`)
	e.claim(t, e.pat, done.ID)
	list := func(c *http.Cookie) string {
		return doSSO(t, e.h.srv, http.MethodGet, "/api/v1/me/invitations", c, "").Body.String()
	}
	if got := list(e.pat); !strings.Contains(got, mine.ID.String()) || strings.Contains(got, done.ID.String()) {
		t.Errorf("target's pending list = %s, want only %s", got, mine.ID)
	}
	if got := list(e.other); strings.Contains(got, mine.ID.String()) {
		t.Errorf("another person's list shows the invitation: %s", got)
	}
	if w := doSSO(t, e.h.srv, http.MethodGet, "/api/v1/invitations?state=bogus", e.sec, ""); w.Code != http.StatusBadRequest {
		t.Errorf("an unknown state: %d, want 400", w.Code)
	}
}
