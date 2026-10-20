// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// The sidecar PROCESS: wiring a Proxy from a validated Config, serving it, and
// shutting it down cleanly. A real seam from proxy.go, not a size dodge:
// everything here is lifecycle — construction, listen, shutdown — while
// proxy.go is the request path itself.

import (
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/contentscan"
	"github.com/cjohnstoniv/wardyn/internal/hoptls"
)

// Server bundles a Proxy with its http.Server and async decision sink for
// lifecycle management.
type Server struct {
	proxy *Proxy
	http  *http.Server
	sink  *decisionSink
	// renewStop stops the run-token renewer started by NewServer; renewStopped
	// closes once it has exited. Both set ONCE in NewServer (never from
	// ListenAndServe): production runs ListenAndServe in one goroutine and
	// calls Shutdown from another, so writing at serve time would race the
	// read in Shutdown. Nil when no renewer was started.
	renewStop    context.CancelFunc
	renewStopped chan struct{}
	// maskKey is a ViaOrgEngine's own procRegistry key, evicted at Shutdown; uuid.Nil for a sidecar.
	maskKey uuid.UUID
	// allowedPeers is Config.AllowedPeers.
	allowedPeers []netip.Prefix
	// signinListen is Config.SigninListen: the person's sign-in lane, "" = none.
	signinListen string
}

// NewServer wires a Proxy from a validated Config and dependencies. It mints
// injection credentials once (fail-closed on error) before returning.
func NewServer(ctx context.Context, cfg *Config, client *http.Client, stdout io.Writer) (*Server, error) {
	return newServer(ctx, cfg, client, stdout, serverHooks{})
}

// serverHooks are what a ViaOrgEngine changes about a Server: no token renewer
// (engine), and for tests a dialer and resolver in place of the network.
type serverHooks struct {
	dial    func(ctx context.Context, network, addr string) (net.Conn, error)
	res     resolver
	engine  bool
	maskKey uuid.UUID // where this server's secrets are filed (procMaskFor)
	// lapseClock and lapseTune replace the self-lapse clock and renew cadence
	// in tests; zero values are CLOCK_BOOTTIME and the design's cadence.
	lapseClock bootClock
	lapseTune  *lapseRenewTuning
}

// start finishes a Server: a sidecar starts its token renewer; a ViaOrgEngine
// starts none, and its brokered mints and approval polls ride the caller's
// client too: it is the organisation, and never dials itself.
func (h serverHooks) start(out *Server, p *Proxy, ts *tokenSource, cfg *Config, client *http.Client) {
	if h.dial != nil {
		p.dial = h.dial // every transport dials through p.dial at call time
	}
	if h.res != nil {
		p.res = h.res
	}
	p.maskKey, out.maskKey = h.maskKey, h.maskKey
	if h.engine {
		p.localClient = client
		return
	}
	tune := defaultLapseRenewTuning()
	if h.lapseTune != nil {
		tune = *h.lapseTune
	}
	startRunTokenRenewer(out, p, ts, cfg, client, tune)
}

// egressTLSConfig is the egress trust set: the system roots plus pemBundle, nil (the default roots) when
// pemBundle is empty.
func egressTLSConfig(pemBundle string) (*tls.Config, error) {
	if pemBundle == "" {
		return nil, nil
	}
	pool, perr := x509.SystemCertPool()
	if perr != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM([]byte(pemBundle)) {
		return nil, fmt.Errorf("trusted ca: no valid PEM certificates in trusted_ca_pem")
	}
	return &tls.Config{RootCAs: pool}, nil
}

// controlPlaneClient returns the client every control-plane caller rides.
//
// SECURITY: the four control-plane clients below (sink, injector, approval
// client, token renewer) all ride THIS client. A caller-supplied client
// with no Transport rides http.DefaultTransport, which would ride
// ProxyFromEnvironment instead of nil-ing Proxy out. With HTTP(S)_PROXY
// visible to the sidecar, the run token, approvals, decisions and MINTED
// CREDENTIAL VALUES would transit the corporate proxy and skip
// resolveTrustedURL's pin.
//
// So the transport is owned UNCONDITIONALLY and Proxy is cleared
// explicitly: Transport.Clone() PRESERVES the proxy function, so this
// can't skip the clearing. A nil client is built here, never left to a
// callee's fallback.
func controlPlaneClient(client *http.Client, cpTLS *tls.Config) *http.Client {
	if client == nil {
		client = &http.Client{Timeout: controlPlaneCallTimeout}
	}
	if client.Transport == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.Proxy = nil
		// A COPY, not the shared pointer: this transport has HTTP/2 enabled,
		// and enabling it prepends "h2" to the config's own NextProtos on
		// first use — a shared config made the egress transport offer h2 (#359).
		tr.TLSClientConfig = cpTLS.Clone()
		c := *client
		c.Transport = tr
		client = &c
	}
	return client
}

// upstreamProxyFor parses and validates the upstream/parent corp proxy
// (optional) and registers any embedded credential in the mask registry so it
// can never leak into a decision log or stdout. The credential is held
// proxy-memory-only.
func upstreamProxyFor(ctx context.Context, cfg *Config, maskKey uuid.UUID) (*upstreamProxy, error) {
	up, err := parseUpstreamProxy(cfg.UpstreamProxyURL)
	if err != nil {
		return nil, err
	}
	if up != nil {
		for _, v := range up.maskValues() {
			procMaskFor(maskKey, v)
		}
		slog.InfoContext(ctx, "wardyn-proxy: chaining egress through upstream proxy (private-IP guard relaxed for this hop; control-plane bypasses it)",
			slog.String("upstream_addr", up.addr))
		// Say LOUDLY and EXHAUSTIVELY what is NOT chained: the deployment's
		// log must name the bypass entries themselves, not just a count. Also
		// state what it does NOT do, because "bypass" reads like "exempt" and
		// isn't: a bypassed dial still runs the private-IP guard and needs
		// its policy allow.
		if noProxy := compileNoProxy(cfg.UpstreamProxyNoProxy); len(noProxy) > 0 {
			slog.InfoContext(ctx, "wardyn-proxy: upstream proxy BYPASSED for declared destinations — dialed directly "+
				"(still SSRF-guarded: a private address needs a site-config internal_hosts declaration; still policy-gated)",
				slog.Any("bypass", noProxyStrings(noProxy)))
		}
	}
	return up, nil
}

func newServer(ctx context.Context, cfg *Config, client *http.Client, stdout io.Writer, hooks serverHooks) (*Server, error) {
	pol := CompilePolicy(cfg.Policy)
	// Beside the compile, not inside it: CompilePolicy is a pure function with
	// several callers, and this is a once-per-boot report about the policy
	// this sidecar was actually dispatched.
	warnDeadDomainEntries(ctx, cfg.Policy)

	// Captured ONCE here (never per-request): the internal-host lift's
	// own-subnet/control-plane exclusion needs both before any request is
	// served. net.InterfaceAddrs() already sees the control-plane network at
	// this point, since NetworkConnect precedes ContainerStart on the docker
	// driver.
	//
	// A lookup failure for either is non-fatal but NOT free: the exclusion is
	// a CLAMP on the lift, so an empty clamp would widen it instead of
	// narrowing it. The failure is carried into the Proxy as
	// ExclusionUnknown, which makes the clamp refuse every lift/trust instead.
	localSubnets, subnetsOK := localInterfaceSubnets()
	cpIPs, cpOK := resolveControlPlaneIPs(cfg.ControlPlaneURL)
	exclusionUnknown := !subnetsOK || !cpOK
	if exclusionUnknown {
		slog.WarnContext(ctx, "wardyn-proxy: own-subnet/control-plane exclusion unavailable; the internal-host lift and the exact-literal-IP redirect trust are refused for every address",
			slog.Bool("interface_subnets_ok", subnetsOK),
			slog.Bool("control_plane_resolved", cpOK))
	}

	// ONE live token for the whole sidecar: the config's run token is the
	// seed, and the renewer (started by ListenAndServe) rotates it in place
	// before its short TTL lapses. Every control-plane caller below shares
	// this source, so a renew reaches all of them at once.
	ts := newTokenSource(cfg.RunToken)

	// SECURITY: two TLS trust sets, never merged and never shared. The
	// corporate CA pool (system roots plus WARDYN_TRUSTED_CA_FILE) is for
	// EGRESS only: a TLS-inspecting middlebox sits between this sidecar and
	// the internet, not between it and wardynd. Control-plane calls trust
	// wardynd's internal CA and nothing else — an empty pin fails every
	// handshake rather than falling back to the system roots.
	tlsCfg, err := egressTLSConfig(cfg.TrustedCAPEM)
	if err != nil {
		return nil, err
	}
	cpTLS, err := hoptls.ClientConfig(cfg.ControlPlaneCAPEM)
	if err != nil {
		return nil, err
	}
	client = controlPlaneClient(client, cpTLS)

	sink, err := configuredDecisionSink(cfg, ts, client, stdout)
	if err != nil {
		return nil, err
	}

	inj, err := buildInjectorFor(ctx, hooks.maskKey, cfg.ControlPlaneURL, ts, pol, cfg.Injection, client)
	if err != nil {
		_ = sink.close(context.Background())
		return nil, fmt.Errorf("build injector: %w", err)
	}

	// Build the OPTIONAL outbound content-inspection engine, off unless the
	// policy carries an llm_inspection block. Register operator-declared
	// workspace secret values in the proxy-global mask registry FIRST so they
	// (a) form the scan corpus and (b) are masked from decision-log output
	// defense-in-depth — then snapshot (buildInjector already registered any
	// injected credentials above). A global kill-switch
	// (WARDYN_LLM_SCAN=off) is applied upstream by clearing cfg.Policy.LLMInspection.
	var scanner *contentscan.Engine
	if spec := cfg.Policy.LLMInspection; spec != nil {
		for _, v := range spec.WorkspaceSecretValues {
			procMaskFor(hooks.maskKey, []byte(v))
		}
		eng, eerr := contentscan.NewEngine(*spec, procRegistry.Snapshot(hooks.maskKey))
		if eerr != nil {
			_ = sink.close(context.Background())
			return nil, fmt.Errorf("build content scanner: %w", eerr)
		}
		scanner = eng
		if eng == nil && spec.Mode != "" && spec.Mode != "off" {
			// Configured to inspect but the effective secret corpus is empty —
			// scanning is a no-op. Surface it so the operator is not misled.
			slog.WarnContext(ctx, "wardyn-proxy: llm_inspection configured but no effective secret corpus — scanning disabled", slog.String("mode", spec.Mode))
		}
	}

	// TLS-MITM CA: build whenever the per-run PEMs are provided. MITM serves
	// TWO purposes — content inspection (scanner) AND subscription credential
	// injection (which must terminate TLS to swap the Authorization header
	// for the live host token). Dispatch only delivers the PEMs when one of
	// those is wanted, so their presence is the authoritative signal. With a
	// nil scanner the terminated tunnel is forward+inject only.
	var ca *certAuthority
	if cfg.MITMCACertPEM != "" && cfg.MITMCAKeyPEM != "" {
		ca, err = newCertAuthority([]byte(cfg.MITMCACertPEM), []byte(cfg.MITMCAKeyPEM))
		if err != nil {
			_ = sink.close(context.Background())
			return nil, fmt.Errorf("build mitm CA: %w", err)
		}
		slog.InfoContext(ctx, "wardyn-proxy: TLS-MITM enabled (content inspection and/or subscription credential injection)")
	}

	up, err := upstreamProxyFor(ctx, cfg, hooks.maskKey)
	if err != nil {
		_ = sink.close(context.Background())
		return nil, fmt.Errorf("upstream proxy: %w", err)
	}

	ap := newApprovalClient(cfg.ControlPlaneURL, ts, cfg.RunID, client)
	// 0/absent for either knob keeps configureHold's built-in defaults (30s / 16).
	ap.configureHold(cfg.Policy.FirstUseApproval.Normalize(), time.Duration(cfg.Policy.FirstUseHoldSeconds)*time.Second, cfg.Policy.MaxHolds)

	// Corporate CA trust (WARDYN_TRUSTED_CA_FILE): additive to the system
	// roots for THIS sidecar's egress TLS. Each transport that may EDIT it
	// (anything with HTTP/2 enabled) takes its own copy first. Nil (unset)
	// leaves it nil (system roots, ServerName from URL).
	// applyDefaultsAndValidate already fail-fast-checked this PEM at
	// config-load time; this is the live proxy's own parse.
	p := newProxy(Options{
		RunID:                cfg.RunID,
		Policy:               pol,
		Approval:             ap,
		Injector:             inj,
		Sink:                 sink,
		Scanner:              scanner,
		CA:                   ca,
		MITMHosts:            cfg.MITMHosts,
		MITMLLM:              cfg.MITMLLM,
		GitGrants:            cfg.GitGrants,
		PATGrants:            cfg.PATGrants,
		BrokeredPATGrantIDs:  cfg.BrokeredPATGrantIDs,
		ADOGrants:            newADOGrantsByHost(cfg.ADOGrant),
		AzureGates:           cfg.AzureGates,
		GitHubAPI:            cfg.GitHubAPI,
		AzureARM:             cfg.AzureARM,
		AWSSigV4:             cfg.AWSSigV4,
		ControlPlaneURL:      cfg.ControlPlaneURL,
		RunToken:             ts,
		Upstream:             up,
		UpstreamNoProxy:      cfg.UpstreamProxyNoProxy,
		TLSClientConfig:      tlsCfg,
		ControlTLS:           cpTLS,
		InternalHosts:        cfg.InternalHosts,
		LLMUpstreams:         cfg.LLMUpstreams,
		LLMChannelHosts:      cfg.LLMChannelHosts,
		LLMUnavailableDetail: cfg.LLMUnavailableDetail,
		LocalModels:          cfg.LocalModels,
		Unattended:           cfg.Unattended,
		ScrubResponses:       cfg.ScrubResponses,
		Attribution:          cfg.Attribution,
		LocalSubnets:         localSubnets,
		ControlPlaneIPs:      cpIPs,
		ExclusionUnknown:     exclusionUnknown,
		ViaOrgRoutes:         cfg.ViaOrgRoutes,
	})
	p.residentMints = residentMintsByGrant(cfg.ResidentMints)
	p.vm, p.portRule = cfg.VM, effectivePortRule(cfg.VM, cfg.PortRule)
	p.pushSets, p.pushBase, p.harnessRules, p.harnessCreds, p.mcp = cfg.PushRuleSets, compilePushRules(cfg.PushRuleBase), cfg.HarnessToolRules, cfg.HarnessCredentials, newMCPBroker(cfg.MCPServers, cfg.MCPBindings)
	if p.hardDeny, err = compileHardDeny(cfg.HardDeny); err != nil {
		_ = sink.close(context.Background())
		return nil, fmt.Errorf("hard deny: %w", err)
	}
	// k8s runners only: the proxy is closed until its first renew and lapses
	// for good at the last renew's send time + RunTokenTTL (§6.2).
	if cfg.SelfLapse {
		clock := hooks.lapseClock
		if clock == nil {
			clock = sysBootClock{}
		}
		p.lapse = newLapseGate(clock, selfLapseTTL)
		if sink.remote != nil {
			p.lapse.onLapse = sink.remote.lapse
		}
	}
	if ca != nil && len(cfg.MITMHosts) > 0 {
		slog.InfoContext(ctx, "wardyn-proxy: TLS-MITM also enabled for operator-configured corp artifact host(s) (token injection)",
			slog.Int("host_count", len(cfg.MITMHosts)))
	}

	srv := &http.Server{
		Addr:    cfg.Listen,
		Handler: p,
		// SECURITY: the agent-facing listener is the untrusted side of the
		// boundary. With ReadTimeout 0 there is NO header deadline unless this
		// is set, so a partial-header connection would pin a goroutine
		// forever in a 256 MiB sidecar. Independent of ReadTimeout, and
		// cleared by the CONNECT hijack — tunnels and streaming bodies are
		// unaffected. Matches the inner MITM server.
		ReadHeaderTimeout: 30 * time.Second,
		ReadTimeout:       0, // streaming/tunnels: no whole-request deadline
		WriteTimeout:      0,
		IdleTimeout:       90 * time.Second,
		// Marks the sign-in listener's connections (desktop_signin_lane.go).
		ConnContext: signinConnContext,
	}
	out := &Server{proxy: p, http: srv, sink: sink, allowedPeers: cfg.AllowedPeers, signinListen: cfg.SigninListen}

	// Start the run-token renewer, which keeps this sidecar's short-TTL token
	// fresh for the life of the run and, alongside it, the activity reporter
	// for the pause presence clock.
	hooks.start(out, p, ts, cfg, client)
	return out, nil
}

// startRunTokenRenewer starts the run-token renewer, which keeps this
// sidecar's short-TTL token fresh for the life of the run. Without it every
// control-plane call starts 401ing once the startup token's 1h TTL lapses,
// with no recovery. Starts here so it's running before the first request and
// torn down by Shutdown; NewServer's ctx is a STARTUP context, so the
// renewer gets its own lifetime instead. Alongside it, the activity reporter
// streams the pause's presence clock for as long as the renewer runs.
func startRunTokenRenewer(out *Server, p *Proxy, ts *tokenSource, cfg *Config, client *http.Client, tune lapseRenewTuning) {
	if cfg.ControlPlaneURL == "" {
		return
	}
	rctx, cancel := context.WithCancel(context.Background())
	out.renewStop = cancel
	out.renewStopped = make(chan struct{})
	go func() {
		defer close(out.renewStopped)
		if p.lapse != nil {
			go p.lapse.watch(rctx, tune.watchEvery)
			runSelfLapseRenewer(rctx, ts, cfg.ControlPlaneURL, client, p.lapse, tune)
			return
		}
		runTokenRenewer(rctx, ts, cfg.ControlPlaneURL, client)
	}()
	go runActivityReporter(rctx, &p.streamMoved, ts, cfg.ControlPlaneURL, client, activityReportEvery)
}

// ListenAndServe starts serving and blocks until the server stops.
func (s *Server) ListenAndServe() error {
	// The sign-in lane listens first, so a port it cannot have fails the start.
	if s.signinListen != "" {
		ln, err := s.listen(s.signinListen)
		if err != nil {
			return err
		}
		go func() {
			if err := s.http.Serve(signinListener{ln}); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("wardyn-proxy: the sign-in listener stopped", slog.Any("err", err))
			}
		}()
	}
	ln, err := s.listen(cmp.Or(s.http.Addr, ":http"))
	if err == nil {
		err = s.http.Serve(ln)
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// listen opens addr, serving only AllowedPeers when they are set.
func (s *Server) listen(addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil || len(s.allowedPeers) == 0 {
		return ln, err
	}
	return &peerListener{Listener: ln, allowed: s.allowedPeers}, nil
}

// shutdownDrainReserve is the most of Shutdown's deadline held back for the sink drain.
const shutdownDrainReserve = 2 * time.Second

// Shutdown gracefully stops the HTTP server, stops the token renewer, and drains
// the decision sink.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.renewStop != nil {
		s.renewStop()
		<-s.renewStopped
	}
	// Before http.Shutdown, which waits on open sandbox connections: the
	// runtime's stop timeout may kill the process before anything after it.
	s.proxy.revokeGitHubAPITokens(ctx)
	// http.Shutdown waits on open sandbox connections for as long as it is allowed, so it gets only
	// part of a deadline: the rest is the drain reserve, which the final rows (the memo flush
	// below among them) need to reach the control plane. The drain context also outlives a
	// cancelled ctx, so a signal that cancels the caller does not discard the audit tail.
	reserve := shutdownDrainReserve
	httpCtx := ctx
	if dl, ok := ctx.Deadline(); ok {
		reserve = min(max(time.Until(dl), 0)/4, shutdownDrainReserve)
		var cancel context.CancelFunc
		httpCtx, cancel = context.WithDeadline(ctx, dl.Add(-reserve))
		defer cancel()
	}
	httpErr := s.http.Shutdown(httpCtx)
	// End any open credential re-auth hold: its poll loop is detached from the
	// request that opened it (so a hung-up SDK can't end a hold its owner is
	// still signing in for), which also means nothing else would stop it
	// talking to the control plane about a run that has ended.
	s.proxy.stopReauthHolds()
	// Run end closes every open private-ip streak BEFORE the sink drains, so
	// a repeat count that never hit the eviction path is still recorded.
	s.proxy.flushPrivateIPMemo()
	// The drain keeps whatever the caller's deadline still allows (at least the reserve): a backlog that
	// outlasts the reserve on an idle proxy must still reach the control plane.
	drain := reserve
	if dl, ok := ctx.Deadline(); ok {
		drain = max(time.Until(dl), reserve)
	}
	drainCtx, cancelDrain := context.WithTimeout(context.WithoutCancel(ctx), drain)
	defer cancelDrain()
	sinkErr := s.sink.close(drainCtx)
	// An engine's own secrets leave with it, after its last row is masked: a
	// close that gave up waiting leaves the sink's worker posting its queue, so
	// the key is evicted only once that worker has finished. A sidecar's
	// (uuid.Nil) are the process's and are never evicted.
	if key := s.maskKey; key != uuid.Nil {
		go func() {
			s.sink.wait()
			procRegistry.Evict(key)
		}()
	}
	return errors.Join(httpErr, sinkErr)
}

// Lapsed closes when a self-lapsing proxy lapses; the process must then shut
// down and exit non-zero, never serve again. Nil (never fires) otherwise.
func (s *Server) Lapsed() <-chan struct{} { return s.proxy.lapse.Done() }

// Addr returns the configured listen address.
func (s *Server) Addr() string { return s.http.Addr }

// localInterfaceSubnets returns this process's own interface subnets and
// whether the lookup SUCCEEDED. The second return is the point: a nil slice
// from a failed net.InterfaceAddrs() is indistinguishable from a host with no
// addresses, and an empty exclusion set widens the internal-host lift rather
// than narrowing it. Used only by the lift's own-subnet exclusion.
func localInterfaceSubnets() ([]*net.IPNet, bool) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, false
	}
	out := make([]*net.IPNet, 0, len(addrs))
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			out = append(out, n)
		}
	}
	return out, true
}

// resolveControlPlaneIPs resolves rawURL's host to EVERY address it names,
// mirroring resolveTrustedURL's own resolve step — but runs before any Proxy
// exists, so it can't go through a *Proxy method. The bool reports whether
// the resolve succeeded, since a nil result must fail the exclusion CLOSED
// rather than silently widen the lift.
//
// Every answer, not ips[0]: a wardynd behind two A records had exactly one
// of them excluded — the other was liftable by a declared internal host.
func resolveControlPlaneIPs(rawURL string) ([]net.IP, bool) {
	host, _, err := hostPortFromURL(rawURL)
	if err != nil || host == "" {
		return nil, false
	}
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, true
	}
	ips, err := (netResolver{}).LookupIP(host)
	if err != nil || len(ips) == 0 {
		return nil, false
	}
	return ips, true
}
