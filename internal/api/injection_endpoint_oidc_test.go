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

	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/test/entrafake"
)

// A custom endpoint that signs people in, at run time: the dispatch grant, the per-person resolve and the
// sign-in hold, against test/entrafake in its generic mode and the in-memory doubles. Nothing here reaches a
// real authority.

const oidcGatewayHost = "gateway.corp.example"

type oidcRunFixture struct {
	*oidcFlow
	st *azA3Store
}

func newOIDCRunFixture(t *testing.T) *oidcRunFixture {
	t.Helper()
	f := newOIDCFlow(t, "")
	fa := newFakeApprovals()
	st := &azA3Store{azureSiteStore: f.site, approvals: fa}
	f.srv.cfg.Store, f.srv.cfg.Approvals = st, fa
	st.run = types.AgentRun{ID: uuid.New(), Agent: "claude-code", CreatedBy: oidcPersonSub, ModelProviderID: f.row.ID}
	return &oidcRunFixture{oidcFlow: f, st: st}
}

func (f *oidcRunFixture) signIn(t *testing.T) {
	t.Helper()
	if _, w := f.capture(t, oidcPersonSub); w.Header().Get("Location") != f.doneRedirect() {
		t.Fatalf("sign-in: %d %s", w.Code, w.Header().Get("Location"))
	}
}

// liveRow is the provider row as the site configuration holds it now.
func (f *oidcRunFixture) liveRow(t *testing.T) types.ModelProvider {
	t.Helper()
	sc, err := f.site.GetSiteConfig(context.Background())
	if err != nil || sc.ModelProviders == nil || len(sc.ModelProviders.Providers) != 1 {
		t.Fatalf("site config: %v %+v", err, sc.ModelProviders)
	}
	return sc.ModelProviders.Providers[0]
}

// dispatch runs the REAL LLM phase for the fixture's run.
func (f *oidcRunFixture) dispatch(t *testing.T) (dispatchLLMPlan, map[string]string, *types.RunPolicySpec, bool) {
	t.Helper()
	site, err := f.site.GetSiteConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	env, policy := map[string]string{}, &types.RunPolicySpec{}
	plan, ok := f.srv.resolveLLMInjections(context.Background(), f.st.run, dispatchParams{}, policy, env, nil,
		"http://wardyn-proxy:3128", artifactRedirectPlan{}, false, site, true, false, bedrockCredUngraded())
	return plan, env, policy, ok
}

// oidcResolve is one sink resolve as the proxy makes it; a test edits the parts it wants to differ.
type oidcResolve struct {
	rule  egress.InjectionRule
	grant uuid.UUID
	sub   string
	run   *types.AgentRun
}

func (f *oidcRunFixture) request(plan dispatchLLMPlan) oidcResolve {
	run := f.st.run
	return oidcResolve{rule: plan.injections[0].Rule, grant: plan.injections[0].GrantID, sub: oidcPersonSub, run: &run}
}

func (f *oidcRunFixture) resolveWith(in oidcResolve, query string) (*httptest.ResponseRecorder, bool) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/internal/injection/"+in.grant.String()+query, nil)
	handled := f.srv.resolveEndpointOIDCInjection(w, r,
		&identity.Claims{RunID: f.st.run.ID, Sub: in.sub, SPIFFEID: "spiffe://wardyn.local/run"},
		broker.Minted{JTI: "jti-" + uuid.NewString(), Injection: &in.rule}, in.grant, in.run)
	return w, handled
}

func (f *oidcRunFixture) resolve(plan dispatchLLMPlan, query string) *httptest.ResponseRecorder {
	w, _ := f.resolveWith(f.request(plan), query)
	return w
}

// A run on an endpoint that signs people in is authored one grant naming the owner's sealed sign-in, TLS only,
// with the address its digest covers; the owner's access token is then handed to the proxy alone: it is in no
// sandbox variable, no grant, no audit row and no log, and the refresh token is nowhere in the answer.
func TestEndpointOIDCSink_InjectsTheOwnersAccessTokenToTheProxyOnly(t *testing.T) {
	f := newOIDCRunFixture(t)
	f.signIn(t)
	plan, env, policy, ok := f.dispatch(t)
	if !ok || len(plan.injections) != 1 || len(f.st.grants) != 1 {
		t.Fatalf("dispatch ok=%v injections=%d grants=%d, want one authored grant", ok, len(plan.injections), len(f.st.grants))
	}
	sc := scopeOf(t, f.st.grants[0])
	snap, _ := sc["snapshot"].(map[string]any)
	if sc["secret_name"] != providerSecretName(oidcUID, providerOIDCPart) || sc["require_tls"] != true || sc["host"] != oidcGatewayHost ||
		sc["header"] != "Authorization" || snap["owner_subject"] != oidcPersonSub || snap["provider_uid"] != oidcUID ||
		snap["address_digest"] != providerAddressDigest(f.liveRow(t)) {
		t.Fatalf("grant scope = %v, want the owner's -oidc sign-in on %s over TLS with the row's digest", sc, oidcGatewayHost)
	}

	w := f.resolve(plan, "")
	if w.Code != http.StatusOK {
		t.Fatalf("resolve: %d %s", w.Code, w.Body.String())
	}
	var resp types.ResolvedInjection
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	token := strings.TrimPrefix(resp.Value, "Bearer ")
	if resp.Host != oidcGatewayHost || resp.Header != "Authorization" || token == resp.Value || token == "" || !containsString(f.wire.all(), token) {
		t.Fatalf("response = %+v, want one Bearer access token the authority issued, for the endpoint host", resp)
	}
	stored, found := f.stored(t, oidcPersonSub)
	if !found || stored.RefreshToken == "" || strings.Contains(w.Body.String(), stored.RefreshToken) {
		t.Errorf("the answer carries the refresh token (stored=%v)", found)
	}

	planJSON, _ := json.Marshal(policy)
	grantJSON, _ := json.Marshal(f.st.grants)
	sandbox, _ := json.Marshal(env)
	haystack := map[string]string{
		"sandbox env": string(sandbox), "policy": string(planJSON), "grants": string(grantJSON),
		"audit": f.auditText(), "log": f.logs.String(),
	}
	seen := f.wire.all()
	if len(seen) < 5 {
		t.Fatalf("the proxy saw %d token values; want the capture's and the resolve's", len(seen))
	}
	for where, text := range haystack {
		for _, v := range append(seen, stored.RefreshToken, oidcClientSecret) {
			if v != "" && strings.Contains(text, v) {
				t.Errorf("the %s carries a token or secret value (%d bytes)", where, len(v))
			}
		}
	}
	rows := f.audit.find("secret.read")
	if len(rows) == 0 || rows[len(rows)-1].Outcome != "success" || !strings.Contains(string(rows[len(rows)-1].Data), `"owner":"`+oidcPersonSub+`"`) {
		t.Errorf("secret.read rows = %+v, want a success row naming the owner and never the token", rows)
	}
}

func containsString(all []string, v string) bool {
	for _, a := range all {
		if a == v {
			return true
		}
	}
	return false
}

// Every way a resolve could re-authorize the run against something it was not dispatched with is a refusal
// that redeems nothing: a hand-authored grant, another person's token, a run or row that cannot be read, and a
// row that moved, was turned off, was re-created, was re-pointed at another endpoint or authority, or no longer
// signs people in.
func TestEndpointOIDCSink_RefusesAnythingTheSnapshotDoesNotBless(t *testing.T) {
	editOIDC := func(f *oidcRunFixture, edit func(*types.ProviderOIDC)) {
		f.site.set(func(b *types.ModelProviders) {
			o := *b.Providers[0].OIDC
			edit(&o)
			b.Providers[0].OIDC = &o
		})
	}
	for _, tc := range []struct {
		name   string
		edit   func(f *oidcRunFixture, in *oidcResolve)
		code   int
		reason string
	}{
		{"a grant with no snapshot", func(f *oidcRunFixture, in *oidcResolve) { in.grant = uuid.New() }, http.StatusForbidden, reasonMissingScopeSnapshot},
		{"a snapshot for another provider", func(f *oidcRunFixture, in *oidcResolve) {
			in.rule.SecretName = providerSecretName(oidcOther, providerOIDCPart)
		}, http.StatusForbidden, reasonMissingScopeSnapshot},
		{"another person's run token", func(f *oidcRunFixture, in *oidcResolve) { in.sub = "someone-else" }, http.StatusForbidden, reasonOwnerNotCaller},
		{"a run that could not be read", func(f *oidcRunFixture, in *oidcResolve) { in.run = nil }, http.StatusServiceUnavailable, reasonRunUnreadable},
		{"the provider turned off", func(f *oidcRunFixture, in *oidcResolve) {
			f.site.set(func(b *types.ModelProviders) { b.Providers[0].Disabled = true })
		}, http.StatusForbidden, reasonScopeChanged},
		{"the provider re-created under a new uid", func(f *oidcRunFixture, in *oidcResolve) {
			f.site.set(func(b *types.ModelProviders) { b.Providers[0].UID = oidcOther })
		}, http.StatusForbidden, reasonScopeChanged},
		{"the provider no longer serving the harness", func(f *oidcRunFixture, in *oidcResolve) {
			f.site.set(func(b *types.ModelProviders) { b.Providers[0].Harnesses = nil })
		}, http.StatusForbidden, reasonScopeChanged},
		{"the endpoint re-pointed", func(f *oidcRunFixture, in *oidcResolve) {
			f.site.set(func(b *types.ModelProviders) { b.Providers[0].BaseURL = "https://other.corp.example" })
		}, http.StatusForbidden, reasonScopeChanged},
		{"the authority changed", func(f *oidcRunFixture, in *oidcResolve) {
			editOIDC(f, func(o *types.ProviderOIDC) { o.Issuer = "https://elsewhere.example" })
		}, http.StatusForbidden, reasonScopeChanged},
		{"the client changed", func(f *oidcRunFixture, in *oidcResolve) {
			editOIDC(f, func(o *types.ProviderOIDC) { o.ClientID = "another-client" })
		}, http.StatusForbidden, reasonScopeChanged},
		{"the provider no longer signs people in", func(f *oidcRunFixture, in *oidcResolve) {
			f.site.set(func(b *types.ModelProviders) { b.Providers[0].OIDC = nil })
		}, http.StatusForbidden, reasonScopeChanged},
		{"a host that is not the endpoint", func(f *oidcRunFixture, in *oidcResolve) { in.rule.Host = "evil.example" }, http.StatusForbidden, reasonHostNotEndpoint},
		{"a header that is not the row's", func(f *oidcRunFixture, in *oidcResolve) { in.rule.Header = "X-Elsewhere" }, http.StatusForbidden, reasonScopeChanged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newOIDCRunFixture(t)
			f.signIn(t)
			plan, _, _, _ := f.dispatch(t)
			issued := 0
			f.fake.OnIssue(func(entrafake.IssuedToken) { issued++ })
			in := f.request(plan)
			tc.edit(f, &in)
			w, handled := f.resolveWith(in, "")
			if !handled || w.Code != tc.code || !strings.Contains(w.Body.String(), `"reason":"`+tc.reason+`"`) {
				t.Fatalf("resolve = handled %v %d %s, want %d %s", handled, w.Code, w.Body.String(), tc.code, tc.reason)
			}
			if issued != 0 || strings.Contains(w.Body.String(), "Bearer") {
				t.Errorf("a refused resolve redeemed the sign-in %d time(s) or carries a token: %s", issued, w.Body.String())
			}
			if rows := f.audit.find("secret.read"); len(rows) == 0 || rows[len(rows)-1].Outcome != "failure" {
				t.Errorf("secret.read rows = %+v, want a failure row", rows)
			}
		})
	}
}

// A name that is not an endpoint sign-in is not this arm's, and the key arm's destination is never an
// endpoint that signs people in: a -key name is never resolved for a row with no key.
func TestEndpointOIDCSink_StandsAsideAndTheKeyArmNeverServesIt(t *testing.T) {
	f := newOIDCRunFixture(t)
	f.signIn(t)
	plan, _, _, _ := f.dispatch(t)
	for _, name := range []string{
		providerSecretName(oidcUID, providerKeyPart), providerSecretName(oidcUID, providerEntraPart), "wardyn-provider--oidc", "some-secret",
	} {
		in := f.request(plan)
		in.rule.SecretName = name
		if w, handled := f.resolveWith(in, ""); handled || w.Body.Len() != 0 {
			t.Errorf("%s: handled=%v body=%q, want the arm to stand aside", name, handled, w.Body.String())
		}
	}
	if _, _, _, ok := providerKeyDestination(f.liveRow(t), "claude-code"); ok {
		t.Error("the key arm has a destination for an endpoint that signs people in")
	}
	plain := endpointProvider()
	if host, _, _, ok := providerKeyDestination(plain, "claude-code"); !ok || host != oidcGatewayHost {
		t.Errorf("the key arm's destination for a paste-a-token endpoint = %q ok=%v, want it unchanged", host, ok)
	}
}

// A sign-in that ends mid-run holds the run's resolve with a sign-in request, once, in this lane alone; the
// person's next sign-in resolves it and the run's next resolve is served.
func TestEndpointOIDCSink_MidRunHoldIsResolvedByASignIn(t *testing.T) {
	for name, fault := range map[ADOEntraFailure]func(*oidcRunFixture, bool){
		ADOEntraFailureDeadCredential:      func(f *oidcRunFixture, on bool) { f.fake.SetInvalidGrant(on) },
		ADOEntraFailureInteractionRequired: func(f *oidcRunFixture, on bool) { f.fake.SetInteractionRequired(on) },
	} {
		t.Run(string(name), func(t *testing.T) {
			f := newOIDCRunFixture(t)
			f.signIn(t)
			plan, _, _, _ := f.dispatch(t)
			if w := f.resolve(plan, "?phase=boot"); w.Code != http.StatusOK {
				t.Fatalf("boot resolve: %d %s", w.Code, w.Body.String())
			}
			fault(f, true)
			id := pendingID(t, f.resolve(plan, ""), reauthPendingState)
			row, _ := f.st.approvals.Get(context.Background(), id)
			sc, ok := signInScopeFor(row, endpointOIDCApprovalLane)
			if !ok || sc.Owner != oidcPersonSub || sc.ProviderID != oidcUID || sc.Reason != adoSignInReason {
				t.Fatalf("raised row = %+v, want an endpoint sign-in request for the owner and provider", row)
			}
			if _, ado := adoSignInScope(row); ado {
				t.Fatal("the row is visible to the Azure DevOps lane")
			}
			if _, az := signInScopeFor(row, azureApprovalLane); az {
				t.Fatal("the row is visible to the azure_foundry lane")
			}
			if again := pendingID(t, f.resolve(plan, ""), reauthPendingState); again != id {
				t.Fatalf("a second resolve raised %s, want the same request %s", again, id)
			}
			req := f.audit.find("credential.reauth.request")
			if len(req) != 1 || !strings.Contains(string(req[0].Data), `"reason":"`+string(name)+`"`) ||
				!strings.Contains(string(req[0].Data), `"provider":"`+endpointOIDCApprovalLane+`"`) {
				t.Fatalf("credential.reauth.request rows = %+v, want one for the endpoint lane with reason %s", req, name)
			}
			if got := f.srv.reconcileADOReauthOnRead(context.Background(), row); got.State != types.ApprovalPending {
				t.Fatalf("the Azure DevOps reconcile resolved an endpoint row: %s", got.State)
			}
			if got := f.srv.reconcileAzureReauthOnRead(context.Background(), row); got.State != types.ApprovalPending {
				t.Fatalf("resolved before any sign-in: %s", got.State)
			}

			fault(f, false)
			f.srv.cfg.Now = func() time.Time { return time.Now().Add(2 * time.Minute) }
			f.signIn(t)
			if row, _ := f.st.approvals.Get(context.Background(), id); row.State != types.ApprovalApproved {
				t.Fatalf("after the sign-in the request is %s, want APPROVED by the capture itself", row.State)
			}
			if w := f.resolve(plan, ""); w.Code != http.StatusOK {
				t.Fatalf("the held request's re-resolve: %d %s, want 200", w.Code, w.Body.String())
			}
		})
	}
}

// At the sidecar's boot there is no request to hold: a sign-in that ended fails the run with the remedy as its
// hint, and a person who never signed in, or whose consent lapsed, is refused with the class that says why.
func TestEndpointOIDCSink_BootAndClassRefusals(t *testing.T) {
	t.Run("an ended sign-in at boot", func(t *testing.T) {
		f := newOIDCRunFixture(t)
		f.signIn(t)
		plan, _, _, _ := f.dispatch(t)
		f.fake.SetInteractionRequired(true)
		w := f.resolve(plan, "?phase=boot")
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "without the person present") || !strings.Contains(f.st.hint, "sign in again") {
			t.Fatalf("boot resolve = %d %s hint %q, want a 403 and the remedy as the failure hint", w.Code, w.Body.String(), f.st.hint)
		}
		if strings.Contains(w.Body.String(), "Conditional Access") || strings.Contains(w.Body.String(), "Azure") {
			t.Errorf("the refusal speaks Entra's words for a generic authority: %s", w.Body.String())
		}
		if n := len(f.st.approvals.requested); n != 0 {
			t.Errorf("a boot resolve raised %d request(s), want none", n)
		}
	})
	for name, tc := range map[string]struct {
		arrange func(*oidcRunFixture)
		code    int
		reason  string
	}{
		"consent withdrawn": {func(f *oidcRunFixture) { f.fake.SetConsentRequired(true) }, http.StatusForbidden, string(ADOEntraFailureConsentRequired)},
		"never signed in": {func(f *oidcRunFixture) {
			_ = f.srv.cfg.Secrets.For(oidcPersonSub).Delete(context.Background(), providerSecretName(oidcUID, providerOIDCPart))
		}, http.StatusForbidden, string(ADOEntraFailureNotCaptured)},
	} {
		t.Run(name, func(t *testing.T) {
			f := newOIDCRunFixture(t)
			f.signIn(t)
			plan, _, _, _ := f.dispatch(t)
			tc.arrange(f)
			w := f.resolve(plan, "?phase=boot")
			if w.Code != tc.code || !strings.Contains(w.Body.String(), `"reason":"`+tc.reason+`"`) || strings.Contains(w.Body.String(), "Bearer") {
				t.Fatalf("resolve = %d %s, want %d %s", w.Code, w.Body.String(), tc.code, tc.reason)
			}
		})
	}
}

// A run whose owner has not signed in, or whose sign-in has ended, is refused at dispatch with the sentence
// that says so and the console's sign-in as the remedy; a live one is not, and no principal has none to grade.
func TestEndpointOIDCDispatch_RefusesNoSignInAndAnEndedOne(t *testing.T) {
	f := newOIDCRunFixture(t)
	ctx := context.Background()
	live := func() (providerDenial, error) {
		_, d, err := f.srv.providerLiveness(ctx, f.liveRow(t), "claude-code", oidcPersonSub, false)
		return d, err
	}
	d, err := live()
	if err != nil || !d.credential || !strings.Contains(d.msg, mpRunNotSignedIn) || !strings.Contains(d.msg, mpRunRemedySignIn) {
		t.Fatalf("no sign-in: %+v %v, want the credential refusal %q", d, err, mpRunNotSignedIn)
	}
	if plan, _, _, ok := f.dispatch(t); ok || len(plan.injections) != 0 || len(f.st.grants) != 0 {
		t.Fatalf("a run with no sign-in was dispatched: ok=%v injections=%d grants=%d", ok, len(plan.injections), len(f.st.grants))
	}

	f.signIn(t)
	if d, err = live(); err != nil || d.msg != "" {
		t.Fatalf("a live sign-in: %+v %v, want no refusal", d, err)
	}
	f.fake.SetInteractionRequired(true)
	if _, rerr := f.srv.RedeemEndpointOIDCAccess(ctx, oidcPersonSub, f.liveRow(t)); rerr == nil {
		t.Fatal("a refused renewal redeemed")
	}
	if d, err = live(); err != nil || !d.credential || !strings.Contains(d.msg, mpRunSignInEnded) {
		t.Fatalf("an ended sign-in: %+v %v, want the credential refusal %q", d, err, mpRunSignInEnded)
	}
	if _, d, _ := f.srv.providerLiveness(ctx, f.liveRow(t), "claude-code", "", false); d.msg != mpcNoPerson {
		t.Errorf("no principal: %q, want %q", d.msg, mpcNoPerson)
	}
}

// The person's connection state for an endpoint that signs people in: not_configured with no sign-in,
// expired_signin when a renewal found it ended, live otherwise. There is no expiring state.
func TestEndpointOIDCAccess_StatesFollowTheSignIn(t *testing.T) {
	f := newOIDCRunFixture(t)
	ctx := context.Background()
	grade := func() SetupProviderAccess { return f.srv.providerAccessFor(ctx, f.liveRow(t), oidcPersonSub) }
	if got := grade(); got.State != modelAccessNotConfigured || got.Action != "Sign in to Corp gateway" {
		t.Fatalf("no sign-in: %+v, want not_configured with a sign-in action naming the provider", got)
	}
	f.signIn(t)
	if got := grade(); got.State != modelAccessLive || got.Action != "" {
		t.Fatalf("signed in: %+v, want live", got)
	}
	f.fake.SetInteractionRequired(true)
	if _, err := f.srv.RedeemEndpointOIDCAccess(ctx, oidcPersonSub, f.liveRow(t)); err == nil {
		t.Fatal("a refused renewal redeemed")
	}
	if got := grade(); got.State != modelAccessExpiredSignin || !strings.Contains(got.Action, "Corp gateway") {
		t.Fatalf("an ended sign-in: %+v, want expired_signin", got)
	}
	if got := f.srv.providerAccessFor(ctx, f.liveRow(t), ""); got.State != modelAccessNotApplicable {
		t.Errorf("no principal: %+v, want not_applicable", got)
	}
}

// A local runner's ownership proof reads the sealed sign-in, not a key: the capture's secret name is the
// -oidc part for a row that signs people in, and the intent is owned only once the person has signed in.
func TestEndpointOIDCPlacement_NamesTheSignInNotAKey(t *testing.T) {
	f := newOIDCRunFixture(t)
	site, _ := f.site.GetSiteConfig(context.Background())
	name := providerSecretName(oidcUID, providerOIDCPart)
	raw, _ := json.Marshal(providerGrantSnapshot{ProviderUID: oidcUID, OwnerSubject: oidcPersonSub, Harness: "claude-code", AddressDigest: "d"})
	if got, err := localCapturedSecretName(site, oidcPersonSub, name, raw); err != nil || got != name {
		t.Fatalf("captured name = %q %v, want %q", got, err, name)
	}
	if _, err := localCapturedSecretName(site, oidcPersonSub, providerSecretName(oidcUID, providerKeyPart), raw); err == nil {
		t.Error("a -key sentinel was accepted for an endpoint that signs people in")
	}

	intent := func() bool {
		out, ok, err := f.srv.localProviderIntent(httptest.NewRequest(http.MethodGet, "/", nil), oidcPersonSub, f.liveRow(t))
		if err != nil || !ok || len(out) != 1 {
			t.Fatalf("intents = %+v ok=%v err=%v, want one", out, ok, err)
		}
		return out[0].Origin.OwnNamespace
	}
	if intent() {
		t.Error("an intent was owned before any sign-in")
	}
	f.signIn(t)
	if !intent() {
		t.Error("the intent is not owned after the person signed in")
	}
}
