// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// The sidecar PROCESS: wiring a Proxy from a validated Config, serving it, and
// shutting it down cleanly. A real seam from proxy.go, not a size dodge:
// everything here is lifecycle — construction, listen, shutdown — while
// proxy.go is the request path itself.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
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
}

// NewServer wires a Proxy from a validated Config and dependencies. It mints
// injection credentials once (fail-closed on error) before returning.
func NewServer(ctx context.Context, cfg *Config, client *http.Client, stdout io.Writer) (*Server, error) {
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
	var tlsCfg *tls.Config
	if cfg.TrustedCAPEM != "" {
		pool, perr := x509.SystemCertPool()
		if perr != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM([]byte(cfg.TrustedCAPEM)) {
			return nil, fmt.Errorf("trusted ca: no valid PEM certificates in trusted_ca_pem")
		}
		tlsCfg = &tls.Config{RootCAs: pool}
	}
	cpTLS, err := hoptls.ClientConfig(cfg.ControlPlaneCAPEM)
	if err != nil {
		return nil, err
	}
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

	sink := newDecisionSink(cfg.ControlPlaneURL, ts, cfg.DecisionBufferSize, client, stdout)

	inj, err := buildInjector(ctx, cfg.ControlPlaneURL, ts, pol, cfg.Injection, client)
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
			procMask([]byte(v))
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
			slog.WarnContext(ctx, "wardyn-proxy: llm_inspection configured but no effective secret corpus — scanning disabled",
				slog.String("mode", spec.Mode))
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
			procMask(v)
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

	ap := newApprovalClient(cfg.ControlPlaneURL, ts, cfg.RunID, client)
	// 0/absent for either knob keeps configureHold's built-in defaults (30s / 16).
	ap.configureHold(cfg.Policy.FirstUseApproval.Normalize(),
		time.Duration(cfg.Policy.FirstUseHoldSeconds)*time.Second, cfg.Policy.MaxHolds)

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
		ControlPlaneURL:      cfg.ControlPlaneURL,
		RunToken:             ts,
		Upstream:             up,
		UpstreamNoProxy:      cfg.UpstreamProxyNoProxy,
		TLSClientConfig:      tlsCfg,
		ControlTLS:           cpTLS,
		InternalHosts:        cfg.InternalHosts,
		LLMUpstreams:         cfg.LLMUpstreams,
		LLMUnavailableDetail: cfg.LLMUnavailableDetail,
		Unattended:           cfg.Unattended,
		LocalSubnets:         localSubnets,
		ControlPlaneIPs:      cpIPs,
		ExclusionUnknown:     exclusionUnknown,
	})
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
	}
	out := &Server{proxy: p, http: srv, sink: sink}

	// Start the run-token renewer, which keeps this sidecar's short-TTL token
	// fresh for the life of the run and, alongside it, the activity reporter
	// for the pause presence clock.
	startRunTokenRenewer(out, p, ts, cfg, client)
	return out, nil
}

// startRunTokenRenewer starts the run-token renewer, which keeps this
// sidecar's short-TTL token fresh for the life of the run. Without it every
// control-plane call starts 401ing once the startup token's 1h TTL lapses,
// with no recovery. Starts here so it's running before the first request and
// torn down by Shutdown; NewServer's ctx is a STARTUP context, so the
// renewer gets its own lifetime instead. Alongside it, the activity reporter
// streams the pause's presence clock for as long as the renewer runs.
func startRunTokenRenewer(out *Server, p *Proxy, ts *tokenSource, cfg *Config, client *http.Client) {
	if cfg.ControlPlaneURL == "" {
		return
	}
	rctx, cancel := context.WithCancel(context.Background())
	out.renewStop = cancel
	out.renewStopped = make(chan struct{})
	go func() {
		defer close(out.renewStopped)
		runTokenRenewer(rctx, ts, cfg.ControlPlaneURL, client)
	}()
	go runActivityReporter(rctx, &p.streamMoved, ts, cfg.ControlPlaneURL, client, activityReportEvery)
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
	// End any open credential re-auth hold: its poll loop is detached from the
	// request that opened it (so a hung-up SDK can't end a hold its owner is
	// still signing in for), which also means nothing else would stop it
	// talking to the control plane about a run that has ended.
	s.proxy.stopReauthHolds()
	// Run end closes every open private-ip streak BEFORE the sink drains, so
	// a repeat count that never hit the eviction path is still recorded.
	s.proxy.flushPrivateIPMemo()
	sinkErr := s.sink.close(ctx)
	if httpErr != nil {
		return httpErr
	}
	return sinkErr
}

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
