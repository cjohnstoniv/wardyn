// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The internal relay listener (design 0.9 §4.2-§4.4). A local run's proxy keeps
// its control-plane URL and CA exactly as dispatched; its runner relays the TLS
// bytes, unread, over the runner's own authenticated stream, and they end here:
// an in-memory listener whose TLS server is the internal listener's twin. Every
// request it serves is tagged with the stream's runner, which internalAuth binds
// to the run's placement (refuseRelayBinding).

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// relayedInternalRoutes is the closed list of routes a relayed request may
// reach (design §4.3), keyed "METHOD pattern" as the router registers them:
// true serves the route under its own rule, false refuses it. A route missing
// from the map is refused, and TestEveryInternalRouteIsClassifiedForRelay fails
// until every /internal/ route is classified here.
var relayedInternalRoutes = map[string]bool{
	"POST /api/v1/internal/decisions":                      true,
	"POST /api/v1/internal/activity":                       true,
	"POST /api/v1/internal/approvals":                      true,
	"GET /api/v1/internal/approvals/{id}":                  true,
	"POST /api/v1/internal/approvals/{id}/expire":          true,
	"POST /api/v1/internal/token/renew":                    true,
	"GET /api/v1/internal/injection/{grantID}":             true, // delivery = own only: refuseRelayedDelivery
	"POST /api/v1/internal/credentials/mint":               true, // delivery = own only: refuseRelayedDelivery
	"PUT /api/v1/internal/recordings/{runID}":              true,
	"PUT /api/v1/internal/recordings/{runID}/parts/{part}": true,
	"GET /api/v1/internal/via-org/{grantID}/*":             true, // delivery = via_org only: handleViaOrg
	"HEAD /api/v1/internal/via-org/{grantID}/*":            true,
	"POST /api/v1/internal/via-org/{grantID}/*":            true,
	"PUT /api/v1/internal/via-org/{grantID}/*":             true,
	"PATCH /api/v1/internal/via-org/{grantID}/*":           true,
	"DELETE /api/v1/internal/via-org/{grantID}/*":          true,
	"OPTIONS /api/v1/internal/via-org/{grantID}/*":         true,
	"PUT /api/v1/internal/scan-results/{runID}":            false, // trusted output is cluster-only
	"PUT /api/v1/internal/sso-token/{runID}":               false, // container login is cluster-only
	"POST /api/v1/internal/groundtruth":                    false, // no host sensor on a laptop
	"POST /api/v1/internal/desktop-signin/check":           false, // sign-ins never run on a runner (signinTarget)
}

type relayRunnerKey struct{}

// relayRunner is the runner whose relay stream carried this request; false
// when the request did not arrive through the relay listener.
func relayRunner(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(relayRunnerKey{}).(uuid.UUID)
	return id, ok && id != uuid.Nil
}

// relayedConn is a relay stream tagged with the runner whose authenticated
// session opened it.
type relayedConn struct {
	net.Conn
	runner uuid.UUID
	hold   *relayHold // returns the runner's relay slot on Close; nil when uncounted
}

// relayHold gives one relay slot back, once.
type relayHold struct {
	once    sync.Once
	release func()
}

func (h *relayHold) drop() { h.once.Do(h.release) }

func (c relayedConn) Close() error {
	if c.hold != nil {
		c.hold.drop()
	}
	return c.Conn.Close()
}

// maxRelayPerRunner bounds the relay streams one runner may hold open at once. A runner session
// carries at most runnerwire.MaxStreams streams, and the relay shares them with the org's own
// calls and output streams, so a runner whose local clients stall relays must not be able to
// fill the session and cut itself off from the org.
const maxRelayPerRunner = runnerwire.MaxStreams / 2

var errRelayRunnerCap = errors.New("relay stream cap for this runner reached")

// relayConnContext is the relay server's ConnContext: every request on the
// connection carries its runner, for the binding and for data.relay on every
// audit row it writes.
func relayConnContext(ctx context.Context, c net.Conn) context.Context {
	if tc, ok := c.(*tls.Conn); ok {
		c = tc.NetConn()
	}
	rc, ok := c.(relayedConn)
	if !ok {
		return ctx
	}
	return context.WithValue(audit.WithRelay(ctx, rc.runner), relayRunnerKey{}, rc.runner)
}

// relayListener is the in-memory net.Listener the relay server accepts from.
// It refuses every stream until ServeRelay starts it, and after Close.
type relayListener struct {
	once    sync.Once
	conns   chan net.Conn
	closed  chan struct{}
	stop    sync.Once
	serving atomic.Bool
	held    sync.Map // runner uuid.UUID -> *atomic.Int32: its open relay streams
}

// relayBacklog bounds the relay streams waiting for Accept; past it a stream
// is refused and the proxy's own retry comes back.
const relayBacklog = 64

var errRelayNotServed = errors.New("no internal TLS listener: the relay is not served")

func (l *relayListener) start() {
	l.once.Do(func() {
		l.conns, l.closed = make(chan net.Conn, relayBacklog), make(chan struct{})
		l.serving.Store(true)
	})
}

func (l *relayListener) offer(c net.Conn) error {
	if !l.serving.Load() {
		return errRelayNotServed
	}
	select {
	case <-l.closed:
		return net.ErrClosed
	case l.conns <- c:
		return nil
	default:
		return errors.New("relay backlog full")
	}
}

// offerFor offers c as runner id's relay stream, refusing it past maxRelayPerRunner open ones.
func (l *relayListener) offerFor(id uuid.UUID, c net.Conn) error {
	v, _ := l.held.LoadOrStore(id, new(atomic.Int32))
	n := v.(*atomic.Int32)
	if n.Add(1) > maxRelayPerRunner {
		n.Add(-1)
		return errRelayRunnerCap
	}
	rc := relayedConn{Conn: c, runner: id, hold: &relayHold{release: func() { n.Add(-1) }}}
	if err := l.offer(rc); err != nil {
		rc.hold.drop()
		return err
	}
	return nil
}

func (l *relayListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *relayListener) Close() error {
	l.serving.Store(false)
	l.stop.Do(func() { close(l.closed) })
	return nil
}

func (l *relayListener) Addr() net.Addr { return relayListenerAddr{} }

type relayListenerAddr struct{}

func (relayListenerAddr) Network() string { return "runnerwire" }
func (relayListenerAddr) String() string  { return "internal-relay" }

// acceptRelay is the runner endpoints' remote.Options.Relay. It must not
// block: it runs on the runner session's read loop.
func (s *Server) acceptRelay(runnerID string, c net.Conn) error {
	id, err := uuid.Parse(runnerID)
	if err != nil {
		return err
	}
	if err := s.runnerStreams.relay.offerFor(id, c); err != nil {
		slog.Warn("wardynd: refused a runner's relay stream", slog.String("runner_id", runnerID), slog.Any("err", err))
		return err
	}
	return nil
}

// newRelayHTTPServer is the relay's HTTP server. It speaks HTTP/1.1 only: an h2 connection
// multiplexes requests over the one relay stream, and the per-connection runner tag and the
// per-runner stream cap assume one request at a time per stream.
func newRelayHTTPServer(h http.Handler, tlsCfg *tls.Config) *http.Server {
	cfg := tlsCfg.Clone()
	cfg.NextProtos = slices.DeleteFunc(slices.Clone(cfg.NextProtos), func(p string) bool { return p == "h2" })
	return &http.Server{
		Handler:        h,
		TLSConfig:      cfg,
		TLSNextProto:   map[string]func(*http.Server, *tls.Conn, http.Handler){},
		ConnContext:    relayConnContext,
		MaxHeaderBytes: 1 << 20,
	}
}

// ServeRelay serves the relay listener with tlsCfg, the internal listener's
// own serving config, until the returned server is shut down: TLS ends here,
// so the runner forwards ciphertext only. Without it (no internal TLS
// listener) every relay stream is refused.
func (s *Server) ServeRelay(tlsCfg *tls.Config) *http.Server {
	l := &s.runnerStreams.relay
	l.start()
	srv := newRelayHTTPServer(s.relayRoutes(s.router), tlsCfg)
	go func() {
		defer l.Close()
		if err := srv.ServeTLS(l, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("wardynd: internal relay listener", slog.Any("err", err))
		}
	}()
	return srv
}

// relayRoutes is the relay listener's route filter: only relayedInternalRoutes'
// allowed rows reach the router, matched before any middleware runs, so a route
// behind its own auth (groundtruth) is refused here too.
func (s *Server) relayRoutes(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		runner, relayed := relayRunner(r.Context())
		path := r.URL.RawPath // the path chi routes on
		if path == "" {
			path = r.URL.Path
		}
		if !relayed || !relayedInternalRoutes[r.Method+" "+s.router.Find(chi.NewRouteContext(), r.Method, path)] {
			reason := placement.ReasonRelayRouteRefused
			d := authz.Deny(authz.Reason(placement.ReasonRelayRouteRefused), r.URL.Path, "").With("runner_id", runner.String())
			ev := s.refusalEvent(r.Context(), types.ActorSystem, placement.RunnerSubstrateName(runner.String()), r.Method, d)
			ev.SourceIP = r.RemoteAddr
			s.recordAudit(r.Context(), ev)
			writeErrorReason(w, reason.Status(), string(reason), "this route is not served through a runner's relay")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// refuseRelayBinding is internalAuth's relay half (design §4.3), after
// refuseTerminalRun and on the run row that read (nil when it read none, as on
// token renew). A relayed request must be for a local run on the relaying
// runner, with no runner action pending; a local run's token is refused off the
// relay. A remote run's unrelayed request is unchanged. It writes its own
// refusal and returns true once it has.
func (s *Server) refuseRelayBinding(w http.ResponseWriter, r *http.Request, claims *identity.Claims, run *types.AgentRun) bool {
	runner, relayed := relayRunner(r.Context())
	if s.cfg.Store == nil {
		return relayed && s.denyRelayed(w, r, claims, placement.ReasonRelayRouteRefused, runner.String())
	}
	if run == nil {
		got, err := s.cfg.Store.GetRun(r.Context(), claims.RunID)
		switch {
		case err != nil && !relayed:
			return false // unchanged: the self-gated route reads and refuses for itself
		case errors.Is(err, store.ErrNotFound):
			return s.denyRelayed(w, r, claims, placement.ReasonRelayRunnerMismatch, runner.String())
		case err != nil:
			slog.ErrorContext(r.Context(), "api: read run for the relay binding", slog.String("path", r.URL.Path), slog.Any("err", err))
			writeErrorReason(w, http.StatusServiceUnavailable, reasonInternalLivenessReadFailed, "read run failed; retry")
			return true
		}
		run = &got
	}
	local := run.Placement == types.PlacementLocal
	switch {
	case relayed && (!local || run.RunnerID == nil || *run.RunnerID != runner):
		return s.denyRelayed(w, r, claims, placement.ReasonRelayRunnerMismatch, runner.String())
	case !relayed && local:
		bound := ""
		if run.RunnerID != nil {
			bound = run.RunnerID.String()
		}
		return s.denyRelayed(w, r, claims, placement.ReasonRelayRequired, bound)
	case !relayed:
		return false
	}
	st, ok := s.cfg.Store.(store.RunnerStore)
	if !ok {
		return s.denyRelayed(w, r, claims, placement.ReasonRelayRouteRefused, runner.String())
	}
	pending, err := st.RunHasPendingRunnerAction(r.Context(), run.ID)
	if err != nil {
		slog.ErrorContext(r.Context(), "api: read pending runner actions for the relay fence", slog.String("path", r.URL.Path), slog.Any("err", err))
		writeErrorReason(w, http.StatusServiceUnavailable, reasonInternalLivenessReadFailed, "read pending runner actions failed; retry")
		return true
	}
	return pending && s.denyRelayed(w, r, claims, placement.ReasonRunnerActionPending, runner.String())
}

// refuseRelayedDelivery is the relayed injection and mint rule: only a grant
// delivered `own` is served through a relay. A via_org grant is used at the
// org (only its in-process engine is served it) and a runner_resident one
// already lives on the runner; a grant with no recorded delivery fails closed.
// The one runner_resident grant served is an Azure DevOps minted token's
// injection (injection true): the runner's proxy fetches it on each request
// and holds it in memory only (relayedResidentADO). Unrelayed requests are
// unchanged.
func (s *Server) refuseRelayedDelivery(w http.ResponseWriter, r *http.Request, claims *identity.Claims, grantID uuid.UUID, injection bool) bool {
	runner, relayed := relayRunner(r.Context())
	if !relayed {
		return false
	}
	var grant types.CredentialGrant
	if s.cfg.Store != nil {
		grants, err := s.cfg.Store.ListGrantsByRun(r.Context(), claims.RunID)
		if err != nil {
			writeServerError(w, r, "list grants for the relayed delivery rule", err)
			return true
		}
		for _, g := range grants {
			if g.ID == grantID {
				grant = g
			}
		}
	}
	delivery := grant.Delivery
	// The organisation's own via_org engine (viaOrgTransport) is served a
	// via_org grant and nothing else; no relayed request can carry its mark.
	if viaOrgCall(r.Context()) {
		if delivery == types.GrantDeliveryViaOrg {
			return false
		}
		return s.denyRelayed(w, r, claims, placement.ReasonDeliveryNotViaOrg, runner.String())
	}
	reason := placement.ReasonRelayRouteRefused
	switch delivery {
	case types.GrantDeliveryOwn:
		return false
	case types.GrantDeliveryViaOrg:
		reason = placement.ReasonDeliveryViaOrg
	case types.GrantDeliveryResident:
		if injection && s.relayedResidentADO(r.Context(), claims.RunID, runner, grant) {
			return false
		}
		reason = placement.ReasonDeliveryRunnerResident
	}
	return s.denyRelayed(w, r, claims, reason, runner.String())
}

// relayedResidentADO reports whether grant, recorded runner_resident, is an
// Azure DevOps minted token runner may be served now: re-judged on every
// resolve against the delivery policy, the runner's trust class, owner kind and
// posture and the connection's opt-in as they are now. The proxy resolves at
// least every residentRejudgeEvery (resolveADORunPAT), so a policy flip, a
// trust change or a withdrawn opt-in refuses within that interval, and the
// run's tokens are then revoked at Azure DevOps (reason drift), so a token the
// proxy still holds is dead at the provider too. Every other resident grant is
// refused.
func (s *Server) relayedResidentADO(ctx context.Context, runID, runner uuid.UUID, grant types.CredentialGrant) bool {
	if grant.RunID != runID || grant.Spec.Kind != types.GrantAPIKey {
		return false
	}
	run, err := s.cfg.Store.GetRun(ctx, runID)
	if err != nil || run.Placement != types.PlacementLocal || run.RunnerID == nil || *run.RunnerID != runner {
		return false
	}
	origin, ref := s.localGrantOrigin(ctx, runIdentitySubject(ctx, run.CreatedBy), grant.Spec)
	if ref != nil || !residentADOOrigin(origin) {
		return false
	}
	r, err := s.residentADOAdmits(ctx, run, origin)
	if err != nil {
		return false
	}
	if r != nil {
		s.revokeRunPATs(ctx, runID, adoPATRevokeDrift)
		return false
	}
	return true
}

// denyRelayed writes a relay refusal: authz.denied with runner_id (the
// relaying runner, or the run's own for relay_required), then the reason's status.
func (s *Server) denyRelayed(w http.ResponseWriter, r *http.Request, claims *identity.Claims, reason placement.Reason, runnerID string) bool {
	s.auditInternalDenied(r, claims, authz.Reason(reason), "runner_id", runnerID)
	writeErrorReason(w, reason.Status(), string(reason), "refused for a run placed on a runner: "+string(reason))
	return true
}
