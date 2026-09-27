// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package directory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
)

const (
	entraProvider = "entra"

	// graphBaseURL / entraTokenURL are the production endpoints, injected in
	// tests via newEntra; not Config fields since an operator never varies them.
	graphBaseURL     = "https://graph.microsoft.com/v1.0"
	entraTokenURLFmt = "https://login.microsoftonline.com/%s/oauth2/v2.0/token"

	// graphScope is the client-credentials scope: ".default" means every
	// APPLICATION permission already admin-consented in Entra by a tenant admin.
	graphScope = "https://graph.microsoft.com/.default"

	// tokenRefreshMargin renews the app token this long before it actually
	// expires, so an in-flight search never races the expiry.
	tokenRefreshMargin = 60 * time.Second

	// cacheTTL / cacheEntries bound the suggestion cache. A minute-stale
	// suggestion is harmless, and the daemon is single-replica by construction,
	// so an in-memory cache needs no coherence story.
	cacheTTL     = 60 * time.Second
	cacheEntries = 256

	// httpTimeout bounds every Graph call and, critically, the token call —
	// see the note in newEntra about the token source's detached context.
	httpTimeout = 10 * time.Second
)

// EntraConfig carries the client-credentials app registration. This package
// reads NO environment: the boot slice resolves the credentials and passes
// them in, so the fail-closed refusal for a PUBLIC OIDC client (no secret,
// cannot do client credentials) lives at boot, where it can refuse startup,
// not here where it could only 503.
type EntraConfig struct {
	TenantID     string
	ClientID     string
	ClientSecret string
}

type entraDirectory struct {
	clientID  string
	graphBase string
	hc        *http.Client
	tokens    oauth2.TokenSource
	cache     *ttlCache

	// approleDenied latches when Graph refuses the servicePrincipals read
	// (Application.Read.All not granted): App Roles are best-effort, so this
	// marks the connector DEGRADED rather than failing the search.
	//
	// ponytail: latched until restart. Re-consent in Entra needs a daemon
	// restart to take effect; the alternative is re-probing a known-403 on every
	// keystroke.
	approleDenied atomic.Bool
}

// NewEntra builds the Microsoft Graph connector. It returns ErrUnconfigured
// when any credential is absent (the "feature is off" answer, not a failure)
// and performs no network I/O: a tenant reachable at boot but not at first
// search would fail either way.
func NewEntra(cfg EntraConfig) (Directory, error) {
	return newEntra(cfg, fmt.Sprintf(entraTokenURLFmt, url.PathEscape(strings.TrimSpace(cfg.TenantID))), graphBaseURL)
}

func newEntra(cfg EntraConfig, tokenURL, graphBase string) (Directory, error) {
	if strings.TrimSpace(cfg.TenantID) == "" || strings.TrimSpace(cfg.ClientID) == "" || cfg.ClientSecret == "" {
		return nil, ErrUnconfigured
	}
	// The token source runs on a detached context (below), so the Timeout is
	// the only thing bounding a hung token endpoint.
	hc := &http.Client{Timeout: httpTimeout}

	// Built once over a DETACHED context carrying our HTTP client: binding it to
	// a request context instead would let one cancelled autocomplete keystroke
	// poison the cached app token for every later search. Trade-off: a token
	// fetch does not observe the calling request's deadline.
	ccfg := &clientcredentials.Config{
		ClientID:     strings.TrimSpace(cfg.ClientID),
		ClientSecret: cfg.ClientSecret,
		TokenURL:     tokenURL,
		Scopes:       []string{graphScope},
		// Pinning AuthStyleInParams avoids x/oauth2's AutoDetect probe, which
		// tries HTTP Basic first and would cost a cold fetch two round trips.
		AuthStyle: oauth2.AuthStyleInParams,
	}
	tokenCtx := context.WithValue(context.Background(), oauth2.HTTPClient, hc)
	// Re-uses the cached token until expiry-tokenRefreshMargin, retuning the
	// reuse wrapper clientcredentials already returns rather than nesting a
	// second one.
	tokens := oauth2.ReuseTokenSourceWithExpiry(nil, ccfg.TokenSource(tokenCtx), tokenRefreshMargin)

	return &entraDirectory{
		clientID:  strings.TrimSpace(cfg.ClientID),
		graphBase: strings.TrimRight(graphBase, "/"),
		hc:        hc,
		tokens:    tokens,
		cache:     newTTLCache(cacheEntries, cacheTTL),
	}, nil
}

// Search implements Directory.
func (d *entraDirectory) Search(ctx context.Context, q string, kind Kind) ([]Entry, error) {
	q = strings.TrimSpace(q)
	if len([]rune(q)) < MinQueryLen {
		return nil, nil
	}
	if kind == "" {
		kind = KindAny
	}
	if !kind.Valid() {
		return nil, fmt.Errorf("directory: unknown kind %q", kind)
	}

	key := string(kind) + "\x00" + q
	if hit, ok := d.cache.get(key); ok {
		return hit, nil
	}

	var (
		out []Entry
		err error
	)
	if kind == KindAny {
		out, err = searchAny(ctx, q, d.searchKind)
	} else {
		out, err = d.searchKind(ctx, q, kind)
	}
	if err != nil {
		return nil, err // never cache a failure
	}
	d.cache.put(key, out)
	return out, nil
}

func (d *entraDirectory) searchKind(ctx context.Context, q string, kind Kind) ([]Entry, error) {
	switch kind {
	case KindUser:
		return d.searchUsers(ctx, q)
	case KindGroup:
		return d.searchGroups(ctx, q)
	case KindAppRole:
		return d.searchAppRoles(ctx, q)
	default:
		return nil, fmt.Errorf("directory: unknown kind %q", kind)
	}
}

type graphUser struct {
	DisplayName       string `json:"displayName"`
	Mail              string `json:"mail"`
	UserPrincipalName string `json:"userPrincipalName"`
}

func (d *entraDirectory) searchUsers(ctx context.Context, q string) ([]Entry, error) {
	t := searchTerm(q)
	v := url.Values{}
	// Matching mail and UPN as well as displayName is what makes typing an address work.
	v.Set("$search", fmt.Sprintf(`"displayName:%s" OR "mail:%s" OR "userPrincipalName:%s"`, t, t, t))
	v.Set("$select", "displayName,mail,userPrincipalName")
	v.Set("$top", strconv.Itoa(MaxResults))

	var body struct {
		Value []graphUser `json:"value"`
	}
	if err := d.graphSearch(ctx, "users", "/users", v, &body); err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(body.Value))
	for _, u := range body.Value {
		claim, detail := u.Mail, u.UserPrincipalName
		if claim == "" {
			// No mailbox: the UPN is the address the claim will carry.
			claim, detail = u.UserPrincipalName, u.Mail
		}
		if claim == "" {
			continue // nothing storable — a row that inserts "" is worse than no row
		}
		if detail == "" || detail == claim {
			// Always leave the row a disambiguator: two people can share a
			// display name, and the address is the thing that tells them apart.
			detail = claim
		}
		name := u.DisplayName
		if name == "" {
			name = claim
		}
		out = append(out, Entry{DisplayName: name, ClaimValue: claim, Kind: KindUser, Detail: detail})
	}
	return out[:min(len(out), MaxResults)], nil
}

type graphGroup struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
}

func (d *entraDirectory) searchGroups(ctx context.Context, q string) ([]Entry, error) {
	v := url.Values{}
	// The `groups` claim carries the object GUID, so ClaimValue is the GUID
	// while DisplayName stays the name.
	v.Set("$search", fmt.Sprintf(`"displayName:%s"`, searchTerm(q)))
	v.Set("$select", "id,displayName")
	v.Set("$top", strconv.Itoa(MaxResults))

	var body struct {
		Value []graphGroup `json:"value"`
	}
	if err := d.graphSearch(ctx, "groups", "/groups", v, &body); err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(body.Value))
	for _, g := range body.Value {
		if g.ID == "" {
			continue
		}
		name := g.DisplayName
		if name == "" {
			name = g.ID
		}
		// The GUID prefix is shown so an admin can tell two same-named groups
		// apart, and so the row visibly explains why the stored value is a GUID.
		out = append(out, Entry{
			DisplayName: name,
			ClaimValue:  g.ID,
			Kind:        KindGroup,
			Detail:      "group · " + g.ID[:min(8, len(g.ID))],
		})
	}
	return out[:min(len(out), MaxResults)], nil
}

type graphAppRole struct {
	DisplayName string `json:"displayName"`
	Value       string `json:"value"`
	IsEnabled   bool   `json:"isEnabled"`
}

// searchAppRoles reads this app registration's own service principal and
// filters its appRoles client-side (a handful of authored entries, so no
// server-side query is worth building). Best-effort by contract: without
// Application.Read.All the read is refused and the App Role kind is simply
// ABSENT from results, not an error.
func (d *entraDirectory) searchAppRoles(ctx context.Context, q string) ([]Entry, error) {
	if d.approleDenied.Load() {
		return nil, nil
	}
	v := url.Values{}
	v.Set("$filter", fmt.Sprintf("appId eq '%s'", odataQuote(d.clientID)))
	v.Set("$select", "appRoles")
	v.Set("$top", "1")

	var body struct {
		Value []struct {
			AppRoles []graphAppRole `json:"appRoles"`
		} `json:"value"`
	}
	// A plain $filter query — no $search, so no ConsistencyLevel/$count pair needed.
	if err := d.graphGet(ctx, "approles", "/servicePrincipals", v, false, &body); err != nil {
		var pe *ProviderError
		if errors.As(err, &pe) && (pe.Status == http.StatusForbidden || pe.Status == http.StatusUnauthorized) {
			d.markAppRolesDenied(pe)
			return nil, nil
		}
		return nil, err
	}

	needle := strings.ToLower(q)
	out := make([]Entry, 0, MaxResults)
	for _, sp := range body.Value {
		for _, r := range sp.AppRoles {
			if !r.IsEnabled || r.Value == "" {
				continue // a disabled role, or one with no manifest value, is not assignable
			}
			if !strings.Contains(strings.ToLower(r.DisplayName), needle) && !strings.Contains(strings.ToLower(r.Value), needle) {
				continue
			}
			name := r.DisplayName
			if name == "" {
				name = r.Value
			}
			// Detail shows the manifest value because that is what gets STORED —
			// the admin should see it before picking.
			out = append(out, Entry{
				DisplayName: name,
				ClaimValue:  r.Value,
				Kind:        KindAppRole,
				Detail:      "app role · " + r.Value,
			})
		}
	}
	return out[:min(len(out), MaxResults)], nil
}

func (d *entraDirectory) markAppRolesDenied(pe *ProviderError) {
	if d.approleDenied.CompareAndSwap(false, true) {
		slog.Warn("directory: App Role suggestions unavailable — Graph refused the service-principal read; grant Application.Read.All to include roles",
			slog.Int("status", pe.Status))
	}
}

// graphSearch is the ONLY way this connector issues a $search: Graph rejects
// $search on directory objects unless BOTH the `ConsistencyLevel: eventual`
// header AND `$count=true` are present, so setting them in one place makes the
// pairing structural rather than a rule each call site must remember.
func (d *entraDirectory) graphSearch(ctx context.Context, op, path string, v url.Values, out any) error {
	return d.graphGet(ctx, op, path, v, true, out)
}

func (d *entraDirectory) graphGet(ctx context.Context, op, path string, v url.Values, search bool, out any) error {
	if search {
		v.Set("$count", "true")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.graphBase+path+"?"+v.Encode(), nil)
	if err != nil {
		return &ProviderError{Provider: entraProvider, Op: op, Err: err}
	}
	if search {
		req.Header.Set("ConsistencyLevel", "eventual")
	}
	req.Header.Set("Accept", "application/json")

	tok, err := d.tokens.Token()
	if err != nil {
		return &ProviderError{Provider: entraProvider, Op: "token", Status: retrieveStatus(err), Err: err}
	}
	tok.SetAuthHeader(req)

	resp, err := d.hc.Do(req)
	if err != nil {
		return &ProviderError{Provider: entraProvider, Op: op, Err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Graph error bodies name the missing permission, but can also echo the
		// query, so the excerpt is bounded.
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &ProviderError{
			Provider: entraProvider,
			Op:       op,
			Status:   resp.StatusCode,
			Err:      errors.New(strings.TrimSpace(string(snippet))),
		}
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil {
		return &ProviderError{Provider: entraProvider, Op: op, Status: resp.StatusCode, Err: fmt.Errorf("decode response: %w", err)}
	}
	return nil
}

func retrieveStatus(err error) int {
	var re *oauth2.RetrieveError
	if errors.As(err, &re) && re.Response != nil {
		return re.Response.StatusCode
	}
	return 0
}

// searchTerm strips the two characters that would break out of an OData
// $search string literal (stripped rather than escaped: Graph's $search
// grammar has no portable escape for a quote inside a term).
func searchTerm(q string) string {
	return strings.NewReplacer(`"`, "", `\`, "").Replace(q)
}

// odataQuote escapes a single quote for an OData string literal by doubling it.
func odataQuote(s string) string { return strings.ReplaceAll(s, "'", "''") }

// --- cache ---------------------------------------------------------------

// ttlCache is a bounded cache with a per-entry TTL: an entry is served only
// while fresh, and the map is flushed wholesale once it is full.
//
// ponytail: the ceiling is the bound, not the eviction ORDER. At 256 entries /
// 60 s every entry expires within a minute anyway, so flush-at-bound is
// indistinguishable from LRU here and costs no bookkeeping.
type ttlCache struct {
	mu  sync.Mutex
	max int
	ttl time.Duration
	m   map[string]cacheItem
	now func() time.Time // overridable in tests
}

type cacheItem struct {
	entries []Entry
	expires time.Time
}

func newTTLCache(max int, ttl time.Duration) *ttlCache {
	return &ttlCache{max: max, ttl: ttl, m: make(map[string]cacheItem, max), now: time.Now}
}

func (c *ttlCache) get(key string) ([]Entry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	it, ok := c.m[key]
	if !ok {
		return nil, false
	}
	if !c.now().Before(it.expires) {
		delete(c.m, key)
		return nil, false
	}
	// Defensive copy: the cache's slice must never be shared with callers —
	// an in-place sort/append by a future caller would be a cross-request race.
	return append([]Entry(nil), it.entries...), true
}

func (c *ttlCache) put(key string, entries []Entry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.m[key]; !ok && len(c.m) >= c.max {
		clear(c.m)
	}
	c.m[key] = cacheItem{entries: entries, expires: c.now().Add(c.ttl)}
}
