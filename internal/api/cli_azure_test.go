// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/armscope"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
	"github.com/cjohnstoniv/wardyn/test/entrafake"
)

// The Azure CLI row: its sign-in, its dispatch and its sink, against
// test/entrafake and the in-memory doubles. Nothing here reaches a real tenant.

const (
	azARMScope = "https://management.azure.com/user_impersonation"
	azCLISubA  = "1111aaaa-2222-3333-4444-5555bbbbcccc"
	azCLISubB  = "9999dddd-8888-7777-6666-5555eeeeffff"
	azCLISubC  = "0000abab-1111-2222-3333-4444cdcdefef"
)

// azCLIStore is the Foundry sink fixture's run, grant and approval store with
// the component store the Azure CLI arm reads the row and the run's snapshot from.
type azCLIStore struct {
	*azA3Store
	fakeComponentStore
	attached []types.RunComponent
}

func (s *azCLIStore) ListRunComponents(context.Context, uuid.UUID) ([]types.RunComponent, error) {
	return s.attached, nil
}

// azCLIFixture is a deployment whose console signs people in with the fake
// Entra tenant, one Azure CLI org row, and a run of the tenant's subject that
// attached it.
type azCLIFixture struct {
	*azureFixture
	st    *azCLIStore
	caps  *capStore
	owner string
	id    uuid.UUID
}

func azCLIRowDef(access string, subs ...string) types.ComponentDefinition {
	return types.ComponentDefinition{CLI: &types.ComponentCLI{Kind: types.CLIKindAzure, Azure: &types.CLIAzure{Subscriptions: subs, Access: access}}}
}

func newAzCLIFixture(t *testing.T, access string, subs ...string) *azCLIFixture {
	t.Helper()
	f := newAzureFixture(t)
	f.fake.SetConsentedScopes(azARMScope, azFoundryScope)
	fa := newFakeApprovals()
	caps := &capStore{}
	f.site.Store = caps // the capability reads the attach door makes
	st := &azCLIStore{azA3Store: &azA3Store{azureSiteStore: f.site, approvals: fa}}
	f.srv.cfg.Store, f.srv.cfg.Approvals = st, fa
	f.srv.cfg.AzureCLIEntra = func(id string) ADOEntraConfig {
		cfg := f.app
		cfg.RowID = id
		return cfg
	}
	c := &azCLIFixture{azureFixture: f, st: st, caps: caps, owner: f.fake.Subject(), id: uuid.New()}
	c.setRow(t, azCLIRowDef(access, subs...))
	st.run = types.AgentRun{ID: uuid.New(), CreatedBy: c.owner}
	st.attached = []types.RunComponent{{ComponentID: &c.id, Name: "Azure", Definition: azCLIRowDef(access, subs...)}}
	return c
}

// setRow writes the live org row; a nil CLI block deletes it.
func (f *azCLIFixture) setRow(t *testing.T, def types.ComponentDefinition) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.st.GetComponent(ctx, f.id, ""); err == nil {
		if _, err := f.st.DeleteComponent(ctx, f.id, ""); err != nil {
			t.Fatal(err)
		}
	}
	if def.CLI == nil {
		return
	}
	if _, err := f.st.CreateComponent(ctx, types.Component{ID: f.id, Name: "Azure", Definition: def}); err != nil {
		t.Fatal(err)
	}
}

func (f *azCLIFixture) start(subject, id string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, azureCLISignInRoute+"?component="+url.QueryEscape(id), nil)
	r = r.WithContext(withOIDCHuman(r.Context(), subject))
	w := httptest.NewRecorder()
	f.srv.handleAzureCLISignIn(w, r)
	return w
}

// begin starts a sign-in for the row and walks the authority, returning what
// the shared callback needs.
func (f *azCLIFixture) begin(t *testing.T) (url.Values, []*http.Cookie, string) {
	t.Helper()
	w := f.start(f.owner, f.id.String())
	if w.Code != http.StatusFound {
		t.Fatalf("start: status %d body %q; want a 302 to the authority", w.Code, w.Body.String())
	}
	authURL := w.Header().Get("Location")
	q := follow(t, authURL)
	if q.Get("code") == "" {
		t.Fatalf("the authority issued no code: %v", q)
	}
	return q, w.Result().Cookies(), authURL
}

func (f *azCLIFixture) signIn(t *testing.T) {
	t.Helper()
	q, cookies, _ := f.begin(t)
	w := f.callback(f.owner, q, cookies)
	if want := azureCLISignInDonePath + azureCLIComponentParam(f.id.String()); w.Code != http.StatusFound || w.Header().Get("Location") != want {
		t.Fatalf("sign-in: %d %s, want a 302 to %s", w.Code, w.Header().Get("Location"), want)
	}
}

func (f *azCLIFixture) stored(t *testing.T, owner string) (adoEntraBlob, bool) {
	t.Helper()
	ec, err := azureARMCapture(f.id.String())
	if err != nil {
		t.Fatal(err)
	}
	blob, found, err := f.srv.readEntraBlob(context.Background(), owner, ec)
	if err != nil {
		t.Fatalf("read the stored sign-in: %v", err)
	}
	return blob, found
}

// dispatch runs the arm's dispatch step for the fixture's run.
func (f *azCLIFixture) dispatch() ([]runner.InjectionGrant, azureCLIPlan, map[string]string, *types.RunPolicySpec, bool) {
	env, policy := map[string]string{}, &types.RunPolicySpec{}
	inj, plan, ok := f.srv.authorAzureCLI(context.Background(), f.st.run, runComponents{attached: []attachedComponent{{snapshot: f.st.attached[0]}}}.azureCLI(),
		"ca-cert", "ca-key", policy, env, nil)
	return inj, plan, env, policy, ok
}

func (f *azCLIFixture) resolveRule(rule egress.InjectionRule, grantID uuid.UUID, sub, query string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/internal/injection/"+grantID.String()+query, nil)
	if !f.srv.resolveAzureARMInjection(w, r,
		&identity.Claims{RunID: f.st.run.ID, Sub: sub, SPIFFEID: "spiffe://wardyn.local/run"},
		broker.Minted{JTI: "jti-" + uuid.NewString(), Injection: &rule}, grantID) {
		w.WriteHeader(http.StatusTeapot) // the arm stood aside
	}
	return w
}

// azCLIFiles decodes the staged ~/.azure files out of the sandbox environment.
func azCLIFiles(t *testing.T, env map[string]string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, line := range strings.Split(env[azureCLIConfigEnvVar], "\n") {
		path, b64, ok := strings.Cut(line, "\t")
		raw, err := base64.StdEncoding.DecodeString(b64)
		if !ok || err != nil {
			t.Fatalf("staged record %q is not path<TAB>base64", line)
		}
		out[path] = string(raw)
	}
	return out
}

// ── scope policy ────────────────────────────────────────────────────────────

// An Azure CLI capture asks for Resource Manager and nothing else, keeps the
// permissions granted under it, and is no model provider's audience.
func TestAzureARMCapture_ScopePolicy(t *testing.T) {
	id := uuid.NewString()
	ec, err := azureARMCapture(id)
	if err != nil {
		t.Fatal(err)
	}
	if ec.secretName != "wardyn-cli-"+id+"-entra" || !slices.Equal(ec.scopes, []string{azureARMAudience}) || !cliSignInSecret(ec.secretName) {
		t.Fatalf("capture = %+v, want the row's reserved name and the one audience", ec)
	}
	if err := ec.checkRequested([]string{azureARMAudience, entraOfflineAccessScope, entraOpenIDScope}); err != nil {
		t.Errorf("the lane's own request = %v", err)
	}
	for _, sc := range []string{azureFoundryAudience, "https://graph.microsoft.com/.default", ".default", "https://management.azure.com/user_impersonation"} {
		if err := ec.checkRequested([]string{sc}); err == nil {
			t.Errorf("a request for %q was admitted", sc)
		}
	}
	got := ec.capturedScopes(azARMScope + " " + azFoundryScope + " https://management.azure.com/.default openid")
	if !slices.Equal(got, []string{azARMScope}) {
		t.Errorf("captured scopes = %v, want %v alone", got, azARMScope)
	}
	if azureAudienceClosed(azureARMAudience) {
		t.Error("the Resource Manager audience is one a model provider's grant may name")
	}
	if _, err := azureFoundryCapture(azUIDAnthropic, azureARMAudience); err == nil {
		t.Error("a model provider's capture took the Resource Manager audience")
	}
	for _, bad := range []string{"", "a/b", "x-oauth", "wardyn-harness-x"} {
		if _, err := azureARMCapture(bad); err == nil {
			t.Errorf("azureARMCapture(%q) was accepted", bad)
		}
	}
}

// ── the sign-in ─────────────────────────────────────────────────────────────

// A sign-in asks the tenant for Resource Manager alone and lands sealed under
// the row's own name in the signer's namespace, audited as cli.signin.capture.
func TestAzureCLISignIn_CapturesUnderTheRowsNameInTheCallersNamespace(t *testing.T) {
	f := newAzCLIFixture(t, types.ComponentAccessRead, azCLISubA)
	var issued []string
	f.fake.OnIssue(func(it entrafake.IssuedToken) { issued = append(issued, it.AccessToken) })
	q, cookies, authURL := f.begin(t)
	u, _ := url.Parse(authURL)
	if got := u.Query().Get("scope"); got != azureARMAudience+" offline_access openid" || u.Query().Get("code_challenge_method") != "S256" {
		t.Fatalf("authorize request scope %q method %q, want Resource Manager alone under S256", got, u.Query().Get("code_challenge_method"))
	}
	for _, name := range []string{azureCLIStateCookieName, azureCLINonceCookieName, azureCLIPKCECookieName} {
		cookieNamed(t, cookies, name)
	}
	w := f.callback(f.owner, q, cookies)
	if want := azureCLISignInDonePath + "&component=" + f.id.String(); w.Code != http.StatusFound || w.Header().Get("Location") != want {
		t.Fatalf("callback: %d %s, want a 302 to %s", w.Code, w.Header().Get("Location"), want)
	}
	blob, found := f.stored(t, f.owner)
	if !found || blob.RefreshToken == "" || !slices.Equal(blob.Scopes, []string{azARMScope}) || blob.Subject != f.owner {
		t.Fatalf("stored blob = %+v (found %v), want the owner's refresh token for Resource Manager", blob, found)
	}
	if names := f.storedNames(t, f.owner); !slices.Equal(names, []string{"wardyn-cli-" + f.id.String() + "-entra"}) {
		t.Errorf("the owner's namespace holds %v, want the row's one name", names)
	}
	if names := f.storedNames(t, ""); len(names) != 0 {
		t.Errorf("the operator namespace holds %v, want nothing", names)
	}
	rows := f.audit.find("cli.signin.capture")
	if len(rows) != 1 || rows[0].Outcome != "success" || rows[0].Target != "wardyn-cli-"+f.id.String()+"-entra" || rows[0].Actor != f.owner {
		t.Fatalf("cli.signin.capture rows = %+v, want one success for the row", rows)
	}
	data := string(rows[0].Data)
	if !strings.Contains(data, `"cli":"azure"`) || !strings.Contains(data, `"component_id":"`+f.id.String()+`"`) {
		t.Errorf("audit data %s lacks the cli and the component", data)
	}
	for _, secret := range append(issued, blob.RefreshToken, q.Get("code")) {
		if secret != "" && strings.Contains(data, secret) {
			t.Errorf("the audit row carries a token or the code")
		}
	}
}

// The row is the cookie's claim, re-checked: a row deleted, turned into another
// kind or re-pinned to other subscriptions while the sign-in was open stores
// nothing. The same subscriptions in another order or case are the same row.
func TestAzureCLISignIn_RefusesARowThatIsNoLongerWhatStarted(t *testing.T) {
	for name, tc := range map[string]struct {
		change types.ComponentDefinition
		stored bool
	}{
		"deleted":                   {types.ComponentDefinition{}, false},
		"another kind":              {types.ComponentDefinition{Hosts: []string{"api.example.com"}}, false},
		"another subscription":      {azCLIRowDef(types.ComponentAccessRead, azCLISubB), false},
		"a subscription added":      {azCLIRowDef(types.ComponentAccessRead, azCLISubA, azCLISubB, azCLISubC), false},
		"reordered and re-cased":    {azCLIRowDef(types.ComponentAccessRead, strings.ToUpper(azCLISubB), azCLISubA), true},
		"only the access is edited": {azCLIRowDef(types.ComponentAccessWrite, azCLISubA, azCLISubB), true},
	} {
		t.Run(name, func(t *testing.T) {
			f := newAzCLIFixture(t, types.ComponentAccessRead, azCLISubA, azCLISubB)
			q, cookies, _ := f.begin(t)
			f.setRow(t, tc.change)
			w := f.callback(f.owner, q, cookies)
			_, found := f.stored(t, f.owner)
			if found != tc.stored {
				t.Fatalf("stored = %v, want %v (%d %s)", found, tc.stored, w.Code, w.Header().Get("Location"))
			}
			if tc.stored {
				return
			}
			if want := azureCLISignInErrorPath + azureCaptureRowChanged + azureCLIComponentParam(f.id.String()); w.Header().Get("Location") != want {
				t.Errorf("redirect %q, want %q", w.Header().Get("Location"), want)
			}
			rows := f.audit.find("cli.signin.capture")
			if len(rows) != 1 || rows[0].Outcome != "failure" || !strings.Contains(string(rows[0].Data), `"reason":"row_changed"`) {
				t.Errorf("cli.signin.capture rows = %+v, want one row_changed failure", rows)
			}
		})
	}
}

// A forged stamp cannot point the capture at another row, another person
// cannot finish someone's sign-in, and a callback missing a one-time cookie
// spends nothing.
func TestAzureCLISignIn_CallbackBindsTheStampAndTheSubject(t *testing.T) {
	f := newAzCLIFixture(t, types.ComponentAccessRead, azCLISubA)
	q, cookies, _ := f.begin(t)
	other := uuid.New()
	forged := azureCLIState{State: q.Get("state"), ComponentID: other.String(), Pin: azureCLIPin(types.CLIAzure{Subscriptions: []string{azCLISubA}})}
	w := f.callback(f.owner, q, withCookie(cookies, azureCLIStateCookieName, forged.encode()))
	if !strings.Contains(w.Header().Get("Location"), azureCLISignInErrorPath+azureCaptureRowChanged) {
		t.Fatalf("a stamp naming another row: %d %s, want row_changed", w.Code, w.Header().Get("Location"))
	}
	if names := f.storedNames(t, f.owner); len(names) != 0 {
		t.Fatalf("a forged stamp stored %v", names)
	}

	// Another spelling of the row's own id would store under a name the row's
	// purge does not delete.
	f = newAzCLIFixture(t, types.ComponentAccessRead, azCLISubA)
	q, cookies, _ = f.begin(t)
	respelled := azureCLIState{State: q.Get("state"), ComponentID: strings.ToUpper(f.id.String()), Pin: azureCLIPin(types.CLIAzure{Subscriptions: []string{azCLISubA}})}
	w = f.callback(f.owner, q, withCookie(cookies, azureCLIStateCookieName, respelled.encode()))
	if w.Code != http.StatusBadRequest || reasonOf(t, w) != reasonAzureCallbackCookiesInvalid {
		t.Fatalf("a re-spelled component id: %d %s, want 400 %s", w.Code, w.Body.String(), reasonAzureCallbackCookiesInvalid)
	}
	if names := f.storedNames(t, f.owner); len(names) != 0 {
		t.Fatalf("a re-spelled component id stored %v", names)
	}

	f = newAzCLIFixture(t, types.ComponentAccessRead, azCLISubA)
	q, cookies, _ = f.begin(t)
	w = f.callback("someone-else", q, cookies)
	if !strings.Contains(w.Header().Get("Location"), azureCLISignInErrorPath+reasonADOCallbackIdentityBinding) {
		t.Fatalf("another person's session: %d %s, want identity_binding", w.Code, w.Header().Get("Location"))
	}
	if _, found := f.stored(t, "someone-else"); found {
		t.Fatal("a sign-in landed under a subject the tenant did not authenticate")
	}

	f = newAzCLIFixture(t, types.ComponentAccessRead, azCLISubA)
	q, cookies, _ = f.begin(t)
	w = f.callback(f.owner, q, withCookie(cookies, azureCLIPKCECookieName, ""))
	if w.Code != http.StatusBadRequest || reasonOf(t, w) != reasonAzureCallbackCookiesInvalid {
		t.Fatalf("a missing verifier: %d %s, want 400 %s", w.Code, w.Body.String(), reasonAzureCallbackCookiesInvalid)
	}
	if rows := f.audit.find("cli.signin.capture"); len(rows) != 0 {
		t.Errorf("an attack-shaped refusal emitted %d audit row(s)", len(rows))
	}
}

// Every unusable start is refused before any redirect.
func TestAzureCLISignIn_StartRefusals(t *testing.T) {
	f := newAzCLIFixture(t, types.ComponentAccessRead, azCLISubA)
	for name, tc := range map[string]struct {
		arrange func()
		subject string
		id      string
		code    int
		reason  string
	}{
		"no session":            {nil, "", f.id.String(), http.StatusForbidden, reasonAzureSignInNoSession},
		"not a component id":    {nil, f.owner, "nope", http.StatusBadRequest, reasonInvalidIDParam},
		"no such row":           {nil, f.owner, uuid.NewString(), http.StatusUnprocessableEntity, reasonCLISignInNotAzure},
		"a row of another kind": {func() { f.setRow(t, types.ComponentDefinition{Hosts: []string{"api.example.com"}}) }, f.owner, f.id.String(), http.StatusUnprocessableEntity, reasonCLISignInNotAzure},
		"no console Entra": {func() {
			f.setRow(t, azCLIRowDef(types.ComponentAccessRead, azCLISubA))
			f.srv.cfg.AzureCLIEntra = func(string) ADOEntraConfig { return ADOEntraConfig{} }
		}, f.owner, f.id.String(), http.StatusNotFound, reasonAzureSignInUnconfigured},
	} {
		if tc.arrange != nil {
			tc.arrange()
		}
		w := f.start(tc.subject, tc.id)
		if w.Code != tc.code || reasonOf(t, w) != tc.reason || w.Header().Get("Location") != "" {
			t.Errorf("%s: %d %s, want %d %s and no redirect", name, w.Code, w.Body.String(), tc.code, tc.reason)
		}
	}
}

// The door is mounted for a signed-in member. A row the member may not attach
// answers exactly as the run doors answer it, before anything about the row.
func TestAzureCLISignIn_DoorAnswersAsTheAttachDoor(t *testing.T) {
	member := ssoSession(t, "sub-member", "member@corp.example", oidc.RoleUser)
	id := uuid.New()
	door := "/api/v1" + azureCLISignInRoute + "?component=" + id.String()
	fixture := func(cs *capStore) *Server {
		srv, st, _, _ := signInFixture(t, cs, types.SiteConfig{})
		cst := &cliSignInStore{signInStore: st}
		if _, err := cst.CreateComponent(context.Background(), types.Component{ID: id, Name: "Azure", Definition: azCLIRowDef(types.ComponentAccessRead, azCLISubA)}); err != nil {
			t.Fatal(err)
		}
		srv.cfg.Store = cst
		return srv
	}
	if w := doSSO(t, fixture(&capStore{enf: map[string]bool{capComponent: true}}), http.MethodGet, door, member, ""); w.Code != http.StatusForbidden || reasonOf(t, w) != "capability_component" {
		t.Errorf("ungranted sign-in = %d %s, want the attach door's refusal", w.Code, w.Body.String())
	}
	// Granted, on a deployment whose console does not sign people in with Entra.
	if w := doSSO(t, fixture(nil), http.MethodGet, door, member, ""); w.Code != http.StatusNotFound || reasonOf(t, w) != reasonAzureSignInUnconfigured {
		t.Errorf("granted sign-in with no console Entra = %d %s, want 404 %s", w.Code, w.Body.String(), reasonAzureSignInUnconfigured)
	}
}

// A row's sign-ins are for the subscriptions it pins: an update that changes
// the set purges them, and one that only reorders, re-cases or edits the access
// does not.
func TestAzureCLISignInPinChanged(t *testing.T) {
	base := azCLIRowDef(types.ComponentAccessRead, azCLISubA, azCLISubB)
	for name, tc := range map[string]struct {
		before, after types.ComponentDefinition
		want          bool
	}{
		"a subscription removed": {base, azCLIRowDef(types.ComponentAccessRead, azCLISubA), true},
		"a subscription swapped": {base, azCLIRowDef(types.ComponentAccessRead, azCLISubA, azCLISubC), true},
		"no longer an Azure CLI": {base, types.ComponentDefinition{Hosts: []string{"api.example.com"}}, true},
		"reordered and re-cased": {base, azCLIRowDef(types.ComponentAccessRead, strings.ToUpper(azCLISubB), azCLISubA), false},
		"access edited":          {base, azCLIRowDef(types.ComponentAccessWrite, azCLISubA, azCLISubB), false},
		"was never an Azure CLI": {types.ComponentDefinition{}, base, false},
	} {
		if got := azureCLISignInPinChanged(tc.before, tc.after); got != tc.want {
			t.Errorf("%s: changed = %v, want %v", name, got, tc.want)
		}
	}
}

// ── the gate ────────────────────────────────────────────────────────────────

// The component gate admits one Azure CLI row per run, only where the console
// signs people in with Entra, and only once the caller has signed in for it;
// the preview reports the missing sign-in as a setup item instead of refusing.
func TestAzureCLI_ComponentGate(t *testing.T) {
	f := newAzCLIFixture(t, types.ComponentAccessRead, azCLISubA)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil)
	admit := func(adm *componentAdmission) (attachedComponent, *componentRefusal) {
		c := attachedComponent{snapshot: f.st.attached[0], source: "org"}
		return c, f.srv.componentAzureCLIAdmitted(req, adm, &c, 0)
	}
	launch := func() *componentAdmission { return &componentAdmission{caller: f.owner, credentials: true} }

	_, refused := admit(launch())
	if refused == nil || refused.reason != reasonComponentCLISignInRequired {
		t.Fatalf("launch before a sign-in = %+v, want %s", refused, reasonComponentCLISignInRequired)
	}
	preview := &componentAdmission{caller: f.owner}
	c := attachedComponent{snapshot: f.st.attached[0], source: "org"}
	if refused := f.srv.componentAzureCLIAdmitted(req, preview, &c, 0); refused != nil || !c.needsSignIn {
		t.Fatalf("preview before a sign-in: refused %+v needsSignIn %v, want a setup item", refused, c.needsSignIn)
	}
	fact := c.fact(0, runComponents{}, composer.Baseline{})
	want := SetupItem{Kind: "cli_access", ID: "cli_access:" + fact.ID, Label: "Sign in to Azure for Azure", RequiredBy: "Azure", Status: "missing",
		Fix: &SetupFix{Action: fixSignIn, Target: signInAzureCLI, ID: fact.ID}}
	if len(fact.Requirements) != 1 || fact.Requirements[0].Fix == nil || *fact.Requirements[0].Fix != *want.Fix ||
		fact.Requirements[0].Label != want.Label || fact.Status != componentNeedsInput {
		t.Fatalf("fact requirements = %+v status %q, want %+v", fact.Requirements, fact.Status, want)
	}

	f.signIn(t)
	adm := launch()
	if _, refused := admit(adm); refused != nil {
		t.Fatalf("launch after a sign-in = %+v, want admitted", refused.refusal)
	}
	if _, refused := admit(adm); refused == nil || refused.reason != reasonComponentDefinitionInvalid {
		t.Fatalf("a second Azure CLI row = %+v, want refused", refused)
	}
	if _, refused := admit(&componentAdmission{caller: "someone-else", credentials: true}); refused == nil || refused.reason != reasonComponentCLISignInRequired {
		t.Fatalf("another person's launch = %+v, want their own sign-in asked for", refused)
	}

	f.srv.cfg.AzureCLIEntra = nil
	if _, refused := admit(launch()); refused == nil || refused.reason != reasonComponentCLIAzureNoEntra {
		t.Fatalf("no console Entra sign-in = %+v, want %s", refused, reasonComponentCLIAzureNoEntra)
	}
}

// Through the three run doors. Not signed in: launch and Review refuse, and the
// preview lists a sign_in item for the row, the door's two hosts as injected,
// and one proxy-only credential that is the person's own, renewed while the
// run lives and gone when it ends. Signed in, Review admits the row. Without a
// console Entra sign-in every door refuses it.
func TestAzureCLI_DoorsAndPreviewFacts(t *testing.T) {
	const id = "6f0c1d2e-0000-4000-8000-0000000c0a07"
	f := newComponentFixture(t)
	f.srv.cfg.AzureCLIEntra = func(row string) ADOEntraConfig {
		return ADOEntraConfig{RowID: row, TenantID: "tenant-1", ClientID: "console-app", RedirectURL: adoCallbackURL,
			LoginClientID: "console-app", LoginTenantID: "tenant-1"}
	}
	ref := f.org(id, azCLIRowDef(types.ComponentAccessRead, azCLISubA))
	for _, door := range componentDoors[:2] {
		w := f.ask(t, door, componentBody(ref))
		if w.Code != http.StatusUnprocessableEntity || reasonOf(t, w) != reasonComponentCLISignInRequired {
			t.Errorf("%s = %d %s, want 422 %s", door, w.Code, w.Body.String(), reasonComponentCLISignInRequired)
		}
	}
	ans := decodeAccess(t, componentDoors[2], f.ask(t, componentDoors[2], componentBody(ref)))
	i := slices.IndexFunc(ans.Access.Setup, func(it SetupItem) bool { return it.Fix != nil && it.Fix.Target == signInAzureCLI })
	if i < 0 || ans.Access.Setup[i].Fix.Action != fixSignIn || ans.Access.Setup[i].Fix.ID != id {
		t.Fatalf("setup = %+v, want a sign_in item for the row", ans.Access.Setup)
	}
	if row, ok := accessRow(ans.Access, id); !ok || row.Kind != accessKindCLI || row.Status != accessNeedsSetup {
		t.Errorf("row = %+v (%t), want the CLI row needing setup", row, ok)
	}
	c := slices.IndexFunc(ans.Access.Credentials, func(c client.AccessCredential) bool { return c.Kind == credKindAzureARM })
	if c < 0 || ans.Access.Credentials[c].Delivery != deliveryProxy || ans.Access.Credentials[c].Owner != credOwnerOwn ||
		!ans.Access.Credentials[c].Renewed || ans.Access.Credentials[c].AfterEnd != afterEndGone {
		t.Errorf("credentials = %+v, want one proxy-only credential of the person's own, renewed and gone at the end", ans.Access.Credentials)
	}
	for _, host := range []string{armscope.Host, proxy.AzureLoginHost} {
		if h, ok := accessHost(ans.Access, host); !ok || h.Via != viaInjected || h.Verdict != verdictAllowed {
			t.Errorf("%s = %+v (%t), want injected and allowed", host, h, ok)
		}
	}

	blob := adoEntraBlob{RefreshToken: "refresh", Scopes: []string{azARMScope}, TenantID: "tenant-1", ClientID: "console-app",
		Subject: capSub, CapturedAt: time.Now().UTC(), ExpiresAt: time.Now().Add(time.Hour).UTC()}
	if err := f.srv.cfg.Secrets.For(capSub).Put(context.Background(), cliSecretName(id, providerEntraPart), mustJSON(blob)); err != nil {
		t.Fatal(err)
	}
	if w := f.ask(t, componentDoors[1], componentBody(ref)); w.Code != http.StatusOK {
		t.Fatalf("Review after a sign-in = %d %s, want 200", w.Code, w.Body.String())
	}

	f.srv.cfg.AzureCLIEntra = nil
	for _, door := range componentDoors {
		w := f.ask(t, door, componentBody(ref))
		if w.Code != http.StatusUnprocessableEntity || reasonOf(t, w) != reasonComponentCLIAzureNoEntra {
			t.Errorf("%s with no console Entra sign-in = %d %s, want 422 %s", door, w.Code, w.Body.String(), reasonComponentCLIAzureNoEntra)
		}
	}
}

// ── dispatch ────────────────────────────────────────────────────────────────

// Dispatch stages the profile and the inert cache, allows the door's two hosts
// on 443, and authors one TLS-only grant whose snapshot pins the row, the
// owner, the tenant, the audience, the subscriptions and the access.
func TestAzureCLI_DispatchStaging(t *testing.T) {
	f := newAzCLIFixture(t, types.ComponentAccessWrite, strings.ToUpper(azCLISubB), azCLISubA)
	f.signIn(t)
	inj, plan, env, policy, ok := f.dispatch()
	if !ok {
		t.Fatalf("dispatch refused: %q", f.st.hint)
	}
	subs := []string{azCLISubA, azCLISubB}
	if g := plan.gateFor(inj); g == nil || g.TenantID != f.fake.TenantID() || !slices.Equal(g.Subscriptions, subs) || !g.Write {
		t.Fatalf("door grant = %+v, want the tenant, the lowercase sorted subscriptions and write", g)
	}
	if plan.gateFor(nil) != nil {
		t.Error("the door outlived its credential")
	}
	for _, host := range []string{armscope.Host + ":443", proxy.AzureLoginHost + ":443"} {
		if !slices.Contains(policy.AllowedDomains, host) {
			t.Errorf("allowed domains %v lack %s", policy.AllowedDomains, host)
		}
	}
	if len(inj) != 1 || inj[0].Rule.Host != armscope.Host || !inj[0].Rule.RequireTLS || inj[0].Rule.Header != "Authorization" ||
		inj[0].Rule.SecretName != "wardyn-cli-"+f.id.String()+"-entra" || len(inj[0].Rule.PinRoutes) != 0 {
		t.Fatalf("injection = %+v, want one TLS-only rule on Resource Manager naming the row's sign-in", inj)
	}
	var scope struct {
		Snapshot azureARMGrantSnapshot `json:"snapshot"`
	}
	if err := json.Unmarshal(f.st.grants[0].Spec.Scope, &scope); err != nil {
		t.Fatal(err)
	}
	want := azureARMGrantSnapshot{Mechanism: azureCLIMechanism, ComponentID: f.id.String(), OwnerSubject: f.owner, TenantID: f.fake.TenantID(),
		Audience: azureARMAudience, Subscriptions: subs, Access: types.ComponentAccessWrite}
	if got := scope.Snapshot; got.Mechanism != want.Mechanism || got.ComponentID != want.ComponentID || got.OwnerSubject != want.OwnerSubject ||
		got.TenantID != want.TenantID || got.Audience != want.Audience || !slices.Equal(got.Subscriptions, want.Subscriptions) || got.Access != want.Access {
		t.Fatalf("snapshot = %+v, want %+v", got, want)
	}
	if env[envAzureTelemetry] != "0" {
		t.Errorf("%s = %q, want 0", envAzureTelemetry, env[envAzureTelemetry])
	}
	var profile struct {
		Subscriptions []struct {
			ID, TenantID string
			IsDefault    bool
			User         struct{ Name string }
		}
	}
	files := azCLIFiles(t, env)
	if err := json.Unmarshal([]byte(files[".azure/azureProfile.json"]), &profile); err != nil || len(profile.Subscriptions) != 2 {
		t.Fatalf("profile %s (%v), want two subscriptions", files[".azure/azureProfile.json"], err)
	}
	for i, sub := range profile.Subscriptions {
		if sub.ID != subs[i] || sub.TenantID != f.fake.TenantID() || sub.IsDefault != (i == 0) || sub.User.Name != azureCLIAccountName {
			t.Errorf("profile subscription %d = %+v", i, sub)
		}
	}
}

// A run the lane cannot serve without a token in the sandbox is refused, and
// nothing is staged for it.
func TestAzureCLI_DispatchRefusals(t *testing.T) {
	for name, arrange := range map[string]func(f *azCLIFixture) (ca string){
		"not signed in":    func(*azCLIFixture) string { return "ca" },
		"no per-run CA":    func(f *azCLIFixture) string { f.signIn(t); return "" },
		"no console Entra": func(f *azCLIFixture) string { f.signIn(t); f.srv.cfg.AzureCLIEntra = nil; return "ca" },
		"the admin token": func(f *azCLIFixture) string {
			f.signIn(t)
			f.st.run.CreatedBy = ""
			return "ca"
		},
		"a sign-in that has ended": func(f *azCLIFixture) string {
			f.signIn(t)
			ec, _ := azureARMCapture(f.id.String())
			blob, _ := f.stored(t, f.owner)
			blob.DeadAt, blob.DeadReason = time.Now(), ADOEntraFailureInteractionRequired
			if err := f.srv.storeEntraBlob(context.Background(), f.owner, ec, blob); err != nil {
				t.Fatal(err)
			}
			return "ca"
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newAzCLIFixture(t, types.ComponentAccessRead, azCLISubA)
			ca := arrange(f)
			env, policy := map[string]string{}, &types.RunPolicySpec{}
			inj, plan, ok := f.srv.authorAzureCLI(context.Background(), f.st.run, &azureCLIDispatch{ComponentID: f.id.String(), Azure: *f.st.attached[0].Definition.CLI.Azure},
				ca, ca, policy, env, nil)
			if ok || len(inj) != 0 || plan.gate != nil || len(env) != 0 || len(policy.AllowedDomains) != 0 || len(f.st.grants) != 0 {
				t.Fatalf("ok %v injections %v gate %v env %v domains %v grants %d, want a refusal that stages nothing", ok, inj, plan.gate, env, policy.AllowedDomains, len(f.st.grants))
			}
			if f.st.failedTo != types.RunFailed || f.st.hint == "" {
				t.Errorf("run state %q hint %q, want FAILED with a reason", f.st.failedTo, f.st.hint)
			}
		})
	}
}

// NO REAL TOKEN IN THE SANDBOX. Whatever the person's sign-in holds and
// whatever Entra issues for it, the sandbox gets a profile and a cache whose
// one access token is the sentinel: dispatch redeems nothing, and neither the
// refresh token nor any access token Entra issued for this person appears in
// the sandbox's environment, its staged files, or the proxy's door grant. The
// token exists only in the sink's answer to the proxy.
func TestAzureCLI_NoRealTokenInSandbox(t *testing.T) {
	f := newAzCLIFixture(t, types.ComponentAccessWrite, azCLISubA)
	var issued []string
	f.fake.OnIssue(func(it entrafake.IssuedToken) { issued = append(issued, it.AccessToken) })
	f.signIn(t)
	blob, _ := f.stored(t, f.owner)
	before := len(issued)
	inj, plan, env, _, ok := f.dispatch()
	if !ok {
		t.Fatalf("dispatch refused: %q", f.st.hint)
	}
	if len(issued) != before {
		t.Fatalf("dispatch redeemed the sign-in %d time(s); only the proxy's resolve may", len(issued)-before)
	}
	// The sink's answer: the one place a real token is written.
	w := f.resolveRule(inj[0].Rule, inj[0].GrantID, f.owner, "?phase=boot")
	var resp types.ResolvedInjection
	if err := json.Unmarshal(w.Body.Bytes(), &resp); w.Code != http.StatusOK || err != nil {
		t.Fatalf("resolve: %d %s", w.Code, w.Body.String())
	}
	real := append(slices.Clone(issued), blob.RefreshToken, strings.TrimPrefix(resp.Value, "Bearer "))
	if len(issued) == before || slices.Contains(real, "") {
		t.Fatalf("the fixture issued no token to look for: %q", real)
	}

	files := azCLIFiles(t, env)
	sandbox := []string{string(mustJSON(env)), string(mustJSON(plan.gateFor(inj))), string(mustJSON(inj))}
	for _, body := range files {
		sandbox = append(sandbox, body)
	}
	for _, grant := range f.st.grants {
		sandbox = append(sandbox, string(grant.Spec.Scope))
	}
	for _, text := range sandbox {
		for _, secret := range real {
			if strings.Contains(text, secret) {
				t.Fatalf("a real token is staged outside the sink's answer: %.80q…", text)
			}
		}
	}

	var cache struct {
		AccessToken  map[string]map[string]string
		RefreshToken map[string]any
		IdToken      map[string]any
		Account      map[string]map[string]string
	}
	if err := json.Unmarshal([]byte(files[".azure/msal_token_cache.json"]), &cache); err != nil {
		t.Fatal(err)
	}
	if len(cache.AccessToken) != 1 || len(cache.Account) != 1 || len(cache.RefreshToken) != 0 || len(cache.IdToken) != 0 {
		t.Fatalf("cache = %+v, want one account, one access token, no refresh token and no id token", cache)
	}
	tid := f.fake.TenantID()
	key := strings.ToLower(azureCLIAccountID + "." + tid + "-login.microsoftonline.com-accesstoken-" + azureCLIClientID + "-" + tid + "-" + azureCLICacheScope)
	entry, ok := cache.AccessToken[key]
	if !ok || entry["secret"] != azureCLITokenSentinel || entry["target"] != azureCLICacheScope || entry["expires_on"] != azureCLICacheExpiry {
		t.Fatalf("access token entry %q = %+v, want the sentinel under the CLI's own key", key, cache.AccessToken)
	}
	if _, has := entry["refresh_on"]; has {
		t.Error("the entry carries refresh_on, which makes the CLI's library renew")
	}
	if len(files) != 2 {
		t.Errorf("staged files %v, want the profile and the cache alone", files)
	}
}

// ── the sink ────────────────────────────────────────────────────────────────

// The sink redeems the owner's sign-in for Resource Manager and answers one
// Bearer header for that host; every way a resolve could re-authorize the run
// against something it was not dispatched with is a 403 that redeems nothing:
// another person's run token, a hand-authored grant, a snapshot for another
// row or another audience, another host, a row that was re-pinned, re-scoped,
// deleted or is no longer on the run, and a console that moved tenant.
func TestAzureARM_ResolveSnapshotPinned(t *testing.T) {
	type change func(f *azCLIFixture, in runner.InjectionGrant) (sub string, rule egress.InjectionRule, grant uuid.UUID)
	same := func(f *azCLIFixture, in runner.InjectionGrant) (string, egress.InjectionRule, uuid.UUID) {
		return f.owner, in.Rule, in.GrantID
	}
	handAuthored := func(snapshot func(azureARMGrantSnapshot) any) change {
		return func(f *azCLIFixture, in runner.InjectionGrant) (string, egress.InjectionRule, uuid.UUID) {
			var scope struct {
				Snapshot azureARMGrantSnapshot `json:"snapshot"`
			}
			_ = json.Unmarshal(f.st.grants[0].Spec.Scope, &scope)
			id := uuid.New()
			body := map[string]any{"host": armscope.Host, "secret_name": in.Rule.SecretName}
			if sn := snapshot(scope.Snapshot); sn != nil {
				body["snapshot"] = sn
			}
			f.st.grants = append(f.st.grants, types.CredentialGrant{ID: id, RunID: f.st.run.ID,
				Spec: types.GrantSpec{Kind: types.GrantAPIKey, Scope: mustJSON(body)}})
			return f.owner, in.Rule, id
		}
	}
	row := func(def types.ComponentDefinition) change {
		return func(f *azCLIFixture, in runner.InjectionGrant) (string, egress.InjectionRule, uuid.UUID) {
			f.setRow(t, def)
			return same(f, in)
		}
	}
	for _, tc := range []struct {
		name   string
		change change
		code   int
		reason string
	}{
		{"live and equal", same, http.StatusOK, ""},
		{"the row re-cased and reordered", row(azCLIRowDef(types.ComponentAccessRead, strings.ToUpper(azCLISubB), azCLISubA)), http.StatusOK, ""},
		{"another person's run token", func(f *azCLIFixture, in runner.InjectionGrant) (string, egress.InjectionRule, uuid.UUID) {
			return "someone-else", in.Rule, in.GrantID
		}, http.StatusForbidden, reasonOwnerNotCaller},
		{"a grant with no snapshot", handAuthored(func(azureARMGrantSnapshot) any { return nil }), http.StatusForbidden, reasonMissingScopeSnapshot},
		{"a snapshot naming another audience", handAuthored(func(sn azureARMGrantSnapshot) any {
			sn.Audience = azureFoundryAudience
			return sn
		}), http.StatusForbidden, reasonMissingScopeSnapshot},
		{"a model provider's snapshot", handAuthored(func(sn azureARMGrantSnapshot) any {
			return azureGrantSnapshot{ProviderUID: sn.ComponentID, OwnerSubject: sn.OwnerSubject, Audience: azureFoundryAudience}
		}), http.StatusForbidden, reasonMissingScopeSnapshot},
		{"a snapshot for another row", func(f *azCLIFixture, in runner.InjectionGrant) (string, egress.InjectionRule, uuid.UUID) {
			in.Rule.SecretName = cliSecretName(uuid.NewString(), providerEntraPart)
			return same(f, in)
		}, http.StatusForbidden, reasonMissingScopeSnapshot},
		{"a host that is not Resource Manager", func(f *azCLIFixture, in runner.InjectionGrant) (string, egress.InjectionRule, uuid.UUID) {
			in.Rule.Host = "graph.microsoft.com"
			return same(f, in)
		}, http.StatusForbidden, reasonHostNotEndpoint},
		{"the row re-pinned to another subscription", row(azCLIRowDef(types.ComponentAccessRead, azCLISubA, azCLISubC)), http.StatusForbidden, reasonScopeChanged},
		{"the row narrowed to one subscription", row(azCLIRowDef(types.ComponentAccessRead, azCLISubA)), http.StatusForbidden, reasonScopeChanged},
		{"the row's access changed", row(azCLIRowDef(types.ComponentAccessWrite, azCLISubA, azCLISubB)), http.StatusForbidden, reasonScopeChanged},
		{"the row deleted", row(types.ComponentDefinition{}), http.StatusForbidden, reasonScopeChanged},
		{"the row turned into another kind", row(types.ComponentDefinition{Hosts: []string{"api.example.com"}}), http.StatusForbidden, reasonScopeChanged},
		{"the run no longer carries the row", func(f *azCLIFixture, in runner.InjectionGrant) (string, egress.InjectionRule, uuid.UUID) {
			f.st.attached = nil
			return same(f, in)
		}, http.StatusForbidden, reasonScopeChanged},
		{"the console moved tenant", func(f *azCLIFixture, in runner.InjectionGrant) (string, egress.InjectionRule, uuid.UUID) {
			f.app.TenantID, f.app.LoginTenantID = "another-tenant", "another-tenant"
			return same(f, in)
		}, http.StatusForbidden, reasonScopeChanged},
		{"the console no longer signs in with Entra", func(f *azCLIFixture, in runner.InjectionGrant) (string, egress.InjectionRule, uuid.UUID) {
			f.srv.cfg.AzureCLIEntra = nil
			return same(f, in)
		}, http.StatusForbidden, reasonSigninUnconfigured},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAzCLIFixture(t, types.ComponentAccessRead, azCLISubA, azCLISubB)
			f.signIn(t)
			inj, _, _, _, ok := f.dispatch()
			if !ok || len(inj) != 1 {
				t.Fatalf("dispatch: ok %v injections %d (%q)", ok, len(inj), f.st.hint)
			}
			var carried []string
			f.fake.OnIssue(func(it entrafake.IssuedToken) { carried = append(carried, it.Scopes...) })
			sub, rule, grant := tc.change(f, inj[0])
			w := f.resolveRule(rule, grant, sub, "")
			if w.Code != tc.code {
				t.Fatalf("resolve = %d %s, want %d", w.Code, w.Body.String(), tc.code)
			}
			rows := f.audit.find("secret.read")
			if tc.code != http.StatusOK {
				if reasonOf(t, w) != tc.reason {
					t.Errorf("reason = %q, want %q", reasonOf(t, w), tc.reason)
				}
				if len(carried) != 0 {
					t.Errorf("a refused resolve redeemed the sign-in: %v", carried)
				}
				if len(rows) == 0 || rows[len(rows)-1].Outcome != "failure" {
					t.Errorf("secret.read rows = %+v, want a failure row", rows)
				}
				return
			}
			var resp types.ResolvedInjection
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if resp.Host != armscope.Host || resp.Header != "Authorization" || !strings.HasPrefix(resp.Value, "Bearer ") || len(resp.Value) < 20 || resp.ExpiresAt == 0 {
				t.Errorf("response = %+v, want one expiring Bearer header for Resource Manager", resp)
			}
			if !slices.Contains(carried, azARMScope) || slices.Contains(carried, azFoundryScope) {
				t.Errorf("the redeemed token carries %v, want %s and not %s", carried, azARMScope, azFoundryScope)
			}
			last := rows[len(rows)-1]
			if last.Outcome != "success" || !strings.Contains(string(last.Data), azARMScope) || strings.Contains(string(last.Data), strings.TrimPrefix(resp.Value, "Bearer ")) {
				t.Errorf("secret.read row = %+v, want a success naming the granted scope and never the token", last)
			}
		})
	}
}

// A name that is not an Azure CLI sign-in is not this arm's.
func TestAzureARM_ResolveStandsAsideForOtherNames(t *testing.T) {
	f := newAzCLIFixture(t, types.ComponentAccessRead, azCLISubA)
	for _, name := range []string{
		cliSecretName(f.id.String(), providerSSOPart), providerSecretName(azUIDAnthropic, providerEntraPart),
		"wardyn-cli--entra", "some-token", types.AWSSSOAccessTokenSecret,
	} {
		w := f.resolveRule(egress.InjectionRule{Host: armscope.Host, SecretName: name}, uuid.New(), f.owner, "")
		if w.Code != http.StatusTeapot {
			t.Errorf("%q: the arm answered %d %s, want it to stand aside", name, w.Code, w.Body.String())
		}
	}
}

// A sign-in that dies mid-run holds the request on a sign-in request for this
// row on this lane, which only the owner's new sign-in for the row answers; at
// the proxy's boot there is no request to hold and the run fails with the remedy.
func TestAzureARM_DeadSignInHoldsThenResumes(t *testing.T) {
	f := newAzCLIFixture(t, types.ComponentAccessRead, azCLISubA)
	f.signIn(t)
	inj, _, _, _, _ := f.dispatch()
	resolve := func(query string) *httptest.ResponseRecorder {
		return f.resolveRule(inj[0].Rule, inj[0].GrantID, f.owner, query)
	}
	if w := resolve("?phase=boot"); w.Code != http.StatusOK {
		t.Fatalf("boot resolve: %d %s", w.Code, w.Body.String())
	}
	f.fake.SetInvalidGrant(true)
	id := pendingID(t, resolve(""), reauthPendingState)
	held, _ := f.st.approvals.Get(context.Background(), id)
	sc, ok := signInScopeFor(held, azureCLIMechanism)
	if !ok || sc.Owner != f.owner || sc.ProviderID != f.id.String() {
		t.Fatalf("raised row = %+v, want an Azure CLI sign-in request for the owner and the row", held)
	}
	if _, foundry := signInScopeFor(held, azureApprovalLane); foundry {
		t.Fatal("the row is visible to the model-provider lane")
	}
	if _, found := f.stored(t, f.owner); found {
		t.Fatal("a dead sign-in is still stored")
	}
	if again := pendingID(t, resolve(""), reauthPendingState); again != id {
		t.Fatalf("a second resolve raised %s, want the same request %s", again, id)
	}
	if got := f.srv.reconcileAzureReauthOnRead(context.Background(), held); got.State != types.ApprovalPending {
		t.Fatalf("resolved before any sign-in: %s", got.State)
	}

	f.fake.SetInvalidGrant(false)
	f.srv.cfg.Now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	f.signIn(t)
	if got, _ := f.st.approvals.Get(context.Background(), id); got.State != types.ApprovalApproved {
		t.Fatalf("after the sign-in the request is %s, want APPROVED by the capture itself", got.State)
	}
	if w := resolve(""); w.Code != http.StatusOK {
		t.Fatalf("the held request's re-resolve: %d %s, want 200", w.Code, w.Body.String())
	}

	f.fake.SetInvalidGrant(true)
	boot := newAzCLIFixture(t, types.ComponentAccessRead, azCLISubA)
	boot.signIn(t)
	binj, _, _, _, _ := boot.dispatch()
	boot.fake.SetInvalidGrant(true)
	w := boot.resolveRule(binj[0].Rule, binj[0].GrantID, boot.owner, "?phase=boot")
	if w.Code != http.StatusForbidden || reasonOf(t, w) != string(ADOEntraFailureDeadCredential) || !strings.Contains(boot.st.hint, "sign in to Azure again") {
		t.Fatalf("boot resolve = %d %s (hint %q), want a 403 that leaves the remedy as the run's hint", w.Code, w.Body.String(), boot.st.hint)
	}
	if n := len(boot.st.approvals.requested); n != 0 {
		t.Errorf("a boot resolve raised %d request(s), want none", n)
	}
}
