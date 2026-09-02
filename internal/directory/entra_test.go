// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package directory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeGraph is a stand-in for login.microsoftonline.com + graph.microsoft.com.
// It records what was actually put on the wire so the tests can assert the
// Graph contract (the ConsistencyLevel/$count pair) rather than the Go code's
// intent to honour it.
type fakeGraph struct {
	srv *httptest.Server

	tokenCalls atomic.Int32
	spCalls    atomic.Int32

	// last request seen per path, for header/query assertions.
	lastUsers  atomic.Pointer[recorded]
	lastGroups atomic.Pointer[recorded]

	// canned responses
	users     []graphUser
	groups    []graphGroup
	appRoles  []graphAppRole
	spStatus  int // non-zero => /servicePrincipals answers with this status
	authSeen  atomic.Pointer[string]
	userError int
}

type recorded struct {
	query   url.Values
	headers http.Header
}

func newFakeGraph(t *testing.T) *fakeGraph {
	t.Helper()
	f := &fakeGraph{}
	mux := http.NewServeMux()

	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		f.tokenCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":"tok-%d","token_type":"Bearer","expires_in":3600}`, f.tokenCalls.Load())
	})

	mux.HandleFunc("/v1.0/users", func(w http.ResponseWriter, r *http.Request) {
		f.lastUsers.Store(&recorded{query: r.URL.Query(), headers: r.Header.Clone()})
		a := r.Header.Get("Authorization")
		f.authSeen.Store(&a)
		if f.userError != 0 {
			w.WriteHeader(f.userError)
			_, _ = w.Write([]byte(`{"error":{"code":"Authorization_RequestDenied","message":"Insufficient privileges"}}`))
			return
		}
		writeJSON(w, map[string]any{"value": f.users})
	})

	mux.HandleFunc("/v1.0/groups", func(w http.ResponseWriter, r *http.Request) {
		f.lastGroups.Store(&recorded{query: r.URL.Query(), headers: r.Header.Clone()})
		writeJSON(w, map[string]any{"value": f.groups})
	})

	mux.HandleFunc("/v1.0/servicePrincipals", func(w http.ResponseWriter, r *http.Request) {
		f.spCalls.Add(1)
		if f.spStatus != 0 {
			w.WriteHeader(f.spStatus)
			_, _ = w.Write([]byte(`{"error":{"code":"Authorization_RequestDenied","message":"Insufficient privileges to complete the operation."}}`))
			return
		}
		writeJSON(w, map[string]any{"value": []map[string]any{{"appRoles": f.appRoles}}})
	})

	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	_ = enc.Encode(v)
}

func (f *fakeGraph) dir(t *testing.T) *entraDirectory {
	t.Helper()
	d, err := newEntra(EntraConfig{
		TenantID:     "tenant-guid",
		ClientID:     "client-guid",
		ClientSecret: "s3cret",
		HTTPClient:   f.srv.Client(),
	}, f.srv.URL+"/token", f.srv.URL+"/v1.0")
	if err != nil {
		t.Fatalf("newEntra: %v", err)
	}
	return d.(*entraDirectory)
}

// --- the round-6 catch: $search REQUIRES ConsistencyLevel:eventual AND
// $count=true. Omitting either one 400s at Graph, and neither is visible in a
// green unit test that only checks the parsed result — so pin the wire.

func TestSearchSendsConsistencyLevelAndCountPair(t *testing.T) {
	f := newFakeGraph(t)
	f.users = []graphUser{{DisplayName: "Ada Lovelace", Mail: "ada@corp.com", UserPrincipalName: "ada@corp.onmicrosoft.com"}}
	f.groups = []graphGroup{{ID: "8f3c1a2b-0000-4000-8000-000000000001", DisplayName: "Platform Eng"}}
	d := f.dir(t)

	if _, err := d.Search(context.Background(), "ada", KindUser); err != nil {
		t.Fatalf("user search: %v", err)
	}
	if _, err := d.Search(context.Background(), "plat", KindGroup); err != nil {
		t.Fatalf("group search: %v", err)
	}

	for name, got := range map[string]*recorded{"users": f.lastUsers.Load(), "groups": f.lastGroups.Load()} {
		if got == nil {
			t.Fatalf("%s: endpoint was never called", name)
		}
		if h := got.headers.Get("ConsistencyLevel"); h != "eventual" {
			t.Errorf("%s: ConsistencyLevel header = %q, want %q (Graph 400s on $search without it)", name, h, "eventual")
		}
		if c := got.query.Get("$count"); c != "true" {
			t.Errorf("%s: $count = %q, want \"true\" (required together with ConsistencyLevel)", name, c)
		}
		if s := got.query.Get("$search"); s == "" {
			t.Errorf("%s: no $search on the request", name)
		}
		if got.query.Get("$top") == "" {
			t.Errorf("%s: $top not bounded", name)
		}
	}

	// The group $search must tokenize displayName only — there is no mail leg
	// on /groups, and asking for one is a 400.
	if s := f.lastGroups.Load().query.Get("$search"); s != `"displayName:plat"` {
		t.Errorf("group $search = %q, want displayName-only", s)
	}
	// The user $search covers the addresses too, which is what makes typing an
	// email work.
	us := f.lastUsers.Load().query.Get("$search")
	for _, want := range []string{"displayName:ada", "mail:ada", "userPrincipalName:ada"} {
		if !strings.Contains(us, want) {
			t.Errorf("user $search %q missing %q", us, want)
		}
	}
}

// The App Role lookup is a $filter, not a $search: sending the pair there would
// be cargo-culting, and the test pins the distinction.
func TestAppRoleLookupIsFilterNotSearch(t *testing.T) {
	f := newFakeGraph(t)
	f.appRoles = []graphAppRole{{DisplayName: "Wardyn Admin", Value: "Wardyn.Admin", IsEnabled: true}}
	d := f.dir(t)
	if _, err := d.Search(context.Background(), "war", KindAppRole); err != nil {
		t.Fatalf("approle search: %v", err)
	}
	if f.spCalls.Load() != 1 {
		t.Fatalf("servicePrincipals calls = %d, want 1", f.spCalls.Load())
	}
}

func TestEntryMappingClaimValueIsTheContract(t *testing.T) {
	f := newFakeGraph(t)
	f.users = []graphUser{
		{DisplayName: "Ada Lovelace", Mail: "ada@corp.com", UserPrincipalName: "ada@corp.onmicrosoft.com"},
		{DisplayName: "Guest User", UserPrincipalName: "guest_ext#EXT#@corp.onmicrosoft.com"}, // no mailbox
	}
	f.groups = []graphGroup{{ID: "8f3c1a2b-0000-4000-8000-000000000001", DisplayName: "Platform Eng"}}
	f.appRoles = []graphAppRole{
		{DisplayName: "Wardyn Admin", Value: "Wardyn.Admin", IsEnabled: true},
		{DisplayName: "Retired", Value: "Wardyn.Old", IsEnabled: false},
		{DisplayName: "No Value", Value: "", IsEnabled: true},
	}
	d := f.dir(t)

	users, err := d.Search(context.Background(), "a", KindUser)
	if err != nil || users != nil {
		t.Fatalf("1-char query: got (%v, %v), want (nil, nil) with no round trip", users, err)
	}
	users, err = d.Search(context.Background(), "ada", KindUser)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 2 {
		t.Fatalf("users = %d, want 2", len(users))
	}
	if users[0].ClaimValue != "ada@corp.com" || users[0].DisplayName != "Ada Lovelace" {
		t.Errorf("user[0] = %+v, want mail as ClaimValue", users[0])
	}
	if users[0].Detail != "ada@corp.onmicrosoft.com" {
		t.Errorf("user[0].Detail = %q, want the other address", users[0].Detail)
	}
	if users[1].ClaimValue != "guest_ext#EXT#@corp.onmicrosoft.com" {
		t.Errorf("mailbox-less user[1] = %+v, want UPN fallback as ClaimValue", users[1])
	}
	if users[1].Detail == "" {
		t.Errorf("user[1].Detail is empty; every row needs a disambiguator")
	}

	groups, err := d.Search(context.Background(), "plat", KindGroup)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 {
		t.Fatalf("groups = %d, want 1", len(groups))
	}
	// The whole point: the name is shown, the GUID is stored.
	if groups[0].DisplayName != "Platform Eng" || groups[0].ClaimValue != "8f3c1a2b-0000-4000-8000-000000000001" {
		t.Errorf("group = %+v, want name displayed and GUID stored", groups[0])
	}
	if groups[0].Detail != "group · 8f3c1a2b" {
		t.Errorf("group Detail = %q, want the GUID-prefix hint", groups[0].Detail)
	}

	roles, err := d.Search(context.Background(), "war", KindAppRole)
	if err != nil {
		t.Fatal(err)
	}
	if len(roles) != 1 || roles[0].ClaimValue != "Wardyn.Admin" {
		t.Fatalf("roles = %+v, want only the enabled, valued role matching the query", roles)
	}
}

// --- any-mode: fixed order approle -> group -> user, ONE global cap of 20.

func TestAnyModeFixedOrderAndGlobalCap(t *testing.T) {
	f := newFakeGraph(t)
	f.appRoles = []graphAppRole{
		{DisplayName: "wardyn admin", Value: "Wardyn.Admin", IsEnabled: true},
		{DisplayName: "wardyn member", Value: "Wardyn.Member", IsEnabled: true},
	}
	for i := range 12 {
		f.groups = append(f.groups, graphGroup{ID: fmt.Sprintf("g%011d-0000-4000-8000-000000000001", i), DisplayName: fmt.Sprintf("wardyn group %d", i)})
	}
	for i := range 12 {
		f.users = append(f.users, graphUser{DisplayName: fmt.Sprintf("wardyn user %d", i), Mail: fmt.Sprintf("u%d@corp.com", i)})
	}
	d := f.dir(t)

	got, err := d.Search(context.Background(), "wardyn", KindAny)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != MaxResults {
		t.Fatalf("len = %d, want the single global cap %d", len(got), MaxResults)
	}
	// 2 app roles, then 12 groups, then the first 6 users — concatenated, never
	// interleaved, never per-kind quota'd.
	wantKinds := make([]Kind, 0, MaxResults)
	for range 2 {
		wantKinds = append(wantKinds, KindAppRole)
	}
	for range 12 {
		wantKinds = append(wantKinds, KindGroup)
	}
	for range 6 {
		wantKinds = append(wantKinds, KindUser)
	}
	for i, want := range wantKinds {
		if got[i].Kind != want {
			t.Fatalf("entry %d kind = %q, want %q (fixed order approle→group→user)", i, got[i].Kind, want)
		}
	}
}

func TestAnyModePropagatesUserFailure(t *testing.T) {
	f := newFakeGraph(t)
	f.userError = http.StatusForbidden
	d := f.dir(t)

	_, err := d.Search(context.Background(), "wardyn", KindAny)
	var pe *ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want a *ProviderError (a partial result silently missing users is worse)", err)
	}
	if pe.Status != http.StatusForbidden || pe.Op != "users" || pe.Provider != "entra" {
		t.Errorf("ProviderError = %+v, want entra/users/403", pe)
	}
}

// --- token: one client-credentials call serves many searches.

func TestTokenIsFetchedOnceAndReused(t *testing.T) {
	f := newFakeGraph(t)
	d := f.dir(t)

	if _, err := d.Search(context.Background(), "aaa", KindUser); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Search(context.Background(), "bbb", KindGroup); err != nil {
		t.Fatal(err)
	}
	if n := f.tokenCalls.Load(); n != 1 {
		t.Fatalf("token calls = %d, want 1 (cached until expiry-%s)", n, tokenRefreshMargin)
	}
	if a := f.authSeen.Load(); a == nil || *a != "Bearer tok-1" {
		t.Fatalf("Authorization = %v, want the app token", a)
	}
}

func TestTokenFailureIsAProviderError(t *testing.T) {
	f := newFakeGraph(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_client"}`))
	}))
	t.Cleanup(srv.Close)
	dd, err := newEntra(EntraConfig{TenantID: "t", ClientID: "c", ClientSecret: "s", HTTPClient: srv.Client()}, srv.URL, f.srv.URL+"/v1.0")
	if err != nil {
		t.Fatal(err)
	}
	_, err = dd.Search(context.Background(), "ada", KindUser)
	var pe *ProviderError
	if !errors.As(err, &pe) || pe.Op != "token" {
		t.Fatalf("err = %v, want a *ProviderError with Op=token", err)
	}
	if pe.Status != http.StatusUnauthorized {
		t.Errorf("Status = %d, want 401 carried through from the token endpoint", pe.Status)
	}
}

// --- App Roles are best-effort: a 403 on that ONE call degrades to empty.

func TestAppRoles403DegradesToEmptyNotError(t *testing.T) {
	f := newFakeGraph(t)
	f.spStatus = http.StatusForbidden
	f.users = []graphUser{{DisplayName: "Ada", Mail: "ada@corp.com"}}
	d := f.dir(t)

	roles, err := d.Search(context.Background(), "war", KindAppRole)
	if err != nil {
		t.Fatalf("approle 403 returned err = %v, want nil (best-effort by contract)", err)
	}
	if len(roles) != 0 {
		t.Fatalf("roles = %+v, want empty", roles)
	}
	if !d.approleDenied.Load() {
		t.Error("connector not marked degraded after the refusal")
	}

	// A mixed-kind search must still work — users and groups are the v1 promise.
	mixed, err := d.Search(context.Background(), "ada", KindAny)
	if err != nil {
		t.Fatalf("any search after approle denial: %v", err)
	}
	if len(mixed) != 1 || mixed[0].Kind != KindUser {
		t.Fatalf("any = %+v, want the user hit with the App Role kind simply absent", mixed)
	}
	// Latched: no second doomed round trip.
	if n := f.spCalls.Load(); n != 1 {
		t.Errorf("servicePrincipals calls = %d, want 1 (denial is latched)", n)
	}
}

// --- cache: hit within the TTL, miss after it.

func TestCacheHitThenExpiry(t *testing.T) {
	f := newFakeGraph(t)
	f.users = []graphUser{{DisplayName: "Ada", Mail: "ada@corp.com"}}
	d := f.dir(t)
	now := time.Now()
	d.cache.now = func() time.Time { return now }

	if _, err := d.Search(context.Background(), "ada", KindUser); err != nil {
		t.Fatal(err)
	}
	first := f.lastUsers.Load()
	f.lastUsers.Store(nil)

	if _, err := d.Search(context.Background(), "ada", KindUser); err != nil {
		t.Fatal(err)
	}
	if f.lastUsers.Load() != nil {
		t.Error("second identical search hit the network; expected a cache hit")
	}
	if first == nil {
		t.Fatal("first search never reached the fake")
	}

	// A different kind is a different cache key even for the same q.
	if _, err := d.Search(context.Background(), "ada", KindGroup); err != nil {
		t.Fatal(err)
	}
	if f.lastGroups.Load() == nil {
		t.Error("same q, different kind was served from the user cache entry")
	}

	// Past the TTL the entry is gone.
	now = now.Add(cacheTTL + time.Second)
	if _, err := d.Search(context.Background(), "ada", KindUser); err != nil {
		t.Fatal(err)
	}
	if f.lastUsers.Load() == nil {
		t.Errorf("search after %s served a stale cache entry", cacheTTL)
	}
}

func TestCacheDoesNotCacheFailures(t *testing.T) {
	f := newFakeGraph(t)
	f.userError = http.StatusServiceUnavailable
	d := f.dir(t)

	if _, err := d.Search(context.Background(), "ada", KindUser); err == nil {
		t.Fatal("want an error")
	}
	f.userError = 0
	f.users = []graphUser{{DisplayName: "Ada", Mail: "ada@corp.com"}}
	got, err := d.Search(context.Background(), "ada", KindUser)
	if err != nil {
		t.Fatalf("retry after a transient failure: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %+v, want the retry to reach the recovered upstream", got)
	}
}

// The cache's contract is the BOUND, not the eviction order: a keystroke-per-
// request surface must never grow without limit. Which key survives a flush is
// deliberately unspecified (every entry expires within cacheTTL anyway).
func TestCacheNeverExceedsItsBound(t *testing.T) {
	c := newTTLCache(2, cacheTTL)
	for _, k := range []string{"a", "b", "c", "d", "e"} {
		c.put(k, []Entry{{ClaimValue: k}})
		if len(c.m) > 2 {
			t.Fatalf("after put(%q) the cache holds %d entries, want at most 2", k, len(c.m))
		}
	}
	if _, ok := c.get("e"); !ok {
		t.Error("the most recent put must be served from the cache")
	}
	// Re-putting a live key updates in place; it never trips the bound.
	c.put("e", []Entry{{ClaimValue: "e2"}})
	if got, _ := c.get("e"); len(got) != 1 || got[0].ClaimValue != "e2" {
		t.Errorf("re-put e = %+v, want the fresh value", got)
	}
}

// --- unconfigured: the absent-mode signal.

func TestNewEntraUnconfigured(t *testing.T) {
	for name, cfg := range map[string]EntraConfig{
		"all empty":     {},
		"no tenant":     {ClientID: "c", ClientSecret: "s"},
		"no client":     {TenantID: "t", ClientSecret: "s"},
		"no secret":     {TenantID: "t", ClientID: "c"}, // e.g. a PUBLIC OIDC client
		"blank padding": {TenantID: "  ", ClientID: "c", ClientSecret: "s"},
	} {
		d, err := NewEntra(cfg)
		if !errors.Is(err, ErrUnconfigured) {
			t.Errorf("%s: err = %v, want ErrUnconfigured (the API layer's 503 signal)", name, err)
		}
		if d != nil {
			t.Errorf("%s: got a non-nil Directory alongside ErrUnconfigured", name)
		}
	}
	if _, err := NewEntra(EntraConfig{TenantID: "t", ClientID: "c", ClientSecret: "s"}); err != nil {
		t.Errorf("fully configured: err = %v, want nil", err)
	}
}

func TestSearchRejectsUnknownKind(t *testing.T) {
	f := newFakeGraph(t)
	d := f.dir(t)
	if _, err := d.Search(context.Background(), "ada", Kind("everything")); err == nil {
		t.Fatal("want an error for an unknown kind")
	}
}

// searchTerm keeps user input from breaking out of the OData string literal.
func TestSearchTermStripsQuoteBreakers(t *testing.T) {
	if got := searchTerm(`a" OR "displayName:`); strings.ContainsAny(got, `"\`) {
		t.Errorf("searchTerm(%q) = %q, still carries a quote/backslash", `a" OR "displayName:`, got)
	}
	f := newFakeGraph(t)
	d := f.dir(t)
	if _, err := d.Search(context.Background(), `ad"a`, KindGroup); err != nil {
		t.Fatal(err)
	}
	if s := f.lastGroups.Load().query.Get("$search"); s != `"displayName:ada"` {
		t.Errorf("$search = %q, want the quote stripped", s)
	}
}
