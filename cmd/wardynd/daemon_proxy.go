// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"

	"golang.org/x/net/http/httpproxy"
)

// DRAFT (M2 canon pending)

const (
	// daemonProxyInvalidRefusal is the BOOT REFUSAL for a WARDYN_DAEMON_PROXY_URL
	// that fails to parse as a proxy. %s is a CLASSIFICATION of the failure
	// ("not a URL", "an unsupported scheme", "missing a host") — NEVER the raw
	// value or the url.Parse error text, either of which could echo a
	// credential-bearing URL into the boot log.
	//
	// DRAFT (M2 canon pending)
	daemonProxyInvalidRefusal = "refusing to start: WARDYN_DAEMON_PROXY_URL is %s — " +
		"it decides where wardynd's OWN outbound calls go, including the AWS SSO token renewal that " +
		"carries your client secret and refresh token; fix it or unset it"
	// daemonProxyUserinfoRefusal is the BOOT REFUSAL when the URL embeds a
	// credential — refused at parse, never merely documented, because a
	// credential in the process environment is visible to diagnostics and logs.
	// This refusal applies ONLY to the plain WARDYN_DAEMON_PROXY_URL: a
	// credentialed proxy is reached via WARDYN_DAEMON_PROXY_SECRET (a file
	// path) instead, below.
	//
	// DRAFT (M2 canon pending)
	daemonProxyUserinfoRefusal = "refusing to start: WARDYN_DAEMON_PROXY_URL must not embed a " +
		"credential (user:pass@) — put the credential in a file and point WARDYN_DAEMON_PROXY_SECRET at it, " +
		"or put the proxy's credential in the proxy"
	// daemonProxySecretInvalidRefusal is daemonProxyInvalidRefusal's sibling
	// for the WARDYN_DAEMON_PROXY_SECRET file's CONTENT. Same classification
	// vocabulary, never the raw content (which may carry a credential).
	daemonProxySecretInvalidRefusal = "refusing to start: the file WARDYN_DAEMON_PROXY_SECRET points at is %s — " +
		"it must hold exactly one http:// or https:// proxy URL (user:pass@ allowed); fix it or unset the var"
	// daemonProxySecretWritableRefusal is the BOOT REFUSAL when the
	// WARDYN_DAEMON_PROXY_SECRET file is group- or world-writable: anyone in
	// that set could swap the proxy (and its credential) before the next boot,
	// and no supported delivery produces it.
	daemonProxySecretWritableRefusal = "refusing to start: WARDYN_DAEMON_PROXY_SECRET file %q is mode %04o — " +
		"a group- or world-writable secret file lets someone else replace it; remove the write bits (chmod 640)"
	// daemonProxySecretReadableRefusal is the BOOT REFUSAL when the file is
	// other-readable AND owned by wardynd's own non-root uid — the hand-made
	// host file, where any local user could read the credential.
	daemonProxySecretReadableRefusal = "refusing to start: WARDYN_DAEMON_PROXY_SECRET file %q is mode %04o and owned by " +
		"wardynd's own uid %d — any local user can read it; chmod 640 it"
	// daemonProxyBothSetRefusal is the BOOT REFUSAL when both
	// WARDYN_DAEMON_PROXY_URL and WARDYN_DAEMON_PROXY_SECRET name a proxy —
	// refused rather than picking one, because a silent precedence between
	// two ways of saying the same thing is how an operator ends up proxying
	// through something they did not intend.
	daemonProxyBothSetRefusal = "refusing to start: both WARDYN_DAEMON_PROXY_URL and WARDYN_DAEMON_PROXY_SECRET " +
		"are set — unset one; they name the same knob two different ways"
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
	u, classification := classifyProxyURLShape(rawURL)
	if classification != "" {
		return "", fmt.Errorf(daemonProxyInvalidRefusal, classification)
	}
	if u.User != nil {
		return "", errors.New(daemonProxyUserinfoRefusal)
	}
	return wireDaemonProxy(tr, u, noProxy, autoBypass...), nil
}

// installDaemonProxySecret is installDaemonProxy's sibling for
// WARDYN_DAEMON_PROXY_SECRET — a FILE PATH (never a value, so it can carry a
// credential without one ever landing in the process environment) holding
// exactly one proxy URL that MAY embed user:pass@. It exists because the
// transport this wires is installed before the database connects and before
// the secret store is built (see installBootTransport's call site in
// run()), so a secret-store reference cannot be resolved here — a file is
// the only credentialed form boot ordering allows.
//
// secretPath is WARDYN_DAEMON_PROXY_SECRET. Empty (the default) leaves
// tr.Proxy untouched, exactly like installDaemonProxy's own empty case. The
// caller (bootDaemonProxy) refuses boot before this runs if BOTH
// WARDYN_DAEMON_PROXY_URL and WARDYN_DAEMON_PROXY_SECRET are set, so this
// function never has to arbitrate between them.
//
// The file's mode is checked (daemonProxySecretMode) on the OPENED
// descriptor, so the file checked is the file read, and its content is read
// fresh at boot, never cached or re-read later — same "read once at boot, not
// deferred" posture as WARDYN_TRUSTED_CA_FILE.
func installDaemonProxySecret(tr *http.Transport, secretPath, noProxy string, autoBypass ...string) (effective string, err error) {
	secretPath = strings.TrimSpace(secretPath)
	if secretPath == "" {
		return "", nil
	}
	f, err := os.Open(secretPath)
	if err != nil {
		return "", fmt.Errorf("WARDYN_DAEMON_PROXY_SECRET: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("WARDYN_DAEMON_PROXY_SECRET: %w", err)
	}
	owner := -1
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		owner = int(st.Uid)
	}
	if err := daemonProxySecretMode(secretPath, fi.Mode().Perm(), owner, os.Geteuid()); err != nil {
		return "", err
	}
	raw, rerr := io.ReadAll(f)
	if rerr != nil {
		return "", fmt.Errorf("WARDYN_DAEMON_PROXY_SECRET: %w", rerr)
	}
	rawURL := strings.TrimSpace(string(raw))
	if rawURL == "" {
		return "", fmt.Errorf(daemonProxySecretInvalidRefusal, "empty")
	}
	u, classification := classifyProxyURLShape(rawURL)
	if classification != "" {
		return "", fmt.Errorf(daemonProxySecretInvalidRefusal, classification)
	}
	// Userinfo is DELIBERATELY allowed here — this file is exactly the
	// credentialed form WARDYN_DAEMON_PROXY_URL's userinfo refusal exists to
	// push operators toward.
	return wireDaemonProxy(tr, u, noProxy, autoBypass...), nil
}

// daemonProxySecretMode is the WARDYN_DAEMON_PROXY_SECRET mode rule, split
// out so every delivery shape is testable without chown. owner is the file's
// uid (-1 when unknown).
//
// Group- or world-WRITABLE is always refused. Group-READ is allowed: a
// Kubernetes Secret or projected volume under the chart's fsGroup is 0440.
// Other-READABLE is refused only on a file wardynd's own non-root uid owns —
// the hand-made host file — and allowed where a supported mechanism produces
// it: a Secrets Store CSI file is root-owned 0644, reachable by a non-root
// reader only through the other-read bit.
func daemonProxySecretMode(path string, perm os.FileMode, owner, euid int) error {
	if perm&0o022 != 0 {
		return fmt.Errorf(daemonProxySecretWritableRefusal, path, perm)
	}
	if perm&0o004 != 0 && euid != 0 && owner == euid {
		return fmt.Errorf(daemonProxySecretReadableRefusal, path, perm, euid)
	}
	return nil
}

// classifyProxyURLShape parses rawURL and reports whether its SHAPE (scheme,
// host) is a usable proxy target, shared by installDaemonProxy and
// installDaemonProxySecret so both refuse the same malformed shapes the same
// way. The returned classification ("not a URL", "an unsupported scheme",
// "missing a host") is safe to embed in a boot refusal — never the raw value
// or the url.Parse error text, either of which could echo a credential.
// Userinfo is NOT judged here — each caller decides whether to allow it.
func classifyProxyURLShape(rawURL string) (u *url.URL, classification string) {
	pu, perr := url.Parse(rawURL)
	if perr != nil || pu.Scheme == "" || pu.Host == "" {
		return nil, "not a URL"
	}
	if pu.Scheme != "http" && pu.Scheme != "https" {
		return nil, "an unsupported scheme"
	}
	if pu.Hostname() == "" {
		return nil, "missing a host"
	}
	return pu, ""
}

// wireDaemonProxy is installDaemonProxy's and installDaemonProxySecret's
// shared last step: build the NO_PROXY-aware proxy func from an ALREADY
// VALIDATED proxy URL and assign it to tr.Proxy.
func wireDaemonProxy(tr *http.Transport, u *url.URL, noProxy string, autoBypass ...string) string {
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
	return u.String()
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
// installDaemonProxy/installDaemonProxySecret: resolves the auto-bypass
// hosts from the RAW boot flags (validated Configs don't exist yet at this
// point in boot — see installDaemonProxy's doc comment), refuses boot if
// both WARDYN_DAEMON_PROXY_URL and WARDYN_DAEMON_PROXY_SECRET name a proxy,
// and logs the outcome. One boot log line names the proxy host (never any
// userinfo or file content — neither install path ever hands one back
// through effective's scheme://host display) and the effective bypass list,
// so an operator can grep for it.
func bootDaemonProxy(tr *http.Transport, f *bootFlags) error {
	autoBypass := []string{
		os.Getenv("KUBERNETES_SERVICE_HOST"),
		daemonProxyBypassHost(*f.awsSSOEndpointOverride),
		daemonProxyBypassHost(*f.oidcInternalIss),
	}

	plainURL := strings.TrimSpace(*f.daemonProxyURL)
	secretFile := strings.TrimSpace(*f.daemonProxySecretFile)
	if plainURL != "" && secretFile != "" {
		return errors.New(daemonProxyBothSetRefusal)
	}

	var effective, source string
	var err error
	if secretFile != "" {
		source = "WARDYN_DAEMON_PROXY_SECRET"
		effective, err = installDaemonProxySecret(tr, secretFile, *f.daemonNoProxy, autoBypass...)
	} else {
		source = "WARDYN_DAEMON_PROXY_URL"
		effective, err = installDaemonProxy(tr, plainURL, *f.daemonNoProxy, autoBypass...)
	}
	if err != nil {
		return err
	}
	if effective != "" {
		// scheme://host only — never u.String()'s full form, which could carry
		// a query string or userinfo (the secret-file path allows userinfo) into
		// the boot log.
		display := effective
		if u, perr := url.Parse(effective); perr == nil {
			display = u.Scheme + "://" + u.Host
		}
		slog.Info("wardynd: daemon egress proxy configured ("+source+")",
			slog.String("proxy", display),
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
