// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/contentscan"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Proxy is the L2 forward proxy. It serves both absolute-URI plain HTTP
// requests and CONNECT tunnels for TLS (hostname-only visibility; no
// interception in v0). All egress flows through the decision pipeline:
//
//	policy (deny>allow, default-deny) -> first-use approval (if unknown &&
//	  first_use_approval) -> method check -> IP vetting (block private/
//	  loopback/link-local/metadata) -> dial the vetted IP explicitly.
//
// Every decision emits an egress.DecisionLog (async to the control plane,
// mirrored to stdout).
type Proxy struct {
	runID  uuid.UUID
	policy *Policy
	// evaluator decides the host allow/deny/unknown verdict + method (pluggable
	// seam). Defaults to the builtin RunPolicySpec evaluator. The IP guard,
	// approval FSM, IP vetting, and injection stay hardwired in evaluate().
	evaluator egress.Evaluator
	approval  *approvalClient
	inject    *injector
	sink      *decisionSink
	res       resolver
	// scanner is the OPTIONAL outbound content-inspection engine. Nil == off
	// (the common case). It is consulted only on the brokered LLM routes.
	scanner *contentscan.Engine
	// ca is the OPTIONAL TLS-MITM certificate authority. Non-nil only when both a
	// scanner and a CA are configured; it makes opaque CONNECT tunnels to known
	// LLM hosts inspectable (subscription-OAuth path). Nil == opaque passthrough.
	ca *certAuthority
	// mitmHosts is the OPERATOR-CONFIGURED set of corp artifact hosts (exact,
	// lowercased) the proxy may TLS-MITM in addition to the built-in LLM hosts, so
	// a corporate registry token injects on the wire. Compiled at dispatch from
	// site-config; see isMITMHost's trust-boundary comment. Empty == LLM only.
	mitmHosts map[string]bool
	// mitmLLM gates TLS-MITM of the built-in LLM hosts on actual intent (subscription
	// injection or intercept_tls) — a CA minted only for artifact token injection
	// must NOT make Anthropic/OpenAI MITM-eligible. See isMITMHost / handleConnect.
	mitmLLM bool
	// blindHosts deduplicates the one-time llm.scan.blind signal emitted when an
	// inspection-enabled run tunnels to an LLM host over opaque CONNECT (no MITM
	// yet), so coverage is reported honestly without flooding the audit log.
	blindMu    sync.Mutex
	blindHosts map[string]struct{}

	// controlPlaneURL is the base URL of wardynd, used ONLY by the local
	// brokered routes (/wardyn/v1/...) to forward to the internal API.
	controlPlaneURL string
	// runToken is the per-run token that authenticates internal calls. It is
	// held ONLY here in proxy memory and injected toward controlPlaneURL on the
	// local mint/approvals routes; it is NEVER exposed to the sandbox and NEVER
	// injected toward any LLM upstream.
	runToken *tokenSource
	// localClient forwards local-route requests to the control plane. It uses
	// the proxy's pinned, IP-vetted transport so the control-plane host is
	// resolved+vetted once and dialed explicitly (no transport re-resolution).
	localClient *http.Client

	// dial connects to a vetted "ip:port" target. Both the plain-HTTP
	// transport and the CONNECT path use this single seam; tests override it.
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// transport forwards plain-HTTP requests AND the MITM LLM path. Its
	// DialContext is pinned to the vetted IP via the request context (no
	// transport-side re-resolution) UNLESS an upstream corp proxy is configured,
	// in which case it chains every forward-egress dial through that proxy.
	transport *http.Transport
	// controlTransport backs localClient (control-plane forwards). It ALWAYS
	// dials the vetted IP directly and NEVER chains through the upstream corp
	// proxy — the split that keeps the run token off the corp-proxy wire.
	controlTransport *http.Transport
	// upstream is the OPTIONAL corporate parent proxy. Nil == direct dial (the
	// common, backward-compatible case). When set, forward egress is issued as
	// CONNECT <real-host> to it; see upstream.go and dialThroughUpstream.
	upstream *upstreamProxy

	now func() time.Time
}

// Options configures a Proxy. Nil fields fall back to production defaults.
type Options struct {
	RunID  uuid.UUID
	Policy *Policy
	// Evaluator overrides the pluggable host/method policy-verdict engine. Nil =>
	// the builtin evaluator wrapping Policy (default; unchanged behavior).
	Evaluator egress.Evaluator
	Approval  *approvalClient
	Injector  *injector
	Sink      *decisionSink
	Resolver  resolver
	// Scanner is the optional outbound content-inspection engine (nil == off).
	Scanner *contentscan.Engine
	// CA is the optional TLS-MITM certificate authority (nil == opaque CONNECT).
	CA *certAuthority
	// MITMHosts are operator-configured corp artifact hosts eligible for TLS-MITM
	// beyond the built-in LLM hosts (exact hostnames). See Proxy.mitmHosts.
	MITMHosts []string
	// MITMLLM gates TLS-MITM of the built-in LLM hosts on actual intent. See
	// Proxy.mitmLLM.
	MITMLLM bool
	// ControlPlaneURL and RunToken back the local brokered routes. The run
	// token is injected only toward the control plane and never reaches the
	// sandbox or any LLM upstream.
	ControlPlaneURL string
	RunToken        *tokenSource
	// Upstream is the OPTIONAL corporate parent proxy (parsed form). Nil ==
	// direct dial. NewServer parses it from Config.UpstreamProxyURL; tests may
	// build one via parseUpstreamProxy.
	Upstream *upstreamProxy
	// Dial overrides the connection dialer (tests). Production leaves it nil
	// and a net.Dialer is used.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// TLSClientConfig overrides the forwarding transport's TLS config. Tests
	// use this to trust an httptest TLS server standing in for an HTTPS LLM
	// upstream; production leaves it nil (system roots, ServerName from URL).
	TLSClientConfig *tls.Config
	Now             func() time.Time
}

// vettedIPKey carries the pre-resolved, policy-checked dial target through the
// request context so the transport dials it directly instead of re-resolving
// the hostname (TOCTOU / DNS-rebinding guard).
type vettedIPKey struct{}

func newProxy(opts Options) *Proxy {
	dial := opts.Dial
	if dial == nil {
		nd := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
		dial = nd.DialContext
	}
	res := opts.Resolver
	if res == nil {
		res = netResolver{}
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	evaluator := opts.Evaluator
	if evaluator == nil {
		evaluator = builtinEvaluator{p: opts.Policy} // default: builtin RunPolicySpec verdict
	}
	mitmHosts := make(map[string]bool, len(opts.MITMHosts))
	for _, h := range opts.MITMHosts {
		if h = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), "."); h != "" {
			mitmHosts[h] = true
		}
	}
	p := &Proxy{
		runID:           opts.RunID,
		policy:          opts.Policy,
		evaluator:       evaluator,
		approval:        opts.Approval,
		inject:          opts.Injector,
		sink:            opts.Sink,
		res:             res,
		scanner:         opts.Scanner,
		ca:              opts.CA,
		mitmHosts:       mitmHosts,
		mitmLLM:         opts.MITMLLM,
		controlPlaneURL: strings.TrimRight(opts.ControlPlaneURL, "/"),
		runToken:        opts.RunToken,
		upstream:        opts.Upstream,
		dial:            dial,
		now:             now,
	}

	// directDial dials the vetted IP carried on the request context and never
	// re-resolves — the ORIGINAL behavior, and the ONLY behavior of the
	// control-plane transport (which must never traverse the corp proxy).
	directDial := func(ctx context.Context, network, _ string) (net.Conn, error) {
		target, ok := ctx.Value(vettedIPKey{}).(string)
		if !ok || target == "" {
			return nil, errors.New("proxy: missing vetted dial target")
		}
		return p.dial(ctx, network, target)
	}
	// egressDial is directDial UNLESS an upstream corp proxy is configured, in
	// which case EVERY forward-egress dial is chained through it via CONNECT. In
	// upstream mode the context value carries the REAL host:port (a hostname, set
	// by evaluate() / serveMITMRequest) because the corp proxy — not us —
	// resolves and dials it.
	egressDial := directDial
	if p.upstream != nil {
		egressDial = func(ctx context.Context, network, _ string) (net.Conn, error) {
			target, ok := ctx.Value(vettedIPKey{}).(string)
			if !ok || target == "" {
				return nil, errors.New("proxy: missing dial target")
			}
			host, port := splitHostPort(target, 443)
			return p.dialThroughUpstream(ctx, host, port)
		}
	}
	mkTransport := func(dc func(context.Context, string, string) (net.Conn, error)) *http.Transport {
		return &http.Transport{
			DialContext:           dc,
			ForceAttemptHTTP2:     false,
			MaxIdleConns:          64,
			IdleConnTimeout:       60 * time.Second,
			TLSHandshakeTimeout:   15 * time.Second,
			ExpectContinueTimeout: time.Second,
			Proxy:                 nil,
			TLSClientConfig:       opts.TLSClientConfig,
		}
	}
	p.transport = mkTransport(egressDial)
	p.controlTransport = mkTransport(directDial)
	// localClient uses the CONTROL transport: local-route forwards to the control
	// plane carry the vetted dial target on the request context so the host is
	// never re-resolved (same TOCTOU guard), and they NEVER chain through the
	// upstream corp proxy — the run token stays off the corp-proxy wire.
	p.localClient = &http.Client{Transport: p.controlTransport}

	// Audit the deliberate private-IP-guard relaxation for the operator-
	// configured upstream proxy hop (one record per run; each real destination
	// still gets its own per-request decision).
	if p.upstream != nil && p.sink != nil {
		req := egress.Request{
			RunID:  p.runID,
			Host:   p.upstream.host,
			Port:   p.upstream.port,
			Method: http.MethodConnect,
			Time:   now(),
		}
		p.sink.emit(decisionLog(req, egress.Allow, "builtin:upstream-proxy"))
	}
	return p
}

// ServeHTTP routes between CONNECT tunneling, brokered LOCAL routes, and
// plain-HTTP forwarding.
//
// SECURITY: the local brokered routes (/wardyn/...) are reachable ONLY via
// origin-form requests addressed to the proxy listener itself — i.e. the
// request-target is path-only (r.URL.Host == ""). An absolute-URI forward
// request for http://wardyn-proxy:3128/wardyn/v1/... carries a non-empty
// r.URL.Host and is handled by the normal forward-proxy path, where the proxy
// host is not allowlisted and is denied by policy. CONNECT is never a local
// route. This makes the run token unreachable by anything the sandbox can
// route through the forward proxy.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.handleConnect(w, r)
		return
	}
	// Origin-form (path-only) request to the proxy itself: candidate local
	// route. Absolute-URI requests (URL.Host set) always go to the forward
	// proxy path, even when their path begins with /wardyn.
	if r.URL != nil && r.URL.Host == "" && strings.HasPrefix(r.URL.Path, localRoutePrefix) {
		p.handleLocalRoute(w, r)
		return
	}
	p.handlePlain(w, r)
}

// splitHostPort returns the host (lowercased, no port) and port, defaulting
// the port to 80 for plain HTTP / 443 for CONNECT when absent.
func splitHostPort(hostport string, defaultPort int) (host string, port int) {
	hostport = strings.TrimSuffix(hostport, ".")
	if h, ps, err := net.SplitHostPort(hostport); err == nil {
		port, _ = strconv.Atoi(ps)
		return strings.ToLower(h), port
	}
	return strings.ToLower(hostport), defaultPort
}

// evaluate runs the full decision pipeline for a request, emitting the
// decision log, and returns the decision plus the vetted dial target
// ("ip:port") when allowed.
func (p *Proxy) evaluate(ctx context.Context, host string, port int, method string, path string) (egress.Decision, string, *egress.DecisionLog) {
	req := egress.Request{
		RunID:  p.runID,
		Host:   host,
		Port:   port,
		Method: strings.ToUpper(method),
		Path:   path,
		Time:   p.now(),
	}

	// 0. Unconditional literal-IP guard: a blocked address (private/loopback/
	// link-local/metadata) is denied BEFORE policy and BEFORE first-use
	// approval — an approval must never even be raisable for these ranges
	// (invariant 3: blocked regardless of policy). Hostnames that RESOLVE to
	// blocked ranges are caught by VetHost at step 4 after policy/approval.
	if ip := net.ParseIP(strings.TrimSuffix(strings.ToLower(host), ".")); ip != nil {
		if blocked, _ := isBlockedIP(ip); blocked {
			log := decisionLog(req, egress.Deny, "builtin:private-ip")
			return egress.Deny, "", &log
		}
	}

	// 1. Policy host verdict (deny beats allow, default-deny) — delegated to the
	// pluggable Evaluator (default: builtin RunPolicySpec). An evaluator error
	// fails closed. The IP guard (step 0), first-use approval (step 2), and IP
	// vetting (step 4) stay HARDWIRED around this verdict regardless of engine.
	verdict, verr := p.evaluator.EvaluateHost(ctx, req)
	if verr != nil {
		log := decisionLog(req, egress.Deny, "policy:evaluator-error")
		return egress.Deny, "", &log
	}
	switch verdict {
	case egress.VerdictDeny:
		log := decisionLog(req, egress.Deny, "policy:denied")
		return egress.Deny, "", &log
	case egress.VerdictUnknown:
		// 2. First-use approval (only for the review modes). always_deny falls to
		// the else (hard deny). deny_with_review raises + fails fast (Resolve).
		// wait_for_review HOLDS the connection until decided or the hold deadline
		// (ResolveWait) — transparent to the sandbox if approved in time.
		mode := p.policy.FirstUseMode()
		if mode.RaisesApproval() && p.approval != nil {
			var r resolveResult
			if mode == types.FirstUseWaitForReview {
				r = p.approval.ResolveWait(ctx, host)
			} else {
				r = p.approval.Resolve(ctx, host)
			}
			switch r.State {
			case apApproved:
				// fall through to method + IP vetting below.
			case apDenied:
				log := decisionLog(req, egress.Deny, "approval:denied")
				return egress.Deny, "", &log
			default: // apPending / apNone
				log := decisionLog(req, egress.Pending, "approval:pending")
				if r.ApprovalID != uuid.Nil {
					id := r.ApprovalID
					log.ApprovalID = &id
				}
				return egress.Pending, "", &log
			}
		} else {
			log := decisionLog(req, egress.Deny, "policy:default-deny")
			return egress.Deny, "", &log
		}
	case egress.VerdictAllow:
		// fall through.
	}

	// 3. Method restriction (CONNECT counts as method "CONNECT").
	if !p.evaluator.MethodAllowed(req.Method) {
		log := decisionLog(req, egress.Deny, "policy:method")
		return egress.Deny, "", &log
	}

	// 4. IP vetting (unconditional private/loopback/link-local/metadata deny).
	// When an upstream corp proxy is configured we do NOT resolve+pin the real
	// host ourselves: the corp proxy performs the outbound DNS+dial, and the
	// sandbox host frequently CANNOT resolve external names at all. The vetted-IP
	// TOCTOU guard is therefore deliberately relaxed for the upstream hop — the
	// literal-IP guard at step 0 still denies an agent naming a private/metadata
	// IP directly (so SSRF-via-corp-proxy to loopback/metadata stays blocked),
	// and policy/approval/method above are unchanged. Every DIRECT-dial path
	// keeps the full VetHost guard. The target carries the HOSTNAME (not an IP)
	// so egressDial issues CONNECT <real-host> to the corp proxy.
	if p.upstream != nil {
		target := net.JoinHostPort(host, strconv.Itoa(port))
		log := decisionLog(req, egress.Allow, "policy:allowed")
		return egress.Allow, target, &log
	}
	guard := VetHost(host, p.res)
	if guard.Denied {
		log := decisionLog(req, egress.Deny, "builtin:private-ip")
		return egress.Deny, "", &log
	}

	target := net.JoinHostPort(guard.IP.String(), strconv.Itoa(port))
	log := decisionLog(req, egress.Allow, "policy:allowed")
	return egress.Allow, target, &log
}

// handlePlain forwards an absolute-URI plain HTTP request.
func (p *Proxy) handlePlain(w http.ResponseWriter, r *http.Request) {
	// A forward-proxy request carries an absolute URI; the host lives in the
	// URL, not just the Host header.
	if r.URL == nil || r.URL.Host == "" {
		http.Error(w, "proxy requires absolute-form request URI", http.StatusBadRequest)
		return
	}
	host, port := splitHostPort(r.URL.Host, 80)

	decision, target, log := p.evaluate(r.Context(), host, port, r.Method, r.URL.Path)
	switch decision {
	case egress.Deny:
		if log != nil {
			p.sink.emit(*log)
		}
		http.Error(w, "egress denied by policy", http.StatusForbidden)
		return
	case egress.Pending:
		if log != nil {
			p.sink.emit(*log)
		}
		writeApprovalPending(w, log)
		return
	}

	// OPTIONAL generic content inspection of a custom (non-LLM) HTTP connector's
	// body — the walled-garden extension, opt-in via inspect_forward_egress. When
	// disabled (the default) or for bodiless methods this is a no-op and the path
	// below is byte-for-byte the original streaming forward. A confident block
	// writes the 403 itself (before the allow decision is emitted).
	var bodyOverride io.Reader
	if p.scanner != nil && p.scanner.InspectForwardEgress() && p.scanner.Mode() != contentscan.ModeOff && hasScannableBody(r) {
		buffered, summary, blocked := p.inspectForwardBody(w, r, host, port)
		if blocked {
			return
		}
		bodyOverride = buffered
		if summary != nil && log != nil {
			log.Scan = summary
		}
	}
	outReq := r.Clone(context.WithValue(r.Context(), vettedIPKey{}, target))
	// RequestURI must be empty for client requests.
	outReq.RequestURI = ""
	if bodyOverride != nil {
		// Forward the buffered (re-readable) body; bytes are unchanged so the
		// cloned ContentLength still matches.
		outReq.Body = io.NopCloser(bodyOverride)
	}
	// Strip hop-by-hop headers before forwarding.
	removeHopByHop(outReq.Header)

	// 5. Credential injection (plain HTTP only, exact-allow host only).
	p.inject.apply(outReq, host)

	// 6. Forward to the vetted target over the pinned transport. Its DialContext
	// dials the vetted ip:port carried on the request context (vettedIPKey), so the
	// host is never re-resolved. Invoked only post-allow+vet.
	resp, err := p.transport.RoundTrip(outReq)
	if err != nil {
		// The allow decision is emitted only AFTER a successful round-trip (same
		// accuracy fix as handleConnect, E3): a failed upstream dial must NOT
		// over-report an allow. Emit a dial-failed deny (carrying any scan
		// summary) instead.
		if log != nil {
			dl := decisionLog(log.Request, egress.Deny, "builtin:dial-failed")
			dl.Scan = log.Scan
			p.sink.emit(dl)
		}
		http.Error(w, "upstream error: "+err.Error(), http.StatusBadGateway)
		return
	}
	if log != nil {
		p.sink.emit(*log)
	}
	defer func() { _ = resp.Body.Close() }()

	dst := w.Header()
	copyHeader(dst, resp.Header)
	removeHopByHop(dst)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// handleConnect establishes a raw TCP tunnel for CONNECT (TLS passthrough).
// Credentials CANNOT be injected into a CONNECT tunnel: the proxy has
// hostname-only visibility and never sees the encrypted request headers.
func (p *Proxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	host, port := splitHostPort(r.Host, 443)
	if host == "" {
		// Fall back to the request URI authority (some clients put it there).
		host, port = splitHostPort(r.RequestURI, 443)
	}

	decision, target, log := p.evaluate(r.Context(), host, port, http.MethodConnect, "")
	// Deny/Pending are emitted now. The ALLOW decision is deferred until the
	// tunnel is actually established (E3): emitting it before the dial would
	// over-report an allow when the dial then fails.
	switch decision {
	case egress.Deny:
		if log != nil {
			p.sink.emit(*log)
		}
		http.Error(w, "egress denied by policy", http.StatusForbidden)
		return
	case egress.Pending:
		if log != nil {
			p.sink.emit(*log)
		}
		writeApprovalPending(w, log)
		return
	}

	// Operator-configured corp artifact host: terminate TLS so the registry token
	// injects on the wire (config-only redirects never reach here — dispatch adds a
	// host to mitmHosts ONLY when it also authored a paired injection). Checked
	// BEFORE the LLM branch so it stays clear of the LLM-specific blind-coverage
	// bookkeeping; these hosts are not model APIs and are never content-scanned.
	if p.ca != nil && p.isCorpMITMHost(host) {
		if log != nil {
			p.sink.emit(*log)
		}
		p.mitmConnect(w, r, host)
		return
	}

	if isLLMHost(host) {
		// TLS-MITM-eligible host (Anthropic/OpenAI). Terminate TLS only when MITM of
		// the LLM hosts is actually INTENDED for this run (subscription credential
		// injection or intercept_tls content inspection) — p.mitmLLM. The per-run CA
		// may also be minted purely for artifact-token injection (MITMHosts), so a
		// bare "CA present" no longer implies LLM MITM was wanted; without this gate
		// an artifact-only run would TLS-terminate a direct CONNECT to Anthropic/OpenAI
		// it never asked to intercept. serveMITMRequest does inspection AND/OR OAuth
		// injection (inspectLLM no-ops with a nil scanner).
		if p.mitmLLMHost(host) {
			// The MITM path establishes its own TLS-terminated tunnel and emits
			// per-request decisions inside; record the CONNECT allow here.
			if log != nil {
				p.sink.emit(*log)
			}
			p.mitmConnect(w, r, host)
			return
		}
		// Opaque tunnel (no CA, or Bedrock/SigV4). Record a one-time llm.scan.blind
		// ONLY when inspection was expected, so audit never implies coverage we
		// don't have (an injection-only run with no scanner is not "blind").
		if p.scanner != nil && p.scanner.Mode() != contentscan.ModeOff {
			p.emitLLMBlindOnce(host)
		}
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking unsupported", http.StatusInternalServerError)
		return
	}

	// Dial the destination: through the corp proxy (CONNECT <real-host>) when an
	// upstream is configured, else directly to the vetted IP (no re-resolution).
	var (
		upstream net.Conn
		err      error
	)
	if p.upstream != nil {
		upstream, err = p.dialThroughUpstream(r.Context(), host, port)
	} else {
		upstream, err = p.dial(r.Context(), "tcp", target)
	}
	if err != nil {
		// Dial failed: report a DENY (not the earlier-computed allow) so a failed
		// tunnel is never logged as allowed egress (E3).
		if log != nil {
			dl := decisionLog(log.Request, egress.Deny, "builtin:dial-failed")
			p.sink.emit(dl)
		}
		http.Error(w, "upstream dial failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	// Tunnel dial succeeded: NOW record the allow.
	if log != nil {
		p.sink.emit(*log)
	}

	clientConn, _, err := hj.Hijack()
	if err != nil {
		_ = upstream.Close()
		return
	}
	if _, err := clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		_ = upstream.Close()
		_ = clientConn.Close()
		return
	}
	tunnel(clientConn, upstream)
}

// tunnel pipes bytes in both directions until either side closes, then closes
// both connections.
func tunnel(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		// Half-close the write side if supported so the peer sees EOF.
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}
	go cp(a, b)
	go cp(b, a)
	wg.Wait()
	_ = a.Close()
	_ = b.Close()
}

// writeApprovalPending returns the first-use 403 body the sandbox client sees.
func writeApprovalPending(w http.ResponseWriter, log *egress.DecisionLog) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	id := ""
	if log != nil && log.ApprovalID != nil {
		id = log.ApprovalID.String()
	}
	// {"wardyn":"approval_pending","approval_id":...}
	_, _ = fmt.Fprintf(w, `{"wardyn":"approval_pending","approval_id":%q}`, id)
}

// hopByHopHeaders are stripped before forwarding (RFC 7230 §6.1).
var hopByHopHeaders = []string{
	"Connection",
	"Proxy-Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

func removeHopByHop(h http.Header) {
	// Headers named in Connection are also hop-by-hop.
	for _, name := range h.Values("Connection") {
		for _, tok := range strings.Split(name, ",") {
			if t := strings.TrimSpace(tok); t != "" {
				h.Del(t)
			}
		}
	}
	for _, hh := range hopByHopHeaders {
		h.Del(hh)
	}
}

func copyHeader(dst, src http.Header) {
	for k, vs := range src {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// Server bundles a Proxy with its http.Server and async decision sink for
// lifecycle management.
type Server struct {
	proxy *Proxy
	http  *http.Server
	sink  *decisionSink
	// renewStop stops the run-token renewer started by NewServer; renewStopped
	// closes once it has exited. Both are set ONCE in NewServer (never from
	// ListenAndServe) because production runs ListenAndServe in one goroutine and
	// calls Shutdown from another — writing them at serve time would race the read
	// in Shutdown. Nil when no renewer was started.
	renewStop    context.CancelFunc
	renewStopped chan struct{}
}

// NewServer wires a Proxy from a validated Config and dependencies. It mints
// injection credentials once (fail-closed on error) before returning.
func NewServer(ctx context.Context, cfg *Config, client *http.Client, stdout io.Writer) (*Server, error) {
	pol := CompilePolicy(cfg.Policy)

	// ONE live token for the whole sidecar: the config's run token is the seed,
	// and the renewer (started by ListenAndServe) rotates it in place before its
	// short TTL lapses. Every control-plane caller below shares this source, so a
	// renew reaches all of them at once — the sink, the injector's subscription
	// re-resolves, the approval client, and the brokered local routes.
	ts := newTokenSource(cfg.RunToken)

	sink := newDecisionSink(cfg.ControlPlaneURL, ts, cfg.DecisionBufferSize, client, stdout)

	inj, err := buildInjector(ctx, cfg.ControlPlaneURL, ts, pol, cfg.Injection, client)
	if err != nil {
		_ = sink.close(context.Background())
		return nil, fmt.Errorf("build injector: %w", err)
	}

	// Build the OPTIONAL outbound content-inspection engine. Off unless the
	// policy carries an llm_inspection block. Register the operator-declared
	// workspace secret values in the proxy-global mask registry FIRST so they
	// (a) form the scan corpus and (b) are masked from decision-log output
	// defense-in-depth — then snapshot (buildInjector already registered any
	// injected credentials above). A global kill-switch (WARDYN_LLM_SCAN=off) is
	// applied upstream in cmd/wardyn-proxy by clearing cfg.Policy.LLMInspection.
	var scanner *contentscan.Engine
	if spec := cfg.Policy.LLMInspection; spec != nil {
		for _, v := range spec.WorkspaceSecretValues {
			procRegistry.AddGlobal([]byte(v))
		}
		eng, eerr := contentscan.NewEngine(*spec, procRegistry.Snapshot(uuid.Nil))
		if eerr != nil {
			_ = sink.close(context.Background())
			return nil, fmt.Errorf("build content scanner: %w", eerr)
		}
		scanner = eng
		if eng == nil && spec.Mode != "" && spec.Mode != "off" {
			// Configured to inspect but the effective secret corpus is empty —
			// scanning is a no-op. Surface it so the operator is not misled.
			log.Printf("wardyn-proxy: llm_inspection mode=%q but no effective secret corpus — scanning disabled", spec.Mode)
		}
	}

	// TLS-MITM CA: build whenever the per-run PEMs are provided. MITM now serves
	// TWO purposes — content inspection (scanner) AND subscription credential
	// injection (which must terminate TLS to swap the Authorization header for the
	// live host token). Dispatch only delivers the PEMs when one of those is
	// wanted, so their presence is the authoritative signal. With a nil scanner
	// the terminated tunnel is forward+inject only (inspectLLM no-ops).
	var ca *certAuthority
	if cfg.MITMCACertPEM != "" && cfg.MITMCAKeyPEM != "" {
		ca, err = newCertAuthority([]byte(cfg.MITMCACertPEM), []byte(cfg.MITMCAKeyPEM))
		if err != nil {
			_ = sink.close(context.Background())
			return nil, fmt.Errorf("build mitm CA: %w", err)
		}
		log.Printf("wardyn-proxy: TLS-MITM enabled (content inspection and/or subscription credential injection)")
	}

	// Upstream/parent corp proxy (optional). Parse+validate; register any
	// embedded credential in the mask registry so it can never leak into a
	// decision log or stdout. The credential is held proxy-memory-only.
	up, err := parseUpstreamProxy(cfg.UpstreamProxyURL)
	if err != nil {
		_ = sink.close(context.Background())
		return nil, fmt.Errorf("upstream proxy: %w", err)
	}
	if up != nil {
		for _, v := range up.maskValues() {
			procRegistry.AddGlobal(v)
		}
		log.Printf("wardyn-proxy: chaining egress through upstream proxy %s (private-IP guard relaxed for this hop; control-plane bypasses it)", up.addr)
	}

	ap := newApprovalClient(cfg.ControlPlaneURL, ts, cfg.RunID, client)
	ap.configureHold(cfg.Policy.FirstUseApproval.Normalize(),
		time.Duration(cfg.HoldForReviewTimeoutSec)*time.Second, cfg.MaxConcurrentHolds)

	p := newProxy(Options{
		RunID:           cfg.RunID,
		Policy:          pol,
		Approval:        ap,
		Injector:        inj,
		Sink:            sink,
		Scanner:         scanner,
		CA:              ca,
		MITMHosts:       cfg.MITMHosts,
		MITMLLM:         cfg.MITMLLM,
		ControlPlaneURL: cfg.ControlPlaneURL,
		RunToken:        ts,
		Upstream:        up,
	})
	if ca != nil && len(cfg.MITMHosts) > 0 {
		log.Printf("wardyn-proxy: TLS-MITM also enabled for %d operator-configured corp artifact host(s) (token injection)", len(cfg.MITMHosts))
	}

	srv := &http.Server{
		Addr:         cfg.Listen,
		Handler:      p,
		ReadTimeout:  0, // streaming/tunnels: no whole-request deadline
		WriteTimeout: 0,
		IdleTimeout:  90 * time.Second,
	}
	out := &Server{proxy: p, http: srv, sink: sink}

	// Start the run-token renewer, which keeps this sidecar's short-TTL token
	// fresh for the life of the run. Without it every control-plane call (mints,
	// approvals, decision logs, subscription re-resolves) starts 401ing once the
	// startup token's 1h TTL lapses, with no recovery. It starts here — like the
	// decision sink's own goroutine — so it is running before the first request
	// and is torn down by Shutdown; the caller's ctx is a STARTUP context, so the
	// renewer gets its own lifetime instead.
	if cfg.ControlPlaneURL != "" {
		rctx, cancel := context.WithCancel(context.Background())
		out.renewStop = cancel
		out.renewStopped = make(chan struct{})
		go func() {
			defer close(out.renewStopped)
			runTokenRenewer(rctx, ts, cfg.ControlPlaneURL, client)
		}()
	}
	return out, nil
}

// ListenAndServe starts serving and blocks until the server stops.
func (s *Server) ListenAndServe() error {
	if err := s.http.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Shutdown gracefully stops the HTTP server, stops the token renewer, and drains
// the decision sink.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.renewStop != nil {
		s.renewStop()
		<-s.renewStopped
	}
	httpErr := s.http.Shutdown(ctx)
	sinkErr := s.sink.close(ctx)
	if httpErr != nil {
		return httpErr
	}
	return sinkErr
}

// Addr returns the configured listen address.
func (s *Server) Addr() string { return s.http.Addr }
