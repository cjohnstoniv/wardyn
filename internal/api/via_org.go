// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The organisation's via_org egress (design 0.9 §7.3). A local run's proxy forwards each request for
// a via_org destination here through its runner's relay; the request is served by an in-process
// wardyn-proxy engine built from the run's stored config and its via_org grants, which re-runs every
// proxy check as authoritative and injects the credential on a TLS leg the organisation dials. The
// destination comes from the grant, never the request.

import (
	"bytes"
	"context"
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

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// viaOrgMethods are the methods the via_org route serves: the sandbox request's own method is kept.
var viaOrgMethods = []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions}

const (
	viaOrgRoutePattern = "/internal/via-org/{grantID}/*"
	// viaOrgDoor is the route's own prefix; what follows its grant id is the sandbox's.
	viaOrgDoor = "/internal/via-org/"
)

func (s *Server) mountViaOrgRoute(r chi.Router) {
	for _, m := range viaOrgMethods {
		r.Method(m, viaOrgRoutePattern, http.HandlerFunc(s.handleViaOrg))
	}
}

// viaOrgDestination is where one via_org grant's requests go: a header-injected host, or the git
// broker's /wardyn/gh/ route.
type viaOrgDestination struct {
	host   string
	broker bool
}

// handleViaOrg serves one forwarded sandbox request. internalAuth has already bound it to the run
// token, a live run and the run's own runner's relay.
func (s *Server) handleViaOrg(w http.ResponseWriter, r *http.Request) {
	claims, err := claimsFromContext(r)
	if err != nil {
		writeErrorReason(w, http.StatusUnauthorized, reasonMissingRunToken, "missing run claims")
		return
	}
	if s.refuseViaOrgEndedRun(w, r, claims) {
		return
	}
	runnerID, relayed := relayRunner(r.Context())
	grantID, perr := uuid.Parse(chi.URLParam(r, "grantID"))
	if perr != nil || !relayed {
		s.denyViaOrg(w, r, claims, placement.ReasonDeliveryNotViaOrg, "not a via_org grant of this run")
		return
	}
	grant, dest, ok := s.viaOrgGrant(w, r, claims, grantID)
	if !ok {
		return
	}
	rest, restEsc, ok := viaOrgRest(r.URL)
	if !ok || (dest.broker && !strings.HasPrefix(rest, "/wardyn/gh/")) || (!dest.broker && !viaOrgAuthorityIs(r.Header.Get(proxy.ViaOrgAuthorityHeader), dest.host)) {
		s.denyViaOrg(w, r, claims, placement.ReasonViaOrgDestinationRefused, "the request's destination is not this grant's")
		return
	}
	run, ok := s.viaOrgRunFor(claims.RunID, runnerID)
	if !ok {
		s.denyViaOrg(w, r, claims, placement.ReasonRelayRunnerMismatch, "the run's via_org egress belongs to another runner")
		return
	}
	token, _ := bearerToken(r)
	run.token.Store(&token)
	gen, st, refused, ok := s.admitViaOrg(w, r, run, claims, grant.ID)
	if !ok {
		return
	}
	if refused != "" {
		s.denyViaOrg(w, r, claims, placement.ReasonViaOrgCapExceeded, refused)
		return
	}
	defer run.release(st)
	// A cancelled stream (run end, release, idle) must return even while its Write is blocked on a
	// peer that stopped reading: the server sets no write timeout (streams are long), so the
	// deadline is set here. The stop does not wait for a callback already running, so the handler's
	// return is fenced under mu: a deadline is never set on a writer ServeHTTP has handed back.
	var mu sync.Mutex
	returned := false
	stopWriteDeadline := context.AfterFunc(st.ctx, func() {
		mu.Lock()
		defer mu.Unlock()
		if !returned {
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now())
		}
	})
	defer func() {
		stopWriteDeadline()
		mu.Lock()
		returned = true
		mu.Unlock()
	}()
	out := viaOrgEngineRequest(r.WithContext(st.ctx), rest, restEsc, dest, st)
	cw := &viaOrgCountingWriter{ResponseWriter: w, st: st}
	if dest.broker {
		gen.engine.ServeBroker(cw, out)
		return
	}
	gen.engine.ServeInjected(cw, out, dest.host)
}

// viaOrgAfterEngine is a test seam: it runs between taking the run's engine and admitting the stream.
var viaOrgAfterEngine func()

// admitViaOrg takes the run's current engine generation and admits one stream to it. A generation
// retired in between (an idle or offline release) is never served: the request takes the live
// replacement, once, and is told to retry if that one is retired too. ok is false once a refusal
// is written; refused names a bound the stream would cross.
func (s *Server) admitViaOrg(w http.ResponseWriter, r *http.Request, run *viaOrgRun, claims *identity.Claims, grantID uuid.UUID) (gen *viaOrgGen, st *viaOrgStream, refused string, ok bool) {
	for range 2 {
		g, routes, err := s.viaOrgEngine(r.Context(), run, claims.RunID)
		if err != nil {
			slog.ErrorContext(r.Context(), "api: build the via_org engine", slog.String("run_id", claims.RunID.String()), slog.Any("err", err))
			writeErrorReason(w, http.StatusServiceUnavailable, reasonInternalLivenessReadFailed, "the organisation's egress for this run could not be built; retry")
			return nil, nil, "", false
		}
		if !slices.ContainsFunc(routes, func(rt proxy.ViaOrgRoute) bool { return rt.GrantID == grantID }) {
			s.denyViaOrg(w, r, claims, placement.ReasonDeliveryNotViaOrg, "the grant was not dispatched via_org for this run")
			return nil, nil, "", false
		}
		if viaOrgAfterEngine != nil {
			viaOrgAfterEngine()
		}
		st, refused, retired := run.open(r.Context(), g)
		if !retired {
			return g, st, refused, true
		}
	}
	writeErrorReason(w, http.StatusServiceUnavailable, reasonInternalLivenessReadFailed, "the organisation's egress for this run was replaced while the request waited; retry")
	return nil, nil, "", false
}

// refuseViaOrgEndedRun is this route's own liveness check, beside internalAuth's: the request path
// after the grant id is the sandbox's, so the route never relies on a gate that reads the path. A
// terminal or kept run is refused as refuseTerminalRun refuses it, with no grace.
func (s *Server) refuseViaOrgEndedRun(w http.ResponseWriter, r *http.Request, claims *identity.Claims) bool {
	run, err := s.cfg.Store.GetRun(r.Context(), claims.RunID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.auditInternalDenied(r, claims, authz.ReasonRunNotFound, "", "")
		writeErrorReason(w, http.StatusForbidden, string(authz.ReasonRunNotFound), "run not found")
	case err != nil:
		slog.ErrorContext(r.Context(), "api: read run for via_org", slog.String("run_id", claims.RunID.String()), slog.Any("err", err))
		writeErrorReason(w, http.StatusServiceUnavailable, reasonInternalLivenessReadFailed, "read run failed; retry")
	case isTerminalRunState(run.State):
		s.auditInternalDenied(r, claims, authz.ReasonRunTerminal, "run_state", string(run.State))
		writeErrorReason(w, http.StatusForbidden, string(authz.ReasonRunTerminal), "run is terminal")
	case runIsKept(run):
		s.auditInternalDenied(r, claims, authz.ReasonRunKept, "lost_reason", string(run.LostReason))
		writeErrorReason(w, http.StatusForbidden, string(authz.ReasonRunKept), "run is lost")
	default:
		return false
	}
	return true
}

// viaOrgGrant reads the run's grant and its destination: the grant must be this run's and recorded
// via_org, else delivery_not_via_org.
func (s *Server) viaOrgGrant(w http.ResponseWriter, r *http.Request, claims *identity.Claims, grantID uuid.UUID) (types.CredentialGrant, viaOrgDestination, bool) {
	grants, err := s.cfg.Store.ListGrantsByRun(r.Context(), claims.RunID)
	if err != nil {
		writeServerError(w, r, "list grants for via_org", err)
		return types.CredentialGrant{}, viaOrgDestination{}, false
	}
	i := slices.IndexFunc(grants, func(g types.CredentialGrant) bool { return g.ID == grantID })
	if i < 0 || grants[i].RunID != claims.RunID || grants[i].Delivery != types.GrantDeliveryViaOrg {
		s.denyViaOrg(w, r, claims, placement.ReasonDeliveryNotViaOrg, "not a via_org grant of this run")
		return types.CredentialGrant{}, viaOrgDestination{}, false
	}
	g := grants[i]
	switch g.Spec.Kind {
	case types.GrantAPIKey:
		rule, err := injectionRuleFromScope(g.Spec.Scope)
		if err == nil && rule.Host != "" {
			return g, viaOrgDestination{host: rule.Host}, true
		}
	case types.GrantGitHubToken:
		return g, viaOrgDestination{host: githubHost, broker: true}, true
	}
	s.denyViaOrg(w, r, claims, placement.ReasonDeliveryNotViaOrg, "the grant has no via_org destination")
	return types.CredentialGrant{}, viaOrgDestination{}, false
}

// githubHost is the destination of the git broker's /wardyn/gh/ route.
const githubHost = "github.com"

// viaOrgAuthorityIs reports whether a forwarded authority names exactly host on the TLS port: the
// organisation dials host:443 and nothing else.
func viaOrgAuthorityIs(authority, host string) bool {
	h, port := authority, ""
	if i := strings.LastIndexByte(authority, ':'); i >= 0 && !strings.Contains(authority[i:], "]") {
		h, port = authority[:i], authority[i+1:]
	}
	norm := func(v string) string { return strings.TrimRight(strings.ToLower(v), ".") }
	return h != "" && norm(h) == norm(host) && (port == "" || port == "443")
}

// viaOrgRest is the sandbox request's own path: what follows the grant segment, decoded and as sent.
func viaOrgRest(u *url.URL) (rest, escaped string, ok bool) {
	after, ok := strings.CutPrefix(u.EscapedPath(), proxy.ViaOrgPathPrefix)
	if !ok {
		return "", "", false
	}
	if _, escaped, ok = strings.Cut(after, "/"); !ok {
		return "", "", false
	}
	escaped = "/" + escaped
	rest, err := url.PathUnescape(escaped)
	return rest, escaped, err == nil
}

// viaOrgEngineRequest is the request the engine sees: the sandbox's own path, method, headers (its
// Authorization restored) and body, with the run token and the forwarding headers gone.
func viaOrgEngineRequest(r *http.Request, rest, restEsc string, dest viaOrgDestination, st *viaOrgStream) *http.Request {
	out := r.Clone(r.Context())
	out.URL.Path, out.URL.RawPath, out.URL.Host, out.URL.Scheme = rest, restEsc, "", ""
	out.RequestURI = ""
	out.Host = dest.host
	out.Header.Del("Authorization")
	for _, v := range r.Header.Values(proxy.ViaOrgAuthorizationHeader) {
		out.Header.Add("Authorization", v)
	}
	out.Header.Del(proxy.ViaOrgAuthorizationHeader)
	out.Header.Del(proxy.ViaOrgAuthorityHeader)
	if r.Body != nil && r.Body != http.NoBody {
		out.Body = &viaOrgCountingBody{ReadCloser: r.Body, st: st}
	}
	return out
}

// denyViaOrg writes a via_org refusal: authz.denied with the relaying runner, then the reason's status.
func (s *Server) denyViaOrg(w http.ResponseWriter, r *http.Request, claims *identity.Claims, reason placement.Reason, detail string) {
	runnerID, _ := relayRunner(r.Context())
	s.auditInternalDenied(r, claims, authz.Reason(reason), "runner_id", runnerID.String())
	writeErrorReason(w, reason.Status(), string(reason), "via_org: "+detail)
}

// viaOrgEngine is the run's engine, built once from its stored config and grants, and the routes
// the run was dispatched with.
func (s *Server) viaOrgEngine(ctx context.Context, run *viaOrgRun, runID uuid.UUID) (*viaOrgGen, []proxy.ViaOrgRoute, error) {
	run.mu.Lock()
	defer run.mu.Unlock()
	if run.engine != nil {
		return run.engine, run.routes, nil
	}
	cfg, err := s.viaOrgEngineConfig(ctx, runID)
	if err != nil {
		return nil, nil, err
	}
	routes := cfg.ViaOrgRoutes
	cfg.ViaOrgRoutes = nil
	engine, err := proxy.NewViaOrgEngine(s.cfg.BaseCtx, cfg, proxy.ViaOrgEngineOptions{
		Client: &http.Client{Transport: viaOrgTransport{s: s, run: run}},
		Dial:   s.viaOrgDial, LookupIP: s.viaOrgLookupIP,
	})
	if err != nil {
		return nil, nil, err
	}
	if run.isClosed() { // ended while it was built: releaseViaOrgRun will not see it
		_ = engine.Close(ctx)
		return nil, nil, errors.New("via_org: the run's egress closed")
	}
	run.engine, run.routes = &viaOrgGen{engine: engine}, routes
	return run.engine, routes, nil
}

// viaOrgEngineConfig reduces the run's stored config to its via_org grants (design §7.3): each
// header grant's rule is the grant's own, and the dispatched route must name the same host; the git
// broker keeps only via_org grants' repositories. Nothing else that holds a credential is kept. The
// org network applies, but its internal hosts lift only for a via_org destination.
func (s *Server) viaOrgEngineConfig(ctx context.Context, runID uuid.UUID) (*proxy.Config, error) {
	raw, err := s.loadRunProxyConfig(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("read the run's stored proxy config: %w", err)
	}
	cfg, err := proxy.LoadConfigBytes(raw)
	if err != nil {
		return nil, err
	}
	grants, err := s.cfg.Store.ListGrantsByRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	viaOrg := map[uuid.UUID]types.CredentialGrant{}
	for _, g := range grants {
		if g.RunID == runID && g.Delivery == types.GrantDeliveryViaOrg {
			viaOrg[g.ID] = g
		}
	}
	var routes []proxy.ViaOrgRoute
	var hosts []string
	cfg.Injection = nil
	for _, rt := range cfg.ViaOrgRoutes {
		g, ok := viaOrg[rt.GrantID]
		switch {
		case !ok:
			continue
		case g.Spec.Kind == types.GrantAPIKey:
			rule, rerr := injectionRuleFromScope(g.Spec.Scope)
			if rerr != nil || rule.Host != rt.Host {
				continue
			}
			cfg.Injection = append(cfg.Injection, proxy.InjectionConfig{InjectionRule: rule, GrantID: g.ID})
			hosts = append(hosts, rule.Host)
		case g.Spec.Kind == types.GrantGitHubToken && rt.Host == githubHost:
			hosts = append(hosts, githubHost)
		default:
			continue
		}
		routes = append(routes, rt)
	}
	git := map[string]uuid.UUID{}
	for repo, id := range cfg.GitGrants {
		if g, ok := viaOrg[id]; ok && g.Spec.Kind == types.GrantGitHubToken {
			git[repo] = id
		}
	}
	sc, scErr := s.siteConfigForDispatch(ctx)
	if scErr != nil {
		return nil, scErr
	}
	cfg.GitGrants, cfg.ViaOrgRoutes = git, routes
	cfg.PATGrants, cfg.BrokeredPATGrantIDs, cfg.ADOGrant, cfg.AzureGates, cfg.GitHubAPI, cfg.AzureARM = nil, nil, nil, nil, nil, nil
	cfg.ResidentMints = nil // the runner answers these; the organisation holds none of their values
	cfg.MITMHosts, cfg.MITMLLM, cfg.MITMCACertPEM, cfg.MITMCAKeyPEM = slices.Compact(slices.Sorted(slices.Values(hosts))), false, "", ""
	cfg.DecisionSpoolDir, cfg.LLMUpstreams, cfg.LocalModels = "", nil, nil
	// The k8s runner's own sidecar settings: the org engine never lapses with a runner's renew,
	// streams no decision to runnerd, and is not inside the runner's cluster. No writer stores them on
	// a via_org run today; cleared so one never reaches the org side.
	cfg.SelfLapse, cfg.DecisionSink, cfg.HardDeny = false, nil, nil
	cfg.InternalHosts = viaOrgInternalHosts(proxyInternalHosts(sc.InternalHosts), hosts)
	cfg.TrustedCAPEM = s.cfg.TrustedCAPEM
	cfg.UpstreamProxyURL = s.resolveRunUpstreamProxy(ctx, runID, sc, nil)
	cfg.UpstreamProxyNoProxy = sc.UpstreamProxyNoProxy
	return cfg, nil
}

// viaOrgInternalHosts narrows the org's internal-host declarations to the via_org destinations
// themselves: an entry lifts only for a host a grant names (design §7.3), as an exact host.
func viaOrgInternalHosts(declared []types.InternalHost, hosts []string) []types.InternalHost {
	var out []types.InternalHost
	for _, h := range declared {
		suffix := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h.HostSuffix)), ".")
		for _, host := range hosts {
			if suffix != "" && (host == suffix || strings.HasSuffix(host, "."+suffix)) {
				out = append(out, types.InternalHost{HostSuffix: host, CIDRs: h.CIDRs})
			}
		}
	}
	return out
}

// viaOrgCall marks the engine's own in-process control-plane calls: refuseRelayedDelivery serves a
// via_org grant's injection and mint to them, and to nothing else.
type viaOrgCallKey struct{}

func viaOrgCall(ctx context.Context) bool { v, _ := ctx.Value(viaOrgCallKey{}).(bool); return v }

// viaOrgTransport carries the engine's control-plane calls (decisions, approvals, injection, mint)
// into this server's own router, in process: never onto a network, under the run's live token, and
// tagged with the relaying runner so every row they write carries data.relay.
type viaOrgTransport struct {
	s   *Server
	run *viaOrgRun
}

func (t viaOrgTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tok := t.run.token.Load()
	if tok == nil {
		return nil, errors.New("via_org: no run token")
	}
	// A fresh context that only follows the caller's cancellation: the engine's
	// own request context carries the outer request's routing and claims.
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer context.AfterFunc(req.Context(), cancel)()
	ctx := context.WithValue(audit.WithRelay(base, t.run.runner), relayRunnerKey{}, t.run.runner)
	in := req.Clone(context.WithValue(ctx, viaOrgCallKey{}, true))
	in.Header.Set("Authorization", "Bearer "+*tok)
	in.RequestURI = in.URL.RequestURI()
	if in.Body == nil {
		in.Body = http.NoBody
	}
	rec := &viaOrgRecorder{header: http.Header{}, code: http.StatusOK}
	t.s.router.ServeHTTP(rec, in)
	return &http.Response{
		Status: http.StatusText(rec.code), StatusCode: rec.code, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: rec.header, Body: io.NopCloser(bytes.NewReader(rec.body.Bytes())), ContentLength: int64(rec.body.Len()), Request: req,
	}, nil
}

// viaOrgRecorder is the in-process response of one engine call: control-plane answers are small.
type viaOrgRecorder struct {
	header http.Header
	code   int
	body   bytes.Buffer
	wrote  bool
}

func (r *viaOrgRecorder) Header() http.Header { return r.header }
func (r *viaOrgRecorder) WriteHeader(code int) {
	if !r.wrote {
		r.code, r.wrote = code, true
	}
}

func (r *viaOrgRecorder) Write(b []byte) (int, error) {
	r.WriteHeader(http.StatusOK)
	return r.body.Write(b)
}
