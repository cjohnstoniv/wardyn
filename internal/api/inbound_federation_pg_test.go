// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// lateCheckRevocations is the rig's revocation store with a one-shot hook after the SessionStatus call whose issue
// the test names, so a revoke can land between the exchange's last late check and its mint.
type lateCheckRevocations struct {
	*pgTestRevocations
	mu    sync.Mutex
	want  oidc.Issue
	hook  func()
	fired int
}

func (l *lateCheckRevocations) SessionStatus(ctx context.Context, sub, email string, issue oidc.Issue, epoch int64) (oidc.SessionStatus, error) {
	status, err := l.pgTestRevocations.SessionStatus(ctx, sub, email, issue, epoch)
	l.mu.Lock()
	hook := l.hook
	if hook != nil && issue.Seq == l.want.Seq && issue.At.Equal(l.want.At) {
		l.hook, l.fired = nil, l.fired+1
	} else {
		hook = nil
	}
	l.mu.Unlock()
	if hook != nil {
		hook()
	}
	return status, err
}

const inboundTestIssuer = "https://idp.example.com"

// inbEnv is a service identity deployment with a second security admin, the fake verifier and a public address.
type inbEnv struct {
	svcGovEnv
	fake *fakeWorkloadVerifier
	peer int
}

func newInbEnv(t *testing.T) *inbEnv {
	t.Helper()
	e := &inbEnv{svcGovEnv: newSvcGovEnv(t), fake: &fakeWorkloadVerifier{}}
	// Provider tokens live two hours, so the trust's lifetime is the binding cap unless a test sets a nearer exp.
	e.fake.set(oidc.PortalRefusal{}, time.Now().Add(2*time.Hour))
	e.h.srv.cfg.PortalIssuers = e.fake
	e.h.srv.cfg.RunnerOrgURL = inboundTestOrigin
	// The authenticator reads the server's revocation store, as cmd/wardynd wires it, so a cookie from before a
	// revoke is refused at its next request.
	e.h.srv.cfg.OIDC = newPeopleAuth(t, func(c *oidc.Config) { c.Revocations = e.rev })
	e.h.srv.router = e.h.srv.routes()
	return e
}

// identity creates a service identity of svcSponsor and ages its sponsorship, so a token exchanged at once is
// after it.
func (e *inbEnv) identity(t *testing.T, name string) types.ServiceIdentity {
	t.Helper()
	si := e.mustCreate(t, name, svcSponsor)
	ageFreshSponsorships(t, e.pool)
	return si
}

func (e *inbEnv) put(t *testing.T, as *http.Cookie, enabled bool) *httptest.ResponseRecorder {
	t.Helper()
	return doSSO(t, e.h.srv, http.MethodPut, "/api/v1/inbound-federation", as, fmt.Sprintf(`{"enabled":%t}`, enabled))
}

func (e *inbEnv) switchOn(t *testing.T) {
	t.Helper()
	if w := e.put(t, e.sec, true); w.Code != http.StatusOK {
		t.Fatalf("switch on: %d %s", w.Code, w.Body.String())
	}
}

func (e *inbEnv) enabled(t *testing.T) bool {
	t.Helper()
	f, err := e.st.GetInboundFederation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return f.Enabled
}

// trustBody is a create body for a trust named name on service, matching subject sub; extra adds fields.
func trustBody(name, service, sub, extra string) string {
	return `{"name":"` + name + `","issuer":"` + inboundTestIssuer + `","audience":"` + inboundTestOrigin + `","subject":"` + sub +
		`","service_identity":"` + service + `"` + extra + `}`
}

func (e *inbEnv) createTrust(t *testing.T, as *http.Cookie, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doSSO(t, e.h.srv, http.MethodPost, "/api/v1/inbound-trusts", as, body)
}

func (e *inbEnv) mustTrust(t *testing.T, name, service, sub, extra string) types.InboundTrust {
	t.Helper()
	w := e.createTrust(t, e.sec, trustBody(name, service, sub, extra))
	if w.Code != http.StatusCreated {
		t.Fatalf("create trust %s: %d %s", name, w.Code, w.Body.String())
	}
	var tr types.InboundTrust
	if err := json.Unmarshal(w.Body.Bytes(), &tr); err != nil {
		t.Fatal(err)
	}
	stored, err := e.st.GetInboundTrust(context.Background(), tr.ID)
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

func (e *inbEnv) trustCount(t *testing.T) int {
	t.Helper()
	trusts, err := e.st.ListInboundTrusts(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	return len(trusts)
}

// nextPeer is a fresh client address, so the per-address limiter never decides a test that is not about it.
func (e *inbEnv) nextPeer() string {
	e.peer++
	return fmt.Sprintf("203.0.113.%d:%d", e.peer%250+1, 40000+e.peer)
}

func exchangeBody(tok, extra string) string {
	return `{"subject_token":"` + tok + `"` + extra + `}`
}

// exchange presents a token for (inboundTestIssuer, aud, sub) from a fresh address.
func (e *inbEnv) exchange(t *testing.T, aud, sub, extra string) *httptest.ResponseRecorder {
	t.Helper()
	return exchangeFrom(t, e.h.srv, e.nextPeer(), exchangeBody(workloadToken(t, inboundTestIssuer, aud, sub), extra))
}

func (e *inbEnv) mustExchange(t *testing.T, sub, extra string) inboundExchangeResponse {
	t.Helper()
	w := e.exchange(t, inboundTestOrigin, sub, extra)
	if w.Code != http.StatusCreated {
		t.Fatalf("exchange %s: %d %s", sub, w.Code, w.Body.String())
	}
	var out inboundExchangeResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// exchangedCount counts the inbound tokens of a service principal.
func (e *inbEnv) exchangedCount(t *testing.T, name string) (n int) {
	t.Helper()
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM api_tokens WHERE principal = $1 AND kind = 'inbound'`,
		types.ServicePrincipalPrefix+name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// exchangeRows are the inbound.exchange rows with outcome.
func (e *inbEnv) exchangeRows(outcome string) []types.AuditEvent {
	var out []types.AuditEvent
	for _, ev := range e.auditRows("inbound.exchange") {
		if ev.Outcome == outcome {
			out = append(out, ev)
		}
	}
	return out
}

func inboundRowData(t *testing.T, ev types.AuditEvent) map[string]any {
	t.Helper()
	var d map[string]any
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

// refillAuthFailed refills the refusal-row bucket, so a test writing several refusal rows sees each.
func (e *inbEnv) refillAuthFailed() {
	l := &e.h.srv.authFailedLimiter
	l.mu.Lock()
	l.last = time.Time{}
	l.mu.Unlock()
}

// drainAuthFailed empties the refusal-row bucket and sets its clock an hour ahead, so it refills nothing before the
// next refusal however long that request takes.
func (e *inbEnv) drainAuthFailed() {
	for e.h.srv.authFailedLimiter.allow(e.h.srv.cfg.Now()) {
	}
	l := &e.h.srv.authFailedLimiter
	l.mu.Lock()
	l.last = e.h.srv.cfg.Now().Add(time.Hour)
	l.mu.Unlock()
}

// onlyDenied fails unless exactly one denied row was written since before, with reason word and the trust named.
func (e *inbEnv) onlyDenied(t *testing.T, before int, word string, trust *types.InboundTrust) map[string]any {
	t.Helper()
	rows := e.exchangeRows("denied")
	if len(rows) != before+1 {
		t.Fatalf("denied rows = %d, want %d", len(rows), before+1)
	}
	d := inboundRowData(t, rows[len(rows)-1])
	if d["reason"] != word {
		t.Errorf("denied row reason %v, want %s", d["reason"], word)
	}
	if trust != nil && (d["trust"] != trust.ID.String() || d["service_identity"] != trust.ServiceIdentity) {
		t.Errorf("denied row %v, want it to name trust %s on %s", d, trust.ID, trust.ServiceIdentity)
	}
	if rows[len(rows)-1].SourceIP == "" {
		t.Errorf("denied row has no source_ip")
	}
	return d
}

// withParams is r with chi's URL params set, for a handler called directly.
func withParams(r *http.Request, kv ...string) *http.Request {
	rctx := chi.NewRouteContext()
	for i := 0; i+1 < len(kv); i += 2 {
		rctx.URLParams.Add(kv[i], kv[i+1])
	}
	return r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
}

// direct calls h with ctx as the request's identity.
func direct(ctx context.Context, h http.HandlerFunc, method, path, body string, kv ...string) *httptest.ResponseRecorder {
	r := withParams(httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx), kv...)
	w := httptest.NewRecorder()
	h(w, r)
	return w
}

func secTokenCtx(sub, email string) context.Context {
	return withAPITokenID(withHumanIdentity(context.Background(), sub, email, oidc.RoleSecurityAdmin, "", nil, false), uuid.New())
}

func secDelegatedCtx(sub, email string) context.Context {
	return audit.WithDelegation(withHumanIdentity(context.Background(), sub, email, oidc.RoleSecurityAdmin, "", nil, false),
		types.DelegationVia{Delegate: uuid.New(), Grant: uuid.New()})
}

// freshSessionCookie is a session for sub signed in now, recording the current revocation sequence.
func (e *inbEnv) freshSessionCookie(t *testing.T, sub, email string) *http.Cookie {
	t.Helper()
	seq, err := e.rev.RevocationSeq(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(oidc.Session{
		V: oidc.SessionCodecVersion, Sub: sub, Email: email, Role: oidc.RoleUser, UserType: types.UserTypeStandard,
		Expiry: time.Now().UTC().Add(time.Hour), IssuedAt: time.Now().UTC(), RevocationSeq: seq, Groups: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, accessTestHMACKey)
	mac.Write(payload)
	return &http.Cookie{Name: "wardyn_session",
		Value: base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))}
}

func (e *inbEnv) confirm(t *testing.T, as *http.Cookie, service string, id uuid.UUID) *httptest.ResponseRecorder {
	t.Helper()
	return doSSO(t, e.h.srv, http.MethodPost, "/api/v1/me/service-identities/"+service+"/inbound-trusts/"+id.String()+"/confirm", as, "")
}

func (e *inbEnv) storedTrust(t *testing.T, id uuid.UUID) types.InboundTrust {
	t.Helper()
	tr, err := e.st.GetInboundTrust(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

// inboundHeldChange reads the pending change out of a 202.
func inboundHeldChange(t *testing.T, w *httptest.ResponseRecorder) types.GovernanceChange {
	t.Helper()
	var body struct {
		PendingChange types.GovernanceChange `json:"pending_change"`
	}
	if w.Code != http.StatusAccepted || json.Unmarshal(w.Body.Bytes(), &body) != nil || body.PendingChange.ID == uuid.Nil {
		t.Fatalf("want the write held (202 pending_change), got %d %s", w.Code, w.Body.String())
	}
	if loc := w.Header().Get("Location"); loc != "/api/v1/governance/changes/"+body.PendingChange.ID.String() {
		t.Errorf("Location %q, want the change", loc)
	}
	if body.PendingChange.TargetKind != govKindInboundFederation {
		t.Errorf("target_kind %q, want %q", body.PendingChange.TargetKind, govKindInboundFederation)
	}
	return body.PendingChange
}

// pendingInbound counts the pending changes of kind inbound_federation.
func (e *inbEnv) pendingInbound(t *testing.T) (n int) {
	t.Helper()
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM governance_changes WHERE target_kind = $1 AND state = $2`,
		govKindInboundFederation, types.GovernanceChangePending).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (e *inbEnv) changeState(t *testing.T, id uuid.UUID) string {
	t.Helper()
	ch, err := e.st.GetGovernanceChange(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return ch.State
}

// TestPG_InboundExchange_Mints: a matched, verified token becomes a fifteen-minute inbound token of the trust's
// identity, never cached, with one success row naming the sponsor and the caller's address.
func TestPG_InboundExchange_Mints(t *testing.T) {
	e := newInbEnv(t)
	e.identity(t, svcName)
	e.switchOn(t)
	tr := e.mustTrust(t, "gh-deploy", svcName, "repo:org/app", "")
	const peer = "198.51.100.20:5555"
	before := time.Now()
	w := exchangeFrom(t, e.h.srv, peer, exchangeBody(workloadToken(t, inboundTestIssuer, inboundTestOrigin, "repo:org/app"), ""))
	if w.Code != http.StatusCreated {
		t.Fatalf("exchange: %d %s, want 201", w.Code, w.Body.String())
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control %q, want no-store", cc)
	}
	var out inboundExchangeResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.ServiceIdentity != svcName || !strings.HasPrefix(out.Token, apiTokenPrefix) {
		t.Fatalf("exchange answer %s (%v), want a token of %s", w.Body.String(), err, svcName)
	}
	if want := before.Add(900 * time.Second); out.ExpiresAt.Before(want.Add(-time.Minute)) || out.ExpiresAt.After(want.Add(time.Minute)) {
		t.Errorf("expires_at %v, want about %v", out.ExpiresAt, want)
	}
	var principal, name, kind string
	if err := e.pool.QueryRow(context.Background(), `SELECT principal, name, kind FROM api_tokens WHERE principal = $1`, svcPrin).
		Scan(&principal, &name, &kind); err != nil {
		t.Fatal(err)
	}
	if kind != types.APITokenKindInbound || name != "exchange:"+tr.Name {
		t.Errorf("token row = %s %s %s, want kind inbound named exchange:%s", principal, name, kind, tr.Name)
	}
	rows := e.exchangeRows("success")
	if len(rows) != 1 {
		t.Fatalf("success rows = %d, want 1", len(rows))
	}
	if d := inboundRowData(t, rows[0]); d["sponsor"] != svcSponsor || d["trust"] != tr.ID.String() || rows[0].SourceIP != peer {
		t.Errorf("success row %+v data %v, want sponsor %s and source_ip %s", rows[0], d, svcSponsor, peer)
	}
}

// TestPG_InboundExchange_TokenIsInboundOnly: an exchanged token is an inbound token and reaches no API route.
func TestPG_InboundExchange_TokenIsInboundOnly(t *testing.T) {
	e := newInbEnv(t)
	e.identity(t, svcName)
	e.switchOn(t)
	e.mustTrust(t, "gh-deploy", svcName, "repo:org/app", "")
	tok := e.mustExchange(t, "repo:org/app", "").Token
	if _, v := e.verify(tok, false); v == tokenVerified {
		t.Error("the exchanged token verified as an API token")
	}
	if _, v := e.verify(tok, true); v != tokenVerified {
		t.Errorf("the exchanged token as an inbound token: verdict %v, want verified", v)
	}
	if w := e.as(t, tok, http.MethodGet, "/api/v1/me", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("GET /api/v1/me with it: %d, want 401", w.Code)
	}
}

// TestPG_InboundExchange_GatewayCallThenRevoke: a trust delete, a sponsor change and a switch-off each end the
// exchanged tokens.
func TestPG_InboundExchange_GatewayCallThenRevoke(t *testing.T) {
	e := newInbEnv(t)
	e.identity(t, svcName)
	e.switchOn(t)
	tr := e.mustTrust(t, "one", svcName, "repo:org/one", "")
	tok := e.mustExchange(t, "repo:org/one", "").Token
	if _, v := e.verify(tok, true); v != tokenVerified {
		t.Fatalf("fresh token: verdict %v, want verified", v)
	}
	if w := doSSO(t, e.h.srv, http.MethodDelete, "/api/v1/inbound-trusts/"+tr.ID.String(), e.sec, ""); w.Code != http.StatusOK {
		t.Fatalf("delete trust: %d %s", w.Code, w.Body.String())
	}
	if _, v := e.verify(tok, true); v == tokenVerified {
		t.Error("a token of a deleted trust still verifies")
	}

	e.mustTrust(t, "two", svcName, "repo:org/two", "")
	tok = e.mustExchange(t, "repo:org/two", "").Token
	if w := e.patch(t, e.sec, svcName, `{"sponsor":"`+svcSponsor2+`"}`); w.Code != http.StatusOK {
		t.Fatalf("sponsor change: %d %s", w.Code, w.Body.String())
	}
	if _, v := e.verify(tok, true); v == tokenVerified {
		t.Error("a token survived its identity's sponsor change")
	}

	ageFreshSponsorships(t, e.pool)
	e.mustTrust(t, "three", svcName, "repo:org/three", "")
	tok = e.mustExchange(t, "repo:org/three", "").Token
	if w := e.put(t, e.sec, false); w.Code != http.StatusOK {
		t.Fatalf("switch off: %d %s", w.Code, w.Body.String())
	}
	if _, v := e.verify(tok, true); v == tokenVerified {
		t.Error("a token survived the switch-off")
	}
}

// TestPG_InboundExchange_NoFetchForUntrustedIssuer: a triple no trust names is refused before the verifier runs.
func TestPG_InboundExchange_NoFetchForUntrustedIssuer(t *testing.T) {
	e := newInbEnv(t)
	e.identity(t, svcName)
	e.switchOn(t)
	e.mustTrust(t, "gh-deploy", svcName, "repo:org/app", "")
	w := exchangeFrom(t, e.h.srv, e.nextPeer(), exchangeBody(workloadToken(t, "https://evil.example.com", inboundTestOrigin, "repo:org/app"), ""))
	if w.Code != http.StatusUnauthorized || respReason(w) != reasonInboundSubjectTokenInvalid {
		t.Fatalf("untrusted issuer: %d %s, want 401", w.Code, w.Body.String())
	}
	if n := e.fake.calls.Load(); n != 0 {
		t.Errorf("verifier calls = %d, want 0", n)
	}
}

// TestPG_InboundExchange_OneAnswerForEveryTokenRefusal: every refused subject token gets the same status, headers
// and body; only the rows say which check refused it.
func TestPG_InboundExchange_OneAnswerForEveryTokenRefusal(t *testing.T) {
	e := newInbEnv(t)
	e.identity(t, svcName)
	e.switchOn(t)
	e.mustTrust(t, "gh-deploy", svcName, "repo:org/app", "")
	good := workloadToken(t, inboundTestIssuer, inboundTestOrigin, "repo:org/app")
	var answers []*httptest.ResponseRecorder
	send := func(body string) {
		e.refillAuthFailed()
		answers = append(answers, exchangeFrom(t, e.h.srv, e.nextPeer(), body))
	}
	send(exchangeBody("", ""))
	send(exchangeBody("not-a-token", ""))
	send(exchangeBody(workloadToken(t, inboundTestIssuer, inboundTestOrigin, "repo:org/other"), ""))
	for _, reason := range []string{oidc.SubjectTokenInvalid, oidc.SubjectTokenAudienceMismatch, oidc.SubjectTokenSubjectMismatch,
		oidc.SubjectTokenClaimMismatch, oidc.SubjectTokenStale, oidc.SubjectTokenIatFuture} {
		e.fake.set(oidc.PortalRefusal{Reason: reason, Class: oidc.PortalRefusedToken}, time.Time{})
		send(exchangeBody(good, ""))
	}
	e.fake.set(oidc.PortalRefusal{}, time.Time{})
	e.h.srv.cfg.RunnerOrgURL = "https://moved.example.com"
	send(exchangeBody(good, ""))
	first := answers[0]
	for i, w := range answers {
		if w.Code != http.StatusUnauthorized || !bytes.Equal(w.Body.Bytes(), first.Body.Bytes()) || fmt.Sprint(w.Header()) != fmt.Sprint(first.Header()) {
			t.Errorf("refusal %d: %d %v %s, want byte-identical to %d %v %s", i, w.Code, w.Header(), w.Body.String(), first.Code, first.Header(), first.Body.String())
		}
	}
	seen := map[string]bool{}
	for _, ev := range e.exchangeRows("denied") {
		seen[fmt.Sprint(inboundRowData(t, ev)["reason"])] = true
	}
	if len(seen) != len(answers) {
		t.Errorf("denied reasons %v, want %d distinct", seen, len(answers))
	}
}

// TestPG_InboundExchange_OutageAnswerOnlyForAMatchedTrust: an issuer outage is a 503 with Retry-After only for a
// matched triple; an unmatched one is the one 401.
func TestPG_InboundExchange_OutageAnswerOnlyForAMatchedTrust(t *testing.T) {
	e := newInbEnv(t)
	e.identity(t, svcName)
	e.switchOn(t)
	e.mustTrust(t, "gh-deploy", svcName, "repo:org/app", "")
	e.fake.set(oidc.PortalRefusal{Reason: oidc.PortalIssuerUnavailable, Cause: "fetch", Class: oidc.PortalRefusedUnavailable,
		RetryAfter: 6500 * time.Millisecond}, time.Time{})
	w := e.exchange(t, inboundTestOrigin, "repo:org/app", "")
	if w.Code != http.StatusServiceUnavailable || respReason(w) != reasonInboundIssuerUnavailable || w.Header().Get("Retry-After") != "7" {
		t.Errorf("matched triple in an outage: %d Retry-After %q %s, want 503 with Retry-After 7", w.Code, w.Header().Get("Retry-After"), w.Body.String())
	}
	if w := e.exchange(t, inboundTestOrigin, "repo:org/other", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("unmatched triple in an outage: %d %s, want 401", w.Code, w.Body.String())
	}
}

// TestPG_InboundExchange_AudienceNoLongerThisDeployment: a trust made before the address changed is refused at the
// exchange, before the verifier runs.
func TestPG_InboundExchange_AudienceNoLongerThisDeployment(t *testing.T) {
	e := newInbEnv(t)
	e.identity(t, svcName)
	e.switchOn(t)
	tr := e.mustTrust(t, "gh-deploy", svcName, "repo:org/app", "")
	e.h.srv.cfg.RunnerOrgURL = "https://moved.example.com"
	w := e.exchange(t, inboundTestOrigin, "repo:org/app", "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("exchange after the address moved: %d %s, want 401", w.Code, w.Body.String())
	}
	if n := e.fake.calls.Load(); n != 0 {
		t.Errorf("verifier calls = %d, want 0", n)
	}
	e.onlyDenied(t, 0, "audience_not_this_deployment", &tr)
}

// TestPG_InboundExchange_VerifierMissingFailsClosed: no verifier is a 503 and no token.
func TestPG_InboundExchange_VerifierMissingFailsClosed(t *testing.T) {
	e := newInbEnv(t)
	e.identity(t, svcName)
	e.switchOn(t)
	tr := e.mustTrust(t, "gh-deploy", svcName, "repo:org/app", "")
	e.h.srv.cfg.PortalIssuers = nil
	w := e.exchange(t, inboundTestOrigin, "repo:org/app", "")
	if w.Code != http.StatusServiceUnavailable || respReason(w) != reasonInboundIssuerUnavailable {
		t.Fatalf("no verifier: %d %s, want 503 %s", w.Code, w.Body.String(), reasonInboundIssuerUnavailable)
	}
	if n := e.exchangedCount(t, svcName); n != 0 {
		t.Errorf("token rows = %d, want 0", n)
	}
	e.onlyDenied(t, 0, "verifier_unavailable", &tr)
}

// inboundStoreFaults is the PG store with a failing match or mint.
type inboundStoreFaults struct {
	store.PG
	failMatch, failMint bool
}

func (f inboundStoreFaults) MatchInboundTrust(ctx context.Context, issuer, audience, subject string) (types.InboundTrust, error) {
	if f.failMatch {
		return types.InboundTrust{}, errors.New("match unavailable")
	}
	return f.PG.MatchInboundTrust(ctx, issuer, audience, subject)
}

func (f inboundStoreFaults) CreateExchangedToken(ctx context.Context, trust types.InboundTrust, t types.APIToken, raw string) (types.APIToken, error) {
	if f.failMint {
		return types.APIToken{}, errors.New("mint unavailable")
	}
	return f.PG.CreateExchangedToken(ctx, trust, t, raw)
}

// TestPG_InboundExchange_StoreDownFailsClosed: a match that cannot be read is a 503 and no token.
func TestPG_InboundExchange_StoreDownFailsClosed(t *testing.T) {
	e := newInbEnv(t)
	e.identity(t, svcName)
	e.switchOn(t)
	e.mustTrust(t, "gh-deploy", svcName, "repo:org/app", "")
	e.h.srv.cfg.Store = inboundStoreFaults{PG: e.st, failMatch: true}
	w := e.exchange(t, inboundTestOrigin, "repo:org/app", "")
	if w.Code != http.StatusServiceUnavailable || respReason(w) != reasonTokenLookupUnavailable {
		t.Fatalf("match down: %d %s, want 503 %s", w.Code, w.Body.String(), reasonTokenLookupUnavailable)
	}
	if n := e.exchangedCount(t, svcName); n != 0 {
		t.Errorf("token rows = %d, want 0", n)
	}
}

// TestPG_InboundExchange_NoRevocationStoreRefuses: without a revocation store the sponsor cannot be checked, so
// nothing is exchanged.
func TestPG_InboundExchange_NoRevocationStoreRefuses(t *testing.T) {
	e := newInbEnv(t)
	e.identity(t, svcName)
	e.switchOn(t)
	tr := e.mustTrust(t, "gh-deploy", svcName, "repo:org/app", "")
	e.h.srv.cfg.SessionRevocations = nil
	w := e.exchange(t, inboundTestOrigin, "repo:org/app", "")
	if w.Code != http.StatusConflict || respReason(w) != reasonServiceIdentityInactive {
		t.Fatalf("no revocation store: %d %s, want 409", w.Code, w.Body.String())
	}
	if n := e.exchangedCount(t, svcName); n != 0 {
		t.Errorf("token rows = %d, want 0", n)
	}
	e.onlyDenied(t, 0, "identity_inactive", &tr)
}

// TestPG_InboundExchange_SponsorDeactivatedRefuses: a sponsor deactivated as a SCIM suspension does it stops the
// exchange.
func TestPG_InboundExchange_SponsorDeactivatedRefuses(t *testing.T) {
	e := newInbEnv(t)
	e.identity(t, svcName)
	e.switchOn(t)
	tr := e.mustTrust(t, "gh-deploy", svcName, "repo:org/app", "")
	if _, err := e.pool.Exec(context.Background(), `UPDATE principal_identities SET deactivated_at = now() WHERE principal = $1`, svcSponsor); err != nil {
		t.Fatal(err)
	}
	w := e.exchange(t, inboundTestOrigin, "repo:org/app", "")
	if w.Code != http.StatusConflict || respReason(w) != reasonServiceIdentityInactive {
		t.Fatalf("deactivated sponsor: %d %s, want 409", w.Code, w.Body.String())
	}
	if n := e.exchangedCount(t, svcName); n != 0 {
		t.Errorf("token rows = %d, want 0", n)
	}
	e.onlyDenied(t, 0, "identity_inactive", &tr)
}

// TestPG_InboundExchange_SponsorRevokeStopsUntilConfirmed: a revoke of the sponsor's sessions in full, or of every
// session, stops the trust until the sponsor confirms it again from a live session; a sign-out does not. On the real
// clock and the real sequence.
func TestPG_InboundExchange_SponsorRevokeStopsUntilConfirmed(t *testing.T) {
	for _, c := range []struct {
		name    string
		revoke  func(e *inbEnv) error
		stopped bool
	}{
		{"revoke the sponsor", func(e *inbEnv) error { return e.rev.RevokeSub(context.Background(), svcSponsor) }, true},
		{"revoke everyone", func(e *inbEnv) error { return e.rev.RevokeAll(context.Background()) }, true},
		{"sign-out", func(e *inbEnv) error { return e.rev.CutSessions(context.Background(), svcSponsor) }, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newInbEnv(t)
			if e.rev.now != nil {
				t.Fatal("the rig's clock is fixed; this test runs on the real clock")
			}
			e.identity(t, svcName)
			e.switchOn(t)
			tr := e.mustTrust(t, "gh-deploy", svcName, "repo:org/app", "")
			tokA := e.mustExchange(t, "repo:org/app", "").Token
			if err := c.revoke(e); err != nil {
				t.Fatal(err)
			}
			if !c.stopped {
				if w := e.exchange(t, inboundTestOrigin, "repo:org/app", ""); w.Code != http.StatusCreated {
					t.Fatalf("exchange after a sign-out: %d %s, want 201 with no confirm", w.Code, w.Body.String())
				}
				return
			}
			if _, v := e.verify(tokA, true); v == tokenVerified {
				t.Error("token A survived the revoke")
			}
			calls := e.fake.calls.Load()
			e.refillAuthFailed()
			w := e.exchange(t, inboundTestOrigin, "repo:org/app", "")
			if w.Code != http.StatusConflict || respReason(w) != reasonInboundTrustUnconfirmed {
				t.Fatalf("exchange after the revoke: %d %s, want 409 %s", w.Code, w.Body.String(), reasonInboundTrustUnconfirmed)
			}
			if e.fake.calls.Load() != calls+1 {
				t.Error("the refusal did not follow a verified token")
			}
			if n := e.exchangedCount(t, svcName); n != 1 {
				t.Errorf("token rows = %d, want only token A", n)
			}
			e.onlyDenied(t, 0, "trust_unconfirmed", &tr)
			if w := e.confirm(t, e.sponsor, svcName, tr.ID); w.Code == http.StatusOK {
				t.Errorf("the old sponsor cookie confirmed: %d %s", w.Code, w.Body.String())
			}
			if got := e.storedTrust(t, tr.ID); !got.ConfirmedAt.Equal(tr.ConfirmedAt) {
				t.Errorf("confirmed_at moved to %v after a refused confirm", got.ConfirmedAt)
			}
			fresh := e.freshSessionCookie(t, svcSponsor, "sponsor@corp.example")
			if w := e.confirm(t, fresh, svcName, tr.ID); w.Code != http.StatusOK {
				t.Fatalf("fresh sponsor confirm: %d %s, want 200", w.Code, w.Body.String())
			}
			if len(e.auditRows("inbound_trust.confirm")) != 1 {
				t.Error("no inbound_trust.confirm row")
			}
			if w := e.exchange(t, inboundTestOrigin, "repo:org/app", ""); w.Code != http.StatusCreated {
				t.Fatalf("exchange after the confirm: %d %s, want 201", w.Code, w.Body.String())
			}
		})
	}
}

// TestPG_InboundExchange_RevokeRacesExchange: a revoke that lands after the exchange's last late check is not seen
// by it, and the token it mints records the issue read before, so the token is refused at its first use.
func TestPG_InboundExchange_RevokeRacesExchange(t *testing.T) {
	e := newInbEnv(t)
	e.identity(t, svcName)
	e.switchOn(t)
	tr := e.mustTrust(t, "gh-deploy", svcName, "repo:org/app", "")
	late := &lateCheckRevocations{pgTestRevocations: e.rev, want: oidc.Issue{At: tr.ConfirmedAt, Seq: tr.ConfirmedSeq}, hook: func() {
		if err := e.rev.RevokeSub(context.Background(), svcSponsor); err != nil {
			t.Errorf("revoke in the hook: %v", err)
		}
	}}
	e.h.srv.cfg.SessionRevocations = late
	out := e.mustExchange(t, "repo:org/app", "")
	e.h.srv.cfg.SessionRevocations = e.rev
	late.mu.Lock()
	fired := late.fired
	late.mu.Unlock()
	if fired != 1 {
		t.Fatalf("late-check hook fired %d times, want 1: a hook that never fires proves nothing", fired)
	}
	t.Logf("late-check hook fired %d", fired)
	if _, v := e.verify(out.Token, true); v == tokenVerified {
		t.Error("the token minted across the revoke verifies")
	}
}

// TestPG_InboundExchange_RefusesUISandboxOriginAudience: the UI-sandbox origin is never an own audience, at the
// create and, for a trust written before the rule, at the exchange.
func TestPG_InboundExchange_RefusesUISandboxOriginAudience(t *testing.T) {
	e := newInbEnv(t)
	e.identity(t, svcName)
	e.switchOn(t)
	const ui = "https://ui.example.com"
	e.h.srv.cfg.UIAdvertiseURL = ui
	body := strings.Replace(trustBody("ui-aud", svcName, "repo:org/app", ""), inboundTestOrigin, ui, 1)
	if w := e.createTrust(t, e.sec, body); w.Code != http.StatusBadRequest || respReason(w) != reasonInboundTrustReserved {
		t.Fatalf("create with the UI origin: %d %s, want 400 %s", w.Code, w.Body.String(), reasonInboundTrustReserved)
	}
	now := time.Now().UTC()
	tr, _, err := e.st.CreateInboundTrust(context.Background(), types.InboundTrust{Name: "ui-aud", Issuer: inboundTestIssuer,
		Audience: ui, Subject: "repo:org/app", ServiceIdentity: svcName, TokenTTLSeconds: 900, MaxTokenAgeSeconds: 300,
		MaxLive: 100, CreatedBy: "sec", ConfirmedAt: now, ConfirmedBy: "sec"})
	if err != nil {
		t.Fatal(err)
	}
	w := e.exchange(t, ui, "repo:org/app", "")
	if w.Code != http.StatusUnauthorized || respReason(w) != reasonInboundSubjectTokenInvalid {
		t.Fatalf("exchange for the UI origin: %d %s, want 401", w.Code, w.Body.String())
	}
	if n := e.fake.calls.Load(); n != 0 {
		t.Errorf("verifier calls = %d, want 0", n)
	}
	if n := e.exchangedCount(t, svcName); n != 0 {
		t.Errorf("token rows = %d, want 0", n)
	}
	e.onlyDenied(t, 0, "audience_not_this_deployment", &tr)
}

// TestPG_InboundExchange_LifetimeCappedAtSubjectExpiry: an exchanged token never outlives the provider token.
func TestPG_InboundExchange_LifetimeCappedAtSubjectExpiry(t *testing.T) {
	e := newInbEnv(t)
	e.identity(t, svcName)
	e.switchOn(t)
	e.mustTrust(t, "gh-deploy", svcName, "repo:org/app", "")
	near := time.Now().Add(30 * time.Second)
	e.fake.set(oidc.PortalRefusal{}, near)
	for _, extra := range []string{"", `,"ttl_seconds":900`} {
		out := e.mustExchange(t, "repo:org/app", extra)
		if d := out.ExpiresAt.Sub(near); d > 2*time.Second || d < -2*time.Second {
			t.Errorf("ttl %q: expires_at %v, want the provider token's exp %v", extra, out.ExpiresAt, near)
		}
	}
	for _, ev := range e.exchangeRows("success") {
		if _, ok := inboundRowData(t, ev)["ttl_clamped_from_seconds"]; ok {
			t.Errorf("success row %s records a clamp for the cap at exp", ev.Data)
		}
	}
	e.fake.set(oidc.PortalRefusal{}, time.Now().Add(2*time.Hour))
	before := time.Now()
	out := e.mustExchange(t, "repo:org/app", "")
	if want := before.Add(900 * time.Second); out.ExpiresAt.Before(want.Add(-time.Minute)) || out.ExpiresAt.After(want.Add(time.Minute)) {
		t.Errorf("a two-hour provider token: expires_at %v, want about %v", out.ExpiresAt, want)
	}
}

// TestPG_InboundFederation_OffCancelsHeldTurnOn: a switch-off cancels a pending turn-on, which can then never apply.
func TestPG_InboundFederation_OffCancelsHeldTurnOn(t *testing.T) {
	e := newInbEnv(t)
	e.on(t)
	ch := inboundHeldChange(t, e.put(t, e.sec, true))
	w := direct(secTokenCtx("sec2", "sec2@corp.example"), e.h.srv.handlePutInboundFederation, http.MethodPut,
		"/api/v1/inbound-federation", `{"enabled":false}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), ch.ID.String()) {
		t.Fatalf("switch-off by an API token: %d %s, want 200 naming %s", w.Code, w.Body.String(), ch.ID)
	}
	rows := e.auditRows("inbound_federation.update")
	if len(rows) != 1 || !strings.Contains(string(rows[0].Data), ch.ID.String()) {
		t.Errorf("update rows %+v, want one naming the cancelled change", rows)
	}
	if st := e.changeState(t, ch.ID); st != types.GovernanceChangeStale {
		t.Errorf("held turn-on state %q, want stale", st)
	}
	if w := e.decide(t, e.super, ch.ID); w.Code != http.StatusConflict || respReason(w) != reasonGovernanceChangeNotPending {
		t.Errorf("approve after the off: %d %s, want 409 %s", w.Code, w.Body.String(), reasonGovernanceChangeNotPending)
	}
	if e.enabled(t) {
		t.Error("the switch is on")
	}
}

// TestPG_InboundFederation_HeldApprovalNeedsPerson: an API token, the admin token and a delegated request never
// approve a held turn-on or trust create; a signed-in person does.
func TestPG_InboundFederation_HeldApprovalNeedsPerson(t *testing.T) {
	e := newInbEnv(t)
	e.identity(t, svcName)
	e.on(t)
	on := inboundHeldChange(t, e.put(t, e.sec, true))
	create := inboundHeldChange(t, e.createTrust(t, e.sec, trustBody("gh-deploy", svcName, "repo:org/app", "")))
	e.bind(t, "sec2", "https://idp.example", "", "")
	tok := decodeToken(t, e.mintPersonal(t, e.sec2, `{"name":"ci"}`)).Token
	for _, id := range []uuid.UUID{on.ID, create.ID} {
		path := "/api/v1/governance/changes/" + id.String() + "/approve"
		for who, w := range map[string]*httptest.ResponseRecorder{
			"API token":   e.as(t, tok, http.MethodPost, path, ""),
			"admin token": do(t, e.h.srv, http.MethodPost, path, adminToken, ""),
			"delegated": direct(secDelegatedCtx("sec2", "sec2@corp.example"), e.h.srv.handleApproveGovernanceChange,
				http.MethodPost, path, "", "id", id.String()),
		} {
			if w.Code != http.StatusForbidden || respReason(w) != reasonInboundFederationNeedsPerson {
				t.Errorf("%s approving %s: %d %s, want 403 %s", who, id, w.Code, w.Body.String(), reasonInboundFederationNeedsPerson)
			}
		}
		if st := e.changeState(t, id); st != types.GovernanceChangePending {
			t.Errorf("change %s state %q, want pending", id, st)
		}
	}
	if e.enabled(t) || e.trustCount(t) != 0 {
		t.Fatal("a refused approval wrote the switch or a trust")
	}
	for _, id := range []uuid.UUID{on.ID, create.ID} {
		if w := e.decide(t, e.sec2, id); w.Code != http.StatusOK {
			t.Errorf("signed-in approval of %s: %d %s, want 200", id, w.Code, w.Body.String())
		}
	}
	if !e.enabled(t) || e.trustCount(t) != 1 {
		t.Error("the signed-in approvals did not apply")
	}
}

// TestPG_InboundFederation_PutOnWhileOnWritesNothing: a turn-on that finds the switch on writes, audits and holds
// nothing.
func TestPG_InboundFederation_PutOnWhileOnWritesNothing(t *testing.T) {
	e := newInbEnv(t)
	e.switchOn(t)
	before, err := e.st.GetInboundFederation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rows := len(e.auditRows("inbound_federation.update"))
	e.on(t)
	w := direct(secTokenCtx("sec2", "sec2@corp.example"), e.h.srv.handlePutInboundFederation, http.MethodPut,
		"/api/v1/inbound-federation", `{"enabled":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("turn-on while on: %d %s, want 200", w.Code, w.Body.String())
	}
	after, err := e.st.GetInboundFederation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) || len(e.auditRows("inbound_federation.update")) != rows {
		t.Errorf("turn-on while on wrote: updated_at %v -> %v, rows %d -> %d", before.UpdatedAt, after.UpdatedAt, rows, len(e.auditRows("inbound_federation.update")))
	}
	if n := e.pendingInbound(t); n != 0 {
		t.Errorf("turn-on while on held %d changes, want none", n)
	}
}

// TestPG_InboundTrust_ConfirmNeedsSponsorPerson: only the sponsor's own signed-in session confirms a trust.
func TestPG_InboundTrust_ConfirmNeedsSponsorPerson(t *testing.T) {
	e := newInbEnv(t)
	e.identity(t, svcName)
	tr := e.mustTrust(t, "gh-deploy", svcName, "repo:org/app", "")
	path := "/api/v1/me/service-identities/" + svcName + "/inbound-trusts/" + tr.ID.String() + "/confirm"
	sponsorCtx := withHumanIdentity(context.Background(), svcSponsor, "sponsor@corp.example", oidc.RoleUser, "", nil, false)
	params := []string{"name", svcName, "id", tr.ID.String()}
	for who, w := range map[string]*httptest.ResponseRecorder{
		"the sponsor's API token": direct(withAPITokenID(sponsorCtx, uuid.New()), e.h.srv.handleConfirmInboundTrust, http.MethodPost, path, "", params...),
		"a delegated request": direct(audit.WithDelegation(sponsorCtx, types.DelegationVia{Delegate: uuid.New(), Grant: uuid.New()}),
			e.h.srv.handleConfirmInboundTrust, http.MethodPost, path, "", params...),
	} {
		if w.Code != http.StatusForbidden || respReason(w) != reasonInboundFederationNeedsPerson {
			t.Errorf("%s: %d %s, want 403 %s", who, w.Code, w.Body.String(), reasonInboundFederationNeedsPerson)
		}
	}
	if w := e.confirm(t, e.other, svcName, tr.ID); w.Code != http.StatusNotFound || respReason(w) != reasonServiceIdentityNotFound {
		t.Errorf("another person: %d %s, want the missing name's 404", w.Code, w.Body.String())
	}
	if w := e.confirm(t, e.sponsor, svcName, uuid.New()); w.Code != http.StatusNotFound || respReason(w) != reasonInboundTrustNotFound {
		t.Errorf("a missing trust: %d %s, want 404 %s", w.Code, w.Body.String(), reasonInboundTrustNotFound)
	}
	if got := e.storedTrust(t, tr.ID); !got.ConfirmedAt.Equal(tr.ConfirmedAt) {
		t.Errorf("confirmed_at moved to %v", got.ConfirmedAt)
	}
}

// TestPG_InboundTrust_CreateRecordsConfirmation: a new trust is confirmed by its creator, at the request's time, at
// the sequence read then.
func TestPG_InboundTrust_CreateRecordsConfirmation(t *testing.T) {
	e := newInbEnv(t)
	e.identity(t, svcName)
	seqBefore, err := e.rev.RevocationSeq(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	before := time.Now().Add(-time.Second)
	tr := e.mustTrust(t, "gh-deploy", svcName, "repo:org/app", "")
	after := time.Now().Add(time.Second)
	if tr.ConfirmedAt.Before(before) || tr.ConfirmedAt.After(after) {
		t.Errorf("confirmed_at %v, want the request's time", tr.ConfirmedAt)
	}
	if tr.ConfirmedBy == "" || tr.ConfirmedBy != tr.CreatedBy {
		t.Errorf("confirmed_by %q, want the creator %q", tr.ConfirmedBy, tr.CreatedBy)
	}
	if tr.ConfirmedSeq != seqBefore {
		t.Errorf("confirmed_seq %d, want the sequence read at create %d", tr.ConfirmedSeq, seqBefore)
	}
}

// TestPG_InboundFederation_NeedsPerson: turning it on and adding a trust need a signed-in person; turning it off and
// deleting a trust take any security-tier credential.
func TestPG_InboundFederation_NeedsPerson(t *testing.T) {
	e := newInbEnv(t)
	e.identity(t, svcName)
	for who, put := range map[string]*httptest.ResponseRecorder{
		"admin token": do(t, e.h.srv, http.MethodPut, "/api/v1/inbound-federation", adminToken, `{"enabled":true}`),
		"API token": direct(secTokenCtx("sec", "sec@corp.example"), e.h.srv.handlePutInboundFederation, http.MethodPut,
			"/api/v1/inbound-federation", `{"enabled":true}`),
		"delegated": direct(secDelegatedCtx("sec", "sec@corp.example"), e.h.srv.handlePutInboundFederation, http.MethodPut,
			"/api/v1/inbound-federation", `{"enabled":true}`),
	} {
		if put.Code != http.StatusForbidden || respReason(put) != reasonInboundFederationNeedsPerson {
			t.Errorf("%s turning it on: %d %s, want 403 %s", who, put.Code, put.Body.String(), reasonInboundFederationNeedsPerson)
		}
	}
	body := trustBody("gh-deploy", svcName, "repo:org/app", "")
	for who, post := range map[string]*httptest.ResponseRecorder{
		"admin token": do(t, e.h.srv, http.MethodPost, "/api/v1/inbound-trusts", adminToken, body),
		"API token": direct(secTokenCtx("sec", "sec@corp.example"), e.h.srv.handleCreateInboundTrust, http.MethodPost,
			"/api/v1/inbound-trusts", body),
		"delegated": direct(secDelegatedCtx("sec", "sec@corp.example"), e.h.srv.handleCreateInboundTrust, http.MethodPost,
			"/api/v1/inbound-trusts", body),
	} {
		if post.Code != http.StatusForbidden || respReason(post) != reasonInboundFederationNeedsPerson {
			t.Errorf("%s adding a trust: %d %s, want 403 %s", who, post.Code, post.Body.String(), reasonInboundFederationNeedsPerson)
		}
	}
	if e.enabled(t) || e.trustCount(t) != 0 {
		t.Fatal("a refused write changed the switch or wrote a trust")
	}
	tr := e.mustTrust(t, "gh-deploy", svcName, "repo:org/app", "")
	if w := direct(secTokenCtx("sec", "sec@corp.example"), e.h.srv.handlePutInboundFederation, http.MethodPut,
		"/api/v1/inbound-federation", `{"enabled":false}`); w.Code != http.StatusOK {
		t.Errorf("API token turning it off: %d %s, want 200", w.Code, w.Body.String())
	}
	if w := direct(secTokenCtx("sec", "sec@corp.example"), e.h.srv.handleDeleteInboundTrust, http.MethodDelete,
		"/api/v1/inbound-trusts/"+tr.ID.String(), "", "id", tr.ID.String()); w.Code != http.StatusOK {
		t.Errorf("API token deleting a trust: %d %s, want 200", w.Code, w.Body.String())
	}
}

// TestPG_InboundFederation_HeldUnderSecondHuman: under WARDYN_GOVERNANCE_SECOND_HUMAN a turn-on and a trust create
// wait for a distinct human; a switch-off and a delete apply directly.
func TestPG_InboundFederation_HeldUnderSecondHuman(t *testing.T) {
	e := newInbEnv(t)
	e.identity(t, svcName)
	e.identity(t, "ci-old")
	t.Setenv("WARDYN_GOVERNANCE_SECOND_HUMAN", "true")
	on := inboundHeldChange(t, e.put(t, e.sec, true))
	if e.enabled(t) {
		t.Fatal("a held turn-on applied")
	}
	if w := e.decide(t, e.sec, on.ID); w.Code == http.StatusOK {
		t.Errorf("the proposer approved their own turn-on: %d %s", w.Code, w.Body.String())
	}
	if w := e.decide(t, e.sec2, on.ID); w.Code != http.StatusOK {
		t.Fatalf("second admin's approval: %d %s", w.Code, w.Body.String())
	}
	if !e.enabled(t) || len(e.auditRows("inbound_federation.update")) != 1 {
		t.Error("the approved turn-on did not apply with its row")
	}
	create := inboundHeldChange(t, e.createTrust(t, e.sec, trustBody("gh-deploy", svcName, "repo:org/app", "")))
	if e.trustCount(t) != 0 {
		t.Fatal("a held create wrote a trust")
	}
	if w := e.decide(t, e.sec2, create.ID); w.Code != http.StatusOK {
		t.Fatalf("approve the create: %d %s", w.Code, w.Body.String())
	}
	trusts, err := e.st.ListInboundTrusts(context.Background(), svcName)
	if err != nil || len(trusts) != 1 || trusts[0].CreatedBy != create.ProposedBy {
		t.Fatalf("trusts %+v (%v), want one created by the proposer %s", trusts, err, create.ProposedBy)
	}
	stale := inboundHeldChange(t, e.createTrust(t, e.sec, trustBody("old-deploy", "ci-old", "repo:org/old", "")))
	if w := doSSO(t, e.h.srv, http.MethodDelete, "/api/v1/service-identities/ci-old", e.sec, ""); w.Code >= 300 {
		t.Fatalf("retire ci-old: %d %s", w.Code, w.Body.String())
	}
	if w := e.decide(t, e.sec2, stale.ID); w.Code != http.StatusConflict || respReason(w) != reasonGovernanceChangeStale {
		t.Errorf("approve a create whose identity retired: %d %s, want 409 stale", w.Code, w.Body.String())
	}
	if w := doSSO(t, e.h.srv, http.MethodDelete, "/api/v1/inbound-trusts/"+trusts[0].ID.String(), e.sec, ""); w.Code != http.StatusOK {
		t.Errorf("delete under the switch: %d %s, want 200", w.Code, w.Body.String())
	}
	if w := e.put(t, e.sec, false); w.Code != http.StatusOK {
		t.Errorf("switch-off under the switch: %d %s, want 200", w.Code, w.Body.String())
	}
	if e.enabled(t) || e.trustCount(t) != 0 {
		t.Error("the direct off and delete did not apply")
	}
}

// TestPG_InboundExchange_PostVerifyRefusalsAudited: every refusal after the verifier admitted the token writes one
// bounded row naming the trust, the identity and the sponsor.
func TestPG_InboundExchange_PostVerifyRefusalsAudited(t *testing.T) {
	e := newInbEnv(t)
	e.identity(t, svcName)
	e.identity(t, "ci-paused")
	e.switchOn(t)
	paused := e.mustTrust(t, "paused", "ci-paused", "repo:org/paused", "")
	capped := e.mustTrust(t, "capped", svcName, "repo:org/capped", `,"max_live":1`)
	mint := e.mustTrust(t, "mint", svcName, "repo:org/mint", "")
	if w := e.patch(t, e.sec, "ci-paused", `{"suspended":true}`); w.Code != http.StatusOK {
		t.Fatalf("suspend: %d %s", w.Code, w.Body.String())
	}
	e.mustExchange(t, "repo:org/capped", "")
	faults := inboundStoreFaults{PG: e.st, failMint: true}
	cases := []struct {
		sub, word string
		trust     *types.InboundTrust
		status    int
		setup     func()
	}{
		{"repo:org/paused", "identity_inactive", &paused, http.StatusConflict, func() {}},
		{"repo:org/capped", "trust_cap", &capped, http.StatusUnprocessableEntity, func() {}},
		{"repo:org/mint", "store_unavailable", &mint, http.StatusServiceUnavailable, func() { e.h.srv.cfg.Store = faults }},
	}
	for _, c := range cases {
		c.setup()
		e.refillAuthFailed()
		before := len(e.exchangeRows("denied"))
		if w := e.exchange(t, inboundTestOrigin, c.sub, ""); w.Code != c.status {
			t.Errorf("%s: %d %s, want %d", c.word, w.Code, w.Body.String(), c.status)
		}
		d := e.onlyDenied(t, before, c.word, c.trust)
		if d["sponsor"] != svcSponsor {
			t.Errorf("%s row %v, want sponsor %s", c.word, d, svcSponsor)
		}
	}
	for _, c := range cases {
		before := len(e.exchangeRows("denied"))
		e.drainAuthFailed()
		e.exchange(t, inboundTestOrigin, c.sub, "")
		if got := len(e.exchangeRows("denied")); got != before {
			t.Errorf("%s with the bound spent: %d rows written, want 0", c.word, got-before)
		}
	}
}

// TestPG_InboundExchange_LifetimeCapped: the asked lifetime is clamped to the trust's and the deployment's caps,
// and an out-of-range one is refused before anything is written.
func TestPG_InboundExchange_LifetimeCapped(t *testing.T) {
	e := newInbEnv(t)
	e.identity(t, svcName)
	e.switchOn(t)
	e.mustTrust(t, "gh-deploy", svcName, "repo:org/app", "")
	expect := func(what string, out inboundExchangeResponse, before time.Time, want time.Duration) {
		t.Helper()
		if w := before.Add(want); out.ExpiresAt.Before(w.Add(-5*time.Second)) || out.ExpiresAt.After(w.Add(5*time.Second)) {
			t.Errorf("%s: expires_at %v, want about %v", what, out.ExpiresAt, w)
		}
	}
	before := time.Now()
	expect("ttl 99999", e.mustExchange(t, "repo:org/app", `,"ttl_seconds":99999`), before, 900*time.Second)
	rows := e.exchangeRows("success")
	if d := inboundRowData(t, rows[len(rows)-1]); d["ttl_clamped_from_seconds"] != float64(99999) {
		t.Errorf("clamped row %v, want ttl_clamped_from_seconds 99999", d)
	}
	before = time.Now()
	expect("ttl 120", e.mustExchange(t, "repo:org/app", `,"ttl_seconds":120`), before, 120*time.Second)
	e.h.srv.cfg.APITokenMaxTTL = time.Minute
	before = time.Now()
	expect("deployment cap 60 s", e.mustExchange(t, "repo:org/app", ""), before, time.Minute)
	tokens, audits := e.exchangedCount(t, svcName), len(e.auditRows("inbound.exchange"))
	for _, ttl := range []string{"-1", fmt.Sprint(apiTokenMaxTTLSeconds + 1), fmt.Sprint(int64(math.MaxInt64))} {
		w := e.exchange(t, inboundTestOrigin, "repo:org/app", `,"ttl_seconds":`+ttl)
		if w.Code != http.StatusBadRequest || respReason(w) != reasonAPITokenTTLInvalid || !strings.Contains(w.Body.String(), "between 0 and") {
			t.Errorf("ttl %s: %d %s, want 400 %s", ttl, w.Code, w.Body.String(), reasonAPITokenTTLInvalid)
		}
	}
	if e.exchangedCount(t, svcName) != tokens || len(e.auditRows("inbound.exchange")) != audits {
		t.Error("a refused lifetime wrote a token or a row")
	}
}

// TestPG_InboundExchange_CapAnswer: a trust at its live-token limit answers the cap reason.
func TestPG_InboundExchange_CapAnswer(t *testing.T) {
	e := newInbEnv(t)
	e.identity(t, svcName)
	e.switchOn(t)
	e.mustTrust(t, "gh-deploy", svcName, "repo:org/app", `,"max_live":1`)
	e.mustExchange(t, "repo:org/app", "")
	if w := e.exchange(t, inboundTestOrigin, "repo:org/app", ""); w.Code != http.StatusUnprocessableEntity || respReason(w) != reasonAPITokenCapReached {
		t.Errorf("second exchange: %d %s, want 422 %s", w.Code, w.Body.String(), reasonAPITokenCapReached)
	}
}

// TestPG_InboundExchange_BodyCap: a body past the cap is refused.
func TestPG_InboundExchange_BodyCap(t *testing.T) {
	e := newInbEnv(t)
	e.switchOn(t)
	prefix, suffix := `{"subject_token":"`, `"}`
	body := prefix + strings.Repeat("a", maxInboundExchangeBody+1-len(prefix)-len(suffix)) + suffix
	if w := exchangeFrom(t, e.h.srv, e.nextPeer(), body); w.Code != http.StatusBadRequest || respReason(w) != reasonInvalidRequestBody {
		t.Errorf("a 16 KiB + 1 body: %d %s, want 400 %s", w.Code, w.Body.String(), reasonInvalidRequestBody)
	}
}

// TestPG_InboundExchange_RefundsOnSuccess: successful exchanges never drain the address's bucket.
func TestPG_InboundExchange_RefundsOnSuccess(t *testing.T) {
	e := newInbEnv(t)
	e.identity(t, svcName)
	e.switchOn(t)
	e.mustTrust(t, "gh-deploy", svcName, "repo:org/app", "")
	const peer = "198.51.100.30:6000"
	for i := range 20 {
		w := exchangeFrom(t, e.h.srv, peer, exchangeBody(workloadToken(t, inboundTestIssuer, inboundTestOrigin, "repo:org/app"), ""))
		if w.Code != http.StatusCreated {
			t.Fatalf("exchange %d: %d %s, want 201", i+1, w.Code, w.Body.String())
		}
	}
}

// TestPG_InboundExchange_AuditNeverHoldsToken: no row carries the provider token or the minted one.
func TestPG_InboundExchange_AuditNeverHoldsToken(t *testing.T) {
	e := newInbEnv(t)
	e.identity(t, svcName)
	e.switchOn(t)
	e.mustTrust(t, "gh-deploy", svcName, "repo:org/app", "")
	subject := workloadToken(t, inboundTestIssuer, inboundTestOrigin, "repo:org/app")
	w := exchangeFrom(t, e.h.srv, e.nextPeer(), exchangeBody(subject, ""))
	var out inboundExchangeResponse
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatalf("exchange: %d %s", w.Code, w.Body.String())
	}
	e.fake.set(oidc.PortalRefusal{Reason: oidc.SubjectTokenStale, Class: oidc.PortalRefusedToken}, time.Time{})
	e.refillAuthFailed()
	exchangeFrom(t, e.h.srv, e.nextPeer(), exchangeBody(subject, ""))
	if len(e.exchangeRows("success")) != 1 || len(e.exchangeRows("denied")) != 1 {
		t.Fatalf("rows: %d success, %d denied, want 1 each", len(e.exchangeRows("success")), len(e.exchangeRows("denied")))
	}
	for _, ev := range e.h.audit.snapshot() {
		raw, _ := json.Marshal(ev)
		if bytes.Contains(raw, []byte(subject)) || bytes.Contains(raw, []byte(out.Token)) {
			t.Errorf("audit row %s holds a token", ev.Action)
		}
	}
}

// TestPG_InboundFederation_PutOffRevokes: a switch-off revokes every exchanged token and its row counts them.
func TestPG_InboundFederation_PutOffRevokes(t *testing.T) {
	e := newInbEnv(t)
	e.identity(t, svcName)
	e.switchOn(t)
	e.mustTrust(t, "gh-deploy", svcName, "repo:org/app", "")
	e.mustExchange(t, "repo:org/app", "")
	e.mustExchange(t, "repo:org/app", "")
	w := e.put(t, e.sec, false)
	var body struct {
		TokensRevoked int `json:"tokens_revoked"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &body) != nil || body.TokensRevoked != 2 {
		t.Fatalf("switch off: %d %s, want tokens_revoked 2", w.Code, w.Body.String())
	}
	rows := e.auditRows("inbound_federation.update")
	if d := inboundRowData(t, rows[len(rows)-1]); d["tokens_revoked"] != float64(2) || d["enabled"] != false {
		t.Errorf("switch-off row %v, want tokens_revoked 2", d)
	}
	calls, exchangeRows := e.fake.calls.Load(), len(e.auditRows("inbound.exchange"))
	if w := e.exchange(t, inboundTestOrigin, "repo:org/app", ""); w.Code != http.StatusNotFound || respReason(w) != reasonInboundFederationOff {
		t.Errorf("exchange after the off: %d %s, want 404 %s", w.Code, w.Body.String(), reasonInboundFederationOff)
	}
	if e.fake.calls.Load() != calls || len(e.auditRows("inbound.exchange")) != exchangeRows {
		t.Error("the exchange after the off reached the verifier or wrote a row")
	}
}

// TestPG_InboundTrust_CreateAuditNamesSponsor: the create row names the identity's sponsor.
func TestPG_InboundTrust_CreateAuditNamesSponsor(t *testing.T) {
	e := newInbEnv(t)
	e.identity(t, svcName)
	e.mustTrust(t, "gh-deploy", svcName, "repo:org/app", "")
	rows := e.auditRows("inbound_trust.create")
	if len(rows) != 1 || inboundRowData(t, rows[0])["sponsor"] != svcSponsor {
		t.Errorf("create rows %+v, want one naming sponsor %s", rows, svcSponsor)
	}
}

// TestPG_InboundTrust_SponsorListsAndDeletes: the sponsor sees an identity's trusts and removes one, revoking its
// tokens.
func TestPG_InboundTrust_SponsorListsAndDeletes(t *testing.T) {
	e := newInbEnv(t)
	e.identity(t, svcName)
	e.switchOn(t)
	one := e.mustTrust(t, "one", svcName, "repo:org/one", "")
	e.mustTrust(t, "two", svcName, "repo:org/two", "")
	e.mustExchange(t, "repo:org/one", "")
	list := func() int {
		w := doSSO(t, e.h.srv, http.MethodGet, "/api/v1/me/service-identities/"+svcName+"/inbound-trusts", e.sponsor, "")
		var body struct {
			Trusts []types.InboundTrust `json:"trusts"`
		}
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &body) != nil {
			t.Fatalf("sponsor list: %d %s", w.Code, w.Body.String())
		}
		return len(body.Trusts)
	}
	if n := list(); n != 2 {
		t.Fatalf("sponsor lists %d trusts, want 2", n)
	}
	w := doSSO(t, e.h.srv, http.MethodDelete, "/api/v1/me/service-identities/"+svcName+"/inbound-trusts/"+one.ID.String(), e.sponsor, "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"tokens_revoked":1`) {
		t.Fatalf("sponsor delete: %d %s, want 200 with tokens_revoked 1", w.Code, w.Body.String())
	}
	rows := e.auditRows("inbound_trust.delete")
	if len(rows) != 1 || inboundRowData(t, rows[0])["by_sponsor"] != true {
		t.Errorf("delete rows %+v, want one with by_sponsor true", rows)
	}
	if n := list(); n != 1 {
		t.Errorf("sponsor lists %d trusts after the delete, want 1", n)
	}
}

// TestPG_InboundTrust_NonSponsorGets404: another person's list and delete answer as a missing name does.
func TestPG_InboundTrust_NonSponsorGets404(t *testing.T) {
	e := newInbEnv(t)
	e.identity(t, svcName)
	tr := e.mustTrust(t, "gh-deploy", svcName, "repo:org/app", "")
	for _, c := range []struct{ method, suffix string }{{http.MethodGet, "/inbound-trusts"}, {http.MethodDelete, "/inbound-trusts/" + tr.ID.String()}} {
		other := doSSO(t, e.h.srv, c.method, "/api/v1/me/service-identities/"+svcName+c.suffix, e.other, "")
		missing := doSSO(t, e.h.srv, c.method, "/api/v1/me/service-identities/no-such-bot"+c.suffix, e.other, "")
		if other.Code != http.StatusNotFound || other.Code != missing.Code || !bytes.Equal(other.Body.Bytes(), missing.Body.Bytes()) {
			t.Errorf("%s %s: another person %d %s, a missing name %d %s; want byte-identical 404s", c.method, c.suffix,
				other.Code, other.Body.String(), missing.Code, missing.Body.String())
		}
	}
	if e.trustCount(t) != 1 {
		t.Error("another person's delete removed the trust")
	}
}

// TestPG_InboundTrust_CreateRefusals: the store's refusals answer as the direct write's own.
func TestPG_InboundTrust_CreateRefusals(t *testing.T) {
	e := newInbEnv(t)
	e.identity(t, svcName)
	e.identity(t, "ci-old")
	if w := doSSO(t, e.h.srv, http.MethodDelete, "/api/v1/service-identities/ci-old", e.sec, ""); w.Code >= 300 {
		t.Fatalf("retire: %d %s", w.Code, w.Body.String())
	}
	if w := e.createTrust(t, e.sec, trustBody("missing", "no-such-bot", "repo:org/a", "")); w.Code != http.StatusNotFound {
		t.Errorf("missing identity: %d %s, want 404", w.Code, w.Body.String())
	}
	if w := e.createTrust(t, e.sec, trustBody("retired", "ci-old", "repo:org/b", "")); w.Code != http.StatusConflict || respReason(w) != reasonServiceIdentityInactive {
		t.Errorf("retired identity: %d %s, want 409 %s", w.Code, w.Body.String(), reasonServiceIdentityInactive)
	}
	e.mustTrust(t, "gh-deploy", svcName, "repo:org/c", "")
	if w := e.createTrust(t, e.sec, trustBody("gh-deploy", svcName, "repo:org/d", "")); w.Code != http.StatusConflict || respReason(w) != reasonInboundTrustExists {
		t.Errorf("duplicate name: %d %s, want 409 %s", w.Code, w.Body.String(), reasonInboundTrustExists)
	}
	if _, err := e.st.CreateDelegate(context.Background(), types.Delegate{ID: uuid.New(), Name: "portal", IdPClientID: inboundTestOrigin,
		Group: "portal-users"}, newBearer(delegateCredentialPrefix)); err != nil {
		t.Fatal(err)
	}
	if w := e.createTrust(t, e.sec, trustBody("portal-aud", svcName, "repo:org/e", "")); w.Code != http.StatusBadRequest || respReason(w) != reasonInboundTrustReserved {
		t.Errorf("a live portal's client id: %d %s, want 400 %s", w.Code, w.Body.String(), reasonInboundTrustReserved)
	}
}

// TestPG_ServiceIdentityUpdate_AuditCountsTrustsRemoved: a sponsor change and a retire count the trusts they removed.
func TestPG_ServiceIdentityUpdate_AuditCountsTrustsRemoved(t *testing.T) {
	e := newInbEnv(t)
	e.identity(t, svcName)
	e.mustTrust(t, "one", svcName, "repo:org/one", "")
	e.mustTrust(t, "two", svcName, "repo:org/two", "")
	if w := e.patch(t, e.sec, svcName, `{"sponsor":"`+svcSponsor2+`"}`); w.Code != http.StatusOK {
		t.Fatalf("sponsor change: %d %s", w.Code, w.Body.String())
	}
	upd := e.auditRows("service_identity.update")
	if len(upd) == 0 || inboundRowData(t, upd[len(upd)-1])["trusts_removed"] != float64(2) {
		t.Errorf("update rows %+v, want trusts_removed 2", upd)
	}
	e.mustTrust(t, "three", svcName, "repo:org/three", "")
	e.mustTrust(t, "four", svcName, "repo:org/four", "")
	if w := doSSO(t, e.h.srv, http.MethodDelete, "/api/v1/service-identities/"+svcName, e.sec, ""); w.Code >= 300 {
		t.Fatalf("retire: %d %s", w.Code, w.Body.String())
	}
	ret := e.auditRows("service_identity.retire")
	if len(ret) != 1 || inboundRowData(t, ret[0])["trusts_removed"] != float64(2) {
		t.Errorf("retire rows %+v, want trusts_removed 2", ret)
	}
}

// negativeSeqRevocations is the rig's revocation store reading its sequence below zero.
type negativeSeqRevocations struct{ *pgTestRevocations }

func (negativeSeqRevocations) RevocationSeq(context.Context) (int64, error) { return -1, nil }

// TestPG_InboundTrust_NegativeSeqRefused: a revocation sequence read below zero creates no trust: the direct create
// answers 503, and a held create's approval answers 503 and rolls back, leaving the change pending.
func TestPG_InboundTrust_NegativeSeqRefused(t *testing.T) {
	e := newInbEnv(t)
	e.identity(t, svcName)
	e.h.srv.cfg.SessionRevocations = negativeSeqRevocations{e.rev}
	w := e.createTrust(t, e.sec, trustBody("direct", svcName, "repo:org/direct", ""))
	if w.Code != http.StatusServiceUnavailable || respReason(w) != reasonTokenLookupUnavailable {
		t.Errorf("direct create: %d %s, want 503 %s", w.Code, w.Body.String(), reasonTokenLookupUnavailable)
	}
	e.h.srv.cfg.SessionRevocations = e.rev
	e.on(t)
	ch := inboundHeldChange(t, e.createTrust(t, e.sec, trustBody("held", svcName, "repo:org/held", "")))
	e.h.srv.cfg.SessionRevocations = negativeSeqRevocations{e.rev}
	if w := e.decide(t, e.sec2, ch.ID); w.Code != http.StatusServiceUnavailable || respReason(w) != reasonTokenLookupUnavailable {
		t.Errorf("held approval: %d %s, want 503 %s", w.Code, w.Body.String(), reasonTokenLookupUnavailable)
	}
	if st := e.changeState(t, ch.ID); st != types.GovernanceChangePending {
		t.Errorf("held create state %q, want pending", st)
	}
	if n := e.trustCount(t); n != 0 {
		t.Errorf("trusts = %d, want 0", n)
	}
	if n := len(e.auditRows("inbound_trust.create")); n != 0 {
		t.Errorf("inbound_trust.create rows = %d, want 0", n)
	}
}

// TestPG_InboundExchange_RefusesPortalAudienceRegisteredAfter: a portal registered after a trust, whose client id or
// audience is that trust's audience, stops the trust at the exchange before the verifier runs.
func TestPG_InboundExchange_RefusesPortalAudienceRegisteredAfter(t *testing.T) {
	for _, c := range []struct {
		name string
		d    types.Delegate
	}{
		{"client id", types.Delegate{IdPClientID: inboundTestOrigin}},
		{"audience", types.Delegate{IdPClientID: "portal-client", Audience: inboundTestOrigin}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newInbEnv(t)
			e.identity(t, svcName)
			e.switchOn(t)
			tr := e.mustTrust(t, "gh-deploy", svcName, "repo:org/app", "")
			d := c.d
			d.ID, d.Name, d.Group = uuid.New(), "portal", "portal-users"
			if _, err := e.st.CreateDelegate(context.Background(), d, newBearer(delegateCredentialPrefix)); err != nil {
				t.Fatal(err)
			}
			w := e.exchange(t, inboundTestOrigin, "repo:org/app", "")
			if w.Code != http.StatusUnauthorized || respReason(w) != reasonInboundSubjectTokenInvalid {
				t.Fatalf("exchange: %d %s, want 401 %s", w.Code, w.Body.String(), reasonInboundSubjectTokenInvalid)
			}
			if n := e.fake.calls.Load(); n != 0 {
				t.Errorf("verifier calls = %d, want 0", n)
			}
			if n := e.exchangedCount(t, svcName); n != 0 {
				t.Errorf("token rows = %d, want 0", n)
			}
			e.onlyDenied(t, 0, "audience_reserved", &tr)
		})
	}
}
