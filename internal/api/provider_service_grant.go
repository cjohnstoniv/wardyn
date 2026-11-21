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
	"log/slog"
	"net/http"
	"net/url"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

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

// The service grant's time bounds and its permit cap; variables only so tests can shorten them. Each attempt runs
// under one context detached from the caller and bounded 25 s, its one retry included. A try's reads, its
// record's lock and its token request are bounded 10 s and end at least 15 s before the attempt's deadline. That
// leaves the publication, the failure's re-reads and the status write 5 s each. A caller leaves its flight at its
// own deadline. A token request is sent only with one of the replica's 16 permits, which it holds until its row
// is recorded.
var (
	// serviceGrantAttemptBound bounds one attempt end to end, its one retry included. A refresher slot and a caller are
	// held for at most this long.
	serviceGrantAttemptBound = 25 * time.Second
	// serviceGrantTokenBound bounds one try's reads, its record's lock and its token request.
	serviceGrantTokenBound = adoEntraRedeemTimeout
	// serviceGrantTailBound is kept free after a try's request: the publication, the failure's re-reads and the status
	// write, each up to 5 s, which a failed publication uses in turn.
	serviceGrantTailBound = 15 * time.Second
	// serviceGrantPublishBound bounds the publication transaction: its share-locked reads, the mint row and the commit.
	serviceGrantPublishBound = 5 * time.Second
	// serviceGrantRecheckBound bounds a failure's re-reads.
	serviceGrantRecheckBound = 5 * time.Second
	// serviceGrantRowsOutstanding is the replica's permits: token requests in flight plus mint rows not yet recorded.
	serviceGrantRowsOutstanding int64 = 16
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
	// final marks a withdrawal observed with authority (the secret gone, or its predecessor retiring): no
	// later read may weaken it, so settlement carries it through unchanged.
	final     bool
	row       <-chan struct{} // the request's mint row, settled off the path; nil when none
	refused   bool            // no permit: nothing was sent and no row is owed
	afterRead bool            // refused at the final reservation, after the record read: no cached token is served
}

func (e *serviceGrantError) Error() string { return "service grant: " + e.reason }

func svcFail(reason string, definitive bool) *serviceGrantError {
	return &serviceGrantError{reason: reason, definitive: definitive}
}

// svcGone is the final, definitive refusal of a withdrawn credential.
func svcGone() *serviceGrantError {
	e := svcFail(svcReasonSecretGone, true)
	e.final = true
	return e
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
	seq    int64 // the audit seq of the publication that cached it: commit order
}

type serviceGrantFailed struct {
	at     time.Time
	gen    uint64 // the attempt's local generation: older attempts never replace newer ones
	digest string
	rev    string
	err    *serviceGrantError
}

// serviceGrantRuntime is one replica's service-grant state. The zero value is ready to use.
type serviceGrantRuntime struct {
	mu    sync.Mutex
	cache map[string]serviceGrantCached // by provider uid
	// gen is each row's cache generation, moved by every eviction (a rotation, a removal, a withdrawal seen). An
	// attempt's publication is superseded by "eviction" when the generation it started under is no longer the
	// row's at its decision, and a committed publication installs its token only while that generation still holds.
	gen     map[string]uint64
	failed  map[string]serviceGrantFailed  // each row's latest failed attempt
	seq     uint64                         // attempt generations, in the order attempts start on this replica
	succGen map[string]uint64              // the generation of each row's latest published success
	running map[string]bool                // refresher attempts in flight
	nameMu  map[string]chan struct{}       // one per secret name, capacity 1: held across a record's read or write and its mask call
	masked  map[string]bool                // secret names this replica has applied a mask for
	flights map[string]*serviceGrantFlight // under mu: the running flight of each key
	// permits counts token requests in flight plus mint rows not yet recorded (serviceGrantRowsOutstanding).
	permits atomic.Int64
	siem    serviceGrantSIEM
	// activated is set once the activation mark is seen; activation is one-way.
	activated atomic.Bool
	idOnce    sync.Once
	replicaID string
	// cleanups holds each claim's backoff, in memory: a new leader starts with every claim due.
	cleanups map[string]serviceGrantBackoff
	// hook is a test seam: called at a named stage, true stops the work there as a crash would.
	hook func(stage string) bool
	// rowID is a test seam: called with each sent request's mint-row id once it is fixed. Nil in production.
	rowID func(uuid.UUID)
	// spoolAppend is a test seam: when set, the settle calls it in place of s.cfg.AuditSpool.Append. Nil in production.
	spoolAppend func(types.AuditEvent) error
}

func (rt *serviceGrantRuntime) replica() string {
	rt.idOnce.Do(func() { rt.replicaID = uuid.NewString() })
	return rt.replicaID
}

// stop reports whether a test hook ends the work at stage.
func (rt *serviceGrantRuntime) stop(stage string) bool { return rt.hook != nil && rt.hook(stage) }

func (rt *serviceGrantRuntime) nameChan(name string) chan struct{} {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.nameMu == nil {
		rt.nameMu = map[string]chan struct{}{}
	}
	c, ok := rt.nameMu[name]
	if !ok {
		c = make(chan struct{}, 1)
		rt.nameMu[name] = c
	}
	return c
}

// lockName takes name's lock, or gives up when ctx ends.
func (rt *serviceGrantRuntime) lockName(ctx context.Context, name string) (unlock func(), ok bool) {
	c := rt.nameChan(name)
	if ctx.Err() != nil {
		return nil, false
	}
	select {
	case c <- struct{}{}:
		return func() { <-c }, true
	case <-ctx.Done():
		return nil, false
	}
}

// tryLockName takes name's lock only if it is free.
func (rt *serviceGrantRuntime) tryLockName(name string) (unlock func(), ok bool) {
	c := rt.nameChan(name)
	select {
	case c <- struct{}{}:
		return func() { <-c }, true
	default:
		return nil, false
	}
}

// takePermit reserves one of serviceGrantRowsOutstanding permits, or reports false at the cap. release is
// idempotent.
func (rt *serviceGrantRuntime) takePermit() (release func(), ok bool) {
	for {
		n := rt.permits.Load()
		if n >= serviceGrantRowsOutstanding {
			return nil, false
		}
		if rt.permits.CompareAndSwap(n, n+1) {
			return sync.OnceFunc(func() { rt.permits.Add(-1) }), true
		}
	}
}

// serviceGrantRefused is an attempt refused at the permit cap: nothing is sent and no row is owed. afterRead
// marks a refusal at the final reservation, after the record read, which serves no cached token.
func serviceGrantRefused(uid string, afterRead bool) *serviceGrantError {
	slog.Warn("wardynd: service grant permits are all held by requests whose rows are not yet recorded; no token request is sent", slog.String("provider_uid", uid))
	return &serviceGrantError{reason: svcReasonUnavailable, refused: true, afterRead: afterRead}
}

// serviceGrantTryContext is one try's context for its reads, its record's lock and its token request: at most
// serviceGrantTokenBound, never past the attempt's deadline less serviceGrantTailBound.
func serviceGrantTryContext(attempt context.Context) (context.Context, context.CancelFunc) {
	d := time.Now().Add(serviceGrantTokenBound)
	if dl, ok := attempt.Deadline(); ok {
		if tail := dl.Add(-serviceGrantTailBound); tail.Before(d) {
			d = tail
		}
	}
	return context.WithDeadline(attempt, d)
}

// serviceGrantFlight is one key's token attempt. done is closed once tok and err are set; every caller waits on it.
type serviceGrantFlight struct {
	done chan struct{}
	tok  serviceGrantMinted
	err  error
}

// serviceGrantJoin returns key's running flight, or starts fn as a new one on a goBackground goroutine. The flight
// leaves the map before done is closed, so a caller arriving after it ends starts a new one.
func (s *Server) serviceGrantJoin(key string, fn func() (serviceGrantMinted, error)) *serviceGrantFlight {
	s.svcGrant.mu.Lock()
	defer s.svcGrant.mu.Unlock()
	if f, ok := s.svcGrant.flights[key]; ok {
		return f
	}
	if s.svcGrant.flights == nil {
		s.svcGrant.flights = map[string]*serviceGrantFlight{}
	}
	f := &serviceGrantFlight{done: make(chan struct{})}
	s.svcGrant.flights[key] = f
	s.goBackground(func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("wardynd: a service grant attempt panicked; its callers get unavailable",
					slog.Any("panic", r), slog.String("stack", string(debug.Stack())))
				f.tok, f.err = serviceGrantMinted{}, svcFail(svcReasonUnavailable, false)
			}
			func() {
				s.svcGrant.mu.Lock()
				defer s.svcGrant.mu.Unlock()
				delete(s.svcGrant.flights, key)
			}()
			close(f.done)
		}()
		f.tok, f.err = fn()
	})
	return f
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

// serviceGrantGate reads the stored document once: whether the switch-on has been made, and whether the uid
// still has a clean-up claim, that is, a predecessor grant whose secret record is not yet verified retired.
func (s *Server) serviceGrantGate(ctx context.Context, uid string) (active, retiring bool, err error) {
	gs, ok := s.serviceGrantStore()
	if !ok {
		return false, false, nil
	}
	doc, err := gs.ServiceGrantDoc(ctx)
	if err != nil {
		return false, false, err
	}
	if doc.Activated {
		s.svcGrant.activated.Store(true)
	}
	_, retiring = doc.Cleanups[uid]
	return doc.Activated || s.svcGrant.activated.Load(), retiring, nil
}

// nextServiceGrantGen takes the next attempt generation.
func (s *Server) nextServiceGrantGen() uint64 {
	s.svcGrant.mu.Lock()
	defer s.svcGrant.mu.Unlock()
	s.svcGrant.seq++
	return s.svcGrant.seq
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
	// One deadline for the whole attempt, its retry included, detached from the caller: a waiter that hangs up
	// does not cut a flight others share.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), serviceGrantAttemptBound)
	defer cancel()
	for range 2 {
		res, err := s.serviceGrantTokenOnce(ctx, p, digest, trigger)
		if !errors.Is(err, errServiceGrantRevisionMoved) {
			return res, err
		}
	}
	return serviceGrantMinted{}, svcFail(svcReasonUnavailable, false)
}

func (s *Server) serviceGrantTokenOnce(ctx context.Context, p types.ModelProvider, digest, trigger string) (serviceGrantMinted, error) {
	tctx, tcancel := serviceGrantTryContext(ctx)
	defer tcancel()
	started := s.cfg.Now() // the attempt's one timestamp: a failure is recorded at it, never at the time it ends
	attempt := s.nextServiceGrantGen()
	gen := s.serviceGrantCacheGen(p.UID) // read before the revision, so an eviction after it is always seen at publish
	active, retiring, err := s.serviceGrantGate(tctx, p.UID)
	if err != nil {
		return serviceGrantMinted{}, svcFail(svcReasonUnavailable, false)
	}
	if !active {
		return s.serviceGrantFailure(ctx, p, digest, "", started, attempt, svcFail(svcReasonNotActivated, true))
	}
	if retiring {
		return s.serviceGrantWhileRetiring(ctx, p, digest, started)
	}
	name := serviceGrantSecretName(p.UID)
	rev, revOK, err := s.serviceGrantRevision(tctx, name)
	if err != nil {
		return serviceGrantMinted{}, svcFail(svcReasonUnavailable, false)
	}
	if revOK && rev == "" {
		s.dropServiceGrantCache(p.UID)
		return s.serviceGrantFailure(ctx, p, digest, rev, started, attempt, svcGone())
	}
	if hit, ok := s.cachedServiceGrantToken(p.UID, digest, rev, revOK, serviceGrantReuseMargin); ok {
		return hit, nil
	}
	if f, paced := s.serviceGrantPacedFailure(p.UID, digest, rev, trigger); paced {
		return s.serviceGrantFallback(p.UID, digest, rev, revOK, f)
	}
	key := p.UID + "|" + digest + "|" + rev
	// The elected flight publishes the one result, a failure included, so a waiter never records the same
	// failed request again after a newer attempt has succeeded. It runs under the elected attempt's context; a
	// caller waits for it only until its own deadline.
	f := s.serviceGrantJoin(key, func() (serviceGrantMinted, error) {
		tok, merr := s.serviceGrantMint(ctx, tctx, p, digest, rev, revOK, trigger, attempt, gen)
		var se *serviceGrantError
		if errors.As(merr, &se) && se.refused {
			// No permit: nothing was sent and no row is owed, so no pacing entry and no status row. A refusal before the
			// record read stands where a cache hit is admitted, so it may serve one. A refusal after the read may not: a
			// rotation can have committed during it.
			if !se.afterRead {
				if c, ok := s.cachedServiceGrantToken(p.UID, digest, rev, revOK, serviceGrantFallbackMargin); ok {
					return c, nil
				}
			}
			return serviceGrantMinted{}, se
		}
		if errors.As(merr, &se) {
			// A request's row is settled off the path, and the settle owns its permit: the answer does not wait for it.
			return s.serviceGrantFailure(ctx, p, digest, rev, started, attempt, se)
		}
		return tok, merr
	})
	select {
	case <-f.done:
		return f.tok, f.err
	case <-ctx.Done():
		slog.Warn("wardynd: a service grant attempt reached its deadline while its flight was still running", slog.String("provider_uid", p.UID))
		return serviceGrantMinted{}, svcFail(svcReasonUnavailable, false)
	}
}

// serviceGrantWhileRetiring answers a token request for a uid whose predecessor's secret record is not yet
// verified deleted: nothing is served from the old generation, the cache is dropped, and the refusal is
// definitive, so a proxy drops its last header. The row serves again once the claim is settled and a fresh
// secret has been written.
func (s *Server) serviceGrantWhileRetiring(ctx context.Context, p types.ModelProvider, digest string, started time.Time) (serviceGrantMinted, error) {
	s.dropServiceGrantCache(p.UID)
	e := svcGone()
	s.writeServiceGrantStatus(ctx, p, digest, started, nil, e)
	return serviceGrantMinted{}, e
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

// dropServiceGrantCache evicts uid's token and moves its cache generation, so no attempt that started before the
// eviction publishes afterwards.
func (s *Server) dropServiceGrantCache(uid string) {
	s.svcGrant.mu.Lock()
	defer s.svcGrant.mu.Unlock()
	delete(s.svcGrant.cache, uid)
	if s.svcGrant.gen == nil {
		s.svcGrant.gen = map[string]uint64{}
	}
	s.svcGrant.gen[uid]++
}

func (s *Server) serviceGrantCacheGen(uid string) uint64 {
	s.svcGrant.mu.Lock()
	defer s.svcGrant.mu.Unlock()
	return s.svcGrant.gen[uid]
}

// serviceGrantPacedFailure returns the failed attempt this one would repeat, under the same digest and
// revision and started less than a minute ago. Only a resolve or a dispatch is paced: the refresher attempts a
// failed row once a pass. The failure is returned by value, so the answer that follows never reads it again.
func (s *Server) serviceGrantPacedFailure(uid, digest, rev, trigger string) (serviceGrantFailed, bool) {
	if trigger == serviceGrantTriggerRefresh {
		return serviceGrantFailed{}, false
	}
	s.svcGrant.mu.Lock()
	defer s.svcGrant.mu.Unlock()
	f, ok := s.svcGrant.failed[uid]
	return f, ok && f.err != nil && f.digest == digest && f.rev == rev && s.cfg.Now().Sub(f.at) < serviceGrantPacing
}

// serviceGrantFailure records a failed attempt and answers: a cached token with more than a minute left, minted
// under the current digest and revision, is still returned unless the secret is gone; otherwise the error.
func (s *Server) serviceGrantFailure(ctx context.Context, p types.ModelProvider, digest, rev string, started time.Time, gen uint64, e *serviceGrantError) (serviceGrantMinted, error) {
	if e.final {
		s.dropServiceGrantCache(p.UID) // a withdrawal seen with authority is not looked at again
	} else if e.reason != svcReasonNotActivated {
		s.svcGrant.stop("failure.begin")
		// What was read before the request may be gone: look again before a status row is written or a cached
		// token is served. Authoritative absence evicts and refuses; a moved revision gets the one retry; a
		// changed row is drift; a failed read serves nothing from the cache.
		rctx, rcancel := context.WithTimeout(ctx, serviceGrantRecheckBound)
		liveRev, liveOK, rerr := s.serviceGrantRevision(rctx, serviceGrantSecretName(p.UID))
		liveDigest, found, derr := s.liveServiceGrantDigest(rctx, p.UID)
		_, retiring, gerr := s.serviceGrantGate(rctx, p.UID)
		rcancel()
		// Independent read outcomes combine so that a confirmed withdrawal is never replaced by a read that failed:
		// only when nothing decisive was seen does an unreadable read answer as a retryable unavailable.
		switch {
		case derr == nil && (!found || liveDigest != digest):
			return serviceGrantMinted{}, errServiceGrantDrift
		case gerr == nil && retiring:
			// A replacement committed with its predecessor's claim while this attempt ran: whatever the
			// provider said, nothing from the old generation is served and the refusal is definitive.
			s.dropServiceGrantCache(p.UID)
			e = svcGone()
		case rerr == nil && liveOK && liveRev == "":
			s.dropServiceGrantCache(p.UID)
			e = svcGone()
		case derr != nil || gerr != nil:
			// An unreadable configuration is not a withdrawal: retryable, and nothing from the cache is served.
			return serviceGrantMinted{}, svcFail(svcReasonUnavailable, false)
		case rerr != nil:
			return serviceGrantMinted{}, e
		case liveOK && liveRev != rev:
			return serviceGrantMinted{}, errServiceGrantRevisionMoved
		}
	}
	stale := func() bool {
		s.svcGrant.mu.Lock()
		defer s.svcGrant.mu.Unlock()
		if s.svcGrant.failed == nil {
			s.svcGrant.failed = map[string]serviceGrantFailed{}
		}
		// An older attempt that ends late never replaces a newer attempt's record, here or in the status row:
		// it is compared with the latest success and the latest failure by generation.
		prev, had := s.svcGrant.failed[p.UID]
		stale := s.svcGrant.succGen[p.UID] > gen || (had && prev.gen > gen)
		if !stale {
			s.svcGrant.failed[p.UID] = serviceGrantFailed{at: started, gen: gen, digest: digest, rev: rev, err: e}
		}
		return stale
	}()
	if !stale {
		s.writeServiceGrantStatus(ctx, p, digest, started, nil, e)
	}
	if e.reason == svcReasonSecretGone {
		return serviceGrantMinted{}, e
	}
	revOK := rev != ""
	if c, ok := s.cachedServiceGrantToken(p.UID, digest, rev, revOK, serviceGrantFallbackMargin); ok {
		return c, nil
	}
	return serviceGrantMinted{}, e
}

// serviceGrantFallback answers a paced attempt from the failure it was paced by: a live cached token as in
// serviceGrantFailure, or that failure's error.
func (s *Server) serviceGrantFallback(uid, digest, rev string, revOK bool, f serviceGrantFailed) (serviceGrantMinted, error) {
	if c, ok := s.cachedServiceGrantToken(uid, digest, rev, revOK, serviceGrantFallbackMargin); ok && f.err.reason != svcReasonSecretGone {
		return c, nil
	}
	return serviceGrantMinted{}, f.err
}

// serviceGrantMint is one token request: the record's read, the request, and the checks of its answer, then
// the publication. It runs under the flight on (uid, digest, revision): ctx is the attempt's context and tctx the
// try's, which bounds the read, the record's lock and the request. A request is sent only with a permit, which
// the request's mint row takes over: the publication's commit, or the settle of its failure or unpublished row.
func (s *Server) serviceGrantMint(ctx, tctx context.Context, p types.ModelProvider, digest, rev string, revOK bool, trigger string, attempt, cacheGen uint64) (serviceGrantMinted, error) {
	if s.cfg.RunnerAuditPrepare == nil {
		serviceGrantNoPrepare.Do(func() {
			slog.Error("wardynd: service grant mint rows cannot be prepared (no audit preparation is configured); no token request is sent")
		})
		return serviceGrantMinted{}, svcFail(svcReasonUnavailable, false)
	}
	if s.svcGrant.permits.Load() >= serviceGrantRowsOutstanding {
		return serviceGrantMinted{}, serviceGrantRefused(p.UID, false) // at the cap: no record read, so no secret.read row
	}
	g := p.OIDC.ServiceGrant
	name := serviceGrantSecretName(p.UID)
	rec, ferr := s.readServiceGrantRecord(tctx, name)
	if ferr != nil {
		return serviceGrantMinted{}, ferr
	}
	if !rec.destinationMatches(p) {
		return serviceGrantMinted{}, svcFail(svcReasonUnbound, true)
	}
	gen := s.serviceGrantMaskGeneration(tctx)
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
	if tctx.Err() != nil {
		// The record read returned after the try ended, as a wedged secret.read does: send nothing.
		return serviceGrantMinted{}, svcFail(svcReasonUnavailable, false)
	}
	release, ok := s.svcGrant.takePermit()
	if !ok {
		return serviceGrantMinted{}, serviceGrantRefused(p.UID, true)
	}
	handed := false
	defer func() {
		if !handed {
			release() // no row took the permit over: a path that records nothing, or a panic
		}
	}()
	sent := s.cfg.Now()
	egress := serviceGrantEgress(rt, ta.tokenURL)
	status, out, derr := doTokenRequest(tctx, rt, ta, form)
	tok, ferr := checkServiceTokenAnswer(projectServiceTokenAnswer(status, out, derr))
	if ferr == nil {
		tok.expiresAt, tok.egress = sent.Add(time.Duration(out.ExpiresIn)*time.Second), egress
		if merr := s.cfg.MaskRegistry.MergeGlobalUntil(gen, "", name+"#token", tok.expiresAt, []byte(tok.token)); merr != nil {
			ferr = &serviceGrantError{reason: svcReasonUnavailable, sent: true, httpStatus: status}
		}
	}
	if ferr != nil {
		ferr.egress = egress
		id := uuid.New()
		if s.svcGrant.rowID != nil {
			s.svcGrant.rowID(id)
		}
		if ev, err := s.serviceGrantMintEvent(id, s.cfg.Now().UTC(), p, trigger, sent, nil, ferr, ""); err == nil {
			ferr.row = s.settleServiceGrantRow(ev, false, release)
			handed = true // after the call: a panic before the settle exists leaves the release to the deferred call
		} else {
			slog.Error("wardynd: AUDIT WRITE FAILED: a service grant mint row could not be prepared",
				slog.String("event_id", id.String()), slog.Any("err", err))
		}
		return serviceGrantMinted{}, ferr
	}
	// The publication runs under the attempt's context, not the try's: a token that arrives late in the request's
	// window is still published, never discarded for the time the request used.
	res, err := s.publishServiceGrantToken(ctx, p, digest, rev, revOK, trigger, sent, attempt, cacheGen, tok, release)
	var se *serviceGrantError
	if errors.As(err, &se) && se.row != nil {
		handed = true // the publication handed the permit to the settle of its unpublished row
	}
	return res, err
}

// serviceGrantNoPrepare logs the missing audit preparation once per process.
var serviceGrantNoPrepare sync.Once

// The causes a mint row's superseded_by names. A row without one is the success that published its token.
const (
	svcSupersededSettings    = "settings"
	svcSupersededRetiring    = "retiring"
	svcSupersededSecretGone  = "secret_gone"
	svcSupersededRevision    = "revision"
	svcSupersededEviction    = "eviction"
	svcSupersededUnpublished = "publish_unavailable"
)

// serviceGrantSupersededCauses is every superseded_by value, in the order publication checks them.
var serviceGrantSupersededCauses = []string{svcSupersededSettings, svcSupersededRetiring, svcSupersededSecretGone,
	svcSupersededRevision, svcSupersededEviction, svcSupersededUnpublished}

// serviceGrantSupersededBy decides publication: "" publishes, otherwise the cause. The first true case wins:
// settings, retiring, secret_gone, revision, eviction. liveRev and haveRev are the secret's revision as read
// under the share lock or, for a store whose revision is no database row, just before the transaction.
func serviceGrantSupersededBy(sc types.SiteConfig, doc store.ServiceGrantDoc, uid, digest, rev string, revOK bool,
	liveRev string, haveRev bool, genNow, cacheGen uint64,
) string {
	if d, found := serviceGrantDigestIn(sc, uid); !found || d != digest {
		return svcSupersededSettings
	}
	if _, ok := doc.Cleanups[uid]; ok {
		return svcSupersededRetiring
	}
	if revOK && haveRev && liveRev == "" {
		return svcSupersededSecretGone
	}
	if revOK && haveRev && liveRev != rev {
		return svcSupersededRevision
	}
	if genNow != cacheGen {
		return svcSupersededEviction
	}
	return ""
}

// serviceGrantInstalls reports whether a committed publication with seq, begun under cacheGen, replaces the cache
// entry: only while the row's generation is still cacheGen, and only over an entry from an older publication.
func serviceGrantInstalls(held bool, prevSeq, seq int64, genNow, cacheGen uint64) bool {
	return genNow == cacheGen && (!held || prevSeq < seq)
}

// publishServiceGrantToken publishes a minted token in one short transaction: the secret's row and the site
// configuration are share-locked, the decision is made under them, and the request's one mint row commits with
// it. Only after the commit is the token cached, handed to waiters and written to the status row; a token that is
// not published is withheld and stays masked, and its row names the cause in superseded_by. A publication that
// does not commit leaves its row to a settle off the attempt's path, which owns the request's permit from then on.
// ctx is the attempt's context; release is the request's permit, which this function owns.
func (s *Server) publishServiceGrantToken(ctx context.Context, p types.ModelProvider, digest, rev string, revOK bool,
	trigger string, sent time.Time, attempt, cacheGen uint64, tok serviceGrantMinted, release func(),
) (serviceGrantMinted, error) {
	id, at := uuid.New(), s.cfg.Now().UTC()
	if s.svcGrant.rowID != nil {
		s.svcGrant.rowID(id)
	}
	gs, haveStore := s.serviceGrantStore()
	cands, err := s.serviceGrantMintCandidates(id, at, p, trigger, sent, &tok)
	if err == nil && !haveStore {
		err = errors.New("no service grant store")
	}
	if err != nil {
		slog.Error("wardynd: AUDIT WRITE FAILED: mint row could not be prepared; the token is not published",
			slog.String("event_id", id.String()), slog.Any("err", err))
		release()
		return serviceGrantMinted{}, &serviceGrantError{reason: svcReasonUnavailable, sent: true, egress: tok.egress}
	}
	s.svcGrant.stop("publish.begin")
	name := serviceGrantSecretName(p.UID)
	var row *store.ServiceGrantSecretRow
	liveRev, haveRev := "", false
	var perr error
	if revOK {
		if st, ok := s.serviceGrantOperatorSecrets(); ok {
			if owner, isRow := secretstore.RevisionRowOf(st); isRow {
				row = &store.ServiceGrantSecretRow{Owner: owner, Name: name}
			}
		}
		if row == nil {
			// A store whose revision is no database row: read just before the transaction, ordered in-process by
			// the cache generation only.
			lr, lok, rerr := s.serviceGrantRevision(ctx, name)
			liveRev, haveRev, perr = lr, lok, rerr
		}
	}
	var ev types.AuditEvent
	var published bool
	var seq int64
	cause := ""
	if perr == nil {
		pctx, pcancel := context.WithTimeout(ctx, serviceGrantPublishBound)
		ev, published, seq, perr = gs.PublishServiceGrantMint(pctx, store.ServiceGrantPublish{
			Secret: row,
			Decide: func(sc types.SiteConfig, doc store.ServiceGrantDoc, secretRev *string) (types.AuditEvent, bool) {
				s.svcGrant.stop("publish.locked")
				lr, hr := liveRev, haveRev
				if secretRev != nil {
					lr, hr = *secretRev, true
				}
				genNow := s.serviceGrantCacheGen(p.UID)
				cause = serviceGrantSupersededBy(sc, doc, p.UID, digest, rev, revOK, lr, hr, genNow, cacheGen)
				return cands[cause], cause == ""
			},
			BeforeCommit: func() { s.svcGrant.stop("publish.precommit") },
		})
		pcancel()
	}
	if perr != nil {
		// Nothing was published. The flight answers from serviceGrantFailure at once; only the settle waits for the row.
		done := s.settleServiceGrantRow(cands[svcSupersededUnpublished], errors.Is(perr, store.ErrServiceGrantCommitUnknown), release)
		return serviceGrantMinted{}, &serviceGrantError{reason: svcReasonUnavailable, sent: true, egress: tok.egress, row: done}
	}
	s.svcGrant.stop("publish.committed")
	release() // the row is recorded
	s.emitServiceGrantRow(ev)
	if !published {
		switch cause {
		case svcSupersededSettings:
			return serviceGrantMinted{}, errServiceGrantDrift
		case svcSupersededRetiring, svcSupersededSecretGone:
			s.dropServiceGrantCache(p.UID)
			return serviceGrantMinted{}, svcGone()
		default: // revision or eviction: asked once more under the row's current state, inside the try budget
			return serviceGrantMinted{}, errServiceGrantRevisionMoved
		}
	}
	func() {
		s.svcGrant.mu.Lock()
		defer s.svcGrant.mu.Unlock()
		prev, held := s.svcGrant.cache[p.UID]
		if serviceGrantInstalls(held, prev.seq, seq, s.svcGrant.gen[p.UID], cacheGen) {
			if s.svcGrant.cache == nil {
				s.svcGrant.cache = map[string]serviceGrantCached{}
			}
			s.svcGrant.cache[p.UID] = serviceGrantCached{serviceGrantMinted: tok, digest: digest, rev: rev, revOK: revOK, seq: seq}
		}
		if f, ok := s.svcGrant.failed[p.UID]; ok && f.gen < attempt {
			delete(s.svcGrant.failed, p.UID)
		}
		if s.svcGrant.succGen == nil {
			s.svcGrant.succGen = map[string]uint64{}
		}
		if s.svcGrant.succGen[p.UID] < attempt {
			s.svcGrant.succGen[p.UID] = attempt
		}
	}()
	s.writeServiceGrantStatus(ctx, p, digest, sent, &tok, nil)
	return tok, nil
}

// serviceGrantDigestIn is the digest of the enabled row with this uid in sc; found is false for no such row, a
// disabled one, or one with no grant.
func serviceGrantDigestIn(sc types.SiteConfig, uid string) (digest string, found bool) {
	if sc.ModelProviders == nil {
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

// liveServiceGrantDigest is the digest of the row with this uid as the site configuration holds it now.
// found is false for a confirmed absence (no row, disabled, or no grant); a configuration that cannot be read
// is the error, never an absence.
func (s *Server) liveServiceGrantDigest(ctx context.Context, uid string) (digest string, found bool, err error) {
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		return "", false, err
	}
	digest, found = serviceGrantDigestIn(sc, uid)
	return digest, found, nil
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
