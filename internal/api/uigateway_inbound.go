// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Inbound access: an app a run declares `inbound: true` in ui_apps is a service port that the run's owner
// and the callers it names may call through this gateway, on any run, a background one included. People
// reach it with the ticket-to-cookie session uigateway.go mints; machines with an inbound token sent as
// `Authorization: Bearer wdn_…`, the one bearer this listener takes (verifyAPIToken with inbound set, so no
// API, CLI, admin or portal token ever calls a port). From there both ride one pipeline: the session
// re-assert, a per-run request bucket, one inbound.allow row per admitted request, then the relay. The
// token never reaches the sandbox (uiStripInbound drops Authorization); X-Wardyn-Caller names the caller.
// A WebSocket or other upgrade a token opens is admitted again every inboundResolveTTL while it lasts
// (watchInboundStream), so a revoke reaches it as it reaches the token's next request.
//
// The host-mode cross-site check here binds every cookie session, inbound or not: the victim of a foreign
// app on a sibling run's origin is the caller's own editor session, which a same-site page could otherwise
// drive (a same-site WebSocket carries its cookie, and CORS does not cover WebSocket).
package api

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	// inboundResolveTTL is how long a verified token's session for one run and app is reused: the token
	// lookup, the run and its effective apps are read once per window, so a revoked token, or one whose
	// owner lost the run, stops calling within it. The session's own re-assert runs on top (uiReassertRelay).
	inboundResolveTTL = 30 * time.Second
	// inboundRatePerSec and inboundBurst bound the requests to one run's inbound apps, all callers together,
	// and again the requests of one caller, all runs together: each admitted request is an audit row, and
	// the run's 8 relay connections are shared by all of them.
	inboundRatePerSec = 50.0
	inboundBurst      = 100.0
	// maxInboundEntries bounds each of uiInboundState's maps; past it a map is pruned wholesale, like
	// uiReady's. The worst case is one extra token lookup, or one fresh bucket, per live caller.
	maxInboundEntries = 1024
)

// The inbound.deny `reason` values the relay writes itself: audit data, never on the wire. A re-assert
// refusal of an inbound session is its ui.authorize denial, as for any session, except the profile's
// (unit_denies, uiInboundUnitDenies).
const (
	inboundDenyCredentialInvalid = "credential_invalid"
	inboundDenyNotFound          = "not_found"
	inboundDenyCrossSite         = "cross_site"
	inboundDenyRateLimited       = "rate_limited"
	inboundDenyNotRunning        = "not_running"
	inboundDenyUnitDenies        = "unit_denies"
)

const (
	inboundCredentialInvalidMsg = "This is not a live inbound token"
	// inboundNotFoundMsg answers no run, no such app, an app that is not inbound and a token whose holder is
	// not one of its callers alike, so a token is no oracle for which runs exist.
	inboundNotFoundMsg    = "No inbound app here answers this token"
	inboundNotRunningMsg  = "This run is not running; its inbound apps are gone"
	inboundRateLimitedMsg = "Too many requests to this run's inbound apps; retry in a second"
	uiCrossSiteMsg        = "This request came from another site; open the app from its own page"
	uiRunnerOfflineMsg    = "The runner holding this run is offline; try again when it reconnects"
)

// uiInboundState is the inbound arm's process state, in uiGatewayState: process-local like the rest of the
// gateway's, which replicas>1 refuses by construction.
type uiInboundState struct {
	mu sync.Mutex
	// sessions is each verified token's session for one run and app, by inboundSessionKey.
	sessions map[string]inboundCached
	// buckets is each run's request bucket, with the sandbox reference its rows carry (recordStreamAudit).
	buckets map[uuid.UUID]*inboundBucket
	// callers is each caller's request bucket, by principal, over all the runs they call.
	callers map[string]*inboundBucket
}

type inboundCached struct {
	sess uiSession
	at   time.Time
}

type inboundBucket struct {
	tokens float64
	last   time.Time
	ref    string
}

// inboundSessionKey names one token's session for one run and app. The token is hashed, so the cache
// never holds a usable credential.
func inboundSessionKey(raw string, runID uuid.UUID, app string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:]) + "/" + runID.String() + "/" + app
}

func (st *uiInboundState) cached(key string, now time.Time) (uiSession, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	c, ok := st.sessions[key]
	return c.sess, ok && now.Sub(c.at) < inboundResolveTTL
}

func (st *uiInboundState) remember(key string, sess uiSession, now time.Time) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.sessions == nil {
		st.sessions = map[string]inboundCached{}
	} else if len(st.sessions) > maxInboundEntries {
		clear(st.sessions)
	}
	st.sessions[key] = inboundCached{sess: sess, at: now}
}

// refill is k's bucket in *m, full and new when it has none, topped up to now.
func refill[K comparable](m *map[K]*inboundBucket, k K, now time.Time) *inboundBucket {
	b := (*m)[k]
	if b == nil {
		if *m == nil {
			*m = map[K]*inboundBucket{}
		} else if len(*m) > maxInboundEntries {
			clear(*m)
		}
		b = &inboundBucket{tokens: inboundBurst, last: now}
		(*m)[k] = b
	}
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = min(inboundBurst, b.tokens+elapsed*inboundRatePerSec)
	}
	b.last = now
	return b
}

// take spends one request from runID's bucket and from principal's, recording ref as the run's sandbox when
// it is known, and answers whether the request may go and the sandbox reference the run's bucket holds. A
// request that either bucket refuses spends from neither, so a caller over its own limit takes nothing
// from the run's callers.
func (st *uiInboundState) take(runID uuid.UUID, principal string, now time.Time, ref string) (bool, string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	b, c := refill(&st.buckets, runID, now), refill(&st.callers, principal, now)
	if ref != "" {
		b.ref = ref
	}
	if b.tokens < 1 || c.tokens < 1 {
		return false, b.ref
	}
	b.tokens--
	c.tokens--
	return true, b.ref
}

func (st *uiInboundState) setRef(runID uuid.UUID, ref string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if b := st.buckets[runID]; b != nil {
		b.ref = ref
	}
}

// uiRelayAnsweredEarly answers the two kinds of relay request the cookie pipeline never sees: one carrying
// an inbound token, served here whole, and in host mode a cookie request from another site, refused. True
// when it has answered. Any other bearer is not a credential here and falls to the cookie path, which strips it.
func (s *Server) uiRelayAnsweredEarly(w http.ResponseWriter, r *http.Request, runID uuid.UUID, app string) bool {
	if raw, ok := bearerToken(r); ok && strings.HasPrefix(raw, apiTokenPrefix) {
		if sess, ok := s.inboundTokenSession(w, r, runID, app, raw); ok {
			// An upgrade this request becomes is admitted again while it lasts (watchInboundStream).
			a := inboundReadmission{r: r.WithContext(context.WithoutCancel(r.Context())), runID: runID, app: app, raw: raw, principal: sess.Principal}
			s.relayUISession(w, r.WithContext(context.WithValue(r.Context(), inboundReadmitCtxKey{}, a)), sess)
		}
		return true
	}
	if !s.uiCrossSite(r) {
		return false
	}
	data := map[string]any{"reason": inboundDenyCrossSite, "method": r.Method,
		"sec_fetch_site": uiAuditHeader(r, "Sec-Fetch-Site"), "origin": uiAuditHeader(r, "Origin")}
	if sess, ok := s.decodeUISession(r, s.cfg.Now()); ok && sess.Inbound && sess.Run == runID && sess.App == app {
		if s.authFailedAllowed() {
			s.auditInbound(r, sess, "", "inbound.deny", "denied", data)
		}
	} else {
		s.auditUIRefusedBounded(runID, app, data)
	}
	writeErrorReason(w, http.StatusForbidden, reasonUIGatewayCrossSite, uiCrossSiteMsg)
	return true
}

// uiCrossSite is the host-mode check every cookie request passes. A request may come from its own origin,
// from no site at all (Sec-Fetch-Site absent or none: typed, bookmarked, or a client that sends no fetch
// metadata), or from elsewhere only as a top-level GET or HEAD navigation (a link); and an Origin it names
// must be its own scheme and host. Path mode serves every run from one origin, so there is no other run's
// site to tell apart: false.
func (s *Server) uiCrossSite(r *http.Request) bool {
	if s.cfg.UIOriginTemplate == "" {
		return false
	}
	scheme, _, ok := strings.Cut(s.cfg.UIOriginTemplate, "://")
	if !ok {
		scheme = ""
	}
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		if err != nil || !strings.EqualFold(u.Host, r.Host) || (scheme != "" && !strings.EqualFold(u.Scheme, scheme)) {
			return true
		}
	}
	switch strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site"))) {
	case "", "same-origin", "none":
		return false
	}
	navigation := (r.Method == http.MethodGet || r.Method == http.MethodHead) &&
		r.Header.Get("Sec-Fetch-Mode") == "navigate" && r.Header.Get("Sec-Fetch-Dest") == "document"
	return !navigation
}

// uiRunOrigin returns the host this run's apps must be served on in HOST mode
// (WARDYN_UI_SANDBOX_ORIGIN_TEMPLATE), or "" in shared-origin path mode where
// any host that reaches the listener is acceptable.
func (s *Server) uiRunOrigin(runID uuid.UUID) string {
	if s.cfg.UIOriginTemplate == "" {
		return ""
	}
	origin := strings.ReplaceAll(s.cfg.UIOriginTemplate, "{run}", runID.String())
	if i := strings.Index(origin, "://"); i >= 0 {
		origin = origin[i+3:]
	}
	return strings.TrimSuffix(origin, "/")
}

// inboundTokenSession is the bearer arm's credential: the session an inbound token holds on app of runID,
// reused for inboundResolveTTL (admitInboundToken). ok=false when it has answered.
func (s *Server) inboundTokenSession(w http.ResponseWriter, r *http.Request, runID uuid.UUID, app, raw string) (uiSession, bool) {
	now := s.cfg.Now()
	key := inboundSessionKey(raw, runID, app)
	if sess, ok := s.uiInbound.cached(key, now); ok {
		return sess, true
	}
	sess, ok := s.admitInboundToken(w, r, runID, app, raw)
	if ok {
		s.uiInbound.remember(key, sess, now)
	}
	return sess, ok
}

// admitInboundToken is the bearer arm's admission, uncached: a bearer that is not a live inbound token is
// 401 inbound_credential_invalid; a live one that may not call this app — no run, no such app, an app that
// is not inbound, a holder who is neither the owner nor a caller — is 404 inbound_not_found, one answer for
// all four; and one that may, on a run that is not running or is kept, is 409 ui_gateway_not_running. Each
// refusal writes its inbound.deny row. ok=false when it has answered.
func (s *Server) admitInboundToken(w http.ResponseWriter, r *http.Request, runID uuid.UUID, app, raw string) (uiSession, bool) {
	v, r, verdict := s.verifyAPIToken(w, r, raw, true)
	switch verdict {
	case tokenAnswered:
		return uiSession{}, false
	case tokenRefused:
		s.auditInboundRefused(r, runID, app, "", inboundDenyCredentialInvalid)
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeErrorReason(w, http.StatusUnauthorized, reasonInboundCredentialInvalid, inboundCredentialInvalidMsg)
		return uiSession{}, false
	}
	sess, deny := s.inboundSessionFor(r.Context(), runID, app, v)
	switch deny {
	case "":
		return sess, true
	case inboundDenyNotRunning:
		s.auditInboundRefused(r, runID, app, v.Principal, deny)
		writeErrorReason(w, http.StatusConflict, reasonUIGatewayNotRunning, inboundNotRunningMsg)
	default:
		s.auditInboundRefused(r, runID, app, v.Principal, deny)
		writeErrorReason(w, http.StatusNotFound, reasonInboundNotFound, inboundNotFoundMsg)
	}
	return uiSession{}, false
}

// inboundReadmitCtxKey carries an inbound-token request's inboundReadmission to the upgrade it may become
// (uiRelayAnsweredEarly, watchInboundStream).
type inboundReadmitCtxKey struct{}

// inboundReadmission is what a stream needs to admit its token again: the request that opened it, held past
// its cancel, and the token and the principal it proved.
type inboundReadmission struct {
	r                   *http.Request
	runID               uuid.UUID
	app, raw, principal string
}

// readmitInbound admits a's token again, uncached (admitInboundToken). Its answers go nowhere: the stream
// has no response left to write them on. transient is a refusal that is no verdict on the token: a lookup
// that could not be made (verifyAPIToken's 503), which writes no row.
func (s *Server) readmitInbound(a inboundReadmission) (ok, transient bool) {
	w := &inboundReadmitWriter{healthzGateWriter: healthzGateWriter{h: http.Header{}}}
	_, ok = s.admitInboundToken(w, a.r, a.runID, a.app, a.raw)
	return ok, !ok && w.status == http.StatusServiceUnavailable
}

// inboundReadmitWriter is healthzGateWriter that keeps the status of what was written.
type inboundReadmitWriter struct {
	healthzGateWriter
	status int
}

func (w *inboundReadmitWriter) WriteHeader(code int) { w.status = code }

// watchInboundStream holds an upgraded stream an inbound token opened to the authority that admitted it:
// every inboundResolveTTL (uiInboundStreamEvery in tests) the token is admitted again, uncached, by
// admitInboundToken, so the token's revoke or expiry, its service identity or sponsor going, its owner
// losing the run or the app no longer naming its caller all reach the stream as they reach a request. A
// refusal writes the inbound.deny row a refused request writes and closes the stream. A lookup that
// cannot be made keeps the stream and asks again next tick, so one store blip ends no stream; one that
// stays unanswerable for inboundResolveTTL, the staleness the design allows, closes it with an inbound.deny
// token_lookup_unavailable row. A browser session's stream carries no re-admission and is left as it was
// (TestUIGateway_EstablishedConnectionOutlivesTheRevoke).
func (s *Server) watchInboundStream(resp *http.Response) {
	a, ok := resp.Request.Context().Value(inboundReadmitCtxKey{}).(inboundReadmission)
	if !ok {
		return
	}
	body, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		return // ReverseProxy refuses a 101 without a writable body itself
	}
	stream := &inboundStream{ReadWriteCloser: body, done: make(chan struct{})}
	resp.Body = stream
	go func() {
		tick := time.NewTicker(cmp.Or(s.uiInboundStreamEvery, inboundResolveTTL))
		defer tick.Stop()
		var failingSince time.Time // the first of the unanswerable re-admissions in a row
		for {
			select {
			case <-stream.done:
				return
			case <-tick.C:
				ok, transient := s.readmitInbound(a)
				if ok {
					failingSince = time.Time{}
					continue
				}
				if transient {
					now := s.cfg.Now()
					if failingSince.IsZero() {
						failingSince = now
					}
					if now.Sub(failingSince) < inboundResolveTTL {
						continue
					}
					s.auditInboundRefused(a.r, a.runID, a.app, a.principal, reasonTokenLookupUnavailable)
				}
				_ = stream.Close() // the relay's copy ends, and the caller's connection with it
				return
			}
		}
	}()
}

// inboundStream is the sandbox side of an inbound-token stream: its first Close also ends the watch.
type inboundStream struct {
	io.ReadWriteCloser
	once sync.Once
	done chan struct{}
	err  error
}

func (c *inboundStream) Close() error {
	c.once.Do(func() {
		close(c.done)
		c.err = c.ReadWriteCloser.Close()
	})
	return c.err
}

// inboundSessionFor is the session v may hold on app of runID, or the inbound.deny reason it may hold none:
// not_found for no reach, and not_running for a caller whose run is not running, has no sandbox or is kept
// (asked after reach, so the state is no oracle). A store failure is not_found too (logged): the request
// fails closed, and a retry asks again.
//
// The session's authority time is the token's creation, so the re-assert's revocation cutoff and sponsor
// check order it as they do the token itself. It carries no role: no role gives inbound reach.
func (s *Server) inboundSessionFor(ctx context.Context, runID uuid.UUID, app string, v verifiedAPIToken) (uiSession, string) {
	run, err := s.cfg.Store.GetRun(ctx, runID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			slog.ErrorContext(ctx, "wardynd: ui gateway inbound run lookup failed", "run_id", runID, "err", err)
		}
		return uiSession{}, inboundDenyNotFound
	}
	declared, ok := s.inboundApp(ctx, runID, app)
	if !ok {
		return uiSession{}, inboundDenyNotFound
	}
	caller, ok := s.inboundCaller(ctx, run, declared, v.Principal, v.Email, v.Groups, v.CreatedAt)
	if !ok {
		return uiSession{}, inboundDenyNotFound
	}
	// uiDial's state check, made here so a call it would refuse is never admitted (inbound.allow).
	if run.State != types.RunRunning || run.SandboxRef == "" || runIsKept(run) {
		return uiSession{}, inboundDenyNotRunning
	}
	now := s.cfg.Now()
	sess := uiSession{
		SandboxRef: run.SandboxRef, Run: run.ID, App: declared.Name, Port: declared.Port,
		Principal: v.Principal, Email: v.Email, Expires: now.Add(inboundResolveTTL).Unix(), IssuedAt: now.Unix(),
		AuthorizedAt: v.CreatedAt.Unix(), AuthorizedNs: int64(v.CreatedAt.Nanosecond()), RevSeq: v.RevSeq,
		Inbound: true, Caller: caller, tokenID: v.ID,
	}
	// The sponsor arm of the owner rule: a person calling their service identity's run.
	if caller == "" && v.Principal != run.CreatedBy {
		sess.Service = sponsoredService(run)
	}
	if v.service != nil {
		sess.sponsor = v.service.Sponsor
	}
	return sess, ""
}

// inboundApp is app as the run's effective policy declares it, when it is declared inbound. A policy that
// cannot be read declares nothing (logged), so every caller fails closed on it.
func (s *Server) inboundApp(ctx context.Context, runID uuid.UUID, app string) (types.UIApp, bool) {
	apps, err := s.effectiveUIApps(ctx, runID)
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: ui gateway inbound policy lookup failed", "run_id", runID, "err", err)
		return types.UIApp{}, false
	}
	for _, a := range apps {
		if a.Name == app && a.Inbound {
			return a, true
		}
	}
	return types.UIApp{}, false
}

// uiAppIsInbound reports whether app is an inbound app of runID: the background-only door's one exception.
func (s *Server) uiAppIsInbound(ctx context.Context, runID uuid.UUID, app string) bool {
	_, ok := s.inboundApp(ctx, runID, app)
	return ok
}

// inboundCaller is who principal is to app on run: its owner (caller ""), by the run's own entry rule with
// the sponsor arm and never by role; or the first callers entry that names them, `service:` the service
// identity itself, `user:` their subject, or their email where sign-in vouches for emails (emailTrusted,
// the rule capability grants follow), `group:` a group their credential was stamped with. ok=false for
// anyone else. since is the credential's authority time, which the sponsor arm compares.
func (s *Server) inboundCaller(ctx context.Context, run types.AgentRun, app types.UIApp, principal, email string, groups []string, since time.Time) (string, bool) {
	if s.mayEnterRunOrSponsor(ctx, run, principal, false, since) {
		return "", true
	}
	service := isServicePrincipal(principal)
	for _, c := range app.Callers {
		kind, name, _ := strings.Cut(c, ":")
		var match bool
		switch kind {
		case "service":
			match = service && principal == types.ServicePrincipalPrefix+name
		case "user":
			match = !service && (types.CanonicalUserSubject(principal) == name ||
				(email != "" && s.emailTrusted() && types.CanonicalUserSubject(email) == name))
		case "group":
			match = !service && slices.ContainsFunc(groups, func(g string) bool {
				canon, ok := types.CanonicalGroupSubject(g)
				return ok && canon == name
			})
		}
		if match {
			return c, true
		}
	}
	return "", false
}

// uiInboundUnitDenies is the re-assert's profile arm for an inbound session: the run's profile as it stands
// now denies UI apps, or denies interactive sessions, which a service port could serve. Its refusal is the
// session's inbound.deny unit_denies row, rate-bound as the arm's other refusals are, in place of a
// ui.authorize denial; the arm also runs on the connection path (uiDial), where no request is in hand, so
// the row names no method or path.
func (s *Server) uiInboundUnitDenies(ctx context.Context, sess uiSession, run types.AgentRun) *uiDialError {
	p, err := s.runUnit(ctx, run)
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: ui gateway could not read the run's governance profile", "run_id", run.ID, "err", err)
		return s.uiDenyReassert(sess, reasonRunUnreadable, http.StatusServiceUnavailable, uiSessionUnverifiableMsg)
	}
	if p == nil || (!p.Limits.DenyUIApps && !p.Limits.DenyInteractive) {
		return nil
	}
	if s.authFailedAllowed() {
		s.recordInbound(ctx, "", sess, sess.SandboxRef, "inbound.deny", "denied", map[string]any{"reason": inboundDenyUnitDenies})
	}
	return &uiDialError{status: http.StatusForbidden, reason: string(authz.ReasonGovernanceUnit),
		msg: fmt.Sprintf("Inbound apps are not allowed under the governance profile %q this run was launched under.", p.Name)}
}

// admitInbound spends an inbound session's request from its run's bucket and its caller's, and writes its
// inbound.allow row.
// False when it has answered: 429 inbound_rate_limited with Retry-After, its denial rate-bound like auth.fail.
func (s *Server) admitInbound(w http.ResponseWriter, r *http.Request, sess uiSession) bool {
	ok, ref := s.uiInbound.take(sess.Run, sess.Principal, s.cfg.Now(), sess.SandboxRef)
	if ref == "" {
		// A cookie session holds no sandbox reference: read it once per bucket.
		if run, err := s.cfg.Store.GetRun(r.Context(), sess.Run); err == nil && run.SandboxRef != "" {
			ref = run.SandboxRef
			s.uiInbound.setRef(sess.Run, ref)
		}
	}
	if !ok {
		if s.authFailedAllowed() {
			s.auditInbound(r, sess, ref, "inbound.deny", "denied", map[string]any{"reason": inboundDenyRateLimited})
		}
		w.Header().Set("Retry-After", "1")
		writeErrorReason(w, http.StatusTooManyRequests, reasonInboundRateLimited, inboundRateLimitedMsg)
		return false
	}
	s.auditInbound(r, sess, ref, "inbound.allow", "success", map[string]any{})
	return true
}

// auditInbound writes one inbound.* row of sess's request, with the egress row's shape: the caller, the
// app, and what was asked (the path the app sees, never the query). ref, when known, is the run's sandbox,
// so a remote runner's rows carry the relay.
func (s *Server) auditInbound(r *http.Request, sess uiSession, ref, action, outcome string, data map[string]any) {
	data["method"], data["path"] = r.Method, inboundPath(r, sess.Run, sess.App)
	s.recordInbound(r.Context(), r.RemoteAddr, sess, ref, action, outcome, data)
}

// recordInbound writes one inbound.* row of sess with data: the caller and the app. sourceIP is the
// request's, "" where the row has no request (uiInboundUnitDenies).
func (s *Server) recordInbound(ctx context.Context, sourceIP string, sess uiSession, ref, action, outcome string, data map[string]any) {
	data["app"], data["port"] = sess.App, sess.Port
	data["via"], data["caller"] = "session", "owner"
	if sess.tokenID != uuid.Nil {
		data["via"], data["token_id"] = "token", sess.tokenID
	}
	if sess.Caller != "" {
		data["caller"] = sess.Caller
	}
	if sess.Service != "" {
		data["service_identity"], data["entry"] = sess.Service, sponsorEntry
	}
	ev := s.auditEvent(&sess.Run, uiActorType(sess), sess.Principal, action, sess.App, outcome, mustJSON(data))
	ev.SourceIP = sourceIP
	s.recordStreamAudit(s.uiAuditCtx(context.WithoutCancel(ctx), sess), ref, ev)
}

// auditInboundRefused writes the inbound.deny row of a token refused before it held a session, within
// authFailedAllowed's bound. principal is "" until the token proved one.
func (s *Server) auditInboundRefused(r *http.Request, runID uuid.UUID, app, principal, reason string) {
	if !s.authFailedAllowed() {
		return
	}
	actorType := types.ActorHuman
	if isServicePrincipal(principal) {
		actorType = serviceActorType
	}
	ev := s.auditEvent(&runID, actorType, principal, "inbound.deny", app, "denied", mustJSON(map[string]any{
		"reason": reason, "via": "token", "method": r.Method, "path": inboundPath(r, runID, app)}))
	ev.SourceIP = r.RemoteAddr
	s.recordAudit(context.WithoutCancel(r.Context()), ev)
}

// inboundPath is the path an app's request reaches it at: the relay prefix off, no query.
func inboundPath(r *http.Request, runID uuid.UUID, app string) string {
	if p := strings.TrimPrefix(r.URL.Path, uiRelayPrefix(runID, app)); p != "" {
		return p
	}
	return "/"
}

// uiActorType is the audit actor type of sess's principal: a service identity's token calls as a service.
func uiActorType(sess uiSession) types.ActorType {
	if isServicePrincipal(sess.Principal) {
		return serviceActorType
	}
	return types.ActorHuman
}

// uiAuditCtx is ctx for one of sess's rows: a service identity's token adds its sponsor, which the recorder
// stamps as data.sponsor, as on the API.
func (s *Server) uiAuditCtx(ctx context.Context, sess uiSession) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if sess.sponsor != "" {
		ctx = audit.WithSponsor(ctx, sess.sponsor)
	}
	return ctx
}

// uiCallerHeaders is uiRewrite's X-Wardyn-* work: no client-sent X-Wardyn-* header reaches any app, so on
// an inbound app the ones the relay sets are the only ones there are. X-Wardyn-Caller is the caller's
// principal, X-Wardyn-Caller-Email their email when the credential carries one.
func uiCallerHeaders(h http.Header, sess uiSession) {
	for k := range h {
		if strings.HasPrefix(http.CanonicalHeaderKey(k), "X-Wardyn-") {
			delete(h, k)
		}
	}
	if !sess.Inbound {
		return
	}
	h.Set("X-Wardyn-Caller", sess.Principal)
	if sess.Email != "" {
		h.Set("X-Wardyn-Caller-Email", sess.Email)
	}
}

// uiExecFailure is a failed relay or launcher exec as the caller sees it: a runner that is offline is 503
// runner_offline (the run is fine; retry once it reconnects), anything else a 502 under reason.
func uiExecFailure(ctx context.Context, reason, action string, err error) error {
	if errors.Is(err, runner.ErrRunnerOffline) {
		return uiFail(ctx, http.StatusServiceUnavailable, string(placement.ReasonRunnerOffline), uiRunnerOfflineMsg)
	}
	return uiFail(ctx, http.StatusBadGateway, reason, uiExecFailMsg(ctx, action, err))
}
