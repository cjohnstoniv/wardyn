// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/test/adofake"
	"github.com/cjohnstoniv/wardyn/test/entrafake"
)

const mintTestSecret = "console-secret"

// mintFixture is a minted_pat row on the console's own confidential app: an
// Entra fake that issues the two token permissions, and an Azure DevOps fake
// whose token API accepts every access token the Entra fake issues.
type mintFixture struct {
	*adoFixture
	ado     *adofake.Server
	subject string
	site    types.SiteConfig
}

func newMintFixture(t *testing.T) *mintFixture {
	t.Helper()
	f := newADOFixture(t)
	f.fake.SetClientSecret(mintTestSecret)
	f.fake.SetConsentedScopes(adoscope.MintScopes()...)
	f.cfg.Scopes = adoscope.MintScopes()
	f.cfg.TokenMode = types.ADOTokenModeMintedPAT
	f.cfg.ClientSecret = mintTestSecret
	ado := adofake.New()
	t.Cleanup(ado.Close)
	f.cfg.PATAPIOverride = ado.URL()
	f.fake.OnIssue(func(it entrafake.IssuedToken) {
		scopes := make([]string, 0, len(it.Scopes))
		for _, sc := range it.Scopes {
			scopes = append(scopes, adoUnqualified(sc))
		}
		ado.RegisterToken(it.AccessToken, scopes...)
	})
	// The fake's lifespan limit is measured against the wall clock.
	f.srv.cfg.Now = time.Now
	mf := &mintFixture{adoFixture: f, ado: ado, subject: f.fake.Subject()}
	mf.site = adoSite(mf.mintedRow(), types.GitProvider{ID: "gh", Kind: types.GitProviderGitHub,
		BaseURLs: []string{"https://github.com/acme"}})
	f.srv.cfg.Store = &scmTestStore{site: mf.site}
	return mf
}

func (mf *mintFixture) mintedRow() types.GitProvider {
	row := adoEntraTestRow()
	row.ID = mf.cfg.RowID
	row.Entra.TenantID, row.Entra.ClientID = mf.cfg.TenantID, mf.cfg.ClientID
	row.Entra.TokenMode = types.ADOTokenModeMintedPAT
	return row
}

func (mf *mintFixture) connect(t *testing.T) {
	t.Helper()
	if w := mf.capture(t, mf.subject); w.Code != http.StatusFound || !strings.Contains(w.Header().Get("Location"), "connected") {
		t.Fatalf("capture: status %d location %q body %q", w.Code, w.Header().Get("Location"), w.Body.String())
	}
}

func (mf *mintFixture) orgCheck(t *testing.T, id string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/workspace-providers/git/"+id+"/org-check", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", id)
	r = r.WithContext(context.WithValue(withOIDCHuman(r.Context(), mf.subject), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	mf.srv.handleADOOrgCheck(w, r)
	return w
}

func (mf *mintFixture) orgCheckResult(t *testing.T) adoOrgCheckResult {
	t.Helper()
	w := mf.orgCheck(t, mf.cfg.RowID)
	if w.Code != http.StatusOK {
		t.Fatalf("org check: status %d body %q", w.Code, w.Body.String())
	}
	var res adoOrgCheckResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rows := mf.audit.find(adoPATAuditOrgCheck); len(rows) != 1 {
		t.Errorf("org_check audit rows = %d, want 1", len(rows))
	}
	return res
}

// ── S4: what the login and the sign-in door ask for ─────────────────────────

func TestLoginScopes_MintedRowAsksOnlyForTheMintScopes(t *testing.T) {
	mf := newMintFixture(t)
	got := mf.srv.LoginScopes(context.Background())
	if want := append(adoscope.MintScopes(), entraOfflineAccessScope); !slices.Equal(got, want) {
		t.Fatalf("LoginScopes = %v, want exactly %v", got, want)
	}
}

func TestLoginScopes_BearerRowNeverAsksForATokenScope(t *testing.T) {
	f := newADOFixture(t)
	if got := f.srv.LoginScopes(context.Background()); len(got) == 0 || slices.ContainsFunc(got, adoscope.IsTokenScope) {
		t.Fatalf("LoginScopes = %v, want the ceiling with no token scope", got)
	}
	// A bearer configuration carrying one is refused whole, never trimmed.
	f.cfg.Scopes = append(f.cfg.Scopes, adoscope.ResourceID+"/vso.pats_manage")
	if got := f.srv.LoginScopes(context.Background()); got != nil {
		t.Fatalf("LoginScopes = %v for a bearer row naming vso.pats_manage, want nil", got)
	}
	// And a minted configuration naming a capability scope likewise.
	mf := newMintFixture(t)
	mf.cfg.Scopes = append(mf.cfg.Scopes, adoscope.ResourceID+"/vso.code")
	if got := mf.srv.LoginScopes(context.Background()); got != nil {
		t.Fatalf("LoginScopes = %v for a minted row naming vso.code, want nil", got)
	}
}

func TestADOSignIn_MintedRowRequestsTheMintScopes(t *testing.T) {
	mf := newMintFixture(t)
	authURL, _ := mf.signIn(t, mf.subject, "")
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Fields(u.Query().Get("scope"))
	want := append(adoscope.MintScopes(), entraOfflineAccessScope, entraOpenIDScope)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("authorize scope = %v, want %v", got, want)
	}
	mf.connect(t)
	blob, found := mf.stored(t, mf.subject)
	if !found || !adoBlobCoversMint(blob) {
		t.Fatalf("stored = %+v found=%v, want both mint scopes", blob, found)
	}
	if rows := mf.audit.find(adoPATAuditConnect); len(rows) != 1 {
		t.Errorf("ado_pat.connect rows = %d, want 1", len(rows))
	}
}

// ── S1 ──────────────────────────────────────────────────────────────────────

// TestMintAccess_RowNamingAnotherAppNeverMintsAsAPublicClient is F2's exact
// case: a minted_pat row naming an application that is not the console's gets
// no secret, and the Entra fake would redeem it as a public client. It must
// never be asked.
func TestMintAccess_RowNamingAnotherAppNeverMintsAsAPublicClient(t *testing.T) {
	mf := newMintFixture(t)
	mf.fake.SetClientSecret("") // the other application is public: it would answer
	mf.connect(t)
	blob, _ := mf.stored(t, mf.subject)

	var issued int
	mf.fake.OnIssue(func(entrafake.IssuedToken) { issued++ })
	cfg := mf.cfg
	cfg.LoginClientID = "ffffffff-0000-1111-2222-333333333333" // the console signs in with another app
	cfg.ClientSecret = ""                                       // so the source sends this row no secret
	_, err := mf.srv.mintAccess(context.Background(), cfg, mf.subject)
	if !errors.Is(err, ErrADOMintNeedsSecret) {
		t.Fatalf("mintAccess err = %v, want ErrADOMintNeedsSecret", err)
	}
	if live, known := mf.fake.RefreshTokenState(blob.RefreshToken); issued != 0 || !live || !known {
		t.Fatalf("the refresh token was redeemed (issued=%d live=%v known=%v): a row naming another app minted as a public client", issued, live, known)
	}
	if _, err := mf.srv.mintADOPAT(context.Background(), cfg, mf.subject, "contoso",
		adoPATRequest{DisplayName: "x", Scope: "vso.code", ValidTo: time.Now().Add(time.Hour)}); !errors.Is(err, ErrADOMintNeedsSecret) {
		t.Fatalf("mintADOPAT err = %v, want ErrADOMintNeedsSecret", err)
	}
	if n := mf.ado.Count(adofake.EndpointPatsCreate); n != 0 {
		t.Fatalf("%d token creates reached Azure DevOps, want 0", n)
	}
}

func TestMintAccess_RefusesAnEmptySecret(t *testing.T) {
	mf := newMintFixture(t)
	mf.connect(t)
	cfg := mf.cfg
	cfg.ClientSecret = ""
	if _, err := mf.srv.mintAccess(context.Background(), cfg, mf.subject); !errors.Is(err, ErrADOMintNeedsSecret) {
		t.Fatalf("mintAccess err = %v, want ErrADOMintNeedsSecret", err)
	}
	if _, err := mf.srv.mintAccess(context.Background(), mf.cfg, mf.subject); err != nil {
		t.Fatalf("mintAccess with the secret: %v", err)
	}
}

// TestADOPAT_S1FirstReadIsUnusable: a minted_pat row the console cannot
// redeem with its own secret — another application, or no secret at all —
// widens no login, captures nothing, refuses the sign-in door with the reason,
// and reads red in the setup status with it.
func TestADOPAT_S1FirstReadIsUnusable(t *testing.T) {
	for name, mangle := range map[string]func(*ADOEntraConfig){
		"another application": func(c *ADOEntraConfig) {
			c.ClientID = "ffffffff-0000-1111-2222-333333333333"
			c.ClientSecret = ""
		},
		"no console secret": func(c *ADOEntraConfig) { c.ClientSecret = "" },
	} {
		t.Run(name, func(t *testing.T) {
			mf := newMintFixture(t)
			mangle(&mf.cfg)
			ctx := context.Background()
			if got := mf.srv.LoginScopes(ctx); got != nil {
				t.Errorf("LoginScopes = %v, want nil", got)
			}
			mf.srv.CaptureLoginGrant(ctx, mf.subject, oidc.LoginGrant{RefreshToken: "rt-0123456789abcdef",
				Scope: strings.Join(adoscope.MintScopes(), " "), Expiry: time.Now().Add(time.Hour)})
			if _, found := mf.stored(t, mf.subject); found {
				t.Error("an S1-unusable row captured a minting credential")
			}
			r := httptest.NewRequest(http.MethodGet, "/api/v1/scm/azure-devops/signin", nil)
			r = r.WithContext(withOIDCHuman(r.Context(), mf.subject))
			w := httptest.NewRecorder()
			mf.srv.handleADOSignIn(w, r)
			if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), ReasonADOPATNeedsConsoleApp) {
				t.Errorf("sign-in door: status %d body %q, want 409 %s", w.Code, w.Body.String(), ReasonADOPATNeedsConsoleApp)
			}
			row := mf.mintedRow()
			row.Entra.ClientID = mf.cfg.ClientID
			sc := adoSite(row)
			got := mf.srv.scmAccessValue(ctx, sc, mf.subject)
			if got.State != modelAccessExpiredSignin || got.Cause != ReasonADOPATNeedsConsoleApp {
				t.Errorf("setup status scm_access = %+v, want expired_signin / %s", got, ReasonADOPATNeedsConsoleApp)
			}
		})
	}
}

// ── S2 ──────────────────────────────────────────────────────────────────────

// seedBearer puts an access token with granted scopes in the resolve's cache,
// as a redemption that returned them would.
func (rf *adoResolveFixture) seedBearer(t *testing.T, granted []string) {
	t.Helper()
	req := rf.srv.adoRequestScopes(context.Background(), rf.cfg, rf.subject)
	key := strings.Join([]string{rf.subject, rf.cfg.RowID, rf.cfg.TenantID, rf.cfg.ClientID, strings.Join(req, " ")}, "\x00")
	rf.srv.adoEntraTokens.put(key, ADOEntraAccess{AccessToken: "fake-entra-access-seeded", Scopes: granted, ExpiresAt: adoTestNow.Add(time.Hour)})
}

func TestInjectionADO_S2RefusesABearerThatCanMintOrReportsNothing(t *testing.T) {
	for reason, granted := range map[string][]string{
		reasonBearerMintScopes:   {adoscope.ResourceID + "/vso.code", adoscope.ResourceID + "/vso.pats_manage"},
		reasonBearerScopeUnknown: nil,
	} {
		t.Run(reason, func(t *testing.T) {
			rf := newADOResolveFixture(t)
			rf.seedBearer(t, granted)
			w := rf.resolve(t, rf.subject, "dev.azure.com")
			if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"reason":"`+reason+`"`) {
				t.Fatalf("status %d body %q, want 403 reason %s", w.Code, w.Body.String(), reason)
			}
			if strings.Contains(w.Body.String(), "fake-entra-access-seeded") {
				t.Fatal("the bearer reached the response")
			}
			rows := rf.audit.find(adoBearerAuditRefusedMint)
			if len(rows) != 1 || rows[0].Outcome != "denied" {
				t.Fatalf("%s rows = %+v, want one denied row", adoBearerAuditRefusedMint, rows)
			}
			var d map[string]any
			_ = json.Unmarshal(rows[0].Data, &d)
			if d["reason"] != reason {
				t.Errorf("audit reason = %v, want %s", d["reason"], reason)
			}
		})
	}
}

// ── the organisation check ─────────────────────────────────────────────────

func TestOrgCheck_GrantedAcceptedAndOff(t *testing.T) {
	mf := newMintFixture(t)
	mf.connect(t)
	res := mf.orgCheckResult(t)
	if res.Permissions != adoOrgCheckGranted || res.TokenLife != adoOrgCheckAccepted || res.Lifespan != adoOrgCheckOff ||
		res.Organisation != "contoso" || res.PATMaxHours != types.ADOPATMaxHoursDefault || len(res.Unrevoked) != 0 {
		t.Fatalf("result = %+v, want granted / accepted / off for contoso at 8 hours", res)
	}
	if c, r := mf.ado.Count(adofake.EndpointPatsCreate), mf.ado.Count(adofake.EndpointPatsRevoke); c != 2 || r != 2 {
		t.Fatalf("creates=%d revokes=%d, want both canaries created and revoked", c, r)
	}
}

func TestOrgCheck_LifespanOnOnlyOnTheLifespanViolation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		limit time.Duration
		err   adofake.PatTokenError
		want  string
	}{
		{"policy on", 7 * 24 * time.Hour, adofake.PatTokenErrorLifespanPolicyViolation, adoOrgCheckOn},
		{"invalidValidTo is unknown", 30 * 24 * time.Hour, adofake.PatTokenErrorInvalidValidTo, adoOrgCheckUnknown},
		{"any other refusal is unknown", 30 * 24 * time.Hour, adofake.PatTokenErrorFullScopePolicyViolation, adoOrgCheckUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mf := newMintFixture(t)
			mf.connect(t)
			mf.ado.SetPatLifespanLimit(tc.limit, tc.err)
			res := mf.orgCheckResult(t)
			if res.TokenLife != adoOrgCheckAccepted || res.Lifespan != tc.want {
				t.Fatalf("result = %+v, want canary 1 accepted and lifespan %s", res, tc.want)
			}
			if c, r := mf.ado.Count(adofake.EndpointPatsCreate), mf.ado.Count(adofake.EndpointPatsRevoke); c != 2 || r != 1 {
				t.Fatalf("creates=%d revokes=%d, want two creates and canary 1 revoked", c, r)
			}
		})
	}
}

func TestOrgCheck_Canary1Refused(t *testing.T) {
	t.Run("longer than the organisation allows", func(t *testing.T) {
		mf := newMintFixture(t)
		mf.connect(t)
		mf.ado.SetPatLifespanLimit(time.Hour, adofake.PatTokenErrorLifespanPolicyViolation)
		res := mf.orgCheckResult(t)
		if res.TokenLife != adoOrgCheckRefused || res.Refusal != adoPATReasonLifespanPolicy || res.Lifespan != adoOrgCheckOn {
			t.Fatalf("result = %+v, want refused / lifespan policy / on", res)
		}
	})
	t.Run("the organisation blocks token creation", func(t *testing.T) {
		mf := newMintFixture(t)
		mf.connect(t)
		mf.ado.SetPatCreateError(adofake.PatTokenErrorAccessDenied)
		res := mf.orgCheckResult(t)
		if res.TokenLife != adoOrgCheckRefused || res.Refusal != adoPATReasonPolicyBlocked || res.Lifespan != adoOrgCheckUnknown {
			t.Fatalf("result = %+v, want refused / policy blocked / unknown", res)
		}
		got := mf.srv.scmAccessValue(context.Background(), mf.site, mf.subject)
		if got.State != modelAccessExpiredSignin || got.Cause != scmAccessCauseBlocked {
			t.Fatalf("scm access = %+v, want expired_signin / blocked", got)
		}
		mf.ado.SetPatCreateError(adofake.PatTokenErrorNone) // a later token clears it
		if _, err := mf.srv.mintADOPAT(context.Background(), mf.cfg, mf.subject, "contoso",
			adoPATRequest{DisplayName: "x", Scope: "vso.profile", ValidTo: time.Now().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
		if got := mf.srv.scmAccessValue(context.Background(), mf.site, mf.subject); got.State != modelAccessLive {
			t.Fatalf("scm access = %+v after a token was created, want live", got)
		}
	})
}

func TestOrgCheck_PermissionsMissing(t *testing.T) {
	mf := newMintFixture(t)
	if err := mf.srv.storeADOEntraBlob(context.Background(), mf.subject, mf.cfg.RowID, adoEntraBlob{
		RefreshToken: "rt-0123456789abcdef", Scopes: []string{adoscope.ResourceID + "/vso.pats"},
		TenantID: mf.cfg.TenantID, ClientID: mf.cfg.ClientID, Subject: mf.subject, Source: adoEntraSourceLogin,
		CapturedAt: time.Now(), ExpiresAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	res := mf.orgCheckResult(t)
	if res.Permissions != adoOrgCheckMissing || res.TokenLife != "" || res.Lifespan != "" {
		t.Fatalf("result = %+v, want permissions missing and no canary", res)
	}
	if n := mf.ado.Count(adofake.EndpointPatsCreate); n != 0 {
		t.Fatalf("%d canaries created without the permissions", n)
	}
	got := mf.srv.scmAccessValue(context.Background(), mf.site, mf.subject)
	if got.State != modelAccessExpiredSignin || got.Cause != scmAccessCausePermissionsMissing {
		t.Fatalf("scm access = %+v, want expired_signin / permissions_missing", got)
	}
}

// TestOrgCheck_D6 answers every row the check may not run on exactly as it
// answers an unknown id.
func TestOrgCheck_D6(t *testing.T) {
	mf := newMintFixture(t)
	bearer := adoEntraTestRow()
	bearer.ID = "ado-bearer"
	mf.srv.cfg.Store = &scmTestStore{site: adoSite(mf.mintedRow(), bearer,
		types.GitProvider{ID: "gh", Kind: types.GitProviderGitHub, BaseURLs: []string{"https://github.com/acme"}})}
	unknown := mf.orgCheck(t, "no-such-row")
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown row: status %d", unknown.Code)
	}
	for _, id := range []string{"ado-bearer", "gh"} {
		if w := mf.orgCheck(t, id); w.Code != unknown.Code || w.Body.String() != unknown.Body.String() {
			t.Errorf("row %q: %d %q, want the unknown row's %d %q", id, w.Code, w.Body.String(), unknown.Code, unknown.Body.String())
		}
	}
	// A minted row that is not the deployment's sign-in row, likewise.
	other := mf.mintedRow()
	other.ID = "ado-minted-2"
	mf.srv.cfg.Store = &scmTestStore{site: adoSite(mf.mintedRow(), other)}
	if w := mf.orgCheck(t, "ado-minted-2"); w.Code != unknown.Code || w.Body.String() != unknown.Body.String() {
		t.Errorf("a second minted row: %d %q, want the unknown row's answer", w.Code, w.Body.String())
	}
}

func TestOrgCheck_S1RefusedWithTheReason(t *testing.T) {
	mf := newMintFixture(t)
	mf.cfg.ClientSecret = ""
	w := mf.orgCheck(t, mf.cfg.RowID)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), ReasonADOPATNeedsConsoleApp) {
		t.Fatalf("status %d body %q, want 409 %s", w.Code, w.Body.String(), ReasonADOPATNeedsConsoleApp)
	}
}

// ── the dispatch predicate ─────────────────────────────────────────────────

func TestResolveADOEntraRun_SkipsOwnPATRows(t *testing.T) {
	row := adoEntraTestRow()
	row.Entra.TokenMode = types.ADOTokenModeOwnPAT
	if ado, ok := resolveADOEntraRun(adoSite(row), []string{adoTestRepo}, adoTestOwner); ok {
		t.Fatalf("an own_pat row resolved the Entra lane: %+v", ado)
	}
}

// ── the client ─────────────────────────────────────────────────────────────

func TestADOPATClient_CreateAndRevokeAgainstTheFake(t *testing.T) {
	ado := adofake.New()
	defer ado.Close()
	ado.RegisterToken("entra", adofake.ScopePats, adofake.ScopePatsManage)
	c := vsspsPATClient{base: ado.URL()}
	ctx := context.Background()
	validTo := time.Now().Add(8 * time.Hour).UTC().Truncate(time.Second)
	pat, err := c.Create(ctx, "contoso", "entra", adoPATRequest{DisplayName: "Wardyn run 1234abcd", Scope: "vso.code vso.project", ValidTo: validTo})
	if err != nil || pat.Token == "" || pat.AuthorizationID == "" || pat.Scope != "vso.code vso.project" || !pat.ValidTo.Equal(validTo) {
		t.Fatalf("create = %+v, %v", pat, err)
	}
	var create adofake.RecordedRequest
	for _, r := range ado.Requests() {
		if r.Endpoint == adofake.EndpointPatsCreate {
			create = r
		}
	}
	if create.APIVersion != adoPATAPIVersion || create.Path != "/contoso/_apis/tokens/pats" {
		t.Errorf("create request = %+v, want api-version %s on /contoso/_apis/tokens/pats", create, adoPATAPIVersion)
	}
	if err := c.Revoke(ctx, "contoso", "entra", pat.AuthorizationID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if err := c.Revoke(ctx, "contoso", "entra", "../pats"); err == nil {
		t.Error("an authorization id that is not a GUID reached the wire")
	}
	if n := ado.Count(adofake.EndpointPatsRevoke); n != 1 {
		t.Errorf("revokes = %d, want 1", n)
	}
	ado.SetPatCreateError(adofake.PatTokenErrorGlobalPolicyViolation)
	_, err = c.Create(ctx, "contoso", "entra", adoPATRequest{DisplayName: "x", Scope: "vso.code", ValidTo: validTo})
	var perr *adoPATError
	if !errors.As(err, &perr) || perr.Reason() != adoPATReasonPolicyBlocked {
		t.Fatalf("policy refusal = %v, want an adoPATError reading policy blocked", err)
	}
	if _, err := c.Create(ctx, "contoso/../x", "entra", adoPATRequest{}); err == nil {
		t.Error("an organisation that is not a DNS label reached the wire")
	}
}
