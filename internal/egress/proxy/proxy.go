// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/contentscan"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/egress/domainmatch"
	"github.com/cjohnstoniv/wardyn/internal/hoptls"
	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// controlPlaneCallTimeout bounds ONE local-route forward to the control plane
// (mint, approval poll, injection resolve, decision post). See the comment at
// the p.localClient construction in newProxy for why it exists and why the
// value is what it is.
const controlPlaneCallTimeout = 130 * time.Second

//nolint:funlen // one linear wiring of each Options field into the Proxy; splitting it would scatter the wiring
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
	gitGrants := canonicalGitGrants(opts.GitGrants)
	// Same canonicalisation as the repo keys above: a host is matched
	// case-insensitively, so the map key is lowercased once here rather than at
	// every lookup.
	patGrants := canonicalPATGrants(opts.PATGrants)
	internalHosts := compileInternalHosts(opts.InternalHosts)
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
		gatewayVendor[hostKey(host)] = vendor
	}
	mitm := buildMITMTables(opts.MITMHosts, patGrants, opts.GitHubAPI, opts.AzureARM, opts.AWSSigV4, opts.KubeAPI)
	p := &Proxy{
		runID:                opts.RunID,
		policy:               opts.Policy,
		evaluator:            evaluator,
		approval:             opts.Approval,
		inject:               opts.Injector,
		sink:                 opts.Sink,
		res:                  res,
		scanner:              opts.Scanner,
		ca:                   opts.CA,
		mitmHosts:            mitm.hosts,
		mitmPorts:            mitm.ports,
		mitmPlaintext:        mitm.plaintext,
		mitmLLM:              opts.MITMLLM,
		gitGrants:            gitGrants,
		gitPushWithheld:      canonicalHostKeys(opts.GitPushWithheld),
		patGrants:            patGrants,
		brokeredPATGrantIDs:  opts.BrokeredPATGrantIDs,
		adoGrants:            opts.ADOGrants,
		azure:                newAzureGates(opts.AzureGates),
		patAPI:               mitm.patAPI,
		ghAPI:                mitm.ghAPI,
		arm:                  mitm.arm,
		sigv4:                mitm.sigv4,
		kube:                 mitm.kube,
		gitTokens:            make(map[uuid.UUID]*gitTokEntry),
		controlPlaneURL:      controlPlaneEndpoint(opts.ControlPlaneURL, ""),
		runToken:             opts.RunToken,
		upstream:             opts.Upstream,
		topologyRe:           append(topologyPatterns(controlPlaneEndpoint(opts.ControlPlaneURL, ""), opts.Upstream), gatewayPatterns(llmUpstreams)...),
		noProxy:              compileNoProxy(opts.UpstreamNoProxy),
		internalHosts:        internalHosts,
		localSubnets:         opts.LocalSubnets,
		controlPlaneIPs:      opts.ControlPlaneIPs,
		exclusionUnknown:     opts.ExclusionUnknown,
		llmUpstreams:         llmUpstreams,
		llmUnavailableDetail: opts.LLMUnavailableDetail,
		localModels:          compileLocalModels(opts.LocalModels, opts.RunnerLocalModels),
		pushHolds:            pushHolds{unattended: opts.Unattended},
		scrubResponses:       opts.ScrubResponses,
		noNetwork:            opts.NoNetwork,
		canonicalPushRepos:   opts.CanonicalPushRepos,
		attribution:          opts.Attribution,
		gatewayVendor:        gatewayVendor,
		channelHosts:         compileChannelHosts(opts.LLMChannelHosts),
		gatewayRoutes:        opts.GatewayIdentity,
		dial:                 dial,
		now:                  now,
		viaOrgRoutes:         opts.ViaOrgRoutes,
		maxTunnels:           maxOpenTunnels,
	}
	if l := policyref.Layered(opts.Attribution); p.sink != nil && l != nil && l.Layer != "" {
		p.sink.layer = l.Layer
		if l.Layer == policyref.LayerUnit {
			p.sink.layerName = l.Name
		}
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
	egressDial = p.noNetworkDial(egressDial) // never directDial: the control-plane transport shares it
	mkTransport := func(dc func(context.Context, string, string) (net.Conn, error)) *http.Transport {
		return &http.Transport{
			DialContext: dc,
			// HTTP/1.1 only, which is what controlTransport stays; offerHTTP2
			// turns HTTP/2 on for the egress transport alone, a few lines below.
			ForceAttemptHTTP2:     false,
			MaxIdleConns:          64,
			IdleConnTimeout:       60 * time.Second,
			TLSHandshakeTimeout:   15 * time.Second,
			ExpectContinueTimeout: time.Second,
			Proxy:                 nil,
			TLSClientConfig:       opts.TLSClientConfig,
		}
	}
	// A plain-HTTP forward dials through p.transport itself, so a failure before the connection
	// opened is marked as connecting here, like handshakeOver does for TLS: dialStage otherwise reads
	// any error that is not a net dial error as one on an open connection.
	p.transport = mkTransport(func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := egressDial(ctx, network, addr)
		if err != nil {
			return nil, &connectStageError{err}
		}
		return tapHeads(ctx, c), nil
	})
	p.offerHTTP2(egressDial, opts.TLSClientConfig)
	p.kube.bindTransport(p.transport)
	p.gatewayHolders, p.gatewayCert = newGatewayHolders(opts.RunID, opts.GatewayIdentity)
	p.controlTransport = mkTransport(directDial)
	if opts.ControlTLS == nil {
		opts.ControlTLS, _ = hoptls.ClientConfig("")
	}
	p.controlTransport.TLSClientConfig = opts.ControlTLS.Clone()
	// localClient uses the CONTROL transport: local-route forwards to the control
	// plane carry the vetted dial target on the request context so the host is
	// never re-resolved (same TOCTOU guard), and they NEVER chain through the
	// upstream corp proxy — the run token stays off the corp-proxy wire.
	//
	// The Timeout is load-bearing, not hygiene: every caller of
	// forwardToControlPlane rides r.Context(), and the agent-facing listener sets
	// ReadTimeout/WriteTimeout to 0 because streaming bodies and CONNECT tunnels
	// need it (NewServer, server.go), so without it a control plane that accepts
	// a connection and then never answers parked a brokered mint, an approval
	// poll or a credential relay FOREVER — with the sandbox's request goroutine
	// and socket held in a 256 MiB sidecar. Every one of these forwards reads a
	// CAPPED body (maxBrokeredBody), never a stream, so a whole-request ceiling
	// is the right shape here. The value mirrors the shipped client in
	// cmd/wardyn-proxy/main.go for the same reason its comment gives: it must
	// exceed the subscription delegated-refresh budget (120s) so an injection
	// resolve at the token-expiry boundary is not failed closed early.
	p.localClient = &http.Client{Transport: p.controlTransport, Timeout: controlPlaneCallTimeout}

	// The upstream proxy hop's row (auditUpstreamHop); on a runner sink it waits for the first grant.
	if p.sink == nil || p.sink.remote == nil {
		p.auditUpstreamHop()
	}
	return p
}

// ServeHTTP routes between CONNECT tunneling, brokered LOCAL routes, and plain-HTTP forwarding.
//
// The local brokered routes (/wardyn/...) are reachable ONLY via
// origin-form requests addressed to the proxy listener itself — i.e. the
// request-target is path-only (r.URL.Host == ""). An absolute-URI forward
// request for http://wardyn-proxy:3128/wardyn/v1/... carries a non-empty
// r.URL.Host and is handled by the normal forward-proxy path, where the proxy
// host is not allowlisted and is denied by policy. CONNECT is never a local
// route. This makes the run token unreachable by anything the sandbox can
// route through the forward proxy.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r = trackNetworkBody(r)
	removeConnectionNominated(r.Header) // every gate below judges the headers the forward sends
	// A self-lapsing proxy serves nothing while closed or lapsed (§6.2).
	if !p.lapse.serving() {
		refuseClosed(w)
		return
	}
	// The person's sign-in lane has its own listener and rules (desktop_signin_lane.go).
	if onSigninLane(r.Context()) {
		p.serveSignin(w, r)
		return
	}
	// A via_org destination is the organisation's to decide: no spool row, no
	// hold and no decision here (design 0.9 §7.3).
	if route, authority, ok := p.viaOrgRouteFor(r); ok {
		p.serveViaOrg(w, r, route, authority)
		return
	}
	var release func()
	r, release = p.admitSpoolHTTP(w, r, "")
	if r == nil {
		return
	}
	defer release()
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
	p.servePlainJudged(w, r)
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
// spelling to the canonical dot-free host for all downstream matching, and an
// IP literal to its one canonical spelling (domainmatch.CanonHost): the same one
// the policy and every host-keyed table compile to (hostKey).
func splitHostPort(hostport string, defaultPort int) (host string, port int) {
	if h, ps, err := net.SplitHostPort(hostport); err == nil {
		port, _ = strconv.Atoi(ps)
		return domainmatch.CanonHost(strings.TrimRight(h, ".")), port
	}
	// No port: take off an IPv6 authority's brackets as net.SplitHostPort does, so "[::1]" reaches step 0 as an address.
	h := strings.TrimRight(hostport, ".")
	if inner, ok := strings.CutPrefix(h, "["); ok && strings.HasSuffix(inner, "]") {
		h = strings.TrimRight(strings.TrimSuffix(inner, "]"), ".")
	}
	return domainmatch.CanonHost(h), defaultPort
}

// evaluate runs the full decision pipeline for a request, emitting the
// decision log, and returns the decision plus the vetted dial target
// ("ip:port") when allowed.
func (p *Proxy) evaluate(ctx context.Context, host string, port int, method string, path string) (egress.Decision, string, *egress.DecisionLog) {
	req := egress.Request{
		AuditReservation: spoolTicket(ctx),
		RunID:            p.runID,
		Host:             host,
		Port:             port,
		Method:           strings.ToUpper(method),
		Path:             path,
		Time:             p.now(),
		Relayed:          p.relayLane(ctx),
	}

	// 0. Unconditional literal-IP guard, before policy and before first-use
	// approval — see literalIPGuard (literal_ip_guard.go), which holds the rule
	// and both spellings of it.
	trustedLiteralIP, denied := p.literalIPGuard(req, host, port)
	if denied != nil {
		return egress.Deny, "", denied
	}
	// An attached MCP server's host is the broker's alone, above every allow.
	if p.mcp.mcpDirectOrigin(host) {
		log := decisionLog(req, egress.Deny, ruleSourceMCPDirect)
		return egress.Deny, "", &log
	}
	// So is a local model server's: its gate is on the brokered LLM route.
	if p.localModelHost(host) {
		log := decisionLog(req, egress.Deny, ruleSourceLocalModel)
		log.Cause = "a local model server is reached only on the brokered LLM route"
		return egress.Deny, "", &log
	}

	// 1-2. The part of the decision the config alone makes (connect_gate.go): the policy host verdict,
	// the port rule and the method restriction, in that order. The IP guard (step 0), first-use approval
	// (step 3), and IP vetting (step 4) stay HARDWIRED around it regardless of engine.
	gate := gateConnect(ctx, req, p.evaluator, p.policy, p.portRule)
	if gate.refusal != "" {
		log := decisionLog(req, egress.Deny, gate.refusal)
		return egress.Deny, "", &log
	}
	if log := p.hostRuleConnectDeny(ctx, req); log != nil { // before any approval (host_rules.go)
		return egress.Deny, "", log
	}
	// Under push_rules.deny_all, a code host no door serves (push_deny_all_forge.go).
	if log := p.pushDenyAllForgeDeny(req, host, gate.verdict); log != nil {
		return egress.Deny, "", log
	}
	verdict, portDryRun := gate.verdict, gate.portDryRun
	// approvalID records whether (and via which approval) this request's verdict
	// was RELEASED by the first-use approval flow, so the eventual Allow decision
	// below can attribute to "approval:<id>" instead of "policy:allowed" — a
	// released request otherwise logs indistinguishably from a standing policy
	// allow, with no approval_id, breaking the audit join from decision back to
	// who approved the egress. Zero == this request never went
	// through approval (a direct policy allow).
	var approvalID uuid.UUID
	// givenBack: that approval is a once grant refundOnce gave back, so this request is its second use.
	var givenBack bool

	// B6, and it sits HERE — above the first-use approval flow, below
	// policy:denied/policy:method — for the reason privateIPMemoHit's own doc
	// gives (private_ip_memo.go): a memoed host can never be approved into
	// reachability, so asking costs a human a decision the next line discards.
	if p.privateIPMemoHit(host, port) {
		return egress.Deny, "", nil
	}

	switch verdict {
	case egress.VerdictUnknown:
		// 3. First-use approval (only for the review modes). always_deny falls to
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
				approvalID, givenBack = r.ApprovalID, r.GivenBack
			case apDenied:
				log := decisionLog(req, egress.Deny, "approval:denied")
				return egress.Deny, "", &log
			default: // apPending / apNone / apCapped / apRefused / apExpired / apStale
				log := decisionLog(req, egress.Pending, approvalPendingReason(r.State))
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
	default:
		// A verdict the proxy does not recognise is as unevaluatable as an
		// error: fail closed under the same rule source.
		log := decisionLog(req, egress.Deny, "policy:evaluator-error")
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
		log := p.allowLog(req, approvalID, givenBack)
		// Attribute the grant rather than leaving it as an ordinary
		// policy:allowed — reaching a private/reserved address is the one allow
		// an auditor most wants to see the REASON for. Same "an approval is the
		// more specific fact" rule as the internal-host lift below.
		if approvalID == uuid.Nil {
			log.RuleSource = ruleSourceEgressRedirect
		}
		markPortDryRun(&log, portDryRun)
		return egress.Allow, target, &log
	}
	target, ruleSource, terr := p.egressTarget(host, port)
	if terr != nil {
		// A name that never resolved is not an SSRF block, and auditing it as
		// one is a lie with a cost: the operator cannot tell a DNS outage from a
		// real guard denial in the decision stream, and the deny's own detail
		// sentence then sends them to widen an SSRF control over an unrelated
		// fault. Both still fail closed — only the attribution differs.
		if errors.Is(terr, errHostUnresolved) {
			log := decisionLog(req, egress.Deny, "builtin:resolve-failed")
			// With an upstream configured, only a BYPASSED name is resolved
			// here to be dialled: say the upstream was skipped, or the row
			// reads as if the corp proxy could not find the name.
			if p.upstream != nil {
				log.Via, log.Cause = viaDirect, resolveBypassedCause
			}
			return egress.Deny, "", &log
		}
		log := decisionLog(req, egress.Deny, "builtin:private-ip")
		log.Cause = liftHintOf(terr) // a refused CONNECT shows curl no body: the row is where the remedy is sure to land
		p.privateIPRefused(req, blockKindOf(terr), log.Cause)
		return egress.Deny, "", &log
	}
	log := p.allowLog(req, approvalID, givenBack)
	// The internal-host lift is the ONLY egressTarget outcome evaluate()
	// attributes a non-default rule_source to, and only when the request was
	// not already attributed to an approval (a released approval is the more
	// specific, more useful audit fact).
	if ruleSource != "" && approvalID == uuid.Nil {
		log.RuleSource = ruleSource
	}
	markPortDryRun(&log, portDryRun)
	return egress.Allow, target, &log
}

// allowLog builds evaluate()'s Allow decision log. When the request's verdict
// was RELEASED by the first-use approval flow (approvalID != Nil), it
// attributes rule_source to "approval:<id>" and sets ApprovalID — mirroring
// the already-attributed pending/deny branches above — so the audit trail
// self-joins back to the approval that let the traffic through. Otherwise
// (approvalID == Nil, the common case) it is a standing policy allow.
func (p *Proxy) allowLog(req egress.Request, approvalID uuid.UUID, givenBack bool) egress.DecisionLog {
	if approvalID == uuid.Nil {
		return decisionLog(req, egress.Allow, "policy:allowed")
	}
	log := decisionLog(req, egress.Allow, "approval:"+approvalID.String())
	log.ApprovalID = &approvalID
	log.GivenBackUse = givenBack
	return log
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

	// A desktop VM's relay asks before it completes the guest's handshake and
	// tunnels after it has read the first flight (vmnet_flows.go): the tunnel
	// stage resumes the decision the ask parked, so one flow is decided, held
	// and dialled once.
	flow, stage := p.vmnetStage(r)
	decision, target, log, resumed := p.decideConnect(r.Context(), flow, stage, host, port)
	// Deny/Pending are emitted now. The ALLOW decision is deferred until the
	// tunnel is actually established (E3): emitting it before the dial would
	// over-report an allow when the dial then fails.
	switch decision {
	case egress.Deny:
		if log != nil {
			p.sink.emit(*log)
		}
		// memoed=true: this is an evaluate() verdict, and evaluate's memo arm
		// (privateIPMemoHit) returns a log-less Deny for an identical repeat.
		p.writeEgressDeny(w, host, port, log, true)
		return
	case egress.Pending:
		if log != nil {
			p.sink.emit(*log)
		}
		setEgressRefusalHeadersWithReason(w, egressRefusalPending, host, decisionReason(log))
		writeApprovalPending(w, log)
		return
	}
	if stage == vmnetStageAsk {
		p.parkVMFlow(w, flow, host, port, target, log)
		return
	}

	// Operator-configured corp artifact host: terminate TLS so the registry token
	// injects on the wire (config-only redirects never reach here — dispatch adds a
	// host to mitmHosts ONLY when it also authored a paired injection). Checked
	// BEFORE the LLM branch so it stays clear of the LLM-specific blind-coverage
	// bookkeeping; these hosts are not model APIs and are never content-scanned.
	//
	// Port-scoped: mitmHosts is host-only, so also require the CONNECT
	// port to match what was actually configured (mitmPortAllowed; 0 == the
	// entry carried no port and stays any-port, for backward compat with a bare
	// legacy entry). Without this a CONNECT to the same hostname on a port the
	// operator never configured would ALSO be MITM'd and token-injected — wider
	// than the redirect actually authored. A non-matching port falls through to
	// an ordinary opaque tunnel, still gated by the policy decision above — and
	// the SAME clamp is inside mitmLLMHost below, so the fall-through cannot be
	// re-admitted by the LLM branch for a host that is both.
	// A rule host is always opened here, so each request inside is judged (host_rules.go).
	if p.ca != nil && (p.isCorpMITMHost(host) || p.policy.namesRuleHost(host)) {
		if p.mitmPortAllowed(host, port) {
			if log != nil {
				p.sink.emit(*log)
			}
			p.mitmConnect(w, r, host, port, log)
			return
		}
	}
	// Never a blind tunnel to an Azure DevOps grant host (refuseADOTunnel).
	if p.refuseGrantHostTunnel(w, r, host, port) {
		return
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
		if p.mitmLLMHost(host, port) {
			// The MITM path establishes its own TLS-terminated tunnel and emits
			// per-request decisions inside; record the CONNECT allow here.
			if log != nil {
				p.sink.emit(*log)
			}
			p.mitmConnect(w, r, host, port, log)
			return
		}
		// With allowed_models set, a model host is never an opaque tunnel: the gate cannot see into it.
		if p.refuseOpaqueModelTunnel(w, r, host, port) {
			return
		}
		// Opaque tunnel: mark its allow row whatever the scanner does; llm.scan.bypass only when inspection was expected.
		log.MarkModelTunnel()
		if p.scanner != nil && p.scanner.Mode() != contentscan.ModeOff {
			p.emitLLMBlindOnce(r.Context(), host, port, http.MethodConnect)
		}
	}
	p.openTunnel(w, r, host, port, target, log, p.countActivity, resumed)
}

// openTunnel dials an allowed CONNECT's destination and pipes the client leg
// to it opaquely. count wraps the client leg (the activity counter on the
// agent's listener; the sign-in lane passes none). relayed is a relay flow
// resumed from its ask (decideConnect), never the stage header alone.
func (p *Proxy) openTunnel(w http.ResponseWriter, r *http.Request, host string, port int, target string, log *egress.DecisionLog, count func(net.Conn) net.Conn, relayed bool) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking unsupported", http.StatusInternalServerError)
		return
	}
	// The slot is taken before the dial and given back on every return, which a
	// tunnel reaches once either leg ends (a shutdown that closes it included).
	// A relayed flow takes none: the relay counted it before its ask.
	if !relayed {
		if p.openTunnels.Add(1) > p.maxTunnels {
			p.openTunnels.Add(-1)
			if log != nil {
				p.sink.emit(decisionLog(log.Request, egress.Deny, ruleSourceTunnelLimit))
			}
			// Temporary, so not X-Wardyn-Egress: denied: a retry succeeds once a tunnel closes.
			w.Header().Set(egressHeaderReason, ruleSourceTunnelLimit)
			w.Header().Set(egressHeaderHost, host)
			w.Header().Set("Retry-After", "1")
			w.Header().Set("X-Should-Retry", "true")
			http.Error(w, "wardyn: the run holds the most open tunnels it may; retry once one closes", http.StatusServiceUnavailable)
			return
		}
		defer p.openTunnels.Add(-1)
	}

	// Dial the destination: through the corp proxy (CONNECT <real-host>) when an
	// upstream is configured and this host is not on its bypass list, else
	// directly to the vetted IP (no re-resolution). Same predicate, same real
	// destination host, as egressTarget and egressDial.
	var (
		upstream net.Conn
		err      error
	)
	if p.noNetwork {
		err = errNoNetwork
	} else if p.upstream != nil && !p.bypassUpstream(host) {
		upstream, err = p.dialThroughUpstream(r.Context(), host, port)
	} else {
		upstream, err = p.dial(r.Context(), "tcp", target)
	}
	if err != nil {
		// Dial failed: report a DENY (not the earlier-computed allow) so a failed
		// tunnel is never logged as allowed egress (E3).
		if log != nil {
			dl := p.denyDialFailed("builtin:dial-failed", log.Request, host, err, nil)
			p.refundOnceUse(&dl, log, host) // before the 502, so the sandbox's retry finds the grant
			p.sink.emit(dl)
		}
		p.httpError(w, "upstream dial failed", err, http.StatusBadGateway)
		return
	}
	// Tunnel dial succeeded: NOW record the allow.
	if log != nil {
		p.emitDialledAllow(*log, host)
	}

	clientConn, brw, err := hj.Hijack()
	if err != nil {
		_ = upstream.Close()
		return
	}
	defer p.trackTunnel(clientConn)()
	if _, err := clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		_ = upstream.Close()
		_ = clientConn.Close()
		return
	}
	// Read the client leg through the hijack's OWN buffered reader: a client that
	// writes its CONNECT and the first bytes of the tunnel in one write (a TLS
	// ClientHello behind the CONNECT headers) has those bytes already in that
	// buffer, and reading clientConn directly drops them — the origin would
	// never see the client's opening flight. Same readerConn the MITM lane uses
	// for the same reason.
	watch := newTunnelWatch(upstream)
	dest := net.JoinHostPort(host, strconv.Itoa(port))
	watch.onSSH = func() error { return p.sshAgent.sshTunnel(watch, dest) }
	defer p.sshAgent.dropTunnel(watch)
	var client net.Conn = &readerConn{Conn: clientConn, r: brw.Reader}
	if count != nil {
		client = count(client)
	}
	first, err := tunnel(client, watch)
	p.tunnelEnded(log, host, watch, first, err)
}
