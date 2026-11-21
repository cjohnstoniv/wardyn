// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The Postgres-backed fixture of the service grant tests: the real store and a real router, with the scripted
// identity provider and the revision-keeping secret store of the in-memory fixture. Skipped when WARDYN_TEST_PG
// is unset, like every _pg_test.go file here.

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

type svcPG struct {
	t      *testing.T
	pool   *pgxpool.Pool
	pg     store.PG
	srv    *Server
	sec    *svcSecrets
	idp    *svcIdP
	audit  *memAudit
	admin  *http.Cookie
	person *http.Cookie
}

// newSvcPG builds the fixture over a throwaway database. activated is the 0.10.2 switch-on.
func newSvcPG(t *testing.T, activated bool) *svcPG {
	t.Helper()
	return newSvcPGClocked(t, activated, nil)
}

// newSvcPGClocked is newSvcPG with the server reading clock when it is set.
func newSvcPGClocked(t *testing.T, activated bool, clock *svcClock) *svcPG {
	t.Helper()
	pool := throwawayPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	if _, err := pg.PutSiteConfig(ctx, types.SiteConfig{}); err != nil {
		t.Fatal(err)
	}
	f := &svcPG{t: t, pool: pool, pg: pg, sec: newSvcSecrets(), idp: &svcIdP{}, audit: &memAudit{}}
	f.sec.m[oidcSecretRef] = []byte(oidcClientSecret)
	if activated {
		f.activate()
	}
	h := newHarness(t)
	cfg := baseTestConfig(h, pg)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Audit = f.audit
	cfg.Secrets, cfg.MaskRegistry, cfg.ServiceGrantTransport = f.sec, secretmask.NewRegistry(), f.idp
	cfg.AllowTestEndpoints = true
	cfg.HostResolver = func(string) ([]net.IP, error) { return []net.IP{net.ParseIP("203.0.113.9")}, nil }
	if clock != nil {
		cfg.Now = clock.Now
	}
	f.srv = New(cfg)
	f.admin = ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)
	f.person = ssoSession(t, "sub-person", "person@corp.example", oidc.RoleUser)
	return f
}

// newReplica is a second server on the same database, with its own memory, secrets view and identity provider.
func (f *svcPG) newReplica() *svcPG {
	f.t.Helper()
	return f.newReplicaClocked(nil)
}

// newReplicaClocked is newReplica reading clock when it is set, so a test can bring the replica's cached token
// to its renewal point.
func (f *svcPG) newReplicaClocked(clock *svcClock) *svcPG {
	f.t.Helper()
	h := newHarness(f.t)
	r := &svcPG{t: f.t, pool: f.pool, pg: f.pg, sec: f.sec, idp: &svcIdP{}, audit: &memAudit{}, admin: f.admin, person: f.person}
	cfg := baseTestConfig(h, f.pg)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Audit = r.audit
	cfg.Secrets, cfg.MaskRegistry, cfg.ServiceGrantTransport = f.sec, secretmask.NewRegistry(), r.idp
	cfg.AllowTestEndpoints = true
	cfg.HostResolver = func(string) ([]net.IP, error) { return []net.IP{net.ParseIP("203.0.113.9")}, nil }
	if clock != nil {
		cfg.Now = clock.Now
	}
	r.srv = New(cfg)
	return r
}

// activate stores the 0.10.2 switch-on mark, as lane 2's activate call does.
func (f *svcPG) activate() {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE site_config SET config = config || jsonb_build_object($1::text, to_jsonb(now()::text)) WHERE singleton`,
		store.PortalIssuersActivatedKey); err != nil {
		f.t.Fatal(err)
	}
}

// svcPGRow is a custom endpoint row that signs people in, with a grant when asked.
func svcPGRow(id string, grant bool) types.ModelProvider {
	p := endpointProvider()
	p.ID, p.Name = id, id
	p.BaseURL = "https://" + id + ".corp.example"
	p.OIDC = &types.ProviderOIDC{
		Issuer: "https://login.example.test", AuthorizeURL: "https://login.example.test/authorize", TokenURL: "https://login.example.test/token",
		ClientID: "wardyn-people", ClientSecretRef: oidcSecretRef, Scopes: []string{"api.read"}, IdentityBinding: types.OIDCBindingNone,
	}
	if grant {
		p.OIDC.ServiceGrant = svcPGGrant()
	}
	return p
}

func svcPGGrant() *types.ProviderServiceGrant {
	return &types.ProviderServiceGrant{GrantType: types.ServiceGrantTypeClientCreds, TokenURL: svcTokenURL, ClientID: svcClientID,
		Scope: "api.read", ServiceIdentities: []string{svcOwner}}
}

func (f *svcPG) do(method, path string, cookie *http.Cookie, body any) *httptest.ResponseRecorder {
	f.t.Helper()
	raw := ""
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			f.t.Fatal(err)
		}
		raw = string(b)
	}
	return doSSO(f.t, f.srv, method, "/api/v1"+path, cookie, raw)
}

// put is PUT /model-providers as an admin.
func (f *svcPG) put(rows ...types.ModelProvider) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.do(http.MethodPut, "/model-providers", f.admin, types.ModelProviders{Providers: rows})
}

func (f *svcPG) site() types.SiteConfig {
	f.t.Helper()
	sc, err := f.pg.GetSiteConfig(context.Background())
	if err != nil {
		f.t.Fatal(err)
	}
	return sc
}

// rows are the attached provider rows, as the console reads them back.
func (f *svcPG) rows() []types.ModelProvider {
	sc := f.site()
	if sc.ModelProviders == nil {
		return nil
	}
	return sc.ModelProviders.Providers
}

func (f *svcPG) row(id string) types.ModelProvider {
	f.t.Helper()
	for _, p := range f.rows() {
		if p.ID == id {
			return p
		}
	}
	f.t.Fatalf("no provider %q", id)
	return types.ModelProvider{}
}

func (f *svcPG) uid(id string) string { return f.row(id).UID }

// stored reads one store-only key of the document, "" when it is absent.
func (f *svcPG) stored(key string) string {
	f.t.Helper()
	var raw *string
	if err := f.pool.QueryRow(context.Background(), `SELECT (config -> $1::text)::text FROM site_config WHERE singleton`, key).Scan(&raw); err != nil {
		f.t.Fatal(err)
	}
	if raw == nil {
		return ""
	}
	return *raw
}

func (f *svcPG) storedGrantUIDs() []string {
	f.t.Helper()
	var m map[string]json.RawMessage
	_ = json.Unmarshal([]byte(f.stored(store.ServiceGrantsKey)), &m)
	var out []string
	for uid := range m {
		out = append(out, uid)
	}
	return out
}

func (f *svcPG) claimUIDs() []string {
	f.t.Helper()
	var m map[string]json.RawMessage
	_ = json.Unmarshal([]byte(f.stored(store.ServiceGrantCleanupsKey)), &m)
	var out []string
	for uid := range m {
		out = append(out, uid)
	}
	return out
}

func (f *svcPG) secretPath(id string) string {
	return "/model-providers/" + id + "/service-grant/secret"
}

// writeSecret is PUT .../service-grant/secret as an admin, for the row's present destination.
func (f *svcPG) writeSecret(id, value string) *httptest.ResponseRecorder {
	f.t.Helper()
	p := f.row(id)
	return f.do(http.MethodPut, f.secretPath(id), f.admin, putServiceGrantSecretRequest{
		Value: value, TokenURL: p.OIDC.ServiceGrant.TokenURL, ClientID: p.OIDC.ServiceGrant.ClientID, BaseURL: p.BaseURL})
}

func (f *svcPG) mint(id string) (serviceGrantMinted, error) {
	f.t.Helper()
	return f.srv.serviceGrantToken(context.Background(), f.row(id), "", serviceGrantTriggerRefresh)
}

// oldWriterSave is a 0.10.1 server's whole-document save: the document as the old type marshals it, merged
// over the stored one without touching a key the old type does not declare.
func (f *svcPG) oldWriterSave(cfg types.SiteConfig) {
	f.t.Helper()
	if cfg.ModelProviders != nil {
		block := *cfg.ModelProviders
		block.Providers = append([]types.ModelProvider(nil), block.Providers...)
		for i := range block.Providers {
			if o := block.Providers[i].OIDC; o != nil {
				c := *o
				c.ServiceGrant = nil
				block.Providers[i].OIDC = &c
			}
		}
		cfg.ModelProviders = &block
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		f.t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	_ = json.Unmarshal(raw, &keys)
	declared := []string{}
	for k := range keys {
		declared = append(declared, k)
	}
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE site_config SET config = (config - $1::text[]) || $2::jsonb WHERE singleton`, declared, raw); err != nil {
		f.t.Fatal(err)
	}
}

// secretNameOf is the derived record name of row id's grant.
func (f *svcPG) secretNameOf(id string) string { return serviceGrantSecretName(f.uid(id)) }

// secretBody is the secret route's body for row id's present destination.
func (f *svcPG) secretBody(id, value string) putServiceGrantSecretRequest {
	p := f.row(id)
	return putServiceGrantSecretRequest{Value: value, TokenURL: p.OIDC.ServiceGrant.TokenURL, ClientID: p.OIDC.ServiceGrant.ClientID, BaseURL: p.BaseURL}
}

// tokenOf asks this server for row id's token, as trigger would.
func (f *svcPG) tokenOf(id, trigger string) (serviceGrantMinted, error) {
	f.t.Helper()
	return f.srv.serviceGrantToken(context.Background(), f.row(id), "", trigger)
}

// cachedOn is s's cached token for uid.
func cachedOn(s *Server, uid string) (serviceGrantCached, bool) {
	s.svcGrant.mu.Lock()
	defer s.svcGrant.mu.Unlock()
	c, ok := s.svcGrant.cache[uid]
	return c, ok
}

// seedOne stores corp-a with a grant and writes its secret through the route.
func (f *svcPG) seedOne() {
	f.t.Helper()
	if w := f.put(svcPGRow("corp-a", true)); w.Code/100 != 2 {
		f.t.Fatalf("seed corp-a: %d %s", w.Code, w.Body.String())
	}
	if w := f.writeSecret("corp-a", svcClientSecret); w.Code != http.StatusNoContent {
		f.t.Fatalf("seed corp-a's secret: %d %s", w.Code, w.Body.String())
	}
}

// claimFor adds row id with a grant and a secret beside corp-a, then removes it while its external delete
// fails, so a stored claim is left on its uid. It returns that uid.
func (f *svcPG) claimFor(id string) string {
	f.t.Helper()
	if w := f.put(svcPGRow("corp-a", true), svcPGRow(id, true)); w.Code/100 != 2 {
		f.t.Fatalf("add %s: %d %s", id, w.Code, w.Body.String())
	}
	if w := f.writeSecret(id, svcClientSecret); w.Code != http.StatusNoContent {
		f.t.Fatalf("secret of %s: %d %s", id, w.Code, w.Body.String())
	}
	uid := f.uid(id)
	f.sec.mu.Lock()
	f.sec.delErr = errSvcDown
	f.sec.mu.Unlock()
	w := f.put(svcPGRow("corp-a", true))
	f.sec.mu.Lock()
	f.sec.delErr = nil
	f.sec.mu.Unlock()
	if w.Code/100 != 2 {
		f.t.Fatalf("remove %s: %d %s", id, w.Code, w.Body.String())
	}
	if !slices.Contains(f.claimUIDs(), uid) {
		f.t.Fatalf("no stored claim on %s's uid after its delete failed: %v", id, f.claimUIDs())
	}
	return uid
}

// retry is POST /service-grant-cleanups/{uid}/retry as an admin.
func (f *svcPG) retry(uid string) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.do(http.MethodPost, "/service-grant-cleanups/"+uid+"/retry", f.admin, nil)
}
