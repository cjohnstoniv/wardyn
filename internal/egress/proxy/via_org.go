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
	"io"
	"maps"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
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
	// ViaOrgAuthorityHeader carries the sandbox request's authority (its Host)
	// to the organisation, which compares it with the grant's destination.
	ViaOrgAuthorityHeader = "X-Wardyn-Via-Org-Authority"
	// ViaOrgAuthorizationHeader carries the sandbox's own Authorization header,
	// whose slot the run token takes on the forwarded request; the organisation
	// restores it before its checks, which strip it.
	ViaOrgAuthorizationHeader = "X-Wardyn-Via-Org-Authorization"
)

// viaOrgRouteFor is the via_org route a sandbox request is for, and the
// authority to report: a CONNECT or an absolute-form request to a route's host,
// or a /wardyn/gh/ request for a repository whose grant is a via_org route.
// ok is false for everything else, which this proxy serves itself.
func (p *Proxy) viaOrgRouteFor(r *http.Request) (ViaOrgRoute, string, bool) {
	if len(p.viaOrgRoutes) == 0 {
		return ViaOrgRoute{}, "", false
	}
	if r.Method != http.MethodConnect && r.URL != nil && r.URL.Host == "" {
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

func (p *Proxy) viaOrgRouteForHost(host string) (ViaOrgRoute, bool) {
	i := slices.IndexFunc(p.viaOrgRoutes, func(rt ViaOrgRoute) bool { return strings.EqualFold(rt.Host, host) })
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
	if r.Method != http.MethodConnect {
		p.forwardViaOrg(w, r, route, authority)
		return
	}
	if p.ca == nil {
		http.Error(w, "wardyn: this destination is served by your organisation, and this proxy has no TLS authority to reach it with", http.StatusBadGateway)
		return
	}
	host, port := splitHostPort(authority, 443)
	p.mitmConnect(w, r, host, port)
}

// forwardViaOrg sends one sandbox request to the organisation's via_org route,
// bodies streamed both ways. The run token rides Authorization toward the
// control plane only, as forwardToControlPlane sends it.
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
	}
	out, err := http.NewRequestWithContext(context.WithValue(r.Context(), vettedIPKey{}, target), r.Method, u, body)
	if err != nil {
		http.Error(w, "wardyn: bad request", http.StatusBadRequest)
		return
	}
	out.ContentLength = r.ContentLength
	out.Header = r.Header.Clone()
	removeHopByHop(out.Header)
	out.Header.Del(ViaOrgAuthorizationHeader)
	for _, v := range r.Header.Values("Authorization") {
		out.Header.Add(ViaOrgAuthorizationHeader, v)
	}
	out.Header.Set(ViaOrgAuthorityHeader, authority)
	out.Header.Set("Authorization", "Bearer "+p.runToken.Get())
	resp, err := p.controlTransport.RoundTrip(out)
	if err != nil {
		if r.Context().Err() == nil {
			p.httpError(w, "wardyn: your organisation could not be reached", err, http.StatusBadGateway)
		}
		return
	}
	defer func() { _ = resp.Body.Close() }()
	copyStreamed(w, resp)
}

// copyStreamed writes resp to w, flushing as bytes arrive: a model response
// streams for as long as it runs.
func copyStreamed(w http.ResponseWriter, resp *http.Response) {
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
	srv    *Server
	closed atomic.Bool
}

// ViaOrgEngineOptions are an engine's dependencies. Client is the in-process
// control-plane client; Dial and LookupIP replace the network for tests only.
type ViaOrgEngineOptions struct {
	Client   *http.Client
	Dial     func(ctx context.Context, network, addr string) (net.Conn, error)
	LookupIP func(host string) ([]net.IP, error)
}

type lookupFunc func(host string) ([]net.IP, error)

func (f lookupFunc) LookupIP(host string) ([]net.IP, error) { return f(host) }

// NewViaOrgEngine builds the engine from cfg, which the caller has reduced to the
// run's via_org rules. It starts no token renewer or activity reporter: every
// forwarded request carries the run's live token, which the client presents.
func NewViaOrgEngine(ctx context.Context, cfg *Config, opts ViaOrgEngineOptions) (*ViaOrgEngine, error) {
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
	return &ViaOrgEngine{srv: srv}, nil
}

// errViaOrgCleartext refuses a destination this engine would reach in
// cleartext: the organisation always dials TLS.
var errViaOrgCleartext = errors.New("via_org: the organisation dials TLS only")

// ServeInjected serves a forwarded request for a header-injected destination:
// the policy decision a CONNECT gets (allow and deny lists, first-use approval
// hold, IP guard), then the request path a terminated tunnel gets (method, ADO
// and Azure gates, inspection, the injection pins, the sandbox credential
// strip, the vetted TLS dial and the decision rows).
func (e *ViaOrgEngine) ServeInjected(w http.ResponseWriter, r *http.Request, host string) {
	if e.refuseClosed(w) {
		return
	}
	p := e.srv.proxy
	if slices.ContainsFunc(slices.Collect(maps.Keys(p.mitmPlaintext)), func(k string) bool { return strings.HasPrefix(k, strings.ToLower(host)+"\x00") }) {
		p.httpError(w, "via_org destination refused", errViaOrgCleartext, http.StatusForbidden)
		return
	}
	decision, _, log := p.evaluate(r.Context(), host, 443, http.MethodConnect, "")
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
		setEgressRefusalHeaders(w, egressRefusalPending, host)
		writeApprovalPending(w, log)
		return
	}
	// The allow row, as handleConnect writes it before a terminated tunnel: it
	// carries the approval that released the destination.
	if log != nil {
		p.sink.emit(*log)
	}
	p.serveMITMRequest(w, r, host, 443)
}

// ServeBroker serves a forwarded /wardyn/gh/ request: the git broker's repo
// allowlist, branch namespace and push-content rules, its approval hold and its
// mint, exactly as a sandbox's own proxy serves it.
func (e *ViaOrgEngine) ServeBroker(w http.ResponseWriter, r *http.Request) {
	if e.refuseClosed(w) {
		return
	}
	if !strings.HasPrefix(r.URL.Path, routeGitBroker) {
		http.Error(w, "via_org: not a brokered route", http.StatusForbidden)
		return
	}
	// The control plane's listener sets no read timeout, so the request body
	// gets the same bound a terminated tunnel's does.
	rearmBodyDeadline(w)
	e.srv.proxy.handleGitBroker(w, r)
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
