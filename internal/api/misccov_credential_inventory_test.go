// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/keydomain"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// miscCovMetaSecrets is a secret store that keeps credential metadata. It serves per-owner views, which
// record what they were asked, and the cross-owner read the inventory uses.
type miscCovMetaSecrets struct {
	secretstore.Store
	mu sync.Mutex

	everywhere      []secretstore.Meta
	everywhereErr   error
	askedEverywhere [][]string

	views map[string]*miscCovMetaView
}

func (s *miscCovMetaSecrets) MarkUsed(context.Context, string) error { return nil }
func (s *miscCovMetaSecrets) Metadata(context.Context, []string) ([]secretstore.Meta, error) {
	return nil, nil
}

func (s *miscCovMetaSecrets) MetadataEverywhere(_ context.Context, names []string) ([]secretstore.Meta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.askedEverywhere = append(s.askedEverywhere, slices.Clone(names))
	return s.everywhere, s.everywhereErr
}

func (s *miscCovMetaSecrets) For(owner string) secretstore.Store {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.views[owner]; ok {
		return v
	}
	return &miscCovPlainView{}
}

// miscCovPlainView is an owner's view of a store that keeps no metadata.
type miscCovPlainView struct{ secretstore.Store }

// miscCovMetaView is one owner's view that keeps metadata.
type miscCovMetaView struct {
	secretstore.Store
	mu        sync.Mutex
	used      []string
	markErr   error
	metas     []secretstore.Meta
	metaErr   error
	askedMeta [][]string
}

func (v *miscCovMetaView) MarkUsed(_ context.Context, name string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.used = append(v.used, name)
	return v.markErr
}

func (v *miscCovMetaView) Metadata(_ context.Context, names []string) ([]secretstore.Meta, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.askedMeta = append(v.askedMeta, slices.Clone(names))
	return v.metas, v.metaErr
}

func (v *miscCovMetaView) MetadataEverywhere(context.Context, []string) ([]secretstore.Meta, error) {
	return nil, nil
}

func TestMiscCovProviderCredentialNameIsTheRowOfTheProvidersKind(t *testing.T) {
	for _, tc := range []struct {
		kind types.ModelProviderKind
		part string
	}{
		{types.ModelProviderBedrockSSO, providerSSOPart},
		{types.ModelProviderAnthropicSubscription, providerOAuthPart},
		{types.ModelProviderAzureFoundry, providerEntraPart},
		{types.ModelProviderAnthropicAPIKey, providerKeyPart},
		{types.ModelProviderCustomEndpoint, providerKeyPart},
	} {
		p := types.ModelProvider{UID: "uid-9", Kind: tc.kind}
		if got, want := providerCredentialName(p), providerSecretName("uid-9", tc.part); got != want {
			t.Errorf("kind %q: row %q, want %q", tc.kind, got, want)
		}
	}
}

func TestMiscCovStampCredentialUse(t *testing.T) {
	ctx := t.Context()
	newSrv := func(sec secretstore.Store) *Server {
		s := newHarness(t).srv
		s.cfg.Secrets = sec
		return s
	}

	t.Run("it stamps the owner's own row", func(t *testing.T) {
		view := &miscCovMetaView{}
		newSrv(&miscCovMetaSecrets{views: map[string]*miscCovMetaView{"sub-a": view}}).stampCredentialUse(ctx, "sub-a", "cred-row")
		if !slices.Equal(view.used, []string{"cred-row"}) {
			t.Errorf("stamped %v, want [cred-row]", view.used)
		}
	})
	t.Run("the operator namespace and a missing store stamp nothing", func(t *testing.T) {
		view := &miscCovMetaView{}
		sec := &miscCovMetaSecrets{views: map[string]*miscCovMetaView{"": view}}
		newSrv(sec).stampCredentialUse(ctx, "", "cred-row")
		newSrv(nil).stampCredentialUse(ctx, "sub-a", "cred-row")
		if len(view.used) != 0 {
			t.Errorf("stamped %v for the operator namespace, want nothing", view.used)
		}
	})
	t.Run("a view that keeps no metadata is left alone", func(t *testing.T) {
		logs := miscCovCaptureLogs(t)
		newSrv(&miscCovMetaSecrets{}).stampCredentialUse(ctx, "sub-a", "cred-row")
		if _, ok := logs.find("last use failed"); ok {
			t.Error("a view with no metadata logged a failure")
		}
	})
	t.Run("a failed stamp is logged and never refused, except for a store with no metadata", func(t *testing.T) {
		boom := errors.New("write refused")
		logs := miscCovCaptureLogs(t)
		quiet := &miscCovMetaView{markErr: secretstore.ErrNoMetadata}
		newSrv(&miscCovMetaSecrets{views: map[string]*miscCovMetaView{"sub-q": quiet}}).stampCredentialUse(ctx, "sub-q", "cred-row")
		if _, ok := logs.find("last use failed"); ok {
			t.Fatal("ErrNoMetadata was logged as a failure")
		}
		loud := &miscCovMetaView{markErr: boom}
		newSrv(&miscCovMetaSecrets{views: map[string]*miscCovMetaView{"sub-l": loud}}).stampCredentialUse(ctx, "sub-l", "cred-row")
		rec, ok := logs.find("last use failed")
		if !ok {
			t.Fatal("a failed stamp was not logged")
		}
		if v, _ := logs.attr(rec, "err"); !errors.Is(v.Any().(error), boom) {
			t.Errorf("logged err = %v, want the store's", v)
		}
	})
}

func TestMiscCovAttachOwnCredentialMeta(t *testing.T) {
	added := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	used := added.Add(48 * time.Hour)
	key := types.ModelProvider{ID: "gw", UID: "uid-gw", Kind: types.ModelProviderAnthropicAPIKey}
	sso := types.ModelProvider{ID: "sso", UID: "uid-sso", Kind: types.ModelProviderBedrockSSO}
	sc := types.SiteConfig{ModelProviders: &types.ModelProviders{Providers: []types.ModelProvider{key, sso}}}
	rows := func() []SetupProviderAccess {
		return []SetupProviderAccess{{Provider: "gw"}, {Provider: "ghost"}, {Provider: "sso"}}
	}
	newSrv := func(view *miscCovMetaView) *Server {
		s := newHarness(t).srv
		s.cfg.Secrets = &miscCovMetaSecrets{views: map[string]*miscCovMetaView{"sub-a": view}}
		return s
	}

	t.Run("each provider row gets the caller's own times, and an unknown provider is skipped", func(t *testing.T) {
		view := &miscCovMetaView{metas: []secretstore.Meta{
			{Name: providerCredentialName(key), AddedAt: added, LastUsedAt: &used},
			{Name: providerCredentialName(sso), AddedAt: added},
			{Name: "not-a-provider-row", AddedAt: added},
		}}
		got := rows()
		newSrv(view).attachOwnCredentialMeta(t.Context(), sc, got, "sub-a")
		wantNames := []string{providerCredentialName(key), providerCredentialName(sso)}
		if len(view.askedMeta) != 1 || !slices.Equal(view.askedMeta[0], wantNames) {
			t.Fatalf("asked for %v, want exactly the known providers' rows %v", view.askedMeta, wantNames)
		}
		if got[0].AddedAt == nil || !got[0].AddedAt.Equal(added) || got[0].LastUsedAt == nil || !got[0].LastUsedAt.Equal(used) {
			t.Errorf("gw row = %+v, want added and last-used set", got[0])
		}
		if got[1].AddedAt != nil || got[1].LastUsedAt != nil {
			t.Errorf("the unknown provider's row was stamped: %+v", got[1])
		}
		if got[2].AddedAt == nil || got[2].LastUsedAt != nil {
			t.Errorf("sso row = %+v, want added only", got[2])
		}
	})
	t.Run("a failed read leaves the rows as graded", func(t *testing.T) {
		got := rows()
		newSrv(&miscCovMetaView{metaErr: errors.New("read refused")}).attachOwnCredentialMeta(t.Context(), sc, got, "sub-a")
		for i, r := range got {
			if r.AddedAt != nil || r.LastUsedAt != nil {
				t.Errorf("row %d changed by a failed read: %+v", i, r)
			}
		}
	})
	t.Run("nothing is read for a caller with no namespace of their own", func(t *testing.T) {
		view := &miscCovMetaView{}
		srv := newSrv(view)
		srv.attachOwnCredentialMeta(t.Context(), sc, rows(), "")
		srv.attachOwnCredentialMeta(t.Context(), sc, nil, "sub-a")
		srv.cfg.Secrets = nil
		srv.attachOwnCredentialMeta(t.Context(), sc, rows(), "sub-a")
		if len(view.askedMeta) != 0 {
			t.Errorf("metadata was read %d times, want none", len(view.askedMeta))
		}
	})
	t.Run("a store whose view keeps no metadata leaves the rows", func(t *testing.T) {
		srv := newHarness(t).srv
		srv.cfg.Secrets = &miscCovMetaSecrets{}
		got := rows()
		srv.attachOwnCredentialMeta(t.Context(), sc, got, "sub-a")
		if got[0].AddedAt != nil {
			t.Errorf("row stamped from a view with no metadata: %+v", got[0])
		}
	})
}

// miscCovInvStore is the site config and identity directory the inventory handler reads.
type miscCovInvStore struct {
	store.Store
	sc    types.SiteConfig
	scErr error
	toks  []types.APIToken
}

func (s *miscCovInvStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return s.sc, s.scErr
}
func (s *miscCovInvStore) ListAPITokens(context.Context) ([]types.APIToken, error) {
	return s.toks, nil
}
func (s *miscCovInvStore) ListWorkspaces(context.Context) ([]types.Workspace, error) { return nil, nil }

func miscCovInventoryServer(t *testing.T, st *miscCovInvStore, sec secretstore.Store) *Server {
	t.Helper()
	h := newHarness(t)
	h.srv.cfg.Store = st
	h.srv.cfg.Secrets = sec
	// Mounted while the site config reads, as it does at boot; a failure is then the request's.
	failWith := st.scErr
	st.scErr = nil
	h.srv.router = h.srv.routes()
	st.scErr = failWith
	return h.srv
}

const miscCovInventoryPath = "/api/v1/model-providers/credentials"

func TestMiscCovCredentialInventoryHandlerRefusals(t *testing.T) {
	two := &types.ModelProviders{Providers: []types.ModelProvider{
		{ID: "gw", UID: "uid-gw", Name: "Gateway", Kind: types.ModelProviderAnthropicAPIKey},
	}}
	for _, tc := range []struct {
		name   string
		st     *miscCovInvStore
		sec    secretstore.Store
		want   int
		reason string
	}{
		{"a store that keeps no metadata", &miscCovInvStore{sc: types.SiteConfig{ModelProviders: two}}, &memSecrets{}, http.StatusServiceUnavailable, reasonCredentialInventoryNoMeta},
		{"a wrapper that reports no metadata", &miscCovInvStore{sc: types.SiteConfig{ModelProviders: two}},
			&miscCovMetaSecrets{everywhereErr: secretstore.ErrNoMetadata}, http.StatusServiceUnavailable, reasonCredentialInventoryNoMeta},
		{"a metadata read that fails", &miscCovInvStore{sc: types.SiteConfig{ModelProviders: two}},
			&miscCovMetaSecrets{everywhereErr: errors.New("read refused")}, http.StatusInternalServerError, reasonInternalError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := do(t, miscCovInventoryServer(t, tc.st, tc.sec), http.MethodGet, miscCovInventoryPath, adminToken, "")
			if w.Code != tc.want || errorReason(w) != tc.reason {
				t.Errorf("GET = %d %s (%s), want %d %s", w.Code, errorReason(w), w.Body, tc.want, tc.reason)
			}
		})
	}
}

// The route's own gate answers first when the site config cannot be read, so the handler's refusal is
// asked for directly: a 500, and no metadata read.
func TestMiscCovCredentialInventoryHandlerAnswers500OnAnUnreadableSiteConfig(t *testing.T) {
	sec := &miscCovMetaSecrets{}
	srv := miscCovInventoryServer(t, &miscCovInvStore{scErr: errors.New("db down")}, sec)
	w := httptest.NewRecorder()
	srv.handleCredentialInventory(w, httptest.NewRequest(http.MethodGet, miscCovInventoryPath, nil))
	if w.Code != http.StatusInternalServerError || errorReason(w) != reasonInternalError {
		t.Fatalf("handler = %d %s (%s), want 500 %s", w.Code, errorReason(w), w.Body, reasonInternalError)
	}
	if len(sec.askedEverywhere) != 0 {
		t.Errorf("metadata was read %d times after the site config failed", len(sec.askedEverywhere))
	}
}

func TestMiscCovCredentialInventoryWithNoProvidersReadsNoMetadata(t *testing.T) {
	sec := &miscCovMetaSecrets{}
	w := do(t, miscCovInventoryServer(t, &miscCovInvStore{}, sec), http.MethodGet, miscCovInventoryPath, adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET = %d %s", w.Code, w.Body)
	}
	var inv credentialInventory
	if err := json.Unmarshal(w.Body.Bytes(), &inv); err != nil {
		t.Fatal(err)
	}
	if len(inv.Credentials) != 0 || inv.Counts.People != 0 || len(inv.Counts.ByProvider) != 0 {
		t.Errorf("inventory = %+v, want it empty", inv)
	}
	if len(sec.askedEverywhere) != 0 {
		t.Errorf("metadata was read %d times with no provider configured", len(sec.askedEverywhere))
	}
}

// The inventory lists each person's credential for each configured provider, counts them per
// provider, leaves the operator namespace and rows of no provider out, and (with a key-domain service
// whose database cannot be read) carries no key domain rather than a made-up one.
func TestMiscCovCredentialInventoryListsHoldersAndCounts(t *testing.T) {
	added := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	gw := types.ModelProvider{ID: "gw", UID: "uid-gw", Name: "Gateway", Kind: types.ModelProviderAnthropicAPIKey}
	sso := types.ModelProvider{ID: "sso", UID: "uid-sso", Name: "AWS", Kind: types.ModelProviderBedrockSSO}
	idle := types.ModelProvider{ID: "idle", UID: "uid-idle", Name: "Idle", Kind: types.ModelProviderAnthropicSubscription}
	st := &miscCovInvStore{
		sc:   types.SiteConfig{ModelProviders: &types.ModelProviders{Providers: []types.ModelProvider{gw, sso, idle}}},
		toks: []types.APIToken{{Principal: "sub-alice", Email: "alice@corp.example"}},
	}
	// Expired well before any real clock reads it; the stored one far after.
	past, future := added, added.AddDate(300, 0, 0)
	sec := &miscCovMetaSecrets{everywhere: []secretstore.Meta{
		{Name: providerCredentialName(gw), Owner: "sub-alice", Store: "pg", AddedAt: added, ExpiresAt: &future},
		{Name: providerCredentialName(sso), Owner: "sub-alice", Store: "pg", AddedAt: added, ExpiresAt: &past},
		{Name: providerCredentialName(gw), Owner: "sub-bob", Store: "vault", AddedAt: added},
		{Name: providerCredentialName(gw), Owner: "", Store: "pg", AddedAt: added},
		{Name: "unrelated-secret", Owner: "sub-bob", Store: "pg", AddedAt: added},
	}}
	srv := miscCovInventoryServer(t, st, sec)
	pool, _ := miscCovClosedPool(t)
	srv.cfg.KeyDomains = keydomain.NewService(pool, nil)
	logs := miscCovCaptureLogs(t)

	w := do(t, srv, http.MethodGet, miscCovInventoryPath, adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET = %d %s", w.Code, w.Body)
	}
	var inv credentialInventory
	if err := json.Unmarshal(w.Body.Bytes(), &inv); err != nil {
		t.Fatal(err)
	}

	wantAsked := []string{providerCredentialName(gw), providerCredentialName(idle), providerCredentialName(sso)}
	slices.Sort(wantAsked)
	if len(sec.askedEverywhere) != 1 || !slices.Equal(sec.askedEverywhere[0], wantAsked) {
		t.Errorf("metadata asked for %v, want one read of every provider's row, sorted: %v", sec.askedEverywhere, wantAsked)
	}
	type key struct{ person, provider string }
	got := map[key]credentialInventoryRow{}
	for _, r := range inv.Credentials {
		got[key{r.Person, r.Provider}] = r
	}
	if len(inv.Credentials) != 3 {
		t.Fatalf("rows = %+v, want alice's two and bob's one", inv.Credentials)
	}
	if r := got[key{"sub-alice", "gw"}]; r.State != credStateStored || r.Email != "alice@corp.example" || r.ProviderName != "Gateway" || r.Store != "pg" {
		t.Errorf("alice/gw = %+v", r)
	}
	if r := got[key{"sub-alice", "sso"}]; r.State != credStateExpired {
		t.Errorf("alice/sso state = %q, want expired", r.State)
	}
	if r := got[key{"sub-bob", "gw"}]; r.Email != "" || r.Store != "vault" {
		t.Errorf("bob/gw = %+v, want no invented email and the vault store", r)
	}
	for k, r := range got {
		if r.KeyDomain != "" || r.KeyDomainSource != "" || r.KeyDomainGroup != "" {
			t.Errorf("%v carries a key domain %+v although it could not be read", k, r)
		}
	}
	if inv.Counts.People != 2 || inv.Counts.Credentials != 3 ||
		inv.Counts.ByProvider["gw"] != 2 || inv.Counts.ByProvider["sso"] != 1 || inv.Counts.ByProvider["idle"] != 0 {
		t.Errorf("counts = %+v, want 2 people, 3 credentials, gw 2, sso 1, idle 0", inv.Counts)
	}
	if _, ok := logs.find("reading a person's key domain failed"); !ok {
		t.Error("the unreadable key domain was not logged")
	}
}

func TestMiscCovAnnotateKeyDomainsWithNoServiceChangesNothing(t *testing.T) {
	srv := newHarness(t).srv
	inv := &credentialInventory{Credentials: []credentialInventoryRow{{Person: "sub-a", Provider: "gw"}}}
	srv.annotateKeyDomains(t.Context(), inv)
	if r := inv.Credentials[0]; r.KeyDomain != "" || r.KeyDomainSource != "" || r.KeyDomainGroup != "" {
		t.Errorf("row = %+v, want no key domain", r)
	}
}

// A person's domain is decided once however many credentials they hold.
func TestMiscCovAnnotateKeyDomainsAsksOncePerPerson(t *testing.T) {
	srv := newHarness(t).srv
	pool, _ := miscCovClosedPool(t)
	srv.cfg.KeyDomains = keydomain.NewService(pool, nil)
	logs := miscCovCaptureLogs(t)
	inv := &credentialInventory{Credentials: []credentialInventoryRow{
		{Person: "sub-a", Provider: "gw"}, {Person: "sub-a", Provider: "sso"}, {Person: "sub-b", Provider: "gw"},
	}}
	srv.annotateKeyDomains(t.Context(), inv)
	failures := 0
	logs.mu.Lock()
	for _, r := range logs.recs {
		if r.Message == "wardynd: reading a person's key domain failed" {
			failures++
		}
	}
	logs.mu.Unlock()
	if failures != 2 {
		t.Errorf("the key domain was read %d times for 2 people holding 3 credentials, want 2", failures)
	}
}
