// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// via_org delivery (design 0.9 §7.3). On a runner, the proxy forwards a request
// for a via_org destination to the organisation, one HTTP request per sandbox
// request, and raises no hold and writes no decision for it. At the
// organisation, ViaOrgEngine serves that request with this same proxy's checks,
// as authoritative, and injects the credential on a TLS leg it dials itself.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ViaOrgRoute is one destination of a via_org grant on a runner's proxy: its grant
// and the host it names. No rule, header or secret name reaches the runner.
type ViaOrgRoute struct {
	GrantID uuid.UUID `json:"grant_id"`
	Host    string    `json:"host"`
}

const (
	// ViaOrgPathPrefix is the organisation's via_org route; the grant id and the
	// sandbox request's own path follow it.
	ViaOrgPathPrefix = "/api/v1/internal/via-org/"
	// ViaOrgADOGitPrefix is the sandbox-facing prefix of the Azure DevOps git route, /wardyn/git/<host>/: the
	// organisation serves it only for a via_org Azure DevOps grant, with the host read from the path.
	ViaOrgADOGitPrefix = routePATBroker
	// ViaOrgAuthorityHeader carries the sandbox request's authority (its Host)
	// to the organisation, which compares it with the grant's destination.
	ViaOrgAuthorityHeader = "X-Wardyn-Via-Org-Authority"
	// ViaOrgAuthorizationHeader carries the sandbox's own Authorization header,
	// whose slot the run token takes on the forwarded request; the organisation
	// restores it before its checks, which strip it.
	ViaOrgAuthorizationHeader = "X-Wardyn-Via-Org-Authorization"
	// ViaOrgRelayedHeader marks a forwarded request that came in on a VM run's relay listener, so the
	// organisation does not count its decisions as the run working. Only the runner's proxy sets it.
	ViaOrgRelayedHeader = "X-Wardyn-Via-Org-Relayed"
	// DecisionRelayedHeader marks a decision post whose row is Relayed (egress.Request.Relayed is not
	// serialised); the organisation trusts it only on a via_org engine's post for a VM run.
	DecisionRelayedHeader = "Wardyn-Decision-Relayed"
)

// viaOrgRouteFor is the via_org route a sandbox request is for, and the
// authority to report: a CONNECT or an absolute-form request to a route's host,
// a /wardyn/gh/ request for a repository whose grant is a via_org route, or a
// /wardyn/git/<host>/ request for a route's host that this proxy brokers no other
// way (viaOrgADOGitRoute). For an origin-form request the authority is the
// sandbox's own Host header, so the organisation reads a /wardyn/git/ host from
// the path. ok is false for everything else, which this proxy serves itself.
func (p *Proxy) viaOrgRouteFor(r *http.Request) (ViaOrgRoute, string, bool) {
	if len(p.viaOrgRoutes) == 0 {
		return ViaOrgRoute{}, "", false
	}
	if r.Method != http.MethodConnect && r.URL != nil && r.URL.Host == "" {
		if strings.HasPrefix(r.URL.Path, routePATBroker) {
			return p.viaOrgADOGitRoute(r)
		}
		orgRepo, _, ok := parseGitBrokerPath(r.URL.Path)
		id, granted := p.gitGrants[orgRepo]
		if !strings.HasPrefix(r.URL.Path, routeGitBroker) || !ok || !granted {
			return ViaOrgRoute{}, "", false
		}
		i := slices.IndexFunc(p.viaOrgRoutes, func(rt ViaOrgRoute) bool { return rt.GrantID == id })
		if i < 0 {
			return ViaOrgRoute{}, "", false
		}
		return p.viaOrgRoutes[i], r.Host, true
	}
	authority := r.Host
	if r.Method != http.MethodConnect && r.URL != nil {
		authority = r.URL.Host
	}
	host, _ := splitHostPort(authority, 443)
	route, ok := p.viaOrgRouteForHost(host)
	return route, authority, ok
}

// viaOrgADOGitRoute is the via_org route of a /wardyn/git/<host>/ request whose host is a via_org route's and that this
// proxy does not broker itself (no git_pat grant and no Azure DevOps grant for it). The organisation's engine serves
// it with the run's Azure DevOps grant (ServeBroker), or refuses it.
func (p *Proxy) viaOrgADOGitRoute(r *http.Request) (ViaOrgRoute, string, bool) {
	host, _, ok := parsePATBrokerPath(r.URL.Path)
	if !ok {
		return ViaOrgRoute{}, "", false
	}
	if _, pat := p.patGrants[host]; pat {
		return ViaOrgRoute{}, "", false
	}
	if _, ado := p.adoGitGrant(host); ado {
		return ViaOrgRoute{}, "", false
	}
	route, ok := p.viaOrgRouteForHost(host)
	return route, r.Host, ok
}

func (p *Proxy) viaOrgRouteForHost(host string) (ViaOrgRoute, bool) {
	i := slices.IndexFunc(p.viaOrgRoutes, func(rt ViaOrgRoute) bool { return hostKey(rt.Host) == hostKey(host) })
	if i < 0 {
		return ViaOrgRoute{}, false
	}
	return p.viaOrgRoutes[i], true
}

// serveViaOrg answers a sandbox request for a via_org destination. A CONNECT is
// terminated with the per-run CA (serveMITMRequest forwards each inner request);
// anything else is forwarded as it stands. No hold is raised and no decision is
// written here: the organisation owns both.
func (p *Proxy) serveViaOrg(w http.ResponseWriter, r *http.Request, route ViaOrgRoute, authority string) {
	// The forward rides the control-plane transport, past evaluate and the egress dial, so a run with
	// no network is refused here.
	if p.noNetwork {
		p.refuseNoNetwork(w, r, authority)
		return
	}
	if r.Method != http.MethodConnect {
		// The run's allowed_models judges a plain via_org request to a model host as serveMITMRequest
		// judges a terminated one: the organisation forwards it on, and nothing after this would see it.
		defaultPort := 443
		if r.URL != nil && r.URL.Scheme == "http" {
			defaultPort = 80
		}
		host, port := splitHostPort(authority, defaultPort)
		releaseModels, modelRefused := p.allowedModelRefused(w, r, host, port, strings.TrimPrefix(r.URL.Path, "/"))
		defer releaseModels()
		if modelRefused {
			return
		}
		p.forwardViaOrg(w, r, route, authority)
		return
	}
	if p.ca == nil {
		http.Error(w, "wardyn: this destination is served by your organisation, and this proxy has no TLS authority to reach it with", http.StatusBadGateway)
		return
	}
	host, port := splitHostPort(authority, 443)
	p.mitmConnect(w, r, host, port, nil, false)
}

// forwardViaOrg sends one sandbox request to the organisation's via_org route,
// bodies streamed both ways. The run token rides Authorization toward the
// control plane only, as forwardToControlPlane sends it. Bytes moved either way
// count as the run in use, as a tunnel's do.
func (p *Proxy) forwardViaOrg(w http.ResponseWriter, r *http.Request, route ViaOrgRoute, authority string) {
	if p.controlPlaneURL == "" {
		http.Error(w, "wardyn: control plane url not configured", http.StatusBadGateway)
		return
	}
	target, err := p.resolveTrustedURL(p.controlPlaneURL)
	if err != nil {
		p.httpError(w, "wardyn: your organisation could not be reached", err, http.StatusBadGateway)
		return
	}
	u := strings.TrimSuffix(p.controlPlaneURL, "/") + ViaOrgPathPrefix + route.GrantID.String() + r.URL.EscapedPath()
	if r.URL.RawQuery != "" {
		u += "?" + r.URL.RawQuery
	}
	body := r.Body
	if r.ContentLength == 0 {
		body = http.NoBody
	} else {
		body = activityReader{ReadCloser: body, moved: &p.streamMoved}
	}
	out, err := http.NewRequestWithContext(context.WithValue(r.Context(), vettedIPKey{}, target), r.Method, u, body)
	if err != nil {
		http.Error(w, "wardyn: bad request", http.StatusBadRequest)
		return
	}
	out.ContentLength = r.ContentLength
	out.Header = r.Header.Clone()
	p.stripGatewayHeaders(out.Header)
	// The organisation attaches the credential, so no sandbox routing header may ride to a gateway's
	// host; the proxy does not know which header the organisation owns, so none is kept.
	p.stripGatewayRoutingHeaders(out.Header, route.Host, "")
	removeHopByHop(out.Header)
	out.Header.Del(ViaOrgAuthorizationHeader)
	for _, v := range r.Header.Values("Authorization") {
		out.Header.Add(ViaOrgAuthorizationHeader, v)
	}
	out.Header.Set(ViaOrgAuthorityHeader, authority)
	// The guest's own copies of the marks never ride the hop; the proxy sets the relay mark itself.
	out.Header.Del(ViaOrgRelayedHeader)
	out.Header.Del(DecisionRelayedHeader)
	if p.relayLane(r.Context()) {
		out.Header.Set(ViaOrgRelayedHeader, "1")
	}
	out.Header.Set("Authorization", "Bearer "+p.runToken.Get())
	resp, err := p.controlTransport.RoundTrip(out)
	if err != nil {
		if r.Context().Err() == nil {
			p.httpError(w, "wardyn: your organisation could not be reached", err, http.StatusBadGateway)
		}
		return
	}
	defer func() { _ = resp.Body.Close() }()
	copyStreamed(w, resp, &p.streamMoved)
}

// activityReader marks moved when a Read returns bytes.
type activityReader struct {
	io.ReadCloser
	moved *atomic.Bool
}

func (a activityReader) Read(b []byte) (int, error) {
	n, err := a.ReadCloser.Read(b)
	if n > 0 {
		a.moved.Store(true)
	}
	return n, err
}

// copyStreamed writes resp to w, flushing as bytes arrive: a model response
// streams for as long as it runs, and each write counts as the run in use.
func copyStreamed(w http.ResponseWriter, resp *http.Response, moved *atomic.Bool) {
	removeHopByHop(resp.Header)
	for k, vs := range resp.Header {
		w.Header()[k] = vs
	}
	w.WriteHeader(resp.StatusCode)
	rc := http.NewResponseController(w)
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			moved.Store(true)
			_ = rc.Flush()
		}
		if err != nil {
			return
		}
	}
}

// ViaOrgEngine is the organisation's egress for one local run's via_org grants:
// a proxy built from the run's stored config, reduced to those grants' rules,
// whose control-plane calls (decisions, approvals, injection, mint) ride the
// caller's in-process client.
type ViaOrgEngine struct {
	srv         *Server
	holdCeiling time.Duration // ViaOrgEngineOptions.HoldCeiling, stamped on each request as its hold budget
	closed      atomic.Bool
}

// ViaOrgEngineOptions are an engine's dependencies. Client is the in-process
// control-plane client; Dial and LookupIP replace the network for tests only.
type ViaOrgEngineOptions struct {
	Client   *http.Client
	Dial     func(ctx context.Context, network, addr string) (net.Conn, error)
	LookupIP func(host string) ([]net.IP, error)
	// HoldCeiling bounds the total hold time of one request on the engine (first-use, push,
	// capability, re-auth and mint approvals together): the caller sets it under the relay's idle
	// cut, so a held request ends with the hold's own sentence, not a reset stream. 0 keeps the knobs.
	HoldCeiling time.Duration
}

type lookupFunc func(host string) ([]net.IP, error)

func (f lookupFunc) LookupIP(host string) ([]net.IP, error) { return f(host) }

// NewViaOrgEngine builds the engine from cfg, which the caller has reduced to the
// run's via_org rules. It starts no token renewer or activity reporter: every
// forwarded request carries the run's live token, which the client presents.
func NewViaOrgEngine(ctx context.Context, cfg *Config, opts ViaOrgEngineOptions) (*ViaOrgEngine, error) {
	// The kubectl door holds past the engine's request budget (its own 90 s hold on top of the ceiling would pass the
	// relay's idle cut), and the organisation serves it no route: a config that carries it is refused, not trimmed.
	if cfg.KubeAPI != nil {
		return nil, ErrViaOrgKubeAPI
	}
	// An Azure DevOps credential never travels without its gate: the token bounds nothing on its own.
	ado := newADOGrantsByHost(cfg.ADOGrant)
	for _, in := range cfg.Injection {
		if in.SecretName != types.ADOEntraAccessTokenSecret {
			continue
		}
		if _, ok := ado.ADOGrantFor(in.Host); !ok {
			return nil, fmt.Errorf("%w: %s", ErrViaOrgADOUngated, in.Host)
		}
	}
	h := serverHooks{dial: opts.Dial, engine: true, maskKey: uuid.New()}
	if opts.LookupIP != nil {
		h.res = lookupFunc(opts.LookupIP)
	}
	srv, err := newServer(ctx, cfg, opts.Client, io.Discard, h)
	if err != nil {
		// No engine owns the key: whatever the build registered before it
		// failed (a first grant's credential) leaves with it. newServer has
		// already closed its sink.
		procRegistry.Evict(h.maskKey)
		return nil, err
	}
	return &ViaOrgEngine{srv: srv, holdCeiling: opts.HoldCeiling}, nil
}

// ErrViaOrgKubeAPI refuses an engine config that carries a kubectl grant.
var ErrViaOrgKubeAPI = errors.New("via_org: an engine serves no kubectl grant")

// ErrViaOrgADOUngated refuses an engine config whose Azure DevOps credential has no Azure DevOps grant for its host.
var ErrViaOrgADOUngated = errors.New("via_org: an Azure DevOps credential with no Azure DevOps grant for its host")

// errViaOrgCleartext refuses a destination this engine would reach in
// cleartext: the organisation always dials TLS.
var errViaOrgCleartext = errors.New("via_org: the organisation dials TLS only")

// ServeInjected serves a forwarded request for a header-injected destination:
// the policy decision a CONNECT gets (allow and deny lists, first-use approval
// hold, IP guard), then the request path a terminated tunnel gets (method, ADO
// and Azure gates, inspection, the injection pins, the sandbox credential
// strip, the vetted TLS dial and the decision rows).
func (e *ViaOrgEngine) ServeInjected(w http.ResponseWriter, r *http.Request, host string) {
	r = withHoldBudget(trackNetworkBody(r), e.holdCeiling)
	defer interruptBodyReadOnCancel(w, r)()
	host = hostKey(host) // the grant's destination as stored: the engine's tables are keyed in this one spelling
	if e.refuseClosed(w) {
		return
	}
	p := e.srv.proxy
	if slices.ContainsFunc(slices.Collect(maps.Keys(p.mitmPlaintext)), func(k string) bool { return strings.HasPrefix(k, strings.ToLower(host)+"\x00") }) {
		p.httpError(w, "via_org destination refused", errViaOrgCleartext, http.StatusForbidden)
		return
	}
	decision, _, log := p.evaluate(context.WithValue(r.Context(), engineHeldRequest{}, true), host, 443, http.MethodConnect, "")
	switch decision {
	case egress.Deny:
		if log != nil {
			p.sink.emit(*log)
		}
		p.writeEgressDeny(w, host, 443, log, true)
		return
	case egress.Pending:
		if log != nil {
			p.sink.emit(*log)
		}
		setEgressRefusalHeadersWithReason(w, egressRefusalPending, host, decisionReason(log))
		writeApprovalPending(w, log)
		return
	}
	// The allow row, as handleConnect writes it before a terminated tunnel: it
	// carries the approval that released the destination.
	if log != nil {
		p.sink.emit(*log)
	}
	// The control plane's listener sets no read timeout, so the body gets the bound a terminated
	// tunnel's does, armed after the hold above and before allowed_models, the first gate that reads it.
	rearmBodyDeadline(w, r)
	p.serveMITMRequest(w, r, host, 443)
}

// ServeBroker serves a forwarded /wardyn/gh/ request: the git broker's repo
// allowlist, branch namespace and push-content rules, its approval hold and its
// mint, exactly as a sandbox's own proxy serves it. A /wardyn/git/<host>/ request
// is served only for a host the run's Azure DevOps grant covers, by the same
// Entra broker (org pin, capabilities, content and push rules, holds).
func (e *ViaOrgEngine) ServeBroker(w http.ResponseWriter, r *http.Request) {
	r = withHoldBudget(trackNetworkBody(r), e.holdCeiling)
	defer interruptBodyReadOnCancel(w, r)()
	if e.refuseClosed(w) {
		return
	}
	// The control plane's listener sets no read timeout, so the request body
	// gets the same bound a terminated tunnel's does.
	switch {
	case strings.HasPrefix(r.URL.Path, routeGitBroker):
		rearmBodyDeadline(w, r)
		e.srv.proxy.handleGitBroker(w, r)
	case strings.HasPrefix(r.URL.Path, routePATBroker) && e.adoGitCovered(r.URL.Path):
		rearmBodyDeadline(w, r)
		e.srv.proxy.handlePATBroker(w, r)
	default:
		http.Error(w, "via_org: not a brokered route", http.StatusForbidden)
	}
}

// adoGitCovered reports whether the engine's Azure DevOps grant covers the host a /wardyn/git/<host>/ path names.
func (e *ViaOrgEngine) adoGitCovered(path string) bool {
	host, _, ok := parsePATBrokerPath(path)
	if !ok {
		return false
	}
	_, covered := e.srv.proxy.adoGitGrant(host)
	return covered
}

// interruptBodyReadOnCancel makes the end of a forwarded request's stream (run end, release, idle: the
// control plane cancels r's context) end a body Read blocked on an upload that stopped arriving, by
// re-arming the body deadline, which is now once the context has ended. The control plane's listener
// sets no read timeout and its cancel sets only a write deadline. The returned stop fences the handler's
// return, as the control plane fences that write deadline: AfterFunc's stop does not wait for a callback
// already running, so the deadline is never set once the handler has returned.
func interruptBodyReadOnCancel(w http.ResponseWriter, r *http.Request) (stop func()) {
	var mu sync.Mutex
	returned := false
	stopAfter := context.AfterFunc(r.Context(), func() {
		mu.Lock()
		defer mu.Unlock()
		if !returned {
			rearmBodyDeadline(w, r)
		}
	})
	return func() {
		stopAfter()
		mu.Lock()
		returned = true
		mu.Unlock()
	}
}

// Close flushes the engine's decision rows and releases it.
// Its key is evicted once the sink has posted every queued row (Server.Shutdown). The caller
// closes it only after every request it admitted has returned.
func (e *ViaOrgEngine) Close(ctx context.Context) error {
	e.closed.Store(true)
	return e.srv.Shutdown(ctx)
}

// Closed reports whether Close has been called.
func (e *ViaOrgEngine) Closed() bool { return e.closed.Load() }

// refuseClosed answers a request that reached a closed engine: never served, since its decision
// sink and masking corpus are gone.
func (e *ViaOrgEngine) refuseClosed(w http.ResponseWriter) bool {
	if !e.closed.Load() {
		return false
	}
	http.Error(w, "wardyn: the organisation's egress for this run was replaced; retry", http.StatusServiceUnavailable)
	return true
}
