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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	secretspg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
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
	// sealed, when set, is the Postgres secret store every server of the fixture uses instead of sec, sealed
	// under ageKey, so a released wardynd given the same key reads the same records.
	sealed *secretspg.Store
	ageKey string
	// sink collects this server's mint-row copies; prepares counts its mint-row preparations.
	sink     *svcSink
	prepares *atomic.Int64
	// stop cancels this server's BaseCtx.
	stop context.CancelFunc
	// observer is a pool of its own for every probe, so a probe never asks a pool a holder or a server uses.
	observer *pgxpool.Pool
	idsMu    *sync.Mutex
	ids      *[]uuid.UUID
}

// newSvcPG builds the fixture over a throwaway database. activated is the 0.10.2 switch-on.
func newSvcPG(t *testing.T, activated bool) *svcPG {
	t.Helper()
	return newSvcPGClocked(t, activated, nil)
}

// newSvcPGClocked is newSvcPG with the server reading clock when it is set.
func newSvcPGClocked(t *testing.T, activated bool, clock *svcClock) *svcPG {
	t.Helper()
	return newSvcPGWith(t, activated, clock, false)
}

// newSvcPGSealed is newSvcPG with the Postgres secret store, sealed under a key of its own, for every server.
func newSvcPGSealed(t *testing.T, activated bool) *svcPG {
	t.Helper()
	return newSvcPGWith(t, activated, nil, true)
}

func newSvcPGWith(t *testing.T, activated bool, clock *svcClock, sealed bool) *svcPG {
	t.Helper()
	pool := throwawayPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	if _, err := pg.PutSiteConfig(ctx, types.SiteConfig{}); err != nil {
		t.Fatal(err)
	}
	sink := &svcSink{}
	pg.SIEM = sink
	f := &svcPG{t: t, pool: pool, pg: pg, sec: newSvcSecrets(), idp: &svcIdP{}, audit: &memAudit{}, sink: sink,
		prepares: new(atomic.Int64), idsMu: new(sync.Mutex), ids: new([]uuid.UUID)}
	obs, err := pgxpool.New(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	f.observer = obs
	t.Cleanup(obs.Close)
	f.sec.m[oidcSecretRef] = []byte(oidcClientSecret)
	if sealed {
		id, err := age.GenerateX25519Identity()
		if err != nil {
			t.Fatal(err)
		}
		if f.sealed, err = secretspg.New(pool, id); err != nil {
			t.Fatal(err)
		}
		f.ageKey = id.String()
		if err := f.sealed.Put(ctx, oidcSecretRef, []byte(oidcClientSecret)); err != nil {
			t.Fatal(err)
		}
	}
	if activated {
		f.activate()
	}
	h := newHarness(t)
	cfg := baseTestConfig(h, pg)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Audit = f.audit
	cfg.Secrets, cfg.MaskRegistry, cfg.ServiceGrantTransport = f.secrets(), secretmask.NewRegistry(), f.idp
	cfg.AllowTestEndpoints = true
	cfg.HostResolver = func(string) ([]net.IP, error) { return []net.IP{net.ParseIP("203.0.113.9")}, nil }
	if clock != nil {
		cfg.Now = clock.Now
	}
	cfg.RunnerAuditPrepare = svcPrepare(cfg.MaskRegistry, f.prepares)
	base, stop := context.WithCancel(context.Background())
	cfg.BaseCtx, f.stop = base, stop
	f.srv = New(cfg)
	t.Cleanup(func() { svcQuiesce(t, f.srv); stop() })
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
	r := &svcPG{t: f.t, pool: f.pool, pg: f.pg, sec: f.sec, idp: &svcIdP{}, audit: &memAudit{}, admin: f.admin, person: f.person,
		sealed: f.sealed, ageKey: f.ageKey, sink: f.sink, prepares: f.prepares, observer: f.observer,
		idsMu: new(sync.Mutex), ids: new([]uuid.UUID)}
	cfg := baseTestConfig(h, f.pg)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Audit = r.audit
	cfg.Secrets, cfg.MaskRegistry, cfg.ServiceGrantTransport = f.secrets(), secretmask.NewRegistry(), r.idp
	cfg.AllowTestEndpoints = true
	cfg.HostResolver = func(string) ([]net.IP, error) { return []net.IP{net.ParseIP("203.0.113.9")}, nil }
	if clock != nil {
		cfg.Now = clock.Now
	}
	cfg.RunnerAuditPrepare = svcPrepare(cfg.MaskRegistry, r.prepares)
	base, stop := context.WithCancel(context.Background())
	cfg.BaseCtx, r.stop = base, stop
	r.srv = New(cfg)
	f.t.Cleanup(func() { svcQuiesce(f.t, r.srv); stop() })
	return r
}

// newIndependentReplica is a second replica with a pool, a store, a sink and a Postgres secret store of its own
// over the same database, as another wardynd process has. The fixture must be sealed (newSvcPGSealed).
func (f *svcPG) newIndependentReplica() *svcPG {
	f.t.Helper()
	ctx := context.Background()
	pool, err := db.Connect(ctx, f.pool.Config().ConnString())
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(pool.Close)
	id, err := age.ParseX25519Identity(f.ageKey)
	if err != nil {
		f.t.Fatal(err)
	}
	sealed, err := secretspg.New(pool, id)
	if err != nil {
		f.t.Fatal(err)
	}
	pg := store.NewPG(pool)
	sink := &svcSink{}
	pg.SIEM = sink
	r := &svcPG{t: f.t, pool: pool, pg: pg, sec: f.sec, idp: &svcIdP{}, audit: &memAudit{}, admin: f.admin, person: f.person,
		sealed: sealed, ageKey: f.ageKey, sink: sink, prepares: new(atomic.Int64), observer: f.observer,
		idsMu: new(sync.Mutex), ids: new([]uuid.UUID)}
	h := newHarness(f.t)
	cfg := baseTestConfig(h, pg)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Audit = r.audit
	cfg.Secrets, cfg.MaskRegistry, cfg.ServiceGrantTransport = sealed, secretmask.NewRegistry(), r.idp
	cfg.AllowTestEndpoints = true
	cfg.HostResolver = func(string) ([]net.IP, error) { return []net.IP{net.ParseIP("203.0.113.9")}, nil }
	cfg.RunnerAuditPrepare = svcPrepare(cfg.MaskRegistry, r.prepares)
	base, stop := context.WithCancel(context.Background())
	cfg.BaseCtx, r.stop = base, stop
	r.srv = New(cfg)
	f.t.Cleanup(func() { svcQuiesce(f.t, r.srv); stop() })
	return r
}

// captureRowIDs records each sent request's mint-row id on this server, in order. Called once, before the first mint.
func (f *svcPG) captureRowIDs() func() []uuid.UUID {
	f.srv.svcGrant.rowID = func(id uuid.UUID) {
		f.idsMu.Lock()
		defer f.idsMu.Unlock()
		*f.ids = append(*f.ids, id)
	}
	return func() []uuid.UUID {
		f.idsMu.Lock()
		defer f.idsMu.Unlock()
		return append([]uuid.UUID(nil), *f.ids...)
	}
}

// mintRows are the mint rows in audit_events, in seq order, read through the observer.
func (f *svcPG) mintRows() []types.AuditEvent {
	f.t.Helper()
	return f.queryRows(`SELECT id, time, outcome, data FROM audit_events WHERE action=$1 ORDER BY seq`, auditServiceGrantMintAction)
}

// rowsByID are the rows with id in audit_events, read through the observer.
func (f *svcPG) rowsByID(id uuid.UUID) []types.AuditEvent {
	f.t.Helper()
	return f.queryRows(`SELECT id, time, outcome, data FROM audit_events WHERE id=$1 ORDER BY seq`, id)
}

func (f *svcPG) queryRows(q string, arg any) []types.AuditEvent {
	f.t.Helper()
	rows, err := f.observer.Query(context.Background(), q, arg)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []types.AuditEvent
	for rows.Next() {
		var ev types.AuditEvent
		var data []byte
		if err := rows.Scan(&ev.ID, &ev.Time, &ev.Outcome, &data); err != nil {
			f.t.Fatal(err)
		}
		ev.Action, ev.Data = auditServiceGrantMintAction, data
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatal(err)
	}
	return out
}

// sinkRows are this server's sink copies of mint rows.
func (f *svcPG) sinkRows() []types.AuditEvent { return f.sink.mintCopies() }

// awaitRows waits until each id has exactly one row in audit_events.
func (f *svcPG) awaitRows(t *testing.T, ids ...uuid.UUID) {
	t.Helper()
	deadline := time.Now().Add(svcSettleWait())
	for {
		ok := true
		for _, id := range ids {
			ok = ok && len(f.rowsByID(id)) == 1
		}
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for one row per id %v", ids)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// awaitSink waits until this server's sink holds a copy of each id and returns those copies.
func (f *svcPG) awaitSink(t *testing.T, ids ...uuid.UUID) []types.AuditEvent {
	t.Helper()
	return svcAwaitSink(t, f.sink, ids...)
}

// sinkSettled is svcFix.sinkSettled for this server.
func (f *svcPG) sinkSettled(t *testing.T) {
	t.Helper()
	svcSinkSettled(t, f.srv, f.sink)
}

// drainSink is svcFix.drainSink for this server.
func (f *svcPG) drainSink(ctx context.Context) error { return svcDrainSink(ctx, f.srv, f.sink) }

// holdAuditChain holds the audit chain lock on a pool of its own until release.
func (f *svcPG) holdAuditChain() (release func()) {
	f.t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, f.pool.Config().ConnString())
	if err != nil {
		f.t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		pool.Close()
		f.t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, db.AuditChainLockKey); err != nil {
		_ = tx.Rollback(ctx)
		pool.Close()
		f.t.Fatal(err)
	}
	var once sync.Once
	release = func() {
		once.Do(func() {
			_ = tx.Rollback(context.WithoutCancel(ctx))
			pool.Close()
		})
	}
	f.t.Cleanup(release)
	return release
}

// holdRotation starts, on a pool of its own, a transaction that moves the secret's value_rev and waits on the
// row; it returns at once. commit waits for the UPDATE, commits it and closes the pool.
func (f *svcPG) holdRotation(owner, name string) (commit func()) {
	f.t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, f.pool.Config().ConnString())
	if err != nil {
		f.t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		pool.Close()
		f.t.Fatal(err)
	}
	updated := make(chan error, 1)
	go func() {
		_, err := tx.Exec(ctx, `UPDATE secrets SET value_rev = DEFAULT WHERE owned_by=$1 AND name=$2`, owner, name)
		updated <- err
	}()
	var once sync.Once
	commit = func() {
		once.Do(func() {
			if err := <-updated; err != nil {
				f.t.Errorf("the held rotation: %v", err)
				_ = tx.Rollback(ctx)
			} else if err := tx.Commit(ctx); err != nil {
				f.t.Errorf("commit the held rotation: %v", err)
			}
			pool.Close()
		})
	}
	f.t.Cleanup(commit)
	return commit
}

// lockWaiters counts the sessions of this database waiting on a lock whose query names like.
func (f *svcPG) lockWaiters(like string) int {
	f.t.Helper()
	var n int
	if err := f.observer.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity
		WHERE datname = current_database() AND wait_event_type = 'Lock' AND query ILIKE '%' || $1 || '%'`, like).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

// siteConfigRaw is the stored site_config document, read through the observer.
func (f *svcPG) siteConfigRaw() []byte {
	f.t.Helper()
	var raw []byte
	if err := f.observer.QueryRow(context.Background(), `SELECT config FROM site_config WHERE singleton`).Scan(&raw); err != nil {
		f.t.Fatal(err)
	}
	return raw
}

// valueRev is the secret row's value_rev, read through the observer.
func (f *svcPG) valueRev(owner, name string) string {
	f.t.Helper()
	var rev string
	if err := f.observer.QueryRow(context.Background(), `SELECT value_rev::text FROM secrets WHERE owned_by=$1 AND name=$2`, owner, name).Scan(&rev); err != nil {
		return ""
	}
	return rev
}

// secrets is the store the fixture's servers use: the sealed Postgres one when set, else the double.
func (f *svcPG) secrets() secretstore.Store {
	if f.sealed != nil {
		return f.sealed
	}
	return f.sec
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
