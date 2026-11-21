// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// A custom endpoint's service grant at run time: wardynd gets a client-credentials token for the row's client
// and hands it to the proxy of a listed service identity's run, while people on the row keep their own
// sign-in. This file holds the per-process state, the mint and its cache. The record of the client secret and
// its masks are provider_service_grant_secret.go; the status rows and the setup grading are
// provider_service_grant_status.go; the refresher, the secret routes and the clean-up of a removed grant's
// secret each have their own file beside it.
//
// Each replica keeps its own cache and single-flight in memory. No reusable token is stored at rest: the
// masking registry keeps the token's value, and a run's sealed mask rows keep a copy for output masking.

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/singleflight"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	// serviceGrantReuseMargin: a cached token is reused only while more than this much of its life remains.
	// It must exceed the proxy's own 5-minute renewal margin (injectRefreshMargin), as adoEntraAccessReuseMargin does.
	serviceGrantReuseMargin = 10 * time.Minute
	// serviceGrantFallbackMargin: after a failed attempt a cached token with more than this left is still served.
	serviceGrantFallbackMargin = time.Minute
	// serviceGrantMinLifetime is the shortest token lifetime accepted: the reuse margin plus the proxy's 5 minutes.
	serviceGrantMinLifetime = 900
	// serviceGrantPacing separates the attempts a resolve or a dispatch starts after a failure, per row and replica.
	serviceGrantPacing = time.Minute

	serviceGrantActor = "wardyn/service-grant"

	serviceGrantTriggerRefresh  = "refresh"
	serviceGrantTriggerDispatch = "dispatch"
	serviceGrantTriggerResolve  = "resolve"
)

// The closed set of failure reasons a mint, its audit row and its status row carry. An OAuth error code from the
// identity provider (RFC 6749 section 5.2) is the reason when it names one of the six, so the set is these plus them.
const (
	svcReasonNotActivated     = "not_activated"
	svcReasonSecretGone       = "secret_gone"
	svcReasonUnbound          = "unbound"
	svcReasonSecretUnmaskable = "secret_unmaskable"
	svcReasonTokenUnmaskable  = "token_unmaskable"
	svcReasonLifetimeTooShort = "token_lifetime_too_short"
	svcReasonRefused          = "refused"
	svcReasonUnavailable      = "unavailable"
)

// serviceGrantOAuthErrors are RFC 6749 section 5.2's six error codes; any other string is recorded as "other".
var serviceGrantOAuthErrors = []string{"invalid_request", "invalid_client", "invalid_grant", "unauthorized_client", "unsupported_grant_type", "invalid_scope"}

var (
	// errServiceGrantDrift: the row's grant changed from the one the caller's run was authorised under.
	errServiceGrantDrift = errors.New("the provider's service grant changed")
	// errServiceGrantNotListed: the service identity is not one the grant lists.
	errServiceGrantNotListed = errors.New("the service identity is not listed on the provider's service grant")
	// errServiceGrantRevisionMoved: only the secret's revision moved while a mint ran; the caller mints once more.
	errServiceGrantRevisionMoved = errors.New("the service grant's secret changed during a token request")
)

// serviceGrantError is a failed mint: a closed reason, whether a retry would be the same answer, and what the
// identity provider said, as a code and a status only.
type serviceGrantError struct {
	reason     string
	definitive bool
	sent       bool // a request reached the identity provider
	oauthError string
	httpStatus int
	egress     string
}

func (e *serviceGrantError) Error() string { return "service grant: " + e.reason }

func svcFail(reason string, definitive bool) *serviceGrantError {
	return &serviceGrantError{reason: reason, definitive: definitive}
}

// serviceGrantMinted is a token the caller may hand out, with its own expiry.
type serviceGrantMinted struct {
	token     string
	expiresAt time.Time
	scope     string
	egress    string // proxy or direct: how the request that got it was sent
}

type serviceGrantCached struct {
	serviceGrantMinted
	digest string
	rev    string
	revOK  bool
}

type serviceGrantFailed struct {
	at     time.Time
	digest string
	rev    string
	err    *serviceGrantError
}

// serviceGrantRuntime is one replica's service-grant state. The zero value is ready to use.
type serviceGrantRuntime struct {
	mu      sync.Mutex
	cache   map[string]serviceGrantCached // by provider uid
	failed  map[string]serviceGrantFailed // each row's latest failed attempt
	running map[string]bool               // refresher attempts in flight
	nameMu  map[string]*sync.Mutex        // one per secret name: held across a record's read or write and its mask call
	masked  map[string]bool               // secret names this replica has applied a mask for
	flights singleflight.Group
	// activated is set once the activation mark is seen; activation is one-way.
	activated atomic.Bool
	idOnce    sync.Once
	replicaID string
	// cleanups holds each claim's backoff, in memory: a new leader starts with every claim due.
	cleanups map[string]serviceGrantBackoff
	// hook is a test seam: called at a named stage, true stops the work there as a crash would.
	hook func(stage string) bool
}

func (rt *serviceGrantRuntime) replica() string {
	rt.idOnce.Do(func() { rt.replicaID = uuid.NewString() })
	return rt.replicaID
}

// stop reports whether a test hook ends the work at stage.
func (rt *serviceGrantRuntime) stop(stage string) bool { return rt.hook != nil && rt.hook(stage) }

func (rt *serviceGrantRuntime) nameLock(name string) *sync.Mutex {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.nameMu == nil {
		rt.nameMu = map[string]*sync.Mutex{}
	}
	m, ok := rt.nameMu[name]
	if !ok {
		m = new(sync.Mutex)
		rt.nameMu[name] = m
	}
	return m
}

// serviceGrantTransport is the round tripper of the token request: the configured one, else the daemon's.
func (s *Server) serviceGrantTransport() http.RoundTripper {
	if s.cfg.ServiceGrantTransport != nil {
		return s.cfg.ServiceGrantTransport
	}
	return http.DefaultTransport
}

// serviceGrantStore is the optional store seam for the grant keys and the status table.
func (s *Server) serviceGrantStore() (store.ServiceGrantStore, bool) {
	gs, ok := s.cfg.Store.(store.ServiceGrantStore)
	return gs, ok
}

// serviceGrantOperatorSecrets is the operator namespace of the secret store.
func (s *Server) serviceGrantOperatorSecrets() (secretstore.Store, bool) {
	if s.cfg.Secrets == nil {
		return nil, false
	}
	return s.cfg.Secrets.For(""), true
}

// serviceGrantRevision reads the secret record's revision: ok is false for a store that keeps none, and an
// ok read of "" is the store's authoritative absence.
func (s *Server) serviceGrantRevision(ctx context.Context, name string) (rev string, ok bool, err error) {
	st, have := s.serviceGrantOperatorSecrets()
	if !have {
		return "", false, errors.New("no secret store")
	}
	return secretstore.RevisionOf(ctx, st, name)
}

// serviceGrantsActive reports whether the 0.10.2 switch-on has been made. Activation is one-way, so a true
// answer is cached for the life of the process.
func (s *Server) serviceGrantsActive(ctx context.Context) (bool, error) {
	if s.svcGrant.activated.Load() {
		return true, nil
	}
	gs, ok := s.serviceGrantStore()
	if !ok {
		return false, nil
	}
	doc, err := gs.ServiceGrantDoc(ctx)
	if err != nil {
		return false, err
	}
	if doc.Activated {
		s.svcGrant.activated.Store(true)
	}
	return doc.Activated, nil
}

// serviceGrantToken returns a token for provider p's client. snap is the digest the caller's run was
// authorised under, and empty for the refresher; a digest that differs is drift, with no mint and no cached
// token. trigger is why: refresh, dispatch or resolve.
func (s *Server) serviceGrantToken(ctx context.Context, p types.ModelProvider, snap, trigger string) (serviceGrantMinted, error) {
	if p.OIDC == nil || p.OIDC.ServiceGrant == nil {
		return serviceGrantMinted{}, errServiceGrantDrift
	}
	digest := serviceGrantDigest(p)
	if snap != "" && snap != digest {
		return serviceGrantMinted{}, errServiceGrantDrift
	}
	for range 2 {
		res, err := s.serviceGrantTokenOnce(ctx, p, digest, trigger)
		if !errors.Is(err, errServiceGrantRevisionMoved) {
			return res, err
		}
	}
	return serviceGrantMinted{}, svcFail(svcReasonUnavailable, false)
}

func (s *Server) serviceGrantTokenOnce(ctx context.Context, p types.ModelProvider, digest, trigger string) (serviceGrantMinted, error) {
	active, err := s.serviceGrantsActive(ctx)
	if err != nil {
		return serviceGrantMinted{}, svcFail(svcReasonUnavailable, false)
	}
	if !active {
		return s.serviceGrantFailure(ctx, p, digest, "", svcFail(svcReasonNotActivated, true))
	}
	name := serviceGrantSecretName(p.UID)
	rev, revOK, err := s.serviceGrantRevision(ctx, name)
	if err != nil {
		return serviceGrantMinted{}, svcFail(svcReasonUnavailable, false)
	}
	if revOK && rev == "" {
		s.dropServiceGrantCache(p.UID)
		return s.serviceGrantFailure(ctx, p, digest, rev, svcFail(svcReasonSecretGone, true))
	}
	if hit, ok := s.cachedServiceGrantToken(p.UID, digest, rev, revOK, serviceGrantReuseMargin); ok {
		return hit, nil
	}
	if s.serviceGrantPaced(p.UID, digest, rev, trigger) {
		return s.serviceGrantFallback(p.UID, digest, rev, revOK)
	}
	key := p.UID + "|" + digest + "|" + rev
	v, err, _ := s.svcGrant.flights.Do(key, func() (any, error) {
		return s.serviceGrantMint(ctx, p, digest, rev, revOK, trigger)
	})
	if err != nil {
		var se *serviceGrantError
		if errors.As(err, &se) {
			return s.serviceGrantFailure(ctx, p, digest, rev, se)
		}
		return serviceGrantMinted{}, err
	}
	return v.(serviceGrantMinted), nil
}

// cachedServiceGrantToken is a cached token under the same digest and, when the store keeps revisions, the
// same revision, with more than margin of its life left.
func (s *Server) cachedServiceGrantToken(uid, digest, rev string, revOK bool, margin time.Duration) (serviceGrantMinted, bool) {
	s.svcGrant.mu.Lock()
	defer s.svcGrant.mu.Unlock()
	c, ok := s.svcGrant.cache[uid]
	if !ok || c.digest != digest || (revOK && c.rev != rev) || c.expiresAt.Sub(s.cfg.Now()) <= margin {
		return serviceGrantMinted{}, false
	}
	return c.serviceGrantMinted, true
}

func (s *Server) dropServiceGrantCache(uid string) {
	s.svcGrant.mu.Lock()
	defer s.svcGrant.mu.Unlock()
	delete(s.svcGrant.cache, uid)
}

// serviceGrantPaced reports whether this attempt would repeat, under the same digest and revision, a failed one
// started less than a minute ago. Only a resolve or a dispatch is paced: the refresher attempts a failed row once a pass.
func (s *Server) serviceGrantPaced(uid, digest, rev, trigger string) bool {
	if trigger == serviceGrantTriggerRefresh {
		return false
	}
	s.svcGrant.mu.Lock()
	defer s.svcGrant.mu.Unlock()
	f, ok := s.svcGrant.failed[uid]
	return ok && f.digest == digest && f.rev == rev && s.cfg.Now().Sub(f.at) < serviceGrantPacing
}

// serviceGrantFailure records a failed attempt and answers: a cached token with more than a minute left, minted
// under the current digest and revision, is still returned unless the secret is gone; otherwise the error.
func (s *Server) serviceGrantFailure(ctx context.Context, p types.ModelProvider, digest, rev string, e *serviceGrantError) (serviceGrantMinted, error) {
	now := s.cfg.Now()
	s.svcGrant.mu.Lock()
	if s.svcGrant.failed == nil {
		s.svcGrant.failed = map[string]serviceGrantFailed{}
	}
	s.svcGrant.failed[p.UID] = serviceGrantFailed{at: now, digest: digest, rev: rev, err: e}
	s.svcGrant.mu.Unlock()
	s.writeServiceGrantStatus(ctx, p, digest, now, nil, e)
	if e.reason == svcReasonSecretGone {
		return serviceGrantMinted{}, e
	}
	revOK := rev != ""
	if c, ok := s.cachedServiceGrantToken(p.UID, digest, rev, revOK, serviceGrantFallbackMargin); ok {
		return c, nil
	}
	return serviceGrantMinted{}, e
}

// serviceGrantFallback answers a paced attempt: the last failure's error, or a live cached token as above.
func (s *Server) serviceGrantFallback(uid, digest, rev string, revOK bool) (serviceGrantMinted, error) {
	s.svcGrant.mu.Lock()
	f := s.svcGrant.failed[uid]
	s.svcGrant.mu.Unlock()
	if c, ok := s.cachedServiceGrantToken(uid, digest, rev, revOK, serviceGrantFallbackMargin); ok && f.err != nil && f.err.reason != svcReasonSecretGone {
		return c, nil
	}
	return serviceGrantMinted{}, f.err
}

// serviceGrantMint is one token request: the record's read, the request, and the checks of its answer, then
// the publish. It holds no database lock and runs under the single-flight on (uid, digest, revision).
func (s *Server) serviceGrantMint(ctx context.Context, p types.ModelProvider, digest, rev string, revOK bool, trigger string) (serviceGrantMinted, error) {
	g := p.OIDC.ServiceGrant
	name := serviceGrantSecretName(p.UID)
	mctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), adoEntraRedeemTimeout)
	defer cancel()
	rec, ferr := s.readServiceGrantRecord(mctx, name)
	if ferr != nil {
		return serviceGrantMinted{}, ferr
	}
	if !rec.destinationMatches(p) {
		return serviceGrantMinted{}, svcFail(svcReasonUnbound, true)
	}
	gen := s.serviceGrantMaskGeneration(mctx)
	form := url.Values{"grant_type": {types.ServiceGrantTypeClientCreds}}
	if g.Scope != "" {
		form.Set("scope", g.Scope)
	}
	if g.Audience != "" {
		form.Set("audience", g.Audience)
	}
	ta := tokenAuthority{tokenURL: rec.TokenURL, clientID: rec.ClientID, clientSecret: rec.ClientSecret,
		basicAuth: g.ClientAuth != types.ServiceGrantClientAuthPost}
	rt := s.serviceGrantTransport()
	sent := s.cfg.Now()
	egress := serviceGrantEgress(rt, ta.tokenURL)
	status, out, derr := doTokenRequest(mctx, rt, ta, form)
	tok, ferr := checkServiceTokenAnswer(projectServiceTokenAnswer(status, out, derr))
	if ferr == nil {
		tok.expiresAt, tok.egress = sent.Add(time.Duration(out.ExpiresIn)*time.Second), egress
		if merr := s.cfg.MaskRegistry.MergeGlobalUntil(gen, "", name+"#token", tok.expiresAt, []byte(tok.token)); merr != nil {
			ferr = &serviceGrantError{reason: svcReasonUnavailable, sent: true, httpStatus: status}
		}
	}
	if ferr != nil {
		ferr.egress = egress
		s.auditServiceGrantMint(mctx, p, trigger, sent, nil, ferr, false)
		return serviceGrantMinted{}, ferr
	}
	return s.publishServiceGrantToken(mctx, p, digest, rev, revOK, trigger, sent, tok)
}

// publishServiceGrantToken re-reads the revision and the live digest before the token is cached or returned.
// A token the provider issued but that is not published is audited as superseded and stays masked.
func (s *Server) publishServiceGrantToken(ctx context.Context, p types.ModelProvider, digest, rev string, revOK bool,
	trigger string, sent time.Time, tok serviceGrantMinted,
) (serviceGrantMinted, error) {
	name := serviceGrantSecretName(p.UID)
	liveRev, liveOK, rerr := s.serviceGrantRevision(ctx, name)
	liveDigest, found := s.liveServiceGrantDigest(ctx, p.UID)
	superseded := func() { s.auditServiceGrantMint(ctx, p, trigger, sent, &tok, nil, true) }
	switch {
	case !found || liveDigest != digest:
		superseded()
		return serviceGrantMinted{}, errServiceGrantDrift
	case rerr != nil:
		superseded()
		return serviceGrantMinted{}, svcFail(svcReasonUnavailable, false)
	case revOK && liveOK && liveRev != rev:
		superseded()
		return serviceGrantMinted{}, errServiceGrantRevisionMoved
	}
	// One success row per request sent, written before the token is published to any waiter.
	s.auditServiceGrantMint(ctx, p, trigger, sent, &tok, nil, false)
	s.svcGrant.mu.Lock()
	if s.svcGrant.cache == nil {
		s.svcGrant.cache = map[string]serviceGrantCached{}
	}
	s.svcGrant.cache[p.UID] = serviceGrantCached{serviceGrantMinted: tok, digest: digest, rev: rev, revOK: revOK}
	delete(s.svcGrant.failed, p.UID)
	s.svcGrant.mu.Unlock()
	s.writeServiceGrantStatus(ctx, p, digest, sent, &tok, nil)
	return tok, nil
}

// liveServiceGrantDigest is the digest of the row with this uid as the site configuration holds it now.
func (s *Server) liveServiceGrantDigest(ctx context.Context, uid string) (string, bool) {
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil || sc.ModelProviders == nil {
		return "", false
	}
	for _, p := range sc.ModelProviders.Providers {
		if p.UID == uid && !p.Disabled {
			d := serviceGrantDigest(p)
			return d, d != ""
		}
	}
	return "", false
}

// serviceTokenAnswer is the token answer as the service lane keeps it: nothing of the ID token, the refresh
// token, error_description or the rest of the body outlives the projection.
type serviceTokenAnswer struct {
	status      int
	oauthError  string
	accessToken string
	expiresIn   int
	scope       string
	transport   bool // the request did not complete, or its answer was unparseable
}

func projectServiceTokenAnswer(status int, out adoEntraTokenResponse, derr error) serviceTokenAnswer {
	ans := serviceTokenAnswer{status: status, transport: derr != nil}
	if derr != nil {
		return ans
	}
	ans.accessToken, ans.expiresIn, ans.scope = out.AccessToken, out.ExpiresIn, out.Scope
	if out.Error != "" {
		ans.oauthError = "other"
		for _, c := range serviceGrantOAuthErrors {
			if out.Error == c {
				ans.oauthError = c
			}
		}
	}
	return ans
}

// checkServiceTokenAnswer turns an answer into a token the caller may use, or the failure it is.
func checkServiceTokenAnswer(ans serviceTokenAnswer) (serviceGrantMinted, *serviceGrantError) {
	fail := func(reason string, definitive bool) *serviceGrantError {
		e := svcFail(reason, definitive)
		e.sent, e.oauthError, e.httpStatus = true, ans.oauthError, ans.status
		return e
	}
	switch {
	case ans.transport:
		return serviceGrantMinted{}, fail(svcReasonUnavailable, false)
	case ans.oauthError != "" && ans.status < 500:
		reason := ans.oauthError
		if reason == "other" {
			reason = svcReasonRefused
		}
		return serviceGrantMinted{}, fail(reason, true)
	case ans.status >= 500 || ans.status == http.StatusTooManyRequests || ans.oauthError != "":
		return serviceGrantMinted{}, fail(svcReasonUnavailable, false)
	case ans.status < 200 || ans.status > 299:
		return serviceGrantMinted{}, fail(svcReasonRefused, true)
	case ans.accessToken == "":
		return serviceGrantMinted{}, fail(svcReasonUnavailable, false)
	case len(ans.accessToken) < secretmask.MinLen || secretmask.TrimmedTooShort([]byte(ans.accessToken)):
		return serviceGrantMinted{}, fail(svcReasonTokenUnmaskable, true)
	case ans.expiresIn < serviceGrantMinLifetime:
		return serviceGrantMinted{}, fail(svcReasonLifetimeTooShort, true)
	}
	return serviceGrantMinted{token: ans.accessToken, scope: strings.TrimSpace(ans.scope)}, nil
}

// serviceGrantEgress says whether the transport sends a request to tokenURL through a proxy, from calling its
// Proxy function for the request itself.
func serviceGrantEgress(rt http.RoundTripper, tokenURL string) string {
	t, ok := rt.(*http.Transport)
	if !ok || t.Proxy == nil {
		return "direct"
	}
	req, err := http.NewRequest(http.MethodPost, tokenURL, nil)
	if err != nil {
		return "direct"
	}
	if u, err := t.Proxy(req); err == nil && u != nil {
		return "proxy"
	}
	return "direct"
}

// serviceGrantLock is the site-config document lock, for the paths that are not an HTTP handler.
func (s *Server) serviceGrantLock(ctx context.Context) (context.Context, func(), error) {
	return s.lock(ctx, db.SiteConfigLockClass)
}
