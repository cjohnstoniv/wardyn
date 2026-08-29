// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// The sidecar PROCESS: wiring a Proxy from a validated Config, serving it, and
// shutting it down cleanly.
//
// Split from proxy.go when that file crossed the 1000-line gate. A real seam
// rather than a size dodge: everything here is lifecycle — construction,
// listen, shutdown — while proxy.go is the request path itself. The two change
// for different reasons and are read at different times.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/contentscan"
)

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
			slog.WarnContext(ctx, "wardyn-proxy: llm_inspection configured but no effective secret corpus — scanning disabled",
				slog.String("mode", spec.Mode))
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
			procRegistry.AddGlobal(v)
		}
		slog.InfoContext(ctx, "wardyn-proxy: chaining egress through upstream proxy (private-IP guard relaxed for this hop; control-plane bypasses it)",
			slog.String("upstream_addr", up.addr))
	}

	ap := newApprovalClient(cfg.ControlPlaneURL, ts, cfg.RunID, client)
	// 0/absent for either knob keeps configureHold's built-in defaults (30s / 16).
	ap.configureHold(cfg.Policy.FirstUseApproval.Normalize(),
		time.Duration(cfg.Policy.FirstUseHoldSeconds)*time.Second, cfg.Policy.MaxHolds)

	// Corporate CA trust (WARDYN_TRUSTED_CA_FILE, forwarded from wardynd as
	// trusted_ca_pem): additive to the system roots for THIS sidecar's own
	// outbound TLS. mkTransport (proxy.go) shares opts.TLSClientConfig between
	// the forward/egress transport (MITM-terminated forwards + the brokered
	// LLM/git/PAT routes) and the control-plane transport — one config covers
	// both. Nil (unset) leaves it nil, byte-identical to today (system roots,
	// ServerName from URL). applyDefaultsAndValidate already fail-fast-checked
	// this same PEM at config-load time without retaining a pool; this is the
	// live proxy's own parse, matching parseUpstreamProxy's "validate at load,
	// build for real here" split.
	var tlsCfg *tls.Config
	if cfg.TrustedCAPEM != "" {
		pool, perr := x509.SystemCertPool()
		if perr != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM([]byte(cfg.TrustedCAPEM)) {
			_ = sink.close(context.Background())
			return nil, fmt.Errorf("trusted ca: no valid PEM certificates in trusted_ca_pem")
		}
		tlsCfg = &tls.Config{RootCAs: pool}
	}

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
		GitGrants:       cfg.GitGrants,
		PATGrants:       cfg.PATGrants,
		ControlPlaneURL: cfg.ControlPlaneURL,
		RunToken:        ts,
		Upstream:        up,
		TLSClientConfig: tlsCfg,
	})
	if ca != nil && len(cfg.MITMHosts) > 0 {
		slog.InfoContext(ctx, "wardyn-proxy: TLS-MITM also enabled for operator-configured corp artifact host(s) (token injection)",
			slog.Int("host_count", len(cfg.MITMHosts)))
	}

	srv := &http.Server{
		Addr:    cfg.Listen,
		Handler: p,
		// The agent-facing listener is the untrusted side of the boundary. With
		// ReadTimeout 0 there is NO header deadline unless this is set, so a
		// partial-header connection would pin a goroutine forever in a 256 MiB
		// sidecar. Independent of ReadTimeout, and cleared by the CONNECT hijack —
		// tunnels and streaming bodies are unaffected. Matches the inner MITM
		// server (mitm.go).
		ReadHeaderTimeout: 30 * time.Second,
		ReadTimeout:       0, // streaming/tunnels: no whole-request deadline
		WriteTimeout:      0,
		IdleTimeout:       90 * time.Second,
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
