// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"

	"golang.org/x/net/http/httpproxy"
)

// ── DRAFT (M2 canon pending) ────────────────────────────────────────────────

const (
	// daemonProxyInvalidRefusal is the BOOT REFUSAL for a WARDYN_DAEMON_PROXY_URL
	// that fails to parse as a proxy. %s is a CLASSIFICATION of the failure
	// ("not a URL", "an unsupported scheme", "missing a host") — NEVER the raw
	// value or the url.Parse error text, either of which could echo a
	// credential-bearing URL into the boot log (Codex #13).
	//
	// DRAFT (M2 canon pending)
	daemonProxyInvalidRefusal = "refusing to start: WARDYN_DAEMON_PROXY_URL is %s — " +
		"it decides where wardynd's OWN outbound calls go, including the AWS SSO token renewal that " +
		"carries your client secret and refresh token; fix it or unset it"
	// daemonProxyUserinfoRefusal is the BOOT REFUSAL when the URL embeds a
	// credential — refused at parse, never merely documented, because a
	// credential in the process environment is visible to diagnostics and logs.
	//
	// DRAFT (M2 canon pending)
	daemonProxyUserinfoRefusal = "refusing to start: WARDYN_DAEMON_PROXY_URL must not embed a " +
		"credential (user:pass@) — put the proxy's credential in the proxy, or open an issue for a secret-ref form"
)

// installDaemonProxy sets tr.Proxy from WARDYN_DAEMON_PROXY_URL, scoped to the
// daemon's own outbound calls that ride the shared http.DefaultTransport
// (OIDC discovery/JWKS, audit webhooks, GitHub App minting, AWS SSO
// CreateToken renewal, Entra sync) — the docker client (unix socket) and
// client-go (its own transport, its own env read) never see this. Lives
// beside installTrustedCA on purpose: same call site, same "mutate the one
// process-global transport in place" shape, same refuse-boot-on-malformed
// posture as WARDYN_TRUSTED_CA_FILE.
//
// rawURL is WARDYN_DAEMON_PROXY_URL. Empty (the default) leaves tr.Proxy
// UNTOUCHED — today's http.ProxyFromEnvironment stays exactly as it is; this
// knob does not clear or reject the standard HTTP_PROXY/HTTPS_PROXY/NO_PROXY
// variables, it only adds a scoped alternative (docs/ENV.md).
//
// noProxy is the FULLY resolved NO_PROXY-spelled bypass list — the caller
// (bootDaemonProxy, below) is responsible for appending WARDYN_DAEMON_NO_PROXY
// plus every auto-bypass host it already knows before this runs
// (KUBERNETES_SERVICE_HOST, the WARDYN_AWS_SSO_ENDPOINT_OVERRIDE host, the
// OIDC WARDYN_OIDC_INTERNAL_ISSUER host) via autoBypass, so this function
// stays a pure "validate + wire the transport" seam a test can drive with
// plain host strings instead of real env vars.
//
// Matching is x/net/http/httpproxy's NO_PROXY semantics, never a hand-rolled
// matcher — the same package net/http.ProxyFromEnvironment itself uses.
func installDaemonProxy(tr *http.Transport, rawURL, noProxy string, autoBypass ...string) (effective string, err error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return "", nil
	}
	u, perr := url.Parse(rawURL)
	if perr != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf(daemonProxyInvalidRefusal, "not a URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf(daemonProxyInvalidRefusal, "an unsupported scheme")
	}
	if u.Hostname() == "" {
		return "", fmt.Errorf(daemonProxyInvalidRefusal, "missing a host")
	}
	if u.User != nil {
		return "", errors.New(daemonProxyUserinfoRefusal)
	}

	parts := filterEmpty(append([]string{noProxy}, autoBypass...))
	cfg := &httpproxy.Config{
		HTTPProxy:  u.String(),
		HTTPSProxy: u.String(),
		NoProxy:    strings.Join(parts, ","),
	}
	proxyFunc := cfg.ProxyFunc()
	tr.Proxy = func(req *http.Request) (*url.URL, error) {
		return proxyFunc(req.URL)
	}
	return u.String(), nil
}

// installBootTransport is run()'s single call site for both transport
// mutations (installTrustedCA, WARDYN_DAEMON_PROXY_URL) — extracted so run()
// itself stays at ONE branch here (the error check) instead of two (the type
// assertion's `ok` plus the proxy error), keeping it under the gocyclo
// ratchet its doc comment already claims ("Low branching... just long"). rt
// is http.DefaultTransport, passed in rather than read directly so a test can
// drive both the *http.Transport and non-Transport paths without touching
// process-global state. A DefaultTransport that is not a *http.Transport
// (never true in this binary) is a silent no-op, matching installTrustedCA's
// own existing behavior.
func installBootTransport(rt http.RoundTripper, trustedCAPool *x509.CertPool, f *bootFlags) error {
	tr, ok := rt.(*http.Transport)
	if !ok {
		return nil
	}
	installTrustedCA(tr, trustedCAPool)
	return bootDaemonProxy(tr, f)
}

// bootDaemonProxy is installBootTransport's call-site glue for
// installDaemonProxy: resolves the auto-bypass hosts from the RAW boot flags
// (validated Configs don't exist yet at this point in boot — see
// installDaemonProxy's doc comment) and logs the outcome. One boot log line
// names the proxy host (never any userinfo — installDaemonProxy already
// refused one) and the effective bypass list, so an operator can grep for it.
func bootDaemonProxy(tr *http.Transport, f *bootFlags) error {
	autoBypass := []string{
		os.Getenv("KUBERNETES_SERVICE_HOST"),
		daemonProxyBypassHost(*f.awsSSOEndpointOverride),
		daemonProxyBypassHost(*f.oidcInternalIss),
	}
	effective, err := installDaemonProxy(tr, *f.daemonProxyURL, *f.daemonNoProxy, autoBypass...)
	if err != nil {
		return err
	}
	if effective != "" {
		slog.Info("wardynd: daemon egress proxy configured (WARDYN_DAEMON_PROXY_URL)",
			slog.String("proxy", effective),
			slog.String("no_proxy", *f.daemonNoProxy),
			slog.String("auto_bypass", strings.Join(filterEmpty(autoBypass), ",")))
	}
	return nil
}

// filterEmpty drops blank entries (after trimming), preserving order — used
// to build both the NO_PROXY string installDaemonProxy hands to httpproxy and
// the boot log line naming what actually got bypassed.
func filterEmpty(ss []string) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// daemonProxyBypassHost best-effort extracts a hostname from a raw URL (or a
// bare host) for the auto-bypass list — "" on empty or unparseable input. It
// never errors: a malformed WARDYN_AWS_SSO_ENDPOINT_OVERRIDE or
// WARDYN_OIDC_INTERNAL_ISSUER is refused by ITS OWN validator elsewhere in
// boot; this helper only feeds the daemon-proxy bypass list and degrades to
// "add nothing" rather than a second boot failure over the same typo.
func daemonProxyBypassHost(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	return u.Hostname()
}
