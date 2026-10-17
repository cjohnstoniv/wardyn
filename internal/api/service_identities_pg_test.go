// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	svcSponsor  = "sponsor-sub"
	svcSponsor2 = "sponsor2-sub"
	svcName     = "ci-bot"
	svcPrin     = "service:ci-bot"
)

// mintCall is one MintRunIdentity the server made, with the token it got back.
type mintCall struct {
	humanSub, sponsor string
	operatorOwned     bool
	token             string
}

// recordingIdentity wraps the embedded provider and records what the server asked it to mint.
type recordingIdentity struct {
	identity.Provider
	mu    sync.Mutex
	calls []mintCall
}

func (p *recordingIdentity) MintRunIdentity(ctx context.Context, runID uuid.UUID, humanSub, sponsor, audience string, operatorOwned bool) (identity.RunIdentity, error) {
	id, err := p.Provider.MintRunIdentity(ctx, runID, humanSub, sponsor, audience, operatorOwned)
	p.mu.Lock()
	p.calls = append(p.calls, mintCall{humanSub: humanSub, sponsor: sponsor, operatorOwned: operatorOwned, token: id.Token})
	p.mu.Unlock()
	return id, err
}

func (p *recordingIdentity) last() mintCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[len(p.calls)-1]
}

// svcEnv is peoplePG with two bound sponsors and a revocation store, so a service token's sponsor check has
// something to ask.
type svcEnv struct {
	peoplePG
	rev                      *pgTestRevocations
	ids                      *recordingIdentity
	sponsor, sponsor2, other *http.Cookie
}

func newSvcEnv(t *testing.T) svcEnv {
	t.Helper()
	e := svcEnv{peoplePG: newPeoplePG(t)}
	e.rev = &pgTestRevocations{pool: e.pool, st: e.st}
	e.h.srv.cfg.SessionRevocations = e.rev
	e.ids = &recordingIdentity{Provider: e.h.srv.cfg.Identity}
	e.h.srv.cfg.Identity = e.ids
	for _, sub := range []string{svcSponsor, svcSponsor2} {
		e.bind(t, sub, "https://idp.example", "", "")
	}
	e.sponsor = accessSession(t, svcSponsor, "sponsor@corp.example", oidc.RoleUser, []string{})
	e.sponsor2 = accessSession(t, svcSponsor2, "sponsor2@corp.example", oidc.RoleUser, []string{})
	e.other = accessSession(t, "other-sub", "other@corp.example", oidc.RoleUser, []string{})
	return e
}

// bind records the identity row a sign-in would, for principal.
func (e svcEnv) bind(t *testing.T, principal, issuer, tenant, object string) {
	t.Helper()
	if _, err := e.st.UpsertLoginIdentity(context.Background(), store.LoginIdentity{
		Principal: principal, Issuer: issuer, TenantID: tenant, ObjectID: object, Email: principal + "@corp.example",
	}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
}

func (e svcEnv) create(t *testing.T, as *http.Cookie, name, sponsor string) *httptest.ResponseRecorder {
	t.Helper()
	return doSSO(t, e.h.srv, http.MethodPost, "/api/v1/service-identities", as,
		`{"name":"`+name+`","sponsor":"`+sponsor+`","description":"ci runner"}`)
}

func (e svcEnv) mustCreate(t *testing.T, name, sponsor string) types.ServiceIdentity {
	t.Helper()
	w := e.create(t, e.sec, name, sponsor)
	if w.Code != http.StatusCreated {
		t.Fatalf("create service identity: %d %s, want 201", w.Code, w.Body.String())
	}
	var si types.ServiceIdentity
	if err := json.Unmarshal(w.Body.Bytes(), &si); err != nil {
		t.Fatal(err)
	}
	return si
}

func (e svcEnv) mintReq(t *testing.T, as *http.Cookie, name, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doSSO(t, e.h.srv, http.MethodPost, "/api/v1/me/service-identities/"+name+"/tokens", as, body)
}

// mustMint has the sponsor mint a token and returns its plaintext.
func (e svcEnv) mustMint(t *testing.T, as *http.Cookie, name string) types.APIToken {
	t.Helper()
	w := e.mintReq(t, as, name, `{"name":"ci","ttl_seconds":3600}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("mint service token: %d %s, want 201", w.Code, w.Body.String())
	}
	var tok types.APIToken
	if err := json.Unmarshal(w.Body.Bytes(), &tok); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok.Token, apiTokenPrefix) {
		t.Fatalf("minted token %q has no plaintext", tok.Token)
	}
	return tok
}

// plainPolicy stores a policy with no credential grant and returns its id.
func (e svcEnv) plainPolicy(t *testing.T, name string) string {
	t.Helper()
	w := doSSO(t, e.h.srv, http.MethodPost, "/api/v1/policies", e.admin,
		`{"name":"`+name+`","spec":{"min_confinement_class":"CC2","allowed_domains":["api.anthropic.com"]}}`)
	var p types.RunPolicy
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &p) != nil {
		t.Fatalf("store policy: %d %s", w.Code, w.Body.String())
	}
	return p.ID.String()
}

func (e svcEnv) as(t *testing.T, token, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, e.h.srv, method, path, token, body)
}

// authFailReasons are the reasons of the auth.fail rows written so far.
func (e svcEnv) authFailReasons() []string {
	var out []string
	for _, ev := range e.h.audit.snapshot() {
		if ev.Action == "auth.fail" {
			var d struct {
				Reason string `json:"reason"`
			}
			_ = json.Unmarshal(ev.Data, &d)
			out = append(out, d.Reason)
		}
	}
	return out
}

func (e svcEnv) lastAuthFail() string {
	rs := e.authFailReasons()
	if len(rs) == 0 {
		return ""
	}
	return rs[len(rs)-1]
}

func respReason(w *httptest.ResponseRecorder) string {
	var b struct {
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &b)
	return b.Reason
}

// bothActorForms runs fn once with the service actor and once with D8's fallback (the system actor), the
// two forms every decision about a service has to survive.
func bothActorForms(t *testing.T, fn func(t *testing.T, actor types.ActorType)) {
	t.Helper()
	for _, form := range []struct {
		name  string
		actor types.ActorType
	}{{"service", types.ActorService}, {"fallback_system", types.ActorSystem}} {
		t.Run(form.name, func(t *testing.T) {
			prev := serviceActorType
			serviceActorType = form.actor
			t.Cleanup(func() { serviceActorType = prev })
			fn(t, form.actor)
		})
	}
}

// TestServiceIdentity_RunsUnderItsOwnPrincipal: a run a service token launches belongs to service:<name>,
// is not operator-owned, carries the sponsor in its identity claims, and every row it writes names the
// service as actor and the sponsor in data.sponsor, under both actor forms.
func TestServiceIdentity_RunsUnderItsOwnPrincipal(t *testing.T) {
	bothActorForms(t, func(t *testing.T, actor types.ActorType) {
		e := newSvcEnv(t)
		e.mustCreate(t, svcName, svcSponsor)
		tok := e.mustMint(t, e.sponsor, svcName)
		pol := e.plainPolicy(t, "svc-pol")
		w := e.as(t, tok.Token, http.MethodPost, "/api/v1/runs", `{"agent":"claude-code","task":"t","policy_id":"`+pol+`"}`)
		if w.Code != http.StatusCreated {
			t.Fatalf("service run create: %d %s, want 201", w.Code, w.Body.String())
		}
		var created createRunResponse
		if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
			t.Fatal(err)
		}
		run, err := e.st.GetRun(context.Background(), created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if run.CreatedBy != svcPrin || run.OperatorOwned {
			t.Errorf("run CreatedBy=%q OperatorOwned=%v, want %q and false", run.CreatedBy, run.OperatorOwned, svcPrin)
		}
		call := e.ids.last()
		claims, err := e.h.srv.cfg.Identity.Verify(context.Background(), call.token, internalAudience)
		if err != nil {
			t.Fatal(err)
		}
		if claims.Sub != svcPrin || claims.Sponsor != svcSponsor || claims.OperatorOwned {
			t.Errorf("claims sub=%q sponsor=%q operator_owned=%v, want %q, %q, false", claims.Sub, claims.Sponsor, claims.OperatorOwned, svcPrin, svcSponsor)
		}
		ev := waitForRecAudit(t, e.h.audit, created.ID, "run.create", "success")
		var data struct {
			Sponsor string `json:"sponsor"`
		}
		_ = json.Unmarshal(ev.Data, &data)
		if ev.ActorType != actor || ev.Actor != svcPrin || data.Sponsor != svcSponsor {
			t.Errorf("run.create row actor_type=%q actor=%q data.sponsor=%q, want %q, %q, %q", ev.ActorType, ev.Actor, data.Sponsor, actor, svcPrin, svcSponsor)
		}
	})
}

// TestServiceIdentity_NeverOperatorNeverSecondHuman: whatever role its context was published with, a
// service identity is neither admin tier, is not the admin-token break-glass and is never a second human.
func TestServiceIdentity_NeverOperatorNeverSecondHuman(t *testing.T) {
	bothActorForms(t, func(t *testing.T, actor types.ActorType) {
		srv := newHarness(t).srv
		ctx := withHumanIdentity(context.Background(), svcPrin, "", oidc.RoleAdmin, types.UserTypeStandard, nil, false)
		ctx = withServiceIdentity(ctx, svcName, svcSponsor)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil).WithContext(ctx)
		if srv.isOperator(ctx) || srv.isSecurityOperator(ctx) {
			t.Error("a service identity carrying the admin role is an operator")
		}
		if operatorOwnedRequest(ctx) {
			t.Error("a service request reads as operator-owned")
		}
		if isAdminTokenCaller(req) {
			t.Error("a service request reads as the admin-token break-glass")
		}
		if gotType, gotName := actorFromRequest(req); gotType != actor || gotName != svcPrin {
			t.Errorf("actor = %q/%q, want %q/%q", gotType, gotName, actor, svcPrin)
		}
		t.Setenv(envEgressSecondHuman, "1")
		ap := types.ApprovalRequest{Kind: types.ApprovalEgressDomain}
		if srv.secondHumanAllows(req, ap, types.AgentRun{CreatedBy: svcPrin}) {
			t.Error("a service identity counts as a second human on its own run")
		}
	})
}

// TestServiceIdentity_AllowListRouteWalk: every human-authenticated route the router registers answers a
// service token 403 service_scope, except the ones on the allow-list, which it reaches.
func TestServiceIdentity_AllowListRouteWalk(t *testing.T) {
	e := newSvcEnv(t)
	e.mustCreate(t, svcName, svcSponsor)
	tok := e.mustMint(t, e.sponsor, svcName)
	human := map[routeClass]bool{classAdmin: true, classSecurity: true, classMember: true, classOwner: true}
	seen := map[string]bool{}
	err := chi.Walk(e.h.srv.router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		key := method + " " + route
		rc, ok := routeMatrix[key]
		if !ok || !human[rc.class] {
			return nil
		}
		path := route
		for strings.Contains(path, "{") {
			i, j := strings.Index(path, "{"), strings.Index(path, "}")
			path = path[:i] + uuid.NewString() + path[j+1:]
		}
		w := e.as(t, tok.Token, method, path, `{}`)
		scoped := w.Code == http.StatusForbidden && respReason(w) == "service_scope"
		if serviceAllowed[key] {
			seen[key] = true
			if scoped {
				t.Errorf("%s is on the allow-list and answered service_scope", key)
			}
		} else if !scoped {
			t.Errorf("%s is off the allow-list and answered %d %s, want 403 service_scope", key, w.Code, strings.TrimSpace(w.Body.String()))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	wantAllowed := []string{
		"DELETE /api/v1/model-providers/{id}/credential", "GET /api/v1/me", "GET /api/v1/runs", "GET /api/v1/runs/{id}",
		"GET /api/v1/runs/{id}/events", "GET /api/v1/runs/{id}/output", "GET /api/v1/setup/status", "PATCH /api/v1/runs/{id}",
		"POST /api/v1/runs", "POST /api/v1/runs/preflight", "POST /api/v1/runs/{id}/kill", "PUT /api/v1/model-providers/{id}/credential",
	}
	if got := slices.Sorted(maps.Keys(serviceAllowed)); !slices.Equal(got, wantAllowed) {
		t.Errorf("the service allow-list = %v, want exactly %v", got, wantAllowed)
	}
	for key := range serviceAllowed {
		if !seen[key] {
			t.Errorf("allow-list entry %q matches no registered human route", key)
		}
	}
}

// TestServiceIdentity_CannotReachTheRetentionDrop: the audit partition drop is off the allow-list, so a
// service token is refused 403 service_scope before the handler, and nothing is dropped or audited as a drop.
func TestServiceIdentity_CannotReachTheRetentionDrop(t *testing.T) {
	bothActorForms(t, func(t *testing.T, actor types.ActorType) {
		e := newSvcEnv(t)
		e.mustCreate(t, svcName, svcSponsor)
		tok := e.mustMint(t, e.sponsor, svcName)
		w := e.as(t, tok.Token, http.MethodPost, "/api/v1/audit/retention/drop", `{"partition":"audit_events_2020_01","digest":"x"}`)
		if w.Code != http.StatusForbidden || respReason(w) != "service_scope" {
			t.Fatalf("service retention drop: %d %s, want 403 service_scope", w.Code, w.Body.String())
		}
		for _, ev := range e.h.audit.snapshot() {
			if strings.HasPrefix(ev.Action, "audit.retention") {
				t.Errorf("a retention row %q was written for a refused service request", ev.Action)
			}
			if ev.Action == "authz.denied" && ev.Actor == svcPrin && ev.ActorType != actor {
				t.Errorf("authz.denied row actor_type = %q, want %q", ev.ActorType, actor)
			}
		}
	})
}

// TestServiceIdentity_PrefixReservedAtEveryDoor: the service: prefix is a reserved principal at the sign-in
// predicate, a session cookie, POST /people, an invitation target, a portal token and a wdn_ token with no
// identity behind it.
func TestServiceIdentity_PrefixReservedAtEveryDoor(t *testing.T) {
	e := newSvcEnv(t)
	for _, p := range []string{"service:x", " SERVICE:x", "Service:Ci-Bot"} {
		if !e.h.srv.isReservedPrincipal(p) {
			t.Errorf("isReservedPrincipal(%q) = false", p)
		}
	}
	if w := e.createPerson(t, e.sec, "service:ghost", "ghost@corp.example"); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("POST /people for a service principal: %d %s, want 422", w.Code, w.Body.String())
	}
	if w := e.inviteAs(t, e.sec, "service:ghost", `{}`); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("invitation for a service principal: %d, want 422", w.Code)
	}
	if w := doSSO(t, e.h.srv, http.MethodGet, "/api/v1/me", accessSession(t, svcPrin, "x@corp.example", oidc.RoleUser, nil), ""); w.Code != http.StatusUnauthorized {
		t.Errorf("a session cookie for a service principal: %d, want 401", w.Code)
	}
	portal := inviteEnv{peoplePG: e.peoplePG}.seedPortalToken(t, svcPrin)
	if w := e.as(t, portal, http.MethodGet, "/api/v1/me", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("a portal token for a service principal: %d, want 401", w.Code)
	}
	ghost := e.seedLegacyToken(t, svcPrin, oidc.RoleUser, "")
	if w := e.as(t, ghost.Token, http.MethodGet, "/api/v1/me", ""); w.Code != http.StatusUnauthorized || e.lastAuthFail() != authFailedServiceIdentityInactive {
		t.Errorf("a wdn_ token for a service with no identity: %d (auth.fail %q), want 401 %q", w.Code, e.lastAuthFail(), authFailedServiceIdentityInactive)
	}
	if w := e.create(t, e.sec, "bad-sponsor", "service:other"); w.Code != http.StatusUnprocessableEntity || respReason(w) != "service_identity_sponsor_invalid" {
		t.Errorf("a service as sponsor: %d %s, want 422 service_identity_sponsor_invalid", w.Code, w.Body.String())
	}
}

// TestServiceIdentity_SponsorInactiveRefusesItsTokens: suspending the identity or deactivating its sponsor
// ends its tokens at once, each with its own auth.fail reason, and resuming brings them back.
func TestServiceIdentity_SponsorInactiveRefusesItsTokens(t *testing.T) {
	e := newSvcEnv(t)
	e.mustCreate(t, svcName, svcSponsor)
	tok := e.mustMint(t, e.sponsor, svcName)
	get := func() int { return e.as(t, tok.Token, http.MethodGet, "/api/v1/me", "").Code }
	if got := get(); got != http.StatusOK {
		t.Fatalf("service GET /me: %d, want 200", got)
	}
	patch := func(body string) {
		t.Helper()
		if w := doSSO(t, e.h.srv, http.MethodPatch, "/api/v1/service-identities/"+svcName, e.sec, body); w.Code != http.StatusOK {
			t.Fatalf("PATCH %s: %d %s", body, w.Code, w.Body.String())
		}
	}
	patch(`{"suspended":true}`)
	if got := get(); got != http.StatusUnauthorized || e.lastAuthFail() != authFailedServiceIdentityInactive {
		t.Errorf("suspended identity: %d (auth.fail %q), want 401 %q", got, e.lastAuthFail(), authFailedServiceIdentityInactive)
	}
	if w := e.mintReq(t, e.sponsor, svcName, `{}`); w.Code != http.StatusConflict || respReason(w) != "service_identity_inactive" {
		t.Errorf("sponsor mint on a suspended identity: %d %s, want 409 service_identity_inactive", w.Code, w.Body.String())
	}
	patch(`{"suspended":false}`)
	if got := get(); got != http.StatusOK {
		t.Errorf("resumed identity: %d, want 200", got)
	}
	if _, err := e.pool.Exec(context.Background(), `UPDATE principal_identities SET deactivated_at = now() WHERE principal = $1`, svcSponsor); err != nil {
		t.Fatal(err)
	}
	// The failure arm reports sponsor_inactive; the record says so too.
	e.h.audit.mu.Lock()
	e.h.audit.events = nil
	e.h.audit.mu.Unlock()
	if got := get(); got != http.StatusUnauthorized || e.lastAuthFail() != authFailedServiceSponsorInactive {
		t.Errorf("deactivated sponsor: %d (auth.fail %q), want 401 %q", got, e.lastAuthFail(), authFailedServiceSponsorInactive)
	}
	w := doSSO(t, e.h.srv, http.MethodGet, "/api/v1/service-identities/"+svcName, e.sec, "")
	var si types.ServiceIdentity
	_ = json.Unmarshal(w.Body.Bytes(), &si)
	if si.State != types.ServiceIdentitySponsorInactive {
		t.Errorf("state with a deactivated sponsor = %q, want sponsor_inactive", si.State)
	}
	// Live runs continue (D6): nothing here killed anything; reactivating restores the token.
	if _, err := e.pool.Exec(context.Background(), `UPDATE principal_identities SET deactivated_at = NULL WHERE principal = $1`, svcSponsor); err != nil {
		t.Fatal(err)
	}
	if got := get(); got != http.StatusOK {
		t.Errorf("reactivated sponsor: %d, want 200", got)
	}
}

// TestServiceIdentity_EntraSponsorSuspensionIsSeen: a sponsor bound under a pairwise sub through their
// tenant and object ids is found by the deactivation an identity provider's suspension writes against those
// ids, and the entra: alias form is refused as a sponsor.
func TestServiceIdentity_EntraSponsorSuspensionIsSeen(t *testing.T) {
	e := newSvcEnv(t)
	const pairwise, tid, oid = "pairwise-sub-1", "11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222"
	e.bind(t, pairwise, "https://login.example/"+tid+"/v2.0", tid, oid)
	if w := e.create(t, e.sec, svcName, "entra:"+tid+":"+oid); w.Code != http.StatusUnprocessableEntity || respReason(w) != "service_identity_sponsor_invalid" {
		t.Fatalf("entra alias as sponsor: %d %s, want 422 service_identity_sponsor_invalid", w.Code, w.Body.String())
	}
	e.mustCreate(t, svcName, pairwise)
	tok := e.mustMint(t, accessSession(t, pairwise, "p@corp.example", oidc.RoleUser, []string{}), svcName)
	if w := e.as(t, tok.Token, http.MethodGet, "/api/v1/me", ""); w.Code != http.StatusOK {
		t.Fatalf("token before the suspension: %d", w.Code)
	}
	if _, err := e.pool.Exec(context.Background(),
		`UPDATE principal_identities SET deactivated_at = now(), authority_epoch = authority_epoch + 1 WHERE tenant_id = $1 AND object_id = $2`, tid, oid); err != nil {
		t.Fatal(err)
	}
	if w := e.as(t, tok.Token, http.MethodGet, "/api/v1/me", ""); w.Code != http.StatusUnauthorized || e.lastAuthFail() != authFailedServiceSponsorInactive {
		t.Errorf("token after the sponsor's suspension: %d (auth.fail %q), want 401 %q", w.Code, e.lastAuthFail(), authFailedServiceSponsorInactive)
	}
}

// TestServiceIdentity_SponsorChangeRevokesTokens: a sponsor change revokes the live tokens in one
// transaction and sets the cutoff, so a token whose mint committed after the revoke's snapshot (its
// created_at before the change) still never authenticates; only the new sponsor mints afterwards.
func TestServiceIdentity_SponsorChangeRevokesTokens(t *testing.T) {
	e := newSvcEnv(t)
	e.mustCreate(t, svcName, svcSponsor)
	first := e.mustMint(t, e.sponsor, svcName)
	beforeChange := time.Now().UTC()
	time.Sleep(20 * time.Millisecond)
	w := doSSO(t, e.h.srv, http.MethodPatch, "/api/v1/service-identities/"+svcName, e.sec, `{"sponsor":"`+svcSponsor2+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("sponsor change: %d %s", w.Code, w.Body.String())
	}
	if got := e.as(t, first.Token, http.MethodGet, "/api/v1/me", "").Code; got != http.StatusUnauthorized {
		t.Errorf("the previous sponsor's token after the change: %d, want 401", got)
	}
	if toks, err := e.st.ListAPITokensByPrincipal(context.Background(), svcPrin); err != nil || len(toks) != 1 || toks[0].RevokedAt == nil {
		t.Errorf("the identity's tokens after the change = %+v (%v), want the one token revoked in the store", toks, err)
	}
	rows := e.auditRows("service_identity.update")
	if len(rows) != 1 || !strings.Contains(string(rows[0].Data), `"tokens_revoked":1`) ||
		!strings.Contains(string(rows[0].Data), `"sponsor_from":"`+svcSponsor+`"`) || !strings.Contains(string(rows[0].Data), `"sponsor_to":"`+svcSponsor2+`"`) {
		t.Errorf("service_identity.update rows = %+v, want one naming both sponsors and 1 token revoked", rows)
	}
	// The straggler: a mint whose authorization preceded the change and whose row lands after it.
	late := "wdn_" + uuid.NewString()
	f := false
	if _, err := e.st.CreateAPIToken(context.Background(), types.APIToken{
		ID: uuid.New(), Principal: svcPrin, Role: oidc.RoleUser, UserType: types.UserTypeStandard, Groups: []string{},
		GroupsTruncated: &f, Name: "late", CreatedAt: beforeChange, ExpiresAt: ptrTime(time.Now().Add(time.Hour)),
	}, late); err != nil {
		t.Fatal(err)
	}
	if got := e.as(t, late, http.MethodGet, "/api/v1/me", "").Code; got != http.StatusUnauthorized || e.lastAuthFail() != authFailedServiceIdentityInactive {
		t.Errorf("a token created before the sponsor change: %d (auth.fail %q), want 401 %q", got, e.lastAuthFail(), authFailedServiceIdentityInactive)
	}
	if w := e.mintReq(t, e.sponsor, svcName, `{}`); w.Code != http.StatusNotFound {
		t.Errorf("the previous sponsor's mint: %d, want 404", w.Code)
	}
	if _, err := e.st.CreateServiceToken(context.Background(), svcName, svcSponsor, apiTokenMaxPerPrincipal, types.APIToken{ID: uuid.New(), Principal: svcPrin, CreatedAt: time.Now().UTC()}, "wdn_x"); err == nil {
		t.Error("a store-level mint under the previous sponsor succeeded")
	}
	second := e.mustMint(t, e.sponsor2, svcName)
	if got := e.as(t, second.Token, http.MethodGet, "/api/v1/me", "").Code; got != http.StatusOK {
		t.Errorf("the new sponsor's token: %d, want 200", got)
	}
}

// TestServiceIdentity_MintCallerTable walks the sponsor-mint caller table of the service page.
func TestServiceIdentity_MintCallerTable(t *testing.T) {
	e := newSvcEnv(t)
	e.mustCreate(t, svcName, svcSponsor)
	body := `{"ttl_seconds":3600}`
	check := func(label string, w *httptest.ResponseRecorder, code int, reason string) {
		t.Helper()
		if w.Code != code || (reason != "" && respReason(w) != reason) {
			t.Errorf("%s: %d %s, want %d %s", label, w.Code, strings.TrimSpace(w.Body.String()), code, reason)
		}
	}
	other := e.mintReq(t, e.other, svcName, body)
	missing := e.mintReq(t, e.sponsor, "no-such-name", body)
	check("another person", other, http.StatusNotFound, "service_identity_not_found")
	if other.Body.String() != missing.Body.String() {
		t.Errorf("another person's 404 body %q differs from a missing name's %q", other.Body.String(), missing.Body.String())
	}
	check("a super admin who is not the sponsor", e.mintReq(t, e.super, svcName, body), http.StatusNotFound, "service_identity_not_found")
	check("the admin token", do(t, e.h.srv, http.MethodPost, "/api/v1/me/service-identities/"+svcName+"/tokens", adminToken, body), http.StatusForbidden, "api_token_no_human")
	pat := e.seedLegacyToken(t, svcSponsor, oidc.RoleUser, "")
	check("a wdn_ token, the sponsor's", e.mintAs(t, pat.Token, body), http.StatusForbidden, "api_token_from_api_token")
	portal := inviteEnv{peoplePG: e.peoplePG}.seedPortalToken(t, svcSponsor)
	check("a portal wdg_ token", e.mintAs(t, portal, body), http.StatusForbidden, "delegation_scope")
	svcTok := e.mustMint(t, e.sponsor, svcName)
	check("a service token", e.mintAs(t, svcTok.Token, body), http.StatusForbidden, "service_scope")
	check("negative expiry", e.mintReq(t, e.sponsor, svcName, `{"ttl_seconds":-1}`), http.StatusBadRequest, "api_token_ttl_invalid")
	// An omitted expiry is the 90-day default, and a longer one is clamped to it.
	for _, b := range []string{`{}`, `{"ttl_seconds":999999999}`} {
		w := e.mintReq(t, e.sponsor, svcName, b)
		var tok types.APIToken
		_ = json.Unmarshal(w.Body.Bytes(), &tok)
		if w.Code != http.StatusCreated || tok.ExpiresAt == nil || tok.ExpiresAt.Sub(tok.CreatedAt) != serviceTokenMaxTTL {
			t.Errorf("mint %s: %d expires_at=%v, want 201 and a 90-day expiry", b, w.Code, tok.ExpiresAt)
		}
	}
	for i := 0; i < apiTokenMaxPerPrincipal; i++ {
		if w := e.mintReq(t, e.sponsor, svcName, body); w.Code != http.StatusCreated && w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("mint %d: %d", i, w.Code)
		}
	}
	check("the 21st live token", e.mintReq(t, e.sponsor, svcName, body), http.StatusUnprocessableEntity, "api_token_cap_reached")
	rows := e.auditRows("service_identity.token.create")
	if len(rows) == 0 || rows[0].Actor != svcSponsor || rows[0].ActorType != types.ActorHuman {
		t.Errorf("service_identity.token.create rows = %+v, want the sponsor as a human actor", rows)
	}
	for _, r := range rows {
		if strings.Contains(string(r.Data), "wdn_") {
			t.Errorf("a token row carries a bearer: %s", r.Data)
		}
	}
}

func (e svcEnv) inviteAs(t *testing.T, as *http.Cookie, principal, body string) *httptest.ResponseRecorder {
	t.Helper()
	return inviteEnv{peoplePG: e.peoplePG}.invite(t, as, principal, body)
}

func (e svcEnv) mintAs(t *testing.T, bearer, body string) *httptest.ResponseRecorder {
	t.Helper()
	return e.as(t, bearer, http.MethodPost, "/api/v1/me/service-identities/"+svcName+"/tokens", body)
}

// TestIdentityUnbind_RefusesASponsorOrInvitee: an identity cannot be unbound while its principal sponsors a
// non-retired service identity or is the target of a pending invitation.
func TestIdentityUnbind_RefusesASponsorOrInvitee(t *testing.T) {
	e := newSvcEnv(t)
	const tid = "11111111-1111-1111-1111-111111111111"
	const sponsorOID, inviteeOID = "22222222-2222-2222-2222-222222222221", "22222222-2222-2222-2222-222222222222"
	e.bind(t, "pairwise-sponsor", "https://login.example/v2.0", tid, sponsorOID)
	e.bind(t, "pairwise-invitee", "https://login.example/v2.0", tid, inviteeOID)
	identityID := func(principal string) uuid.UUID {
		rows, err := e.st.IdentitiesByPrincipal(context.Background(), principal)
		if err != nil || len(rows) != 1 {
			t.Fatalf("identity rows for %s = %v (%v)", principal, rows, err)
		}
		return rows[0].ID
	}
	unbind := func(principal string) *httptest.ResponseRecorder {
		return doSSO(t, e.h.srv, http.MethodPost, "/api/v1/admin/identities/"+identityID(principal).String()+"/unbind", e.super, "")
	}
	check := func(label string, w *httptest.ResponseRecorder, code int) {
		t.Helper()
		if w.Code != code || (code == http.StatusConflict && respReason(w) != "identity_principal_in_use") {
			t.Errorf("%s: %d %s, want %d", label, w.Code, strings.TrimSpace(w.Body.String()), code)
		}
	}
	e.mustCreate(t, svcName, "pairwise-sponsor")
	check("a sponsor", unbind("pairwise-sponsor"), http.StatusConflict)
	if w := doSSO(t, e.h.srv, http.MethodDelete, "/api/v1/service-identities/"+svcName, e.sec, ""); w.Code != http.StatusOK {
		t.Fatalf("retire: %d %s", w.Code, w.Body.String())
	}
	check("a sponsor of a retired identity", unbind("pairwise-sponsor"), http.StatusOK)

	if w := e.createPerson(t, e.sec, "pairwise-invitee", "invitee@corp.example"); w.Code != http.StatusCreated {
		t.Fatalf("create invitee: %d %s", w.Code, w.Body.String())
	}
	w := e.inviteAs(t, e.sec, "pairwise-invitee", `{}`)
	var inv types.Invitation
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &inv) != nil {
		t.Fatalf("invite: %d %s", w.Code, w.Body.String())
	}
	check("an invitation target", unbind("pairwise-invitee"), http.StatusConflict)
	if w := doSSO(t, e.h.srv, http.MethodDelete, "/api/v1/invitations/"+inv.ID.String(), e.sec, ""); w.Code != http.StatusNoContent {
		t.Fatalf("revoke invitation: %d", w.Code)
	}
	check("a revoked invitation's target", unbind("pairwise-invitee"), http.StatusOK)
}

// TestServiceIdentity_RetireErasesItsCredentials: retiring revokes the tokens and erases the identity's stored
// credentials, with a credential.erase row beside service_identity.retire; the name is never reused.
func TestServiceIdentity_RetireErasesItsCredentials(t *testing.T) {
	e := newSvcEnv(t)
	e.mustCreate(t, svcName, svcSponsor)
	tok := e.mustMint(t, e.sponsor, svcName)
	ctx := context.Background()
	own := e.ownerOnlyPG.sec.For(svcPrin)
	if err := own.Put(ctx, "model-key", []byte("sk-service")); err != nil {
		t.Fatal(err)
	}
	// The secret store fails during the retire: the identity is retired and its tokens revoked, the erase
	// is answered 500 with a failure row, and the same DELETE run again finishes the erase.
	fail := &atomic.Bool{}
	fail.Store(true)
	e.h.srv.cfg.Secrets = failingErase{Store: e.h.srv.cfg.Secrets, fail: fail}
	del := func() *httptest.ResponseRecorder {
		return doSSO(t, e.h.srv, http.MethodDelete, "/api/v1/service-identities/"+svcName, e.sec, "")
	}
	if w := del(); w.Code != http.StatusInternalServerError {
		t.Fatalf("retire with a failing secret store: %d %s, want 500", w.Code, w.Body.String())
	}
	if names, _ := own.List(ctx); len(names) != 1 {
		t.Fatalf("credentials after the failed erase = %v, want the one left", names)
	}
	if got := e.as(t, tok.Token, http.MethodGet, "/api/v1/me", "").Code; got != http.StatusUnauthorized {
		t.Errorf("token after the failed erase: %d, want 401 (the retire itself is durable)", got)
	}
	fail.Store(false)
	if w := del(); w.Code != http.StatusOK {
		t.Fatalf("second DELETE on a retired identity: %d %s, want 200 with the erase finished", w.Code, w.Body.String())
	}
	if names, err := own.List(ctx); err != nil || len(names) != 0 {
		t.Errorf("credentials left after the retire = %v (%v), want none", names, err)
	}
	if got := e.as(t, tok.Token, http.MethodGet, "/api/v1/me", "").Code; got != http.StatusUnauthorized {
		t.Errorf("token after the retire: %d, want 401", got)
	}
	retire, erase := e.auditRows("service_identity.retire"), e.auditRows("credential.erase")
	if len(retire) != 1 || retire[0].Target != svcPrin || len(erase) != 2 ||
		erase[0].Outcome != "failure" || erase[1].Outcome != "success" || erase[1].Target != svcPrin {
		t.Errorf("retire rows = %+v, erase rows = %+v, want one retire and a failed then a successful erase naming %s", retire, erase, svcPrin)
	}
	if w := e.create(t, e.sec, svcName, svcSponsor); w.Code != http.StatusConflict || respReason(w) != "service_identity_name_taken" {
		t.Errorf("re-creating a retired name: %d %s, want 409 service_identity_name_taken", w.Code, w.Body.String())
	}
	if w := doSSO(t, e.h.srv, http.MethodPatch, "/api/v1/service-identities/"+svcName, e.sec, `{"description":"x"}`); w.Code != http.StatusConflict {
		t.Errorf("PATCH on a retired identity: %d, want 409", w.Code)
	}
}

// TestServiceIdentity_NameTakenAcrossTables: a name that a principal-keyed table already holds is refused,
// so a grant or secret written early is never inherited.
func TestServiceIdentity_NameTakenAcrossTables(t *testing.T) {
	e := newSvcEnv(t)
	if err := e.ownerOnlyPG.sec.For("service:held").Put(context.Background(), "k", []byte("v")); err != nil {
		t.Fatal(err)
	}
	if w := e.create(t, e.sec, "held", svcSponsor); w.Code != http.StatusConflict || respReason(w) != "service_identity_name_taken" {
		t.Errorf("a name a secret owner holds: %d %s, want 409 service_identity_name_taken", w.Code, w.Body.String())
	}
	for _, bad := range []string{"Ab", "ab", "1abc", "has_underscore", strings.Repeat("a", 64)} {
		if w := e.create(t, e.sec, bad, svcSponsor); w.Code != http.StatusUnprocessableEntity || respReason(w) != "service_identity_name_invalid" {
			t.Errorf("name %q: %d %s, want 422 service_identity_name_invalid", bad, w.Code, w.Body.String())
		}
	}
	e.mustCreate(t, svcName, svcSponsor)
	if w := e.create(t, e.sec, svcName, svcSponsor); w.Code != http.StatusConflict {
		t.Errorf("a duplicate name: %d, want 409", w.Code)
	}
	if w := e.create(t, e.mem, "member-made", svcSponsor); w.Code != http.StatusForbidden {
		t.Errorf("a member creating one: %d, want 403", w.Code)
	}
	if w := e.create(t, e.sec, "unbound-sponsor", "nobody-sub"); w.Code != http.StatusUnprocessableEntity || respReason(w) != "service_identity_sponsor_invalid" {
		t.Errorf("an unbound sponsor: %d %s, want 422 service_identity_sponsor_invalid", w.Code, w.Body.String())
	}
}

// TestServiceIdentity_HiddenFromPeopleAndMintedInventory: a service identity is no person and no
// admin-minted token: GET /people and the minted-for-others inventory do not list it.
func TestServiceIdentity_HiddenFromPeopleAndMintedInventory(t *testing.T) {
	e := newSvcEnv(t)
	e.mustCreate(t, svcName, svcSponsor)
	e.mustMint(t, e.sponsor, svcName)
	for _, path := range []string{"/api/v1/people", "/api/v1/tokens?minted_for_others=true"} {
		w := doSSO(t, e.h.srv, http.MethodGet, path, e.sec, "")
		if w.Code != http.StatusOK || strings.Contains(w.Body.String(), svcPrin) {
			t.Errorf("GET %s: %d, lists the service principal: %s", path, w.Code, w.Body.String())
		}
	}
	mine := doSSO(t, e.h.srv, http.MethodGet, "/api/v1/me/service-identities", e.sponsor, "")
	theirs := doSSO(t, e.h.srv, http.MethodGet, "/api/v1/me/service-identities", e.sponsor2, "")
	if !strings.Contains(mine.Body.String(), svcName) || strings.Contains(theirs.Body.String(), svcName) {
		t.Errorf("GET /me/service-identities: sponsor %s / other %s, want only the sponsor to see it", mine.Body.String(), theirs.Body.String())
	}
}

// TestServiceIdentity_RateLimitedLikeAPerson: the preflight limiter counts a service identity against its
// own bucket, as it does a person, under both actor forms.
func TestServiceIdentity_RateLimitedLikeAPerson(t *testing.T) {
	bothActorForms(t, func(t *testing.T, actor types.ActorType) {
		e := newSvcEnv(t)
		e.h.srv.preflightLimiter = &principalLimiter{rate: 1.0 / 60, burst: preflightBurst, max: 100}
		e.mustCreate(t, svcName, svcSponsor)
		tok := e.mustMint(t, e.sponsor, svcName)
		limited := 0
		var codes []int
		for i := 0; i < preflightBurst+4; i++ {
			w := e.as(t, tok.Token, http.MethodPost, "/api/v1/runs/preflight", `{"agent":"claude-code","task":"t"}`)
			codes = append(codes, w.Code)
			if w.Code == http.StatusTooManyRequests && respReason(w) == reasonPreflightRateLimited {
				limited++
			}
		}
		if limited == 0 {
			t.Errorf("%d preflights from one service identity were never rate limited (codes %v)", preflightBurst+4, codes)
		}
	})
}

// TestServiceIdentity_LocalModeRefusesAndRunsStillLaunch: local mode has no person to sponsor a service, so the
// family answers 409 needs_sso, and a run there is still launched, operator-owned.
func TestServiceIdentity_LocalModeRefusesAndRunsStillLaunch(t *testing.T) {
	e := newSvcEnv(t)
	cfg := e.h.srv.cfg
	cfg.OIDC, cfg.LocalMode, cfg.LocalOperator, cfg.LocalLoopback = nil, true, "local:test", true
	srv := New(cfg)
	t.Cleanup(srv.WaitBackground)
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/service-identities", `{"name":"` + svcName + `","sponsor":"` + svcSponsor + `"}`},
		{http.MethodGet, "/api/v1/service-identities", ""},
		{http.MethodGet, "/api/v1/me/service-identities", ""},
	} {
		if w := do(t, srv, c.method, c.path, "", c.body); w.Code != http.StatusConflict || respReason(w) != "needs_sso" {
			t.Errorf("local mode %s %s: %d %s, want 409 needs_sso", c.method, c.path, w.Code, w.Body.String())
		}
	}
	if w := do(t, srv, http.MethodPost, "/api/v1/me/service-identities/"+svcName+"/tokens", "", `{}`); w.Code != http.StatusForbidden || respReason(w) != "api_token_no_human" {
		t.Errorf("local mode sponsor mint: %d %s, want 403 api_token_no_human", w.Code, w.Body.String())
	}
	pol := e.plainPolicy(t, "local-pol")
	w := do(t, srv, http.MethodPost, "/api/v1/runs", "", `{"agent":"claude-code","task":"t","policy_id":"`+pol+`"}`)
	var created createRunResponse
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &created) != nil {
		t.Fatalf("local mode run create: %d %s, want 201", w.Code, w.Body.String())
	}
	if run, err := e.st.GetRun(context.Background(), created.ID); err != nil || !run.OperatorOwned {
		t.Errorf("local run OperatorOwned = %v (%v), want true", run.OperatorOwned, err)
	}
}

// TestServiceIdentity_PatchAndListShapes covers the admin surface's own refusals.
func TestServiceIdentity_PatchAndListShapes(t *testing.T) {
	e := newSvcEnv(t)
	e.mustCreate(t, svcName, svcSponsor)
	patch := func(body string) *httptest.ResponseRecorder {
		return doSSO(t, e.h.srv, http.MethodPatch, "/api/v1/service-identities/"+svcName, e.sec, body)
	}
	if w := patch(`{}`); w.Code != http.StatusBadRequest || respReason(w) != "service_identity_patch_empty" {
		t.Errorf("empty PATCH: %d %s", w.Code, w.Body.String())
	}
	if w := patch(`{"sponsor":"nobody-sub"}`); w.Code != http.StatusUnprocessableEntity || respReason(w) != "service_identity_sponsor_invalid" {
		t.Errorf("unbound sponsor on PATCH: %d %s", w.Code, w.Body.String())
	}
	if w := patch(`{"description":"` + strings.Repeat("x", 201) + `"}`); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("long description: %d", w.Code)
	}
	if w := doSSO(t, e.h.srv, http.MethodGet, "/api/v1/service-identities/missing-name", e.sec, ""); w.Code != http.StatusNotFound {
		t.Errorf("GET missing: %d", w.Code)
	}
	if w := doSSO(t, e.h.srv, http.MethodGet, "/api/v1/service-identities", e.sec, ""); !strings.Contains(w.Body.String(), svcName) {
		t.Errorf("list: %s", w.Body.String())
	}
}
