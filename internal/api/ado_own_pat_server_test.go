// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/test/adofake"
)

const (
	adoServerHost   = "tfs.corp.example"
	adoServerOrgKey = "https://" + adoServerHost + "/DefaultCollection"
	adoServerRepo   = adoServerOrgKey + "/proj/_git/app"
	// adoServerToken is the caller's (capEmail's) own Server token: its
	// Account is a DOMAIN\user name, and its Mail is the caller's email.
	adoServerToken = "bobs-server-pat-0003"
)

// adoServerTestRow is an Azure DevOps Server row as #1429 shapes one: the pat
// lane, per_user, no entra block, an address naming the collection.
func adoServerTestRow(address string) types.GitProvider {
	return types.GitProvider{
		ID: ownPATRowID, Kind: types.GitProviderAzureDevOps, BaseURLs: []string{address},
		Lanes: []types.GitLane{types.GitLanePAT}, CredentialSource: types.CredentialSourcePerUser,
	}
}

// newServerOwnPATDoor is newOwnPATDoor over a Server row, with the identity
// check reaching a TLS fake Azure DevOps Server by its own host name.
func newServerOwnPATDoor(t *testing.T, address string) *ownPATDoor {
	t.Helper()
	fake, h := adofake.NewHandler()
	ts := httptest.NewTLSServer(h)
	t.Cleanup(ts.Close)
	fake.RegisterToken(adoServerToken, adofake.ScopeProjectRead)
	fake.RegisterServerIdentity(adoServerToken, `CORP\bob`, capEmail)
	fake.RegisterToken(ownPATOtherToken, adofake.ScopeProjectRead)
	fake.RegisterServerIdentity(ownPATOtherToken, `CORP\carol`, ownPATOtherEmail)
	pool := x509.NewCertPool()
	pool.AddCert(ts.Certificate())
	prev := adoOwnPATTransport
	adoOwnPATTransport = &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "example.com", MinVersion: tls.VersionTLS12},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if addr != adoServerHost+":443" {
				t.Errorf("the identity check dialled %s, want %s:443", addr, adoServerHost)
			}
			return (&net.Dialer{}).DialContext(ctx, network, ts.Listener.Addr().String())
		},
	}
	t.Cleanup(func() { adoOwnPATTransport = prev })

	hs := newHarness(t)
	secrets := &memSecrets{m: map[string][]byte{}}
	cfg := baseTestConfig(hs, &capStore{Store: r3IntegStore{}, site: adoSite(adoServerTestRow(address))})
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Secrets = secrets
	cfg.MaskRegistry = secretmask.NewRegistry()
	cfg.Now = func() time.Time { return ownPATNow }
	return &ownPATDoor{srv: New(cfg), fake: fake, audit: hs.audit, secrets: secrets}
}

// A Server person's own token is checked against the collection's
// connectionData — no api-version, since a Server answers without one — and
// matched by its Mail, the only email a DOMAIN\user account carries. It is
// stored for the collection, and the row reads git only, 30 days at most.
func TestADOServerOwnPATPut_StoresTheCallersOwnToken(t *testing.T) {
	d := newServerOwnPATDoor(t, adoServerOrgKey)
	code, body := d.put(t, adoServerOrgKey, adoServerToken, days(30))
	if code != http.StatusOK {
		t.Fatalf("PUT = %d %s", code, body)
	}
	var access SCMAccess
	if err := json.Unmarshal([]byte(body), &access); err != nil {
		t.Fatal(err)
	}
	if access.State != modelAccessLive || !access.GitOnly || access.MaxDays != 30 || access.TokenMode != "own_pat" ||
		!slices.Equal(access.TokenScopes, []string{"Code (Read & write)"}) {
		t.Errorf("answer = %+v, want live, git only, 30 days, Code (Read & write)", access)
	}
	blob, found := d.stored(t)
	if !found || blob.Token != adoServerToken || blob.Org != "defaultcollection" {
		t.Fatalf("stored = %+v (found %v)", blob, found)
	}
	var asked bool
	for _, rr := range d.fake.Requests() {
		if rr.Endpoint == adofake.EndpointConnectionData && rr.Path == "/DefaultCollection/_apis/connectionData" &&
			rr.Token == adoServerToken && rr.APIVersion == "" {
			asked = true
		}
	}
	if !asked {
		t.Error("the identity check never asked the collection's connectionData with the token and no api-version")
	}
}

// SECURITY: on Server as on Services — another account's token, a token the
// server rejects, and an expiry past 30 days are each refused and not stored,
// and the other account is never named.
func TestADOServerOwnPATPut_Refusals(t *testing.T) {
	d := newServerOwnPATDoor(t, adoServerOrgKey)
	if code, body := d.put(t, adoServerOrgKey, ownPATOtherToken, days(10)); code != http.StatusForbidden ||
		!strings.Contains(body, reasonADOOwnPATIdentityMismatch) || strings.Contains(strings.ToLower(body), "carol") {
		t.Errorf("another account's token = %d %s", code, body)
	}
	if code, body := d.put(t, adoServerOrgKey, "never-issued", days(10)); code != http.StatusUnprocessableEntity ||
		!strings.Contains(body, reasonADOOwnPATRejected) {
		t.Errorf("a rejected token = %d %s", code, body)
	}
	if code, body := d.put(t, adoServerOrgKey, adoServerToken, days(31)); code != http.StatusBadRequest ||
		!strings.Contains(body, "30-day limit") {
		t.Errorf("31 days = %d %s", code, body)
	}
	if _, found := d.stored(t); found {
		t.Fatal("a refused token was stored")
	}
	mismatch := slices.DeleteFunc(d.auditRows(adoPATAuditOwnStore), func(ev types.AuditEvent) bool {
		return !strings.Contains(string(ev.Data), `"reason":"identity_mismatch"`)
	})
	if len(mismatch) != 1 || strings.Contains(strings.ToLower(string(mismatch[0].Data)), "carol") {
		t.Errorf("identity_mismatch store rows = %+v", mismatch)
	}
}

// SECURITY: a Server row whose address names no collection is not an
// own-token row — its pin would be the whole server — so both doors answer as
// for no row, and the token is never sent anywhere.
func TestADOServerOwnPATDoors_RowWithoutCollectionIsNotServed(t *testing.T) {
	d := newServerOwnPATDoor(t, "https://"+adoServerHost)
	if code, body := d.put(t, "https://"+adoServerHost, adoServerToken, days(10)); code != http.StatusNotFound ||
		!strings.Contains(body, reasonADOOwnPATUnknownRow) {
		t.Errorf("PUT = %d %s, want the unknown-row 404", code, body)
	}
	if n := d.fake.Count(adofake.EndpointConnectionData); n != 0 {
		t.Errorf("the token was sent to the server %d times", n)
	}
}

// DELETE removes a Server person's own token.
func TestADOServerOwnPATDelete(t *testing.T) {
	d := newServerOwnPATDoor(t, adoServerOrgKey)
	if code, body := d.put(t, adoServerOrgKey, adoServerToken, days(10)); code != http.StatusOK {
		t.Fatalf("PUT = %d %s", code, body)
	}
	w := doSSO(t, d.srv, http.MethodDelete, "/api/v1/me/scm/azure-devops/token?org="+url.QueryEscape(adoServerOrgKey), memberCookie(t), "")
	if _, found := d.stored(t); w.Code != http.StatusNoContent || found {
		t.Fatalf("DELETE = %d, still stored %v", w.Code, found)
	}
}

// Dispatch authors a Server run's lane on its one host, git only: one grant
// for the server host pinned to the collection, the host allowlisted on 443
// and on the git broker's list, and nothing else — no TLS interception, no
// placeholder, no grant id and no token in the sandbox's environment, whatever
// the PAT broker is.
func TestAuthorADOEntraInjection_ServerRow(t *testing.T) {
	f := newOwnPATRunOn(t, adoServerTestRow(adoServerOrgKey), adoServerRepo)
	if len(f.st.grants) != 1 {
		t.Fatalf("%d grants, want one, for %s", len(f.st.grants), adoServerHost)
	}
	var sc struct {
		Host       string                `json:"host"`
		SecretName string                `json:"secret_name"`
		RequireTLS bool                  `json:"require_tls"`
		Snapshot   adoEntraScopeSnapshot `json:"snapshot"`
	}
	if err := json.Unmarshal(f.st.grants[0].Spec.Scope, &sc); err != nil {
		t.Fatal(err)
	}
	if sc.Host != adoServerHost || sc.SecretName != types.ADOEntraAccessTokenSecret || !sc.RequireTLS ||
		sc.Snapshot.TokenMode != "own_pat" || sc.Snapshot.Organisation != "defaultcollection" || sc.Snapshot.OwnerSubject != capSub ||
		!slices.Equal(sc.Snapshot.Capabilities, adoServerCapabilities) {
		t.Errorf("grant scope = %+v", sc)
	}
	if !slices.Equal(f.policy.AllowedDomains, []string{adoServerHost + ":443"}) {
		t.Errorf("a Server run's egress = %v, want only %s:443", f.policy.AllowedDomains, adoServerHost)
	}
	if f.env["WARDYN_GIT_PAT_BROKER_HOSTS"] != adoServerHost {
		t.Errorf("git broker hosts = %q, want %q", f.env["WARDYN_GIT_PAT_BROKER_HOSTS"], adoServerHost)
	}
	for k, v := range f.env {
		if k == adoEntraPlaceholderEnv || k == "WARDYN_GIT_PAT_GRANTS" || strings.Contains(v, f.grantID.String()) {
			t.Errorf("the sandbox env carries %s=%q", k, v)
		}
	}

	// The proxy's gate: the one host, the collection, the git lane's set.
	st := &adoTestStore{site: adoSite(adoServerTestRow("https://" + adoServerHost + "/tfs/DefaultCollection"))}
	srv := &Server{cfg: Config{Store: st, Audit: &memAudit{}, Now: time.Now}}
	ado, on := resolveADOEntraRun(st.site, []string{"https://" + adoServerHost + "/tfs/DefaultCollection/proj/_git/app"}, capSub)
	lane, ok := srv.authorADOEntraLane(context.Background(), types.AgentRun{ID: f.runID}, ado, on, adoEntraUngraded(),
		adoStandingBound{}, dispatchLLMPlan{mitmCACertPEM: "CERT", mitmCAKeyPEM: "KEY"}, &types.RunPolicySpec{}, map[string]string{}, nil)
	if !ok || lane.gate == nil || lane.gate.Organization != "tfs/defaultcollection" ||
		!slices.Equal(lane.gate.Hosts, []string{adoServerHost}) || len(lane.mitmHosts) != 0 {
		t.Fatalf("lane = %+v gate = %+v", lane, lane.gate)
	}
}

// A Server run is on the lane only for a repository under the row's
// collection, on its host; a row whose address is deeper than a collection
// under one virtual directory is not a lane at all.
func TestADOServerRunForRepo(t *testing.T) {
	deep := adoServerTestRow("https://" + adoServerHost + "/tfs/DefaultCollection/proj")
	if isADOServerOwnPATRow(deep) {
		t.Error("a three-segment address was taken for a collection")
	}
	if isADOServerOwnPATRow(adoServerTestRow("https://dev.azure.com/contoso")) {
		t.Error("a Services organisation was taken for a Server collection")
	}
	row := adoServerTestRow("https://" + adoServerHost + "/tfs/DefaultCollection")
	for repo, want := range map[string]bool{
		"https://" + adoServerHost + "/tfs/DefaultCollection/proj/_git/app":  true,
		"https://" + adoServerHost + "/tfs/defaultcollection/proj/_git/app":  true,
		"https://" + adoServerHost + "/tfs/OtherCollection/proj/_git/app":    false,
		"https://" + adoServerHost + "/tfs/DefaultCollectionX/proj/_git/app": false,
		"https://elsewhere.example/tfs/DefaultCollection/proj/_git/app":      false,
	} {
		if _, got := adoServerRunForRepo(row, repo, capSub); got != want {
			t.Errorf("%s: on the lane = %v, want %v", repo, got, want)
		}
	}
}

// The resolve injects a Server person's own token as Basic, only on the
// server's host, until the expiry they entered.
func TestResolveADOOwnPAT_ServerInjectsBasicOnItsHostOnly(t *testing.T) {
	f := newOwnPATRunOn(t, adoServerTestRow(adoServerOrgKey), adoServerRepo)
	exp := f.now.Add(2 * time.Hour)
	f.token(t, adoServerToken, "defaultcollection", exp)
	w := f.resolve(t, capSub, adoServerHost, "")
	var resp types.ResolvedInjection
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if w.Code != http.StatusOK || resp.Header != "Authorization" || resp.Value != adoOwnPATHeaderValue(adoServerToken) ||
		resp.ExpiresAt == 0 || resp.ExpiresAt > exp.UnixMilli() || strings.Join(resp.Capabilities, ",") != "code_read,code_write" {
		t.Fatalf("resolve = %d %s", w.Code, w.Body)
	}
	for _, host := range []string{"dev.azure.com", "elsewhere.example"} {
		wantRefused(t, f.resolve(t, capSub, host, ""), http.StatusForbidden, reasonHostNotOrganisation)
	}
}

// SECURITY: a Server run's resolve refuses a capability ask (nothing widens
// the git lane's set), a token stored for another collection, an expired
// token, and a row that moved or was turned off.
func TestResolveADOOwnPAT_ServerRefusals(t *testing.T) {
	t.Run("capability ask", func(t *testing.T) {
		f := newOwnPATRunOn(t, adoServerTestRow(adoServerOrgKey), adoServerRepo)
		f.token(t, adoServerToken, "defaultcollection", f.now.Add(time.Hour))
		wantRefused(t, f.resolve(t, capSub, adoServerHost, "?capability="+string(adoscope.CapPR)+"&first_use=wait_for_review"),
			http.StatusForbidden, reasonCapabilityAboveCeiling)
	})
	t.Run("token for another collection", func(t *testing.T) {
		f := newOwnPATRunOn(t, adoServerTestRow(adoServerOrgKey), adoServerRepo)
		f.token(t, adoServerToken, "othercollection", f.now.Add(time.Hour))
		wantRefused(t, f.resolve(t, capSub, adoServerHost, ""), http.StatusForbidden, reasonADOOwnPATOtherOrg)
	})
	t.Run("expired", func(t *testing.T) {
		f := newOwnPATRunOn(t, adoServerTestRow(adoServerOrgKey), adoServerRepo)
		f.token(t, adoServerToken, "defaultcollection", f.now.Add(-time.Minute))
		wantRefused(t, f.resolve(t, capSub, adoServerHost, "?phase=boot"), http.StatusForbidden, reasonADOOwnPATExpired)
	})
	for name, change := range map[string]func(*types.GitProvider){
		"row turned off":       func(r *types.GitProvider) { r.Disabled = true },
		"collection moved":     func(r *types.GitProvider) { r.BaseURLs = []string{"https://" + adoServerHost + "/OtherCollection"} },
		"shared again":         func(r *types.GitProvider) { r.CredentialSource = types.CredentialSourceShared },
		"address lost its pin": func(r *types.GitProvider) { r.BaseURLs = []string{"https://" + adoServerHost} },
	} {
		t.Run(name, func(t *testing.T) {
			f := newOwnPATRunOn(t, adoServerTestRow(adoServerOrgKey), adoServerRepo)
			f.token(t, adoServerToken, "defaultcollection", f.now.Add(time.Hour))
			row := adoServerTestRow(adoServerOrgKey)
			change(&row)
			f.st.site = adoSite(row)
			wantRefused(t, f.resolve(t, capSub, adoServerHost, ""), http.StatusForbidden, reasonScopeChanged)
		})
	}
}

// The launch gate grades a Server row from the person's own token, as on
// Services: an expired one is refused.
func TestGitCredentialRefusal_ServerOwnPAT(t *testing.T) {
	srv := newSCMTestServer(t, adoSite(adoServerTestRow(adoServerOrgKey)), false)
	srv.cfg.Now = func() time.Time { return ownPATNow }
	if err := srv.gitCredentialRefusalForLauncher(context.Background(), capSub, adoServerRepo); err == nil ||
		err.Error() != gitCredentialNotConnectedRefusal {
		t.Fatalf("no token: refusal = %v", err)
	}
	f := &ownPATRun{srv: srv, now: ownPATNow}
	f.token(t, adoServerToken, "defaultcollection", ownPATNow.Add(-time.Minute))
	if err := srv.gitCredentialRefusalForLauncher(context.Background(), capSub, adoServerRepo); err == nil ||
		err.Error() != gitCredentialOwnPATExpiredRefusal {
		t.Fatalf("expired: refusal = %v", err)
	}
}

// TestADOServerLane_ProxyContract dispatches a Server own_pat run and reads
// back the sidecar configuration it builds, as the sidecar loads it: exactly
// the contract the proxy's Server git door (#1430) is written against — the
// grant names the collection path and the server host, one TLS-only injection
// per host, the host allowlisted on 443 and on the git broker's list, and no
// TLS-interception entry — and the injection resolves to the person's own
// token as Basic, with its expiry.
func TestADOServerLane_ProxyContract(t *testing.T) {
	const collectionURL = "https://" + adoServerHost + "/tfs/DefaultCollection"
	f := newOwnPATRunOn(t, adoServerTestRow(collectionURL), collectionURL+"/proj/_git/app")
	f.token(t, adoServerToken, "tfs/defaultcollection", f.now.Add(3*time.Hour))

	caCert, caKey, err := generateRunCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ado, on := resolveADOEntraRun(f.st.site, []string{collectionURL + "/proj/_git/app"}, capSub)
	policy := types.RunPolicySpec{}
	env := map[string]string{}
	lane, ok := f.srv.authorADOEntraLane(context.Background(), types.AgentRun{ID: f.runID}, ado, on, adoEntraUngraded(),
		adoStandingBound{}, dispatchLLMPlan{mitmCACertPEM: string(caCert), mitmCAKeyPEM: string(caKey)}, &policy, env, nil)
	if !ok {
		t.Fatal("dispatch refused a Server run")
	}
	raw, err := runner.BuildProxyConfig(f.runID, runner.ProxyConfig{
		RunToken: "run-token", ControlPlaneURL: "http://127.0.0.1:1", Policy: policy, Injection: lane.injections,
		MITMCACertPEM: string(caCert), MITMCAKeyPEM: string(caKey), MITMHosts: lane.mitmHosts, ADOGrant: lane.gate,
	}, 3128)
	if err != nil {
		t.Fatalf("BuildProxyConfig: %v", err)
	}
	cfg, err := proxy.LoadConfigBytes(raw)
	if err != nil {
		t.Fatalf("LoadConfigBytes: %v", err)
	}
	if g := cfg.ADOGrant; g == nil || g.Organization != "tfs/defaultcollection" ||
		!slices.Equal(g.Hosts, []string{adoServerHost}) || !slices.Equal(g.Capabilities, adoServerCapabilities) {
		t.Fatalf("ado_grant = %+v", cfg.ADOGrant)
	}
	if len(cfg.Injection) != 1 || cfg.Injection[0].Host != adoServerHost || !cfg.Injection[0].RequireTLS ||
		cfg.Injection[0].SecretName != types.ADOEntraAccessTokenSecret {
		t.Fatalf("injection = %+v, want one TLS-only rule for %s", cfg.Injection, adoServerHost)
	}
	if !slices.Contains(cfg.Policy.AllowedDomains, adoServerHost+":443") {
		t.Errorf("allowlist = %v, want %s:443", cfg.Policy.AllowedDomains, adoServerHost)
	}
	if len(cfg.MITMHosts) != 0 {
		t.Errorf("mitm_hosts = %v, want none", cfg.MITMHosts)
	}
	if env["WARDYN_GIT_PAT_BROKER_HOSTS"] != adoServerHost {
		t.Errorf("WARDYN_GIT_PAT_BROKER_HOSTS = %q", env["WARDYN_GIT_PAT_BROKER_HOSTS"])
	}

	// The injection the sidecar resolves at boot: the person's token as Basic.
	f.grantID = cfg.Injection[0].GrantID
	w := f.resolve(t, capSub, adoServerHost, "?phase=boot")
	var resp types.ResolvedInjection
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || w.Code != http.StatusOK {
		t.Fatalf("resolve = %d %s", w.Code, w.Body)
	}
	if resp.Header != "Authorization" || resp.Value != adoOwnPATHeaderValue(adoServerToken) || resp.ExpiresAt == 0 ||
		resp.ExpiresAt > f.now.Add(3*time.Hour).UnixMilli() || resp.JTI == "" {
		t.Errorf("resolved = %+v", resp)
	}
}

// roundTripFunc is an http.RoundTripper made of a function.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// SECURITY (#1444): a Server row has no Graph door. Even a person whose
// sign-in carries an object id, and a collection whose connectionData names a
// subjectDescriptor, never has the token sent to the Graph host; the token is
// judged by its Mail alone.
func TestADOServerOwnPATPut_NeverReachesTheGraphHost(t *testing.T) {
	var graphs int
	d := newServerOwnPATDoor(t, adoServerOrgKey)
	base := adoOwnPATTransport
	adoOwnPATTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/_apis/graph/") {
			graphs++
			return &http.Response{StatusCode: http.StatusNotFound, Header: http.Header{}, Request: r, Body: http.NoBody}, nil
		}
		if !strings.HasSuffix(r.URL.Path, "/_apis/connectionData") {
			return base.RoundTrip(r)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Request: r, Body: io.NopCloser(strings.NewReader(
			`{"authenticatedUser":{"subjectDescriptor":"s-1-5-21-1","properties":{"Account":{"$value":"CORP\\carol"},"Mail":{"$value":"carol@corp.example"}}}}`))}, nil
	})
	t.Cleanup(func() { adoOwnPATTransport = base })
	payload, err := json.Marshal(oidc.Session{
		V: oidc.SessionCodecVersion, Sub: capSub, Email: capEmail, Role: oidc.RoleUser, UserType: types.UserTypeStandard,
		ObjectID: "0a1b2c3d-1111-2222-3333-444455556666", Expiry: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	code, body := d.putAs(t, signedSessionCookie(payload), adoServerOrgKey, adoServerToken, days(10))
	if graphs != 0 {
		t.Fatalf("a Server row's token reached the Graph host %d times, want none", graphs)
	}
	if code != http.StatusForbidden {
		t.Fatalf("PUT = %d %s, want 403: Mail is carol's, and the Graph door is not consulted", code, body)
	}
}
