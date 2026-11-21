// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/audit/sinks"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/test/entrafake"
)

// The reverse check, the old entry points and the hard refusals for issuer-portal grants, in memory.

// portalGrantFor seeds a live portal grant of principal under portal d and returns its bearer and id.
func (h *exchHarness) portalGrantFor(d types.Delegate, principal string) (string, uuid.UUID) {
	id := uuid.New()
	return h.ps.seedGrant(types.PortalGrant{ID: id, DelegateID: d.ID, Principal: principal, UserType: types.UserTypeStandard}), id
}

func TestDelegatedLane_WdgFromEitherTable(t *testing.T) {
	h := newExchHarness(t)
	d, _ := h.seedPortal(nil)
	inPortal, _ := h.portalGrantFor(d, oidc.PortalPrincipal(portalTestIssuer, "u1"))
	inLegacy, _ := seedDelegation(t, h.ps.fakeDelegateStore, "console-person")
	for name, c := range map[string]struct {
		tok  string
		want int
	}{
		"portal_grants hit":    {inPortal, http.StatusOK},
		"delegated_tokens hit": {inLegacy, http.StatusOK},
		"miss in both":         {delegatedTokenPrefix + "nothing", http.StatusUnauthorized},
	} {
		w := do(t, h.srv, http.MethodGet, "/api/v1/me", c.tok, "")
		if w.Code != c.want {
			t.Errorf("%s: GET /me = %d %s, want %d", name, w.Code, w.Body.String(), c.want)
		}
	}
	// The portal grant's person is the portal person at the user role; the legacy one is the console person.
	w := do(t, h.srv, http.MethodGet, "/api/v1/me", inPortal, "")
	if !strings.Contains(w.Body.String(), oidc.PortalPrincipal(portalTestIssuer, "u1")) || strings.Contains(w.Body.String(), `"role":"admin"`) {
		t.Errorf("GET /me with a portal grant = %s", w.Body.String())
	}
	// A route off the allow-list is refused for a portal grant exactly as for a legacy one.
	for name, tok := range map[string]string{"portal": inPortal, "legacy": inLegacy} {
		if w := do(t, h.srv, http.MethodPost, "/api/v1/me/tokens", tok, `{"name":"x"}`); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "delegation_scope") {
			t.Errorf("%s grant at POST /me/tokens = %d %s, want 403 delegation_scope", name, w.Code, w.Body.String())
		}
	}
}

func TestPortalGrant_PrincipalOutsidePortalKeyRefused(t *testing.T) {
	h := newExchHarness(t)
	d, _ := h.seedPortal(nil)
	other := oidc.PortalPrincipal("https://other.example/oauth2/default", "u1")
	own := oidc.PortalPrincipal(portalTestIssuer, "u1")
	for name, principal := range map[string]string{"a console sub": "console-sub-1", "another portal's person": other, "empty": "", "the key alone": types.PortalPersonPrefix + d.IssuerKey + ":"} {
		tok, id := h.portalGrantFor(d, principal)
		via := types.DelegationVia{Delegate: d.ID, Grant: id}
		// The lane: the single 401, not an identity.
		if w := do(t, h.srv, http.MethodGet, "/api/v1/me", tok, ""); w.Code != http.StatusUnauthorized {
			t.Errorf("%s: the lane answered %d, want the single 401", name, w.Code)
		}
		// delegationLive and ticketAuthority: ended.
		if _, v := h.srv.delegationLive(context.Background(), via); v != verdictDelegationEnded {
			t.Errorf("%s: delegationLive = %v, want ended", name, v)
		}
		ta := ticketActor{principal: principal, authorizedAt: time.Now(), via: &via}
		if _, v := h.srv.ticketAuthority(context.Background(), ta, uuid.New()); v != verdictDelegationEnded {
			t.Errorf("%s: ticketAuthority = %v, want ended", name, v)
		}
	}
	// Control: the portal's own person passes all three.
	tok, id := h.portalGrantFor(d, own)
	via := types.DelegationVia{Delegate: d.ID, Grant: id}
	if w := do(t, h.srv, http.MethodGet, "/api/v1/me", tok, ""); w.Code != http.StatusOK {
		t.Errorf("control: the lane answered %d", w.Code)
	}
	if _, v := h.srv.delegationLive(context.Background(), via); v != verdictOK {
		t.Errorf("control: delegationLive = %v", v)
	}
	// A grant under another portal's id has ended, whoever its person is.
	if _, v := h.srv.delegationLive(context.Background(), types.DelegationVia{Delegate: uuid.New(), Grant: id}); v != verdictDelegationEnded {
		t.Errorf("a grant under another portal: delegationLive = %v, want ended", v)
	}
}

func TestOldEntryPoints_RefuseIssuerPortalArtifacts(t *testing.T) {
	h := newExchHarness(t)
	d, cred := h.seedPortal(nil)
	tok, id := h.portalGrantFor(d, oidc.PortalPrincipal(portalTestIssuer, "u1"))
	// The 0.10.1 view of the same state: a store with no portal tables, which is what the unchanged entry
	// points read. They know the legacy tables only.
	old, _, _, _ := newAuthzMatrixServer(t)
	if w := do(t, old, http.MethodGet, "/api/v1/me", tok, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("the old wdg_ lane with a portal_grants token = %d, want the single 401", w.Code)
	}
	if _, v := old.delegationLive(context.Background(), types.DelegationVia{Delegate: d.ID, Grant: id}); v == verdictOK {
		t.Error("the old delegationLive admitted a portal grant")
	}
	ta := ticketActor{principal: oidc.PortalPrincipal(portalTestIssuer, "u1"), authorizedAt: time.Now(), via: &types.DelegationVia{Delegate: d.ID, Grant: id}}
	if _, v := old.ticketAuthority(context.Background(), ta, uuid.New()); v == verdictOK {
		t.Error("the old ticketAuthority admitted a portal grant")
	}
	// A wdi_ credential at an exchange the old store serves is no portal there.
	form := url.Values{"grant_type": {grantTypeTokenExchange}, "subject_token": {"x"}, "subject_token_type": {tokenTypeAccessToken}}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/token", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.SetBasicAuth(d.ID.String(), cred)
	w := httptest.NewRecorder()
	old.Handler().ServeHTTP(w, r)
	if w.Code == http.StatusOK {
		t.Errorf("a wdi_ credential exchanged at the old view: %d %s", w.Code, w.Body.String())
	}
	// And the legacy lookups themselves never answer for the portal's artifacts.
	if _, err := h.ps.fakeDelegateStore.GetDelegatedTokenByRaw(context.Background(), tok, time.Now()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the legacy token lookup of a portal grant = %v, want not found", err)
	}
	if _, err := h.ps.fakeDelegateStore.GetDelegateByRaw(context.Background(), cred); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the legacy credential lookup of a wdi_ secret = %v, want not found", err)
	}
}

func TestPortalExchange_ProcessConsoleIssuerConflict503(t *testing.T) {
	fake := entrafake.New()
	t.Cleanup(fake.Close)
	auth, err := oidc.New(context.Background(), oidc.Config{
		IssuerURL: fake.Issuer(), ClientID: fake.ClientID(), RedirectURL: "http://console.example.invalid/auth/callback", DefaultRole: oidc.RoleUser,
	}, testLoginHMACKey)
	if err != nil {
		t.Fatal(err)
	}
	h := newExchHarness(t, func(c *Config) { c.OIDC = auth })
	// A portal registered (before the console issuer was set) on the console's own issuer, spelled differently.
	d, cred := h.seedPortal(func(d *types.Delegate) {
		d.Issuer = strings.ToUpper(fake.Issuer()[:4]) + fake.Issuer()[4:] + "/"
		d.IssuerKey = oidc.PortalIssuerKey(d.Issuer)
	})
	w := h.exchange(t, d.ID, cred, "subject-token", nil)
	if w.Code != http.StatusServiceUnavailable || oauthCode(w) != "temporarily_unavailable" {
		t.Fatalf("a portal on the console's issuer: %d %s, want 503", w.Code, w.Body.String())
	}
	if h.ver.asked() != 0 || len(h.ps.grants) != 0 || len(h.ps.idents) != 0 {
		t.Fatal("the conflicting exchange verified, introduced or minted")
	}
	if rows := h.exchangeRows(); len(rows) != 1 || portalRowData(t, rows[0])["reason"] != reasonPortalIssuerConflict {
		t.Fatalf("rows = %+v, want one portal_issuer_conflict denial", rows)
	}
}

// reservedPortalNames are portal-person names in every spelling the console's reserved check must refuse.
func TestPortalPrincipal_NeverAdmittedByConsole(t *testing.T) {
	h := newExchHarness(t)
	rng := rand.New(rand.NewSource(7))
	const alpha = "abcdefghijklmnopqrstuvwxyz234567"
	pick := func(n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = alpha[rng.Intn(len(alpha))]
		}
		return string(b)
	}
	vary := func(s string) string {
		switch rng.Intn(4) {
		case 0:
			return strings.ToUpper(s)
		case 1:
			return " " + s + "\t"
		case 2:
			return strings.ToUpper(s[:3]) + s[3:]
		}
		return s
	}
	for i := 0; i < 400; i++ {
		name := vary(types.PortalPersonPrefix + pick(26) + ":" + pick(1+rng.Intn(40)))
		if !h.srv.isReservedPrincipal(name) {
			t.Fatalf("the console's reserved check let %q through", name)
		}
	}
	// A real exchange's own principal, for the real key and a real person id.
	if p := oidc.PortalPrincipal(portalTestIssuer, "00u-person-1"); !h.srv.isReservedPrincipal(p) {
		t.Fatalf("the console's reserved check let a real portal principal %q through", p)
	}
}

func TestConsoleCallback_DelegatePersonSubRefused(t *testing.T) {
	fake := entrafake.New()
	t.Cleanup(fake.Close)
	redirect := "http://console.example.invalid/auth/callback"
	fake.SetRedirectURI(redirect)
	fake.SetSubject(oidc.PortalPrincipal(portalTestIssuer, "u1"))
	auth, err := oidc.New(context.Background(), oidc.Config{IssuerURL: fake.Issuer(), ClientID: fake.ClientID(), RedirectURL: redirect, DefaultRole: oidc.RoleUser}, testLoginHMACKey)
	if err != nil {
		t.Fatal(err)
	}
	h := newExchHarness(t, func(c *Config) { c.OIDC = auth })
	handler := panicFails(t, h.srv.Handler())
	lw := httptest.NewRecorder()
	handler.ServeHTTP(lw, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	q := follow(t, lw.Header().Get("Location"))
	cb := httptest.NewRequest(http.MethodGet, "/auth/callback?"+url.Values{"state": {q.Get("state")}, "code": {q.Get("code")}}.Encode(), nil)
	for _, c := range lw.Result().Cookies() {
		cb.AddCookie(c)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, cb)
	loc, _ := url.Parse(w.Header().Get("Location"))
	if w.Code != http.StatusFound || loc == nil || loc.Query().Get("auth_error") != "sign_in_refused" || sessionIssued(w) {
		t.Fatalf("callback = %d Location %q (session issued %v), want 302 sign_in_refused and no session", w.Code, w.Header().Get("Location"), sessionIssued(w))
	}
	if got := authFailReasons(h.rec.snapshot(), oidcCallbackActor); !slices.Equal(got, []string{authFailedReservedPrincipal}) {
		t.Errorf("auth.fail reasons = %v, want [%s]", got, authFailedReservedPrincipal)
	}
}

func TestConsoleCookie_DelegatePersonForgedRefused(t *testing.T) {
	h := newExchHarness(t)
	p := oidc.PortalPrincipal(portalTestIssuer, "u1")
	// The cookie is correctly signed (ssoSession signs with the matrix server's key), so the refusal below
	// is the reserved-name check and not a bad signature.
	if w := doSSO(t, h.srv, http.MethodGet, "/api/v1/me", ssoSession(t, "control-person", "c@corp.example", oidc.RoleUser), ""); w.Code != http.StatusOK {
		t.Fatalf("control: a signed person's cookie = %d", w.Code)
	}
	for _, role := range []string{oidc.RoleUser, oidc.RoleAdmin} {
		if w := doSSO(t, h.srv, http.MethodGet, "/api/v1/me", ssoSession(t, p, "", role), ""); w.Code != http.StatusUnauthorized {
			t.Errorf("a signed cookie for %s at role %s = %d, want 401", p, role, w.Code)
		}
	}
	if got := authFailReasons(h.rec.snapshot(), adminAuthActor); len(got) != 2 || got[0] != authFailedReservedPrincipal {
		t.Errorf("auth.fail reasons = %v, want two %s rows", got, authFailedReservedPrincipal)
	}
}

func TestPortalHardRefusals_EveryDoor(t *testing.T) {
	h := newExchHarness(t)
	d, _ := h.seedPortal(nil)
	tok, _ := h.portalGrantFor(d, oidc.PortalPrincipal(portalTestIssuer, "u1"))
	// listedReason is the door's own refusal once the route is listed, where it is not delegation_scope: the
	// shared-secret routes sit behind the operator gate, which a portal person (always the user role) never passes.
	doors := []struct{ method, pattern, path, body, listedReason string }{
		{http.MethodPost, "POST /api/v1/me/tokens", "/api/v1/me/tokens", `{"name":"x"}`, ""},
		{http.MethodPost, "POST /api/v1/me/ssh-keys", "/api/v1/me/ssh-keys", `{"public_key":"ssh-ed25519 AAAA"}`, ""},
		{http.MethodPut, "PUT /api/v1/secrets/{name}", "/api/v1/secrets/k", `{"value":"v"}`, ""},
		{http.MethodPut, "PUT /api/v1/me/secrets/{name}", "/api/v1/me/secrets/k", `{"value":"v"}`, ""},
		{http.MethodDelete, "DELETE /api/v1/me/secrets/{name}", "/api/v1/me/secrets/k", ``, ""},
		{http.MethodPut, "PUT /api/v1/shared-secrets/{name}", "/api/v1/shared-secrets/k", `{"value":"v"}`, string(authz.ReasonAdminSurface)},
	}
	// Each door is asked twice: with the allow-list as it is, and with the route added to it, so the answer
	// is the door's own refusal and does not rest on the list.
	for _, listed := range []bool{false, true} {
		for _, dr := range doors {
			if listed {
				delegationAllowed[dr.pattern] = true
			}
			w := do(t, h.srv, dr.method, dr.path, tok, dr.body)
			delete(delegationAllowed, dr.pattern)
			if w.Code != http.StatusForbidden && w.Code != http.StatusNotFound {
				t.Errorf("%s (listed=%v) = %d %s, want a refusal", dr.pattern, listed, w.Code, w.Body.String())
			}
			if listed && dr.listedReason != "" {
				if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"reason":"`+dr.listedReason+`"`) {
					t.Errorf("%s (listed=%v) = %d %s, want 403 %s", dr.pattern, listed, w.Code, w.Body.String(), dr.listedReason)
				}
				continue
			}
			if w.Code == http.StatusForbidden && !strings.Contains(w.Body.String(), "delegation_scope") && !strings.Contains(w.Body.String(), reasonAPITokenFromDelegatedToken) {
				t.Errorf("%s (listed=%v) refused as %s, want delegation_scope", dr.pattern, listed, w.Body.String())
			}
		}
	}
}

func TestPortalPerson_NeverApprover(t *testing.T) {
	h := newExchHarness(t)
	d, _ := h.seedPortal(nil)
	tok, _ := h.portalGrantFor(d, oidc.PortalPrincipal(portalTestIssuer, "u1"))
	// No decision route is on the allow-list, so a portal person is never anyone's approver.
	for route := range delegationAllowed {
		for _, bad := range []string{"approv", "decide", "decision", "governance", "support", "accept"} {
			if strings.Contains(route, bad) {
				t.Errorf("the allow-list names %q, a decision or approval route", route)
			}
		}
	}
	for _, p := range []string{"/api/v1/approvals/" + uuid.NewString() + "/approve", "/api/v1/approvals/" + uuid.NewString() + "/deny"} {
		if w := do(t, h.srv, http.MethodPost, p, tok, `{}`); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "delegation_scope") {
			t.Errorf("POST %s = %d %s, want 403 delegation_scope", p, w.Code, w.Body.String())
		}
	}
}

func TestPortalPerson_SupportAcceptRefused(t *testing.T) {
	h := newExchHarness(t)
	d, _ := h.seedPortal(nil)
	tok, _ := h.portalGrantFor(d, oidc.PortalPrincipal(portalTestIssuer, "u1"))
	p := "/api/v1/runs/" + uuid.NewString() + "/support/" + uuid.NewString() + "/accept"
	if w := do(t, h.srv, http.MethodPost, p, tok, `{}`); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "delegation_scope") {
		t.Fatalf("POST %s = %d %s, want 403 delegation_scope", p, w.Code, w.Body.String())
	}
	if delegationAllowed["POST /api/v1/runs/{id}/support/{grant}/accept"] {
		t.Fatal("the support accept route is on the allow-list")
	}
}

// chainRecorder is the audit path the exchange writes through: the stored rows, the file sink's JSON line, and
// the spool's line. failOnce makes the first write of one action fail, as a crash between a commit and its audit.
type chainRecorder struct {
	rec      *recRecorder
	sink     audit.Sink
	spool    *AuditSpool
	sealer   *audit.Sealer
	failOnce string
	failed   bool
}

func (c *chainRecorder) Record(ctx context.Context, ev types.AuditEvent) error {
	if c.failOnce != "" && ev.Action == c.failOnce && !c.failed {
		c.failed = true
		return errors.New("audit write failed")
	}
	if c.sealer != nil {
		sealed, _, err := c.sealer.Seal(ctx, ev)
		if err != nil {
			return err
		}
		ev = sealed
	}
	if err := c.rec.Record(ctx, ev); err != nil {
		return err
	}
	if err := c.sink.Emit(ctx, ev); err != nil {
		return err
	}
	return c.spool.Append(ev)
}

type fixedSealKeys struct{}

func (fixedSealKeys) Current(_ context.Context, _, _ string) (uuid.UUID, []byte, error) {
	return uuid.MustParse("00000000-0000-0000-0000-00000000000a"), make([]byte, 32), nil
}

func (fixedSealKeys) Key(context.Context, string, uuid.UUID) ([]byte, error) {
	return make([]byte, 32), nil
}

// introChain wires the harness's audit through chainRecorder and returns the file paths of the sink and spool.
func introChain(t *testing.T, h *exchHarness, sealer *audit.Sealer, failOnce string) (*chainRecorder, string, string) {
	t.Helper()
	dir := t.TempDir()
	sinkPath, spoolPath := filepath.Join(dir, "sink.jsonl"), filepath.Join(dir, "spool.jsonl")
	fs, err := sinks.NewFileSink(sinks.FileConfig{Path: sinkPath})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fs.Close() })
	sp, err := NewAuditSpool(spoolPath)
	if err != nil {
		t.Fatal(err)
	}
	c := &chainRecorder{rec: h.rec, sink: fs, spool: sp, sealer: sealer, failOnce: failOnce}
	h.srv.cfg.Audit = c
	return c, sinkPath, spoolPath
}

// introductions are the person.create rows recorded so far.
func (h *exchHarness) introductions() []types.AuditEvent {
	var out []types.AuditEvent
	for _, ev := range h.rec.snapshot() {
		if ev.Action == "person.create" {
			out = append(out, ev)
		}
	}
	return out
}

func TestPortalIntroduction_NoEmailInAnyAuditForm(t *testing.T) {
	const email = "pat@corp.example"
	for mode, sealer := range map[string]*audit.Sealer{
		"off":    nil,
		"fields": {Keys: fixedSealKeys{}, Pending: func() []byte { return make([]byte, 32) }},
		"full":   {Keys: fixedSealKeys{}, Pending: func() []byte { return make([]byte, 32) }, SealActor: true},
	} {
		t.Run(mode, func(t *testing.T) {
			h := newExchHarness(t)
			_, sinkPath, spoolPath := introChain(t, h, sealer, "")
			d, cred := h.seedPortal(func(d *types.Delegate) {
				d.TokenProfile, d.Audience, d.RequiredScopes = oidc.PortalProfileIDToken, "", nil
			})
			h.ver.subj.Email, h.ver.subj.EmailVerified = email, true
			portalToken(t, h.exchange(t, d.ID, cred, "subject-token", url.Values{"subject_token_type": {tokenTypeIDToken}}))
			if len(h.introductions()) != 1 {
				t.Fatalf("person.create rows = %d, want 1", len(h.introductions()))
			}
			for name, blob := range map[string]string{"stored rows": rowsText(h.rec.snapshot()), "sink payload": readFileText(t, sinkPath), "spool line": readFileText(t, spoolPath)} {
				if strings.Contains(blob, email) || strings.Contains(blob, "pat%40") || strings.Contains(blob, `@`) {
					t.Errorf("%s (seal %s) carries the email form: %s", name, mode, blob)
				}
			}
			for _, ev := range h.introductions() {
				if _, ok := portalRowData(t, ev)["email"]; ok {
					t.Errorf("person.create carries an email field: %s", ev.Data)
				}
			}
		})
	}
}

func rowsText(evs []types.AuditEvent) string {
	var b strings.Builder
	for _, ev := range evs {
		b.WriteString(ev.Actor + "|" + ev.Target + "|" + string(ev.Data) + "\n")
	}
	return b.String()
}

func readFileText(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestPortalIntroduction_ReplayAttributesRightPortalAndIssuer is the introduction replay through the
// production emitter and the file sink's encoder: the first delivery's audit write fails; a later exchange
// through ANOTHER portal of the same issuer replays the one event, under the introducing portal, the
// real issuer, the original id and time, and the sink copy is that same event.
func TestPortalIntroduction_ReplayAttributesRightPortalAndIssuer(t *testing.T) {
	h := newExchHarness(t)
	_, sinkPath, _ := introChain(t, h, nil, "person.create")
	a, credA := h.seedPortal(nil)
	b, credB := h.seedPortal(nil)
	portalToken(t, h.exchange(t, a.ID, credA, "subject-token", nil))
	if len(h.introductions()) != 0 {
		t.Fatal("the failed audit write left a person.create row")
	}
	ident := h.ps.idents[h.ver.subj.Principal]
	if ident == nil || ident.audited {
		t.Fatalf("the introduction was marked audited after a failed write: %+v", ident)
	}
	portalToken(t, h.exchange(t, b.ID, credB, "subject-token", nil))
	rows := h.introductions()
	if len(rows) != 1 {
		t.Fatalf("person.create rows after the replay = %d, want 1", len(rows))
	}
	ev := rows[0]
	data := portalRowData(t, ev)
	if ev.ID != ident.eventID || !ev.Time.Equal(ident.eventAt) {
		t.Errorf("replayed event id/time = %v/%v, want the fixed %v/%v", ev.ID, ev.Time, ident.eventID, ident.eventAt)
	}
	if ev.Actor != delegateActor(a.ID) || ev.Target != h.ver.subj.Principal {
		t.Errorf("actor/target = %q/%q, want the introducing portal %q and the person", ev.Actor, ev.Target, delegateActor(a.ID))
	}
	if data["portal"] != a.ID.String() || data["issuer"] != portalTestIssuer || data["k"] != a.IssuerKey || data["source"] != "portal" {
		t.Errorf("person.create data = %s, want portal A, the real issuer and k", ev.Data)
	}
	if !h.ps.idents[h.ver.subj.Principal].audited {
		t.Error("the replay did not mark the introduction audited")
	}
	// The sink copy is the same event: decodes to the same id, time, actor, target and data.
	lines := strings.Split(strings.TrimSpace(readFileText(t, sinkPath)), "\n")
	var found bool
	for _, l := range lines {
		var got types.AuditEvent
		if err := json.Unmarshal([]byte(l), &got); err != nil {
			t.Fatalf("sink line is not an audit event: %v: %s", err, l)
		}
		if got.Action != "person.create" {
			continue
		}
		found = true
		if got.ID != ev.ID || !got.Time.Equal(ev.Time) || got.Actor != ev.Actor || got.Target != ev.Target || string(got.Data) != string(ev.Data) {
			t.Errorf("sink copy %s differs from the stored event %+v", l, ev)
		}
	}
	if !found {
		t.Error("the sink never received the person.create")
	}
	// A third exchange emits nothing more.
	portalToken(t, h.exchange(t, b.ID, credB, "subject-token", nil))
	if len(h.introductions()) != 1 {
		t.Errorf("person.create rows after a third exchange = %d, want 1", len(h.introductions()))
	}
}
