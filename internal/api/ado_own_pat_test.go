// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/test/adofake"
)

const (
	ownPATRowID  = "ado-own-1"
	ownPATOrgKey = "https://dev.azure.com/contoso"
	// ownPATToken is the caller's (capEmail's) own token; ownPATOtherToken
	// belongs to someone else in the same organisation.
	ownPATToken      = "bobs-own-pat-value-0001"
	ownPATOtherToken = "carols-pat-value-0002"
	ownPATOtherEmail = "carol@corp.example"
)

// ownPATNow is the doors' clock: tokens are dated relative to it.
var ownPATNow = time.Now().UTC()

// ownPATTestRow is an own-token row on organisation contoso, 30 days at most.
func ownPATTestRow() types.GitProvider {
	return types.GitProvider{
		ID: ownPATRowID, Kind: types.GitProviderAzureDevOps,
		BaseURLs:         []string{ownPATOrgKey},
		Lanes:            []types.GitLane{types.GitLaneEntra},
		CredentialSource: types.CredentialSourcePerUser,
		Entra: &types.ADOEntraConfig{
			TokenMode:         types.ADOTokenModeOwnPAT,
			CapabilityCeiling: []adoscope.Capability{adoscope.CapProjectRead, adoscope.CapCodeRead, adoscope.CapCodeWrite, adoscope.CapPR},
			DefaultProfile:    []adoscope.Capability{adoscope.CapProjectRead, adoscope.CapCodeRead, adoscope.CapCodeWrite},
			PATMaxDays:        30,
		},
	}
}

// ownPATDoor is a member-reachable server over one own-token row, with the
// identity check pointed at a fake organisation that knows two tokens: the
// caller's and another person's.
type ownPATDoor struct {
	srv     *Server
	fake    *adofake.Server
	audit   *recRecorder
	secrets *memSecrets
}

func newOwnPATDoor(t *testing.T, site types.SiteConfig, grants []types.CapabilityGrant) *ownPATDoor {
	t.Helper()
	fake := adofake.New()
	t.Cleanup(fake.Close)
	fake.RegisterToken(ownPATToken, adofake.ScopeProjectRead, adofake.ScopeCodeWrite)
	fake.RegisterIdentity(ownPATToken, strings.ToUpper(capEmail)) // case is not identity
	fake.RegisterToken(ownPATOtherToken, adofake.ScopeProjectRead, adofake.ScopeCodeWrite)
	fake.RegisterIdentity(ownPATOtherToken, ownPATOtherEmail)
	prev := adoOwnPATAPIBase
	adoOwnPATAPIBase = fake.URL()
	t.Cleanup(func() { adoOwnPATAPIBase = prev })

	h := newHarness(t)
	secrets := &memSecrets{m: map[string][]byte{}}
	cfg := baseTestConfig(h, &capStore{Store: r3IntegStore{}, site: site, grants: grants})
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Secrets = secrets
	cfg.MaskRegistry = secretmask.NewRegistry()
	cfg.Now = func() time.Time { return ownPATNow }
	return &ownPATDoor{srv: New(cfg), fake: fake, audit: h.audit, secrets: secrets}
}

// put drives PUT /me/scm/azure-devops/token as the member (capSub, capEmail).
func (d *ownPATDoor) put(t *testing.T, org, token, expiresOn string) (int, string) {
	t.Helper()
	return d.putAs(t, memberCookie(t), org, token, expiresOn)
}

func (d *ownPATDoor) putAs(t *testing.T, cookie *http.Cookie, org, token, expiresOn string) (int, string) {
	t.Helper()
	body, _ := json.Marshal(adoOwnPATRequest{Org: org, Token: token, ExpiresOn: expiresOn})
	w := doSSO(t, d.srv, http.MethodPut, "/api/v1/me/scm/azure-devops/token", cookie, string(body))
	return w.Code, w.Body.String()
}

// stored is the caller's own stored token for the row, if any.
func (d *ownPATDoor) stored(t *testing.T) (adoOwnPATBlob, bool) {
	t.Helper()
	blob, found, err := d.srv.readADOOwnPAT(secretstore.WithPurpose(context.Background(), secretstore.PurposeStatus), capSub, ownPATRowID)
	if err != nil {
		t.Fatal(err)
	}
	return blob, found
}

// auditRows is every audit row carrying action.
func (d *ownPATDoor) auditRows(action string) []types.AuditEvent {
	return slices.DeleteFunc(d.audit.snapshot(), func(ev types.AuditEvent) bool { return ev.Action != action })
}

// days is ownPATNow's date plus n days, as the dialog sends it.
func days(n int) string { return ownPATNow.AddDate(0, 0, n).Format(time.DateOnly) }

// The happy path: the caller's own token, within the limit, is stored in their
// own namespace under the sealed name, audited without the token, and the
// answer is the row's fresh access entry with the deadline.
func TestADOOwnPATPut_StoresTheCallersOwnToken(t *testing.T) {
	d := newOwnPATDoor(t, adoSite(ownPATTestRow()), nil)
	code, body := d.put(t, ownPATOrgKey, ownPATToken, days(30))
	if code != http.StatusOK {
		t.Fatalf("PUT = %d %s", code, body)
	}
	var access SCMAccess
	if err := json.Unmarshal([]byte(body), &access); err != nil {
		t.Fatal(err)
	}
	if access.State != modelAccessLive || access.ExpiresOn != days(30) || access.TokenMode != "own_pat" || access.Source != scmAccessSourceOwn {
		t.Errorf("answer = %+v, want live, expiring on %s, own_pat, source own", access, days(30))
	}
	blob, found := d.stored(t)
	if !found || blob.Token != ownPATToken || blob.Org != "contoso" || blob.ExpiresOn.Format(time.DateOnly) != days(30) {
		t.Fatalf("stored = %+v (found %v)", blob, found)
	}
	if _, operatorHas := d.secrets.m[adoOwnPATSecretName(ownPATRowID)]; operatorHas {
		t.Error("the token landed in the operator namespace")
	}
	rows := d.auditRows(adoPATAuditOwnStore)
	if len(rows) != 1 || rows[0].Outcome != "success" || rows[0].Actor != capSub {
		t.Fatalf("ado_pat.own.store rows = %+v, want one success by the caller", rows)
	}
	for _, ev := range d.audit.snapshot() {
		if strings.Contains(string(ev.Data), ownPATToken) || strings.Contains(ev.Target, ownPATToken) {
			t.Fatalf("the token reached audit row %s", ev.Action)
		}
	}
	// The identity check asked the organisation, with the token, over Basic.
	var asked bool
	for _, rr := range d.fake.Requests() {
		if rr.Endpoint == adofake.EndpointConnectionData && rr.Path == "/contoso/_apis/connectionData" && rr.Token == ownPATToken {
			asked = true
		}
	}
	if !asked {
		t.Error("the identity check never asked contoso's connectionData with the token")
	}
}

// SECURITY: a token that belongs to another account is refused, nothing is
// stored, and neither the refusal nor the audit row names that account.
func TestADOOwnPATPut_RefusesAnotherAccountsToken(t *testing.T) {
	d := newOwnPATDoor(t, adoSite(ownPATTestRow()), nil)
	code, body := d.put(t, ownPATOrgKey, ownPATOtherToken, days(10))
	if code != http.StatusForbidden || !strings.Contains(body, reasonADOOwnPATIdentityMismatch) ||
		!strings.Contains(body, adoOwnPATMismatchRefusal) {
		t.Fatalf("PUT another account's token = %d %s, want 403 %s", code, body, reasonADOOwnPATIdentityMismatch)
	}
	if _, found := d.stored(t); found {
		t.Fatal("another account's token was stored")
	}
	rows := d.auditRows(adoPATAuditOwnStore)
	if len(rows) != 1 || rows[0].Outcome != "failure" || !strings.Contains(string(rows[0].Data), `"reason":"identity_mismatch"`) {
		t.Fatalf("ado_pat.own.store rows = %+v, want one failure with reason identity_mismatch", rows)
	}
	for _, s := range []string{body, string(rows[0].Data)} {
		if strings.Contains(strings.ToLower(s), "carol") {
			t.Errorf("the other account was named: %s", s)
		}
	}
}

// SECURITY: a caller whose sign-in carries no email cannot be matched to the
// token's account, so the token is refused rather than trusted.
func TestADOOwnPATPut_RefusesWhenTheCallerHasNoEmail(t *testing.T) {
	d := newOwnPATDoor(t, adoSite(ownPATTestRow()), nil)
	code, body := d.putAs(t, ssoSession(t, capSub, "", oidc.RoleUser), ownPATOrgKey, ownPATToken, days(10))
	if code != http.StatusForbidden || !strings.Contains(body, reasonADOOwnPATIdentityMismatch) ||
		!strings.Contains(body, adoOwnPATNoEmailRefusal) {
		t.Fatalf("PUT with no email = %d %s, want 403 %s saying there is no email to check", code, body, reasonADOOwnPATIdentityMismatch)
	}
	if _, found := d.stored(t); found {
		t.Fatal("a token was stored for a caller nobody could match it to")
	}
}

// SECURITY: a token Azure DevOps does not accept for the organisation —
// unknown to it, or accepted but naming nobody — is refused with its own
// message and nothing is stored.
func TestADOOwnPATPut_RefusesATokenAzureDevOpsRejects(t *testing.T) {
	d := newOwnPATDoor(t, adoSite(ownPATTestRow()), nil)
	d.fake.RegisterToken("anonymous-pat", adofake.ScopeProjectRead) // accepted, but no Account
	for _, token := range []string{"never-issued-pat", "anonymous-pat"} {
		code, body := d.put(t, ownPATOrgKey, token, days(10))
		if code != http.StatusUnprocessableEntity || !strings.Contains(body, reasonADOOwnPATRejected) ||
			!strings.Contains(body, adoOwnPATRejectedRefusal) {
			t.Errorf("PUT %s = %d %s, want 422 %s", token, code, body, reasonADOOwnPATRejected)
		}
	}
	if _, found := d.stored(t); found {
		t.Fatal("a rejected token was stored")
	}
}

// SECURITY: an expiry past the row's pat_max_days is refused with the canon
// sentence naming the limit, and an expiry today or earlier is refused too;
// neither reaches Azure DevOps.
func TestADOOwnPATPut_HoldsTheExpiryToTheAdminsLimit(t *testing.T) {
	d := newOwnPATDoor(t, adoSite(ownPATTestRow()), nil)
	code, body := d.put(t, ownPATOrgKey, ownPATToken, days(31))
	if code != http.StatusBadRequest || !strings.Contains(body, reasonADOOwnPATExpiryTooLong) ||
		!strings.Contains(body, "This token expires after the 30-day limit your administrator set.") {
		t.Errorf("PUT 31 days = %d %s, want 400 %s naming 30 days", code, body, reasonADOOwnPATExpiryTooLong)
	}
	for _, bad := range []string{days(0), days(-1), "27 October", ""} {
		if code, body := d.put(t, ownPATOrgKey, ownPATToken, bad); code != http.StatusBadRequest ||
			!strings.Contains(body, reasonADOOwnPATExpiryInvalid) {
			t.Errorf("PUT expires_on %q = %d %s, want 400 %s", bad, code, body, reasonADOOwnPATExpiryInvalid)
		}
	}
	if _, found := d.stored(t); found {
		t.Fatal("a token with a refused expiry was stored")
	}
	if n := d.fake.Count(adofake.EndpointConnectionData); n != 0 {
		t.Errorf("a refused expiry still asked Azure DevOps %d times", n)
	}
}

// D-6: a row the caller may not use answers byte-identically to a row that
// does not exist, on both doors; a bearer row is not an own-token row either.
func TestADOOwnPATDoors_RefusedRowReadsAsUnknown(t *testing.T) {
	deny := []types.CapabilityGrant{grant(types.CapabilitySubjectAll, "", capWorkspaceProvider, ownPATRowID, types.CapabilityDeny)}
	refused := newOwnPATDoor(t, adoSite(ownPATTestRow()), deny)
	missing := newOwnPATDoor(t, types.SiteConfig{}, nil)
	bearer := ownPATTestRow()
	bearer.Entra.TokenMode = types.ADOTokenModeBearer
	notOwn := newOwnPATDoor(t, adoSite(bearer), nil)

	rc, rb := refused.put(t, ownPATOrgKey, ownPATToken, days(10))
	mc, mb := missing.put(t, ownPATOrgKey, ownPATToken, days(10))
	nc, nb := notOwn.put(t, ownPATOrgKey, ownPATToken, days(10))
	if rc != http.StatusNotFound || rc != mc || rb != mb || nc != mc || nb != mb {
		t.Errorf("PUT: refused %d %s / missing %d %s / bearer row %d %s — want the same 404", rc, rb, mc, mb, nc, nb)
	}
	del := "/api/v1/me/scm/azure-devops/token?org=" + url.QueryEscape(ownPATOrgKey)
	rw := doSSO(t, refused.srv, http.MethodDelete, del, memberCookie(t), "")
	mw := doSSO(t, missing.srv, http.MethodDelete, del, memberCookie(t), "")
	if rw.Code != http.StatusNotFound || rw.Code != mw.Code || rw.Body.String() != mw.Body.String() {
		t.Errorf("DELETE: refused %d %s / missing %d %s — want the same 404", rw.Code, rw.Body, mw.Code, mw.Body)
	}
	if n := refused.fake.Count(adofake.EndpointConnectionData); n != 0 {
		t.Errorf("a refused row still sent the token to Azure DevOps %d times", n)
	}
	// Never vacuous: the same member on the same row, unrefused, is served.
	if code, body := newOwnPATDoor(t, adoSite(ownPATTestRow()), nil).put(t, ownPATOrgKey, ownPATToken, days(10)); code != http.StatusOK {
		t.Fatalf("unrefused PUT = %d %s", code, body)
	}
}

// DELETE removes the caller's own copy, audited, and leaves nothing to read.
func TestADOOwnPATDelete_RemovesTheCallersToken(t *testing.T) {
	d := newOwnPATDoor(t, adoSite(ownPATTestRow()), nil)
	if code, body := d.put(t, ownPATOrgKey, ownPATToken, days(10)); code != http.StatusOK {
		t.Fatalf("PUT = %d %s", code, body)
	}
	w := doSSO(t, d.srv, http.MethodDelete, "/api/v1/me/scm/azure-devops/token?org="+url.QueryEscape(ownPATOrgKey), memberCookie(t), "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d %s", w.Code, w.Body)
	}
	if _, found := d.stored(t); found {
		t.Fatal("the token survived its DELETE")
	}
	if rows := d.auditRows(adoPATAuditOwnDelete); len(rows) != 1 || rows[0].Outcome != "success" {
		t.Fatalf("ado_pat.own.delete rows = %+v", rows)
	}
}

// SECURITY: the stored token is read from its owner's own namespace only — an
// operator row or another person's row under the same sealed name never
// answers for them.
func TestReadADOOwnPAT_OwnerOnlyNoFallback(t *testing.T) {
	secrets := &memSecrets{m: map[string][]byte{}}
	srv := &Server{cfg: Config{Secrets: secrets}}
	raw, _ := json.Marshal(adoOwnPATBlob{Token: "someone-elses", Org: "contoso", ExpiresOn: ownPATNow.AddDate(0, 0, 5)})
	ctx := secretstore.WithPurpose(context.Background(), secretstore.PurposeStatus)
	if err := secrets.For("").Put(ctx, adoOwnPATSecretName(ownPATRowID), raw); err != nil {
		t.Fatal(err)
	}
	if err := secrets.For("carol").Put(ctx, adoOwnPATSecretName(ownPATRowID), raw); err != nil {
		t.Fatal(err)
	}
	if _, found, err := srv.readADOOwnPAT(ctx, "bob", ownPATRowID); err != nil || found {
		t.Fatalf("bob's read found=%v err=%v: another namespace's token answered for him", found, err)
	}
	if _, found, _ := srv.readADOOwnPAT(ctx, "", ownPATRowID); found {
		t.Fatal("an empty owner read the operator's row")
	}
	if blob, found, _ := srv.readADOOwnPAT(ctx, "carol", ownPATRowID); !found || blob.Token != "someone-elses" {
		t.Fatal("carol's own token is not readable by her")
	}
}

// The token name is sealed: the generic secrets API can neither write nor
// read it, and no git or api_key grant can name it.
func TestADOOwnPATSecretName_IsSealed(t *testing.T) {
	name := adoOwnPATSecretName(ownPATRowID)
	if !secretsAPIReserved(name) || !sinkReservedSecret(name) {
		t.Fatalf("%s is not sealed", name)
	}
	if name == adoEntraSecretName(ownPATRowID) {
		t.Fatal("the own-token name collides with the sign-in name")
	}
}

// /me/scm-access grades an own-token row from the person's own token: not
// configured, live, expiring within the window, expired with its cause, each
// with the deadline, the limit and the scopes to tick.
func TestSCMAccessForOwnPAT_States(t *testing.T) {
	row := ownPATTestRow()
	ctx := secretstore.WithPurpose(context.Background(), secretstore.PurposeStatus)
	cases := []struct {
		name      string
		expiresIn time.Duration // 0: no token stored
		state     string
		cause     string
	}{
		{"no token", 0, modelAccessNotConfigured, ""},
		{"live", 20 * 24 * time.Hour, modelAccessLive, ""},
		{"expiring", 3 * 24 * time.Hour, modelAccessExpiring, ""},
		{"expired", -time.Hour, modelAccessExpiredSignin, scmAccessCauseTokenExpired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newSCMTestServer(t, adoSite(row), false)
			srv.cfg.Now = func() time.Time { return ownPATNow }
			var want string
			if tc.expiresIn != 0 {
				exp := ownPATNow.Add(tc.expiresIn)
				want = exp.Format(time.DateOnly)
				raw, _ := json.Marshal(adoOwnPATBlob{Token: ownPATToken, Org: "contoso", ExpiresOn: exp})
				if err := srv.cfg.Secrets.For(capSub).Put(ctx, adoOwnPATSecretName(row.ID), raw); err != nil {
					t.Fatal(err)
				}
			}
			rows, err := srv.computeSCMAccessRows(context.Background(), capSub)
			if err != nil || len(rows) != 1 {
				t.Fatalf("rows = %+v err = %v", rows, err)
			}
			got := rows[0]
			if got.State != tc.state || got.Cause != tc.cause || got.ExpiresOn != want ||
				got.TokenMode != "own_pat" || got.MaxDays != 30 || got.Org != ownPATOrgKey {
				t.Errorf("access = %+v, want state %s cause %q expires_on %q", got, tc.state, tc.cause, want)
			}
			if !slices.Contains(got.TokenScopes, "Code (Read & write)") || slices.Contains(got.TokenScopes, "Code (Read)") {
				t.Errorf("token_scopes = %q, want Code at its widest level only", got.TokenScopes)
			}
		})
	}
}

// SECURITY: the launch gate refuses a run whose person has not added a token
// or whose token has expired, and admits one that is merely expiring.
func TestGitCredentialRefusal_OwnPAT(t *testing.T) {
	row := ownPATTestRow()
	ctx := secretstore.WithPurpose(context.Background(), secretstore.PurposeStatus)
	put := func(t *testing.T, srv *Server, exp time.Time) {
		t.Helper()
		raw, _ := json.Marshal(adoOwnPATBlob{Token: ownPATToken, Org: "contoso", ExpiresOn: exp})
		if err := srv.cfg.Secrets.For(capSub).Put(ctx, adoOwnPATSecretName(row.ID), raw); err != nil {
			t.Fatal(err)
		}
	}
	repo := "https://dev.azure.com/contoso/proj/_git/app"
	for _, tc := range []struct {
		name string
		exp  time.Time
		want string // "" = admitted
	}{
		{"not added", time.Time{}, gitCredentialNotConnectedRefusal},
		{"expired", ownPATNow.Add(-time.Minute), gitCredentialOwnPATExpiredRefusal},
		{"expiring", ownPATNow.Add(48 * time.Hour), ""},
		{"live", ownPATNow.AddDate(0, 0, 20), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newSCMTestServer(t, adoSite(row), false)
			srv.cfg.Now = func() time.Time { return ownPATNow }
			if !tc.exp.IsZero() {
				put(t, srv, tc.exp)
			}
			err := srv.gitCredentialRefusalForLauncher(context.Background(), capSub, repo)
			var got string
			if err != nil {
				got = err.Error()
			}
			if got != tc.want {
				t.Errorf("refusal = %q, want %q", got, tc.want)
			}
		})
	}
}

// The scopes to tick are Azure DevOps' own wording for every scope any
// grantable capability can map to — a new scope with no label would reach the
// dialog as a bare vso.* name.
func TestADOOwnPATTokenScopes_EveryScopeHasAzureDevOpsWording(t *testing.T) {
	for _, c := range adoscope.GrantableCapabilities() {
		scopes, err := adoscope.ScopesFor([]adoscope.Capability{c})
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range scopes {
			if _, ok := adoTokenPageScopes[strings.TrimPrefix(q, adoscope.ResourceID+"/")]; !ok {
				t.Errorf("capability %s maps to %s, which has no Azure DevOps token-page wording", c, q)
			}
		}
	}
	got := adoOwnPATTokenScopes([]adoscope.Capability{adoscope.CapCodeWrite, adoscope.CapWorkWrite})
	if want := []string{"Code (Read & write)", "Graph (Read)", "Work Items (Read & write)"}; !slices.Equal(got, want) {
		t.Errorf("scopes = %q, want %q", got, want)
	}
}

// ownPATPeople gives the door's store the person rows the bind by object id
// reads: capSub is a person set up by object id.
type ownPATPeople struct {
	store.Store
	oid string
}

func (p ownPATPeople) CreatePerson(context.Context, types.Person) (types.Person, bool, error) {
	return types.Person{}, false, store.ErrNotFound
}
func (p ownPATPeople) GetPerson(_ context.Context, principal string) (types.Person, error) {
	if principal != capSub {
		return types.Person{}, store.ErrNotFound
	}
	return types.Person{Principal: capSub, ObjectID: p.oid}, nil
}
func (p ownPATPeople) ListPeople(context.Context) ([]types.Person, error) { return nil, nil }
func (p ownPATPeople) MarkPersonSignedIn(context.Context, string, time.Time) error {
	return nil
}

// The bind by Entra object id: a token whose Graph originId is the caller's
// object id binds whatever its Account and Mail say; any other answer — no
// match, or Graph refusing a token without vso.graph — falls back to the
// Account and Mail compare, and a token neither way the caller's is refused.
func TestADOOwnPATPut_BindsByObjectIDThenByName(t *testing.T) {
	const oid = "0a1b2c3d-1111-2222-3333-444455556666"
	for _, tc := range []struct {
		name       string
		account    string
		originID   string
		graphCode  int
		wantCode   int
		wantGraphs int
	}{
		{"originId matches, account differs", "someone.else@corp.example", strings.ToUpper(oid), http.StatusOK, http.StatusOK, 1},
		{"originId differs, account is the email", capEmail, "ffffffff-0000-0000-0000-000000000000", http.StatusOK, http.StatusOK, 1},
		{"both differ", "someone.else@corp.example", "ffffffff-0000-0000-0000-000000000000", http.StatusOK, http.StatusForbidden, 1},
		{"graph refuses, account is the email", capEmail, oid, http.StatusForbidden, http.StatusOK, 1},
		{"graph refuses, account differs", "someone.else@corp.example", oid, http.StatusForbidden, http.StatusForbidden, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var graphs int
			fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if _, pass, _ := r.BasicAuth(); pass != ownPATToken {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				switch r.URL.Path {
				case "/contoso/_apis/connectionData":
					_, _ = w.Write([]byte(`{"authenticatedUser":{"subjectDescriptor":"aad.abc","properties":{"Account":{"$value":"` + tc.account + `"}}}}`))
				case "/contoso/_apis/graph/users/aad.abc":
					graphs++
					if r.URL.Query().Get("api-version") != "7.1-preview.1" {
						t.Errorf("graph api-version = %q", r.URL.Query().Get("api-version"))
					}
					w.WriteHeader(tc.graphCode)
					if tc.graphCode == http.StatusOK {
						_, _ = w.Write([]byte(`{"originId":"` + tc.originID + `"}`))
					}
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			t.Cleanup(fake.Close)
			d := newOwnPATDoor(t, adoSite(ownPATTestRow()), nil)
			d.srv.cfg.Store = ownPATPeople{Store: d.srv.cfg.Store, oid: oid}
			prevAPI, prevGraph := adoOwnPATAPIBase, adoOwnPATGraphBase
			adoOwnPATAPIBase, adoOwnPATGraphBase = fake.URL, fake.URL
			t.Cleanup(func() { adoOwnPATAPIBase, adoOwnPATGraphBase = prevAPI, prevGraph })

			code, body := d.put(t, ownPATOrgKey, ownPATToken, days(10))
			if code != tc.wantCode {
				t.Fatalf("PUT = %d %s, want %d", code, body, tc.wantCode)
			}
			if _, found := d.stored(t); found != (tc.wantCode == http.StatusOK) {
				t.Errorf("stored = %v after PUT %d", found, code)
			}
			if graphs != tc.wantGraphs {
				t.Errorf("graph asked %d times, want %d", graphs, tc.wantGraphs)
			}
		})
	}
}

// A caller with no person row holding an object id never reaches Graph: the
// token is judged by name alone, as before.
func TestADOOwnPATPut_NoObjectIDSkipsGraph(t *testing.T) {
	var graphs int
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/graph/") {
			graphs++
		}
		_, _ = w.Write([]byte(`{"authenticatedUser":{"subjectDescriptor":"aad.abc","properties":{"Account":{"$value":"` + capEmail + `"}}}}`))
	}))
	t.Cleanup(fake.Close)
	d := newOwnPATDoor(t, adoSite(ownPATTestRow()), nil)
	prev := adoOwnPATAPIBase
	adoOwnPATAPIBase = fake.URL
	t.Cleanup(func() { adoOwnPATAPIBase = prev })
	if code, body := d.put(t, ownPATOrgKey, ownPATToken, days(10)); code != http.StatusOK || graphs != 0 {
		t.Fatalf("PUT = %d %s with %d graph calls, want 200 and none", code, body, graphs)
	}
}

// A re-paste is a new token record: the refusal stamp of the one before does
// not carry over to /me/scm-access.
func TestADOOwnPATPut_RepasteClearsTheRefusalStamp(t *testing.T) {
	d := newOwnPATDoor(t, adoSite(ownPATTestRow()), nil)
	stamped := ownPATNow.Add(-time.Hour)
	raw, _ := json.Marshal(adoOwnPATBlob{Token: ownPATToken, Org: "contoso", ExpiresOn: ownPATNow.AddDate(0, 0, 5), RefusedAt: &stamped})
	if err := d.srv.cfg.Secrets.For(capSub).Put(context.Background(), adoOwnPATSecretName(ownPATRowID), raw); err != nil {
		t.Fatal(err)
	}
	if blob, _ := d.stored(t); blob.RefusedAt == nil {
		t.Fatal("setup: the stamp was not stored")
	}
	code, body := d.put(t, ownPATOrgKey, ownPATToken, days(30))
	if code != http.StatusOK || strings.Contains(body, "refused_at") {
		t.Fatalf("PUT = %d %s, want 200 carrying no refused_at", code, body)
	}
	if blob, _ := d.stored(t); blob.RefusedAt != nil {
		t.Errorf("the re-paste kept the stamp: %+v", blob)
	}
}
