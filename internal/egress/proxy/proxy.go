// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
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
	// mitmPorts pairs each mitmHosts entry (same lowercased host key) with the
	// CONNECT port that entry is scoped to, parsed from an Options.MITMHosts
	// entry's optional ":port" suffix. 0 means the entry carried no port (the
	// historical bare-host format every existing caller sends — Bedrock's
	// runtimeHost, an un-migrated redirect) and stays eligible on ANY port,
	// unchanged from before this field existed. A positive value (an
	// artifact-redirect entry now authors "host:port") is EXACT: a CONNECT to
	// the same host on a DIFFERENT port is never MITM'd or token-injected
	// (W13-S1-5) — handleConnect enforces this alongside isCorpMITMHost so the
	// allowlist stays as tight as isMITMHost's doc comment claims.
	mitmPorts map[string]int
	// mitmLLM gates TLS-MITM of the built-in LLM hosts on actual intent (subscription
	// injection or intercept_tls) — a CA minted only for artifact token injection
	// must NOT make Anthropic/OpenAI MITM-eligible. See isMITMHost / handleConnect.
	mitmLLM bool
	// blindHosts deduplicates the one-time llm.scan.blind signal emitted when an
	// inspection-enabled run tunnels to an LLM host over opaque CONNECT (no MITM
	// yet), so coverage is reported honestly without flooding the audit log.
	blindMu    sync.Mutex
	blindHosts map[string]struct{}

	// gitGrants is the per-run git-broker allowlist: canonical lowercased
	// "<org>/<repo>" -> the github_token grant to mint from. It is the unit of
	// trust for the /wardyn/gh/ route — a repo absent here is 403. Empty/nil ==
	// no repo brokered (the route always 403s). See git_broker.go.
	gitGrants map[string]uuid.UUID
	// patGrants is the per-run git_pat broker allowlist: lowercased host -> the
	// grant to mint from. Empty/nil == no host brokered (the route always 403s),
	// which is also what a deployment with the lane switched off looks like.
	patGrants map[string]PATGrant
	// gitTokens caches minted installation tokens per grant so a single clone
	// (info/refs + git-upload-pack) does not re-mint — mandatory for single-use
	// approval-gated grants. Guarded by gitTokMu; each entry single-flights its
	// own re-mint via gitTokEntry.reMu.
	gitTokMu  sync.Mutex
	gitTokens map[uuid.UUID]*gitTokEntry

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
	// noProxy is the compiled upstream BYPASS list (site-config
	// UpstreamProxyNoProxy) — the destinations dialed DIRECTLY instead of
	// through the corp upstream. Empty == every forward dial chains through the
	// upstream (byte-identical to before this field existed). Consulted only via
	// Proxy.bypassUpstream; it is a routing decision and never lifts the SSRF
	// guard or grants a policy allow. See egress_target.go.
	noProxy []noProxyRule

	// internalHosts are the OPERATOR-DECLARED internal hostnames (site-config
	// InternalHosts) eligible for vetHost's private-IP-guard lift — see
	// Proxy.vetHost / liftInternalHost. Empty == no lift (byte-identical to
	// before this field existed).
	internalHosts []internalHostRule
	// localSubnets are this proxy's OWN interface subnets, captured ONCE at
	// construction (net.InterfaceAddrs, NewServer) — never re-read per request.
	// liftInternalHost refuses to lift an address on one of these: the sidecar
	// shares its control-plane network with postgres/dex/registry, so an
	// internal-host declaration must not let a run reach the proxy's own
	// network neighbors.
	localSubnets []*net.IPNet
	// controlPlaneIP is this run's wardynd address, resolved ONCE at
	// construction (NewServer, before the Proxy exists) — never re-read per
	// request. liftInternalHost refuses to lift this address for the same
	// reason as localSubnets.
	controlPlaneIP net.IP

	// llmUpstreams is the OPERATOR-CONFIGURED internal-gateway table (vendor
	// public host -> {host,port,prefix}), parsed once from Options.LLMUpstreams.
	// Empty == every brokered LLM route dials the vendor host, byte-identical
	// to today. See llmUpstream.
	llmUpstreams map[string]llmUpstream
	// gatewayVendor is the REVERSE of llmUpstreams (gateway host -> vendor
	// public host), feeding isLLMHost/channelForHost so gateway traffic is
	// recognised as LLM traffic (coverage/classification only — the SSRF vet
	// for the gateway host lives in gatewayTarget, not here).
	gatewayVendor map[string]string

	now func() time.Time
}

// llmUpstream is one configured internal-gateway target: the bare host, port
// (443 when the base URL carried none), and path prefix (empty when the base
// URL carried none) parsed from an api.Config.LLMGateways entry.
type llmUpstream struct {
	host   string
	port   int
	prefix string
}

// internalHostRule is one compiled SiteConfig.InternalHosts entry: a
// label-suffix hostname match paired with the CIDRs its lift is scoped to (nil
// == the full ipguard.Liftable set, per SiteConfig.InternalHosts's doc).
type internalHostRule struct {
	suffix string
	cidrs  []*net.IPNet
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
	// GitGrants is the git-broker per-repo allowlist ("<org>/<repo>" -> grant id)
	// backing the /wardyn/gh/ route. See Proxy.gitGrants.
	GitGrants map[string]uuid.UUID
	// PATGrants is the git_pat broker's per-HOST allowlist backing /wardyn/git/.
	// See Config.PATGrants and pat_broker.go for why it is per-host rather than
	// per-repo.
	PATGrants map[string]PATGrant
	// ControlPlaneURL and RunToken back the local brokered routes. The run
	// token is injected only toward the control plane and never reaches the
	// sandbox or any LLM upstream.
	ControlPlaneURL string
	RunToken        *tokenSource
	// Upstream is the OPTIONAL corporate parent proxy (parsed form). Nil ==
	// direct dial. NewServer parses it from Config.UpstreamProxyURL; tests may
	// build one via parseUpstreamProxy.
	Upstream *upstreamProxy
	// UpstreamNoProxy is the operator's upstream BYPASS list
	// (SiteConfig.UpstreamProxyNoProxy, forwarded verbatim): host/domain
	// suffixes and CIDRs dialed DIRECTLY rather than CONNECTed through
	// Upstream. Control-plane-authored, same trust boundary as InternalHosts —
	// the sandbox cannot set it. Empty (the default) => no bypass. Ignored
	// entirely when Upstream is nil. See Proxy.noProxy / Proxy.bypassUpstream.
	UpstreamNoProxy []string
	// InternalHosts are the operator-declared internal hostnames (site-config,
	// CONTROL-PLANE-authored — the sandbox cannot set this) eligible for
	// vetHost's private-IP-guard lift. Empty == no lift (byte-identical to
	// before this field existed). See Proxy.internalHosts.
	InternalHosts []types.InternalHost
	// LocalSubnets are the proxy's own interface subnets (net.InterfaceAddrs,
	// captured once by NewServer before constructing Options). See
	// Proxy.localSubnets.
	LocalSubnets []*net.IPNet
	// ControlPlaneIP is this run's wardynd address, resolved once by NewServer
	// before constructing Options. See Proxy.controlPlaneIP.
	ControlPlaneIP net.IP
	// LLMUpstreams maps a public vendor host to an operator-configured internal
	// gateway base URL (Config.LLMUpstreams, forwarded verbatim). Empty == every
	// brokered LLM route dials the vendor host. See Proxy.llmUpstreams.
	LLMUpstreams map[string]string
	// Dial overrides the connection dialer (tests). Production leaves it nil
	// and a net.Dialer is used.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// TLSClientConfig is the TLS config for BOTH the forwarding and the
	// control-plane transports. Production sets it from Config.TrustedCAPEM
	// (system roots plus the operator's corporate CA bundle — see
	// NewServer); nil means system roots alone. Tests use it to trust an
	// httptest TLS server standing in for an HTTPS upstream.
	TLSClientConfig *tls.Config
	Now             func() time.Time
}

// vettedIPKey carries the pre-resolved, policy-checked dial target through the
// request context so the transport dials it directly instead of re-resolving
// the hostname (TOCTOU / DNS-rebinding guard).
type vettedIPKey struct{}

// parseMITMHostPort normalizes one Options.MITMHosts entry (trim, lowercase,
// drop a trailing dot) and splits its optional ":port" suffix. port==0 means
// the entry carried none — the historical bare-host format (Bedrock's
// runtimeHost, a pre-fix redirect) — and matches any port; a malformed or
// out-of-range port suffix is treated the same as absent rather than guessed.
// A clean "host:port" (what planArtifactRedirect now authors, W13-S1-5) scopes
// the entry to exactly that port.
func parseMITMHostPort(entry string) (host string, port int) {
	entry = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(entry)), ".")
	if entry == "" {
		return "", 0
	}
	if h, ps, err := net.SplitHostPort(entry); err == nil {
		if p, perr := strconv.Atoi(ps); perr == nil && p > 0 && p < 65536 {
			return h, p
		}
	}
	return entry, 0
}

// parseInternalHostCIDRs parses a SiteConfig.InternalHosts entry's CIDR
// list, failing the WHOLE entry (ok=false) the moment one fails to parse —
// never returning a partial list. liftInternalHost treats zero CIDRs as "no
// CIDRs declared" and lifts the FULL liftable set for the suffix, so
// silently dropping only the one bad CIDR out of several would widen an
// entry meant to be narrow into that full-set default, the opposite of what
// a parse failure should do.
func parseInternalHostCIDRs(raw []string) (cidrs []*net.IPNet, ok bool) {
	cidrs = make([]*net.IPNet, 0, len(raw))
	for _, c := range raw {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			return nil, false
		}
		cidrs = append(cidrs, n)
	}
	return cidrs, true
}

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
	mitmPorts := make(map[string]int, len(opts.MITMHosts))
	for _, entry := range opts.MITMHosts {
		h, port := parseMITMHostPort(entry)
		if h == "" {
			continue
		}
		mitmHosts[h] = true
		mitmPorts[h] = port
	}
	// Canonicalise git-broker allowlist keys to lowercase "<org>/<repo>" so lookups
	// match regardless of the slug casing git sends (github owner/repo is
	// case-insensitive).
	gitGrants := make(map[string]uuid.UUID, len(opts.GitGrants))
	for k, id := range opts.GitGrants {
		if k = strings.ToLower(strings.TrimSpace(k)); k != "" && id != uuid.Nil {
			gitGrants[k] = id
		}
	}
	// Same canonicalisation as the repo keys above: a host is matched
	// case-insensitively, so the map key is lowercased once here rather than at
	// every lookup.
	patGrants := make(map[string]PATGrant, len(opts.PATGrants))
	for k, g := range opts.PATGrants {
		if k = strings.ToLower(strings.TrimSpace(k)); k != "" && g.GrantID != uuid.Nil {
			patGrants[k] = g
		}
	}
	// Compile each declared internal host: lowercase + trim the suffix (same
	// normalization VetHost applies to the request host, so the comparison in
	// liftInternalHost is exact), parse its CIDRs (already validated at
	// site-config write time and at proxy Config load) via
	// parseInternalHostCIDRs, which drops the WHOLE entry on a parse failure.
	internalHosts := make([]internalHostRule, 0, len(opts.InternalHosts))
	for _, h := range opts.InternalHosts {
		suffix := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h.HostSuffix)), ".")
		if suffix == "" {
			continue
		}
		cidrs, ok := parseInternalHostCIDRs(h.CIDRs)
		if !ok {
			continue
		}
		internalHosts = append(internalHosts, internalHostRule{suffix: suffix, cidrs: cidrs})
	}
	// Compile the LLM-gateway table + its reverse lookup. LLMUpstreams is
	// already validated (api.ValidateLLMGateways at boot, applyDefaultsAndValidate
	// at config load) — a parse failure here just drops that one entry (falls
	// back to the vendor host) rather than widening scope or panicking.
	llmUpstreams := make(map[string]llmUpstream, len(opts.LLMUpstreams))
	gatewayVendor := make(map[string]string, len(opts.LLMUpstreams))
	for vendor, raw := range opts.LLMUpstreams {
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" {
			continue
		}
		port := 443
		if ps := u.Port(); ps != "" {
			if n, perr := strconv.Atoi(ps); perr == nil && n > 0 {
				port = n
			}
		}
		// Trailing dot trimmed like every lookup (vetTrustedHost, isLLMHost,
		// channelForHost) — an untrimmed key here is unmatchable.
		host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
		llmUpstreams[vendor] = llmUpstream{host: host, port: port, prefix: strings.TrimSuffix(u.Path, "/")}
		gatewayVendor[host] = vendor
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
		mitmPorts:       mitmPorts,
		mitmLLM:         opts.MITMLLM,
		gitGrants:       gitGrants,
		patGrants:       patGrants,
		gitTokens:       make(map[uuid.UUID]*gitTokEntry),
		controlPlaneURL: strings.TrimRight(opts.ControlPlaneURL, "/"),
		runToken:        opts.RunToken,
		upstream:        opts.Upstream,
		noProxy:         compileNoProxy(opts.UpstreamNoProxy),
		internalHosts:   internalHosts,
		localSubnets:    opts.LocalSubnets,
		controlPlaneIP:  opts.ControlPlaneIP,
		llmUpstreams:    llmUpstreams,
		gatewayVendor:   gatewayVendor,
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
	// which case a forward-egress dial is chained through it via CONNECT. In
	// upstream mode the context value carries the REAL host:port (a hostname, set
	// by evaluate() / serveMITMRequest) because the corp proxy — not us —
	// resolves and dials it.
	//
	// addr is http.Transport's own dial address for the OUTBOUND request — i.e.
	// the real destination host:port straight off outReq.URL (Transport.Proxy is
	// nil here, so it is never the proxy's own address). It is the one thing in
	// scope that still names the destination the way the OPERATOR declared it,
	// before any resolve, so it — not the already-resolved context target — is
	// what the bypass list is matched against. Without this the bypass would be
	// decided in egressTarget and then silently undone here: the transport
	// CONNECTs everything through the upstream independently of that branch.
	egressDial := directDial
	if p.upstream != nil {
		egressDial = func(ctx context.Context, network, addr string) (net.Conn, error) {
			target, ok := ctx.Value(vettedIPKey{}).(string)
			if !ok || target == "" {
				return nil, errors.New("proxy: missing dial target")
			}
			if reqHost, _ := splitHostPort(addr, 443); p.bypassUpstream(reqHost) {
				// Bypassed: dial the target egressTarget already resolved and
				// vetted, exactly as directDial does (no re-resolution).
				return p.dial(ctx, network, target)
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
//
// It strips ALL trailing dots from the host (TrimRight, not a single
// TrimSuffix). A single TrimSuffix left "evil.com..:443" as host "evil.com.",
// which every downstream consumer then one-dot-trimmed to "evil.com." — missing
// a dot-free deny key "evil.com" while VetHost still resolved+dialed it (an
// explicit-deny bypass under allow_all_egress). This is the single funnel every
// request host flows through, so normalizing it here reduces every FQDN-root
// spelling to the canonical dot-free host for all downstream matching.
func splitHostPort(hostport string, defaultPort int) (host string, port int) {
	if h, ps, err := net.SplitHostPort(hostport); err == nil {
		port, _ = strconv.Atoi(ps)
		return strings.ToLower(strings.TrimRight(h, ".")), port
	}
	return strings.ToLower(strings.TrimRight(hostport, ".")), defaultPort
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
	//
	// trustedLiteralIP is the ONE deliberate exception: a literal IP the
	// operator explicitly typed into an EXACT AllowedDomains entry (e.g. an
	// egress-redirect "To" target on RFC1918/CGNAT space — see
	// trustsExactLiteralIP) carries no DNS-rebinding risk, since there is
	// no hostname behind it to rebind. Set here, it also skips VetHost at
	// step 4 below (which would otherwise re-derive and re-deny the same
	// address) so the operator's own configured destination is actually
	// reachable instead of always denied with "the customer's network is at
	// fault" (W13-S1-3). Only blockPrivate OFF the proxy's own subnets and
	// control-plane host qualifies: a declared loopback, link-local/metadata or
	// NAT64 literal — or one of the sidecar's own docker-network neighbours —
	// still hits the else and is denied, so the operator can hand the sandbox
	// neither 169.254.169.254 nor the control plane by allow-listing it.
	var trustedLiteralIP net.IP
	if ip := net.ParseIP(strings.TrimSuffix(strings.ToLower(host), ".")); ip != nil {
		if kind, _ := isBlockedIP(ip); kind != blockNone {
			if p.trustsExactLiteralIP(ip, port) {
				trustedLiteralIP = ip
			} else {
				log := decisionLog(req, egress.Deny, "builtin:private-ip")
				return egress.Deny, "", &log
			}
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
	// approvalID records whether (and via which approval) this request's verdict
	// was RELEASED by the first-use approval flow, so the eventual Allow decision
	// below can attribute to "approval:<id>" instead of "policy:allowed" — a
	// released request otherwise logs indistinguishably from a standing policy
	// allow, with no approval_id, breaking the audit join from decision back to
	// who approved the egress (W20-hold-fsm-1). Zero == this request never went
	// through approval (a direct policy allow).
	var approvalID uuid.UUID

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
				// Released by approval: remember it for the Allow log below, then
				// fall through to method + IP vetting.
				approvalID = r.ApprovalID
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
	// When an upstream corp proxy is configured we do not PIN a resolved address:
	// the corp proxy performs the outbound DNS+dial, and the target carries the
	// HOSTNAME (not an IP) so egressDial issues CONNECT <real-host> to it — an
	// upstream handed a resolved literal refuses it. What is relaxed there is the
	// vetted-IP TOCTOU pin, NOT the guard: egressTarget still resolves the name
	// locally for the guard and denies one that answers into blocked space, so
	// SSRF-via-corp-proxy to loopback/metadata is blocked for the NAME spelling
	// and not only for the literal one the step-0 guard catches. Two residuals,
	// stated rather than papered over: a name this proxy cannot resolve at all is
	// forwarded unvetted, because on a private-endpoint estate the sandbox host
	// frequently cannot resolve external names and denying that would break every
	// upstream deployment; and, the target being sent by name, the guard binds it
	// at CHECK time only — the corp proxy resolves again for the dial, so a name
	// that answers differently to the two resolvers is not bound at dial time.
	// Policy/approval/method above are unchanged, and every DIRECT-dial path keeps
	// the full pinning VetHost guard.
	if trustedLiteralIP != nil {
		target := net.JoinHostPort(trustedLiteralIP.String(), strconv.Itoa(port))
		log := p.allowLog(req, approvalID)
		// Attribute the grant rather than leaving it as an ordinary
		// policy:allowed — reaching a private/reserved address is the one allow
		// an auditor most wants to see the REASON for. Same "an approval is the
		// more specific fact" rule as the internal-host lift below.
		if approvalID == uuid.Nil {
			log.RuleSource = ruleSourceEgressRedirect
		}
		return egress.Allow, target, &log
	}
	target, ruleSource, terr := p.egressTarget(host, port)
	if terr != nil {
		log := decisionLog(req, egress.Deny, "builtin:private-ip")
		return egress.Deny, "", &log
	}
	log := p.allowLog(req, approvalID)
	// The internal-host lift is the ONLY egressTarget outcome evaluate()
	// attributes a non-default rule_source to, and only when the request was
	// not already attributed to an approval (a released approval is the more
	// specific, more useful audit fact).
	if ruleSource != "" && approvalID == uuid.Nil {
		log.RuleSource = ruleSource
	}
	return egress.Allow, target, &log
}

// allowLog builds evaluate()'s Allow decision log. When the request's verdict
// was RELEASED by the first-use approval flow (approvalID != Nil), it
// attributes rule_source to "approval:<id>" and sets ApprovalID — mirroring
// the already-attributed pending/deny branches above — so the audit trail
// self-joins back to the approval that let the traffic through. Otherwise
// (approvalID == Nil, the common case) it is a standing policy allow.
func (p *Proxy) allowLog(req egress.Request, approvalID uuid.UUID) egress.DecisionLog {
	if approvalID == uuid.Nil {
		return decisionLog(req, egress.Allow, "policy:allowed")
	}
	log := decisionLog(req, egress.Allow, "approval:"+approvalID.String())
	log.ApprovalID = &approvalID
	return log
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
		p.writeEgressDeny(w, host, port, log)
		return
	case egress.Pending:
		if log != nil {
			p.sink.emit(*log)
		}
		setEgressRefusalHeaders(w, egressRefusalPending, host)
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
		p.httpError(w, "upstream error", err, http.StatusBadGateway)
		return
	}
	if log != nil {
		p.sink.emit(*log)
	}
	defer func() { _ = resp.Body.Close() }()

	relay(w, resp)
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
		p.writeEgressDeny(w, host, port, log)
		return
	case egress.Pending:
		if log != nil {
			p.sink.emit(*log)
		}
		setEgressRefusalHeaders(w, egressRefusalPending, host)
		writeApprovalPending(w, log)
		return
	}

	// Operator-configured corp artifact host: terminate TLS so the registry token
	// injects on the wire (config-only redirects never reach here — dispatch adds a
	// host to mitmHosts ONLY when it also authored a paired injection). Checked
	// BEFORE the LLM branch so it stays clear of the LLM-specific blind-coverage
	// bookkeeping; these hosts are not model APIs and are never content-scanned.
	//
	// PORT-SCOPED (W13-S1-5): mitmHosts is host-only, so also require the CONNECT
	// port to match what was actually configured (mitmPorts; 0 == the entry
	// carried no port and stays any-port, for backward compat with a bare
	// legacy entry). Without this a CONNECT to the same hostname on a port the
	// operator never configured would ALSO be MITM'd and token-injected — wider
	// than the redirect actually authored. A non-matching port falls through to
	// an ordinary opaque tunnel, still gated by the policy decision above.
	if p.ca != nil && p.isCorpMITMHost(host) {
		if cport := p.mitmPorts[host]; cport == 0 || cport == port {
			if log != nil {
				p.sink.emit(*log)
			}
			p.mitmConnect(w, r, host, port)
			return
		}
	}

	if p.isLLMHost(host) {
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
			p.mitmConnect(w, r, host, port)
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
	// upstream is configured and this host is not on its bypass list, else
	// directly to the vetted IP (no re-resolution). Same predicate, same real
	// destination host, as egressTarget and egressDial.
	var (
		upstream net.Conn
		err      error
	)
	if p.upstream != nil && !p.bypassUpstream(host) {
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
		p.httpError(w, "upstream dial failed", err, http.StatusBadGateway)
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

// httpError writes "<msg>: <err>" to the SANDBOX with the process-global secret
// mask applied to the error text — the same mask the decision-log path uses. A
// proxy error routinely wraps upstream/control-plane text (see resolveInjection's
// status echo), and the sandbox is the one party that must never observe an
// injected credential, so error strings are masked here rather than at each site
// — every sandbox-facing error goes through this helper, so no handler in the
// package holds a raw err.Error().
func (p *Proxy) httpError(w http.ResponseWriter, msg string, err error, code int) {
	http.Error(w, msg+": "+string(maskDecisionBytes([]byte(err.Error()))), code)
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
