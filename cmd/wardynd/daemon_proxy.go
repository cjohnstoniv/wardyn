// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/net/http/httpproxy"
	"golang.org/x/net/idna"
)

const (
	// daemonProxyInvalidRefusal is the BOOT REFUSAL for a WARDYN_DAEMON_PROXY_URL
	// that fails to parse as a proxy. %s is a CLASSIFICATION of the failure
	// ("not a URL", "an unsupported scheme", "missing a host") — NEVER the raw
	// value or the url.Parse error text, either of which could echo a
	// credential-bearing URL into the boot log.
	daemonProxyInvalidRefusal = "refusing to start: WARDYN_DAEMON_PROXY_URL is %s — " +
		"it decides where wardynd's OWN outbound calls go, including the AWS SSO token renewal that " +
		"carries your client secret and refresh token; fix it or unset it"
	// daemonProxyUserinfoRefusal is the BOOT REFUSAL when the URL embeds a
	// credential — refused at parse, never merely documented, because a
	// credential in the process environment is visible to diagnostics and logs.
	// This refusal applies ONLY to the plain WARDYN_DAEMON_PROXY_URL: a
	// credentialed proxy is reached via WARDYN_DAEMON_PROXY_SECRET (a file
	// path) instead, below.
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
	// daemonProxyAlwaysNoProxyRefusal is the BOOT REFUSAL when WARDYN_DAEMON_PROXY_ALWAYS
	// lists a host but no daemon proxy exists to send it to.
	daemonProxyAlwaysNoProxyRefusal = "refusing to start: WARDYN_DAEMON_PROXY_ALWAYS lists hosts, but no daemon proxy " +
		"is configured (WARDYN_DAEMON_PROXY_URL, WARDYN_DAEMON_PROXY_SECRET or HTTPS_PROXY); set one or unset the list"
	// daemonProxyAlwaysEnvProxyRefusal is the BOOT REFUSAL when HTTPS_PROXY, the only proxy
	// the list can use, is not an address the transport can dial. It never carries the value.
	daemonProxyAlwaysEnvProxyRefusal = "refusing to start: WARDYN_DAEMON_PROXY_ALWAYS lists hosts, but HTTPS_PROXY (or " +
		"https_proxy) is not a usable proxy address; without one those hosts would be reached directly. Fix it, or set " +
		"WARDYN_DAEMON_PROXY_URL"
	// daemonProxyAlwaysDirectHostRefusal is the BOOT REFUSAL when WARDYN_DAEMON_PROXY_ALWAYS is set
	// and a host wardynd always reaches directly cannot be compared with the list's names (it does
	// not convert to its IDNA-ASCII form). Never the host's text.
	daemonProxyAlwaysDirectHostRefusal = "refusing to start: WARDYN_DAEMON_PROXY_ALWAYS lists hosts, but one of the hosts wardynd " +
		"always reaches directly (the Kubernetes API, the AWS SSO override or the internal issuer) is not a valid host name, " +
		"so the list cannot be checked against it; fix that setting or unset the list"
	// daemonProxyAlwaysPortRefusal is the BOOT REFUSAL when the daemon proxy the list uses names a
	// port outside 1 to 65535: every listed host would fail at dial. Never the proxy's text.
	daemonProxyAlwaysPortRefusal = "refusing to start: WARDYN_DAEMON_PROXY_ALWAYS lists hosts, but the daemon proxy's port " +
		"is not a number from 1 to 65535; fix it or unset the list"
	// daemonProxyAlwaysEntryRefusal is the BOOT REFUSAL for one unusable entry. %d is its
	// 1-based position and %s says why. Never the entry's text: a pasted proxy URL carries a
	// credential, and the refusal is written to the daemon log.
	daemonProxyAlwaysEntryRefusal = "refusing to start: WARDYN_DAEMON_PROXY_ALWAYS entry %d is %s — " +
		"the list takes exact host names that WARDYN_DAEMON_NO_PROXY or NO_PROXY bypasses"
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
//
// The returned RoundTripper is the service grant's token-request transport
// (bootDaemonProxy); nil means the shared transport itself. The returned route
// is the always list's, for issuer portals; nil when the list is empty.
func installBootTransport(rt http.RoundTripper, trustedCAPool *x509.CertPool, f *bootFlags) (http.RoundTripper, *alwaysRoute, error) {
	tr, ok := rt.(*http.Transport)
	if !ok {
		return nil, nil, nil
	}
	installTrustedCA(tr, trustedCAPool)
	return bootDaemonProxy(tr, f)
}

// linkLocalCredentialHosts are the metadata endpoints the AWS SDK's default
// credential chain calls over the daemon transport: IMDS, the ECS task role
// endpoint and the EKS Pod Identity agent (IPv4 and IPv6).
var linkLocalCredentialHosts = []string{"169.254.169.254", "169.254.170.2", "169.254.170.23", "fd00:ec2::23", "fd00:ec2::254"}

// bootDaemonProxy is installBootTransport's call-site glue for
// installDaemonProxy/installDaemonProxySecret: resolves the auto-bypass
// hosts from the RAW boot flags (validated Configs don't exist yet at this
// point in boot — see installDaemonProxy's doc comment), refuses boot if
// both WARDYN_DAEMON_PROXY_URL and WARDYN_DAEMON_PROXY_SECRET name a proxy,
// and logs the outcome. One boot log line names the proxy host (never any
// userinfo or file content — neither install path ever hands one back
// through effective's scheme://host display) and the effective bypass list,
// so an operator can grep for it.
//
// It also returns the transport for the service grant's token request
// (serviceGrantTransport) and the always list's route (resolveAlwaysRoute):
// both nil when WARDYN_DAEMON_PROXY_ALWAYS is empty, which leaves that request
// on the shared transport exactly as before and every portal fetch direct.
func bootDaemonProxy(tr *http.Transport, f *bootFlags) (http.RoundTripper, *alwaysRoute, error) {
	autoBypass := []string{
		os.Getenv("KUBERNETES_SERVICE_HOST"),
		daemonProxyBypassHost(*f.awsSSOEndpointOverride),
		daemonProxyBypassHost(*f.oidcInternalIss),
	}
	// Link-local credential endpoints (EC2 IMDS, ECS task role, EKS Pod Identity
	// agent) are reached only from the host itself, never through a proxy.
	autoBypass = append(autoBypass, linkLocalCredentialHosts...)

	// A portal internal issuer host is reached directly too, so the list may not name it; it is not added to
	// the bypass list.
	direct := slices.Clone(autoBypass)
	if f.portalInternalHosts != nil {
		direct = append(direct, portalInternalHostList(*f.portalInternalHosts)...)
	}
	always, err := parseDaemonProxyAlways(f.daemonProxyAlways, direct)
	if err != nil {
		return nil, nil, err
	}

	plainURL := strings.TrimSpace(*f.daemonProxyURL)
	secretFile := strings.TrimSpace(*f.daemonProxySecretFile)
	if plainURL != "" && secretFile != "" {
		return nil, nil, errors.New(daemonProxyBothSetRefusal)
	}

	var effective, source string
	if secretFile != "" {
		source = "WARDYN_DAEMON_PROXY_SECRET"
		effective, err = installDaemonProxySecret(tr, secretFile, *f.daemonNoProxy, autoBypass...)
	} else {
		source = "WARDYN_DAEMON_PROXY_URL"
		effective, err = installDaemonProxy(tr, plainURL, *f.daemonNoProxy, autoBypass...)
	}
	if err != nil {
		return nil, nil, err
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
			slog.String("auto_bypass", strings.Join(filterEmpty(autoBypass), ",")),
			slog.String("always", strings.Join(sortedHosts(always), ",")))
	}
	var daemonProxy *url.URL
	if effective != "" {
		daemonProxy, _ = url.Parse(effective) // effective is u.String() of a URL parsed above
	}
	route, err := resolveAlwaysRoute(tr, daemonProxy, always)
	if err != nil {
		return nil, nil, err
	}
	grant, err := serviceGrantTransport(tr, route)
	if err != nil {
		return nil, nil, err
	}
	return grant, route, nil
}

// parseDaemonProxyAlways reads WARDYN_DAEMON_PROXY_ALWAYS: comma-separated EXACT
// host names, lower-cased with any trailing dot removed. No suffix and no
// wildcard is understood, so `login.corp.example` never covers
// `x.login.corp.example` or `corp.example`. raw is nil in a bootFlags that
// does not carry the flag (a test's partial flags); direct is the hosts
// wardynd always reaches without the proxy (the autoBypass list), which the
// list may not name. The result is empty when the variable is.
func parseDaemonProxyAlways(raw *string, direct []string) (map[string]struct{}, error) {
	if raw == nil {
		return nil, nil
	}
	out := map[string]struct{}{}
	var entries []string
	for _, entry := range strings.Split(*raw, ",") {
		entries = append(entries, strings.TrimSpace(entry))
	}
	listed := false
	for _, e := range entries {
		listed = listed || e != ""
	}
	if !listed {
		return out, nil
	}
	// The hosts wardynd always reaches directly are compared with an entry, and with the
	// request's host, in one form: IDNA-ASCII, lower-case, no trailing dot, no port. One that
	// does not convert cannot be checked, so boot is refused rather than leaving it unprotected.
	skip := map[string]struct{}{}
	for _, h := range direct {
		if strings.TrimSpace(h) == "" {
			continue
		}
		if hostPart, _, err := net.SplitHostPort(h); err == nil {
			h = hostPart
		}
		c, err := asciiProxyHost(h)
		if err != nil {
			return nil, errors.New(daemonProxyAlwaysDirectHostRefusal)
		}
		skip[c] = struct{}{}
	}
	for i, entry := range entries {
		if entry == "" {
			continue
		}
		host := normalizeProxyHost(entry)
		why := ""
		canon, cerr := asciiProxyHost(entry)
		_, protected := skip[canon]
		switch {
		case net.ParseIP(strings.Trim(host, "[]")) != nil:
			why = "an IP address"
		case host == "localhost":
			why = "localhost"
		case cerr == nil && protected:
			why = "a host wardynd always reaches directly (the Kubernetes API, the AWS SSO override, the internal issuer, a portal internal issuer host or a link-local credential endpoint)"
		case !isFullHostName(host):
			why = "not an exact host name (no port, path, suffix, wildcard or other text)"
		case numericHostName(host):
			why = "a numeric name (an address spelled as a name), not a host name"
		}
		if why != "" {
			return nil, fmt.Errorf(daemonProxyAlwaysEntryRefusal, i+1, why)
		}
		out[host] = struct{}{}
	}
	return out, nil
}

// parseEnvironmentProxy reads an HTTPS_PROXY value the way x/net's httpproxy does: a
// URL with a host, or the host:port shorthand (no "://"), which httpproxy retries with
// "http://" in front. It accepts only what the transport can dial (an http, https or
// socks5 scheme and a host), and refuses what httpproxy would turn into a proxy that
// does not exist, such as a value with "://" that is not a URL.
func parseEnvironmentProxy(raw string) (*url.URL, bool) {
	u, err := url.Parse(raw)
	if (err != nil || u.Scheme == "" || u.Host == "") && !strings.Contains(raw, "://") {
		if u2, err2 := url.Parse("http://" + raw); err2 == nil && (u2.Path == "" || u2.Path == "/") {
			u, err = u2, nil
		}
	}
	if err != nil || u.Hostname() == "" || !proxyPortUsable(u) {
		return nil, false
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
		return u, true
	}
	return nil, false
}

// proxyPortUsable reports whether u's explicit port, if it has one, is a number from 1 to 65535.
// url.Parse accepts any digits, so 0 and 70000 would boot and fail at every dial.
func proxyPortUsable(u *url.URL) bool {
	port := u.Port()
	if port == "" {
		return true
	}
	n, err := strconv.Atoi(port)
	return err == nil && n >= 1 && n <= 65535
}

// isFullHostName reports whether host (already lower-cased, one trailing dot removed) is a
// DNS host name: 1 to 253 characters in dot-separated labels of 1 to 63 characters from
// a-z, 0-9 and "-", none starting or ending with "-". A single label is allowed: the list
// takes exact host names, and a cluster-internal name has no dot. An internationalised
// name is listed in its punycode form.
func isFullHostName(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			if c := label[i]; (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}
	return true
}

// numericHostName reports whether host (already a valid DNS name) is one a resolver reads as
// an address: its last label is all digits (127.1, 2130706433) or starts with 0x (0x7f.0.0.1).
func numericHostName(host string) bool {
	last := host[strings.LastIndexByte(host, '.')+1:]
	if strings.HasPrefix(last, "0x") {
		return true
	}
	for i := 0; i < len(last); i++ {
		if last[i] < '0' || last[i] > '9' {
			return false
		}
	}
	return true
}

// asciiProxyHost is the one form a host is compared in, for the list, the hosts wardynd always
// reaches directly and a request's host alike: net/http converts a non-ASCII host to its
// IDNA-ASCII form before it dials, so the comparison does too (a fullwidth letter, an ideographic
// dot or a U-label spells the same host), then lower-cases it and drops a trailing dot. A host
// that does not convert is an error: a request is never sent direct over it and boot is refused.
func asciiProxyHost(host string) (string, error) {
	for i := 0; i < len(host); i++ {
		if host[i] >= 0x80 {
			ascii, err := idna.Lookup.ToASCII(host)
			if err != nil {
				return "", fmt.Errorf("service grant token host is not a valid internationalised host name: %w", err)
			}
			host = ascii
			break
		}
	}
	return normalizeProxyHost(host), nil
}

// normalizeProxyHost lower-cases a host and drops one trailing dot, the form a
// request URL's host is compared in.
func normalizeProxyHost(h string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
}

func sortedHosts(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for h := range set {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// alwaysRoute is the always list with the proxy it uses: the service grant's
// token request and an issuer portal's discovery and key fetches share it.
type alwaysRoute struct {
	proxy      *url.URL
	hosts      map[string]struct{}
	proxyRoots *x509.CertPool // an https proxy's own certificate: the shared transport's roots
}

// listed reports whether a host is on the always list, compared in the list's
// one form (asciiProxyHost); a host that does not convert is an error.
func (r *alwaysRoute) listed(host string) (bool, error) {
	h, err := asciiProxyHost(host)
	if err != nil {
		return false, err
	}
	_, ok := r.hosts[h]
	return ok, nil
}

// resolveAlwaysRoute resolves the proxy the always hosts go to:
// daemonProxy (WARDYN_DAEMON_PROXY_URL or _SECRET) or, when that is nil,
// HTTPS_PROXY. With neither, or an HTTPS_PROXY that is not a usable address,
// boot is refused; a listed host never goes direct. An empty list returns nil.
func resolveAlwaysRoute(tr *http.Transport, daemonProxy *url.URL, always map[string]struct{}) (*alwaysRoute, error) {
	if len(always) == 0 {
		return nil, nil
	}
	// The proxy is resolved and validated here, once, so a listed host is sent to a
	// usable proxy or boot is refused: httpproxy's own function drops a value it
	// cannot parse and then answers "direct".
	proxy := daemonProxy
	if proxy != nil && !proxyPortUsable(proxy) {
		return nil, errors.New(daemonProxyAlwaysPortRefusal)
	}
	if proxy == nil {
		env := httpproxy.FromEnvironment().HTTPSProxy
		if env == "" {
			return nil, errors.New(daemonProxyAlwaysNoProxyRefusal)
		}
		var ok bool
		if proxy, ok = parseEnvironmentProxy(env); !ok {
			return nil, errors.New(daemonProxyAlwaysEnvProxyRefusal)
		}
	}
	route := &alwaysRoute{proxy: proxy, hosts: always}
	if tr.TLSClientConfig != nil {
		route.proxyRoots = tr.TLSClientConfig.RootCAs
	}
	return route, nil
}

// serviceGrantTransport is the transport of the service grant's token request:
// a clone of the shared transport (so it keeps the trusted CA roots
// installBootTransport put on it) whose Proxy function sends a host in always
// to the daemon proxy, ignoring WARDYN_DAEMON_NO_PROXY and NO_PROXY, and every
// other host wherever the shared transport's own function sends it. The shared
// transport itself is not changed, so every other daemon call, including the
// hosts wardynd reaches directly, routes as it did before the list existed.
//
// The always hosts go to the route's proxy (resolveAlwaysRoute). A nil route
// (an empty list) returns nil: the request uses the shared transport.
func serviceGrantTransport(tr *http.Transport, route *alwaysRoute) (http.RoundTripper, error) {
	if route == nil {
		return nil, nil
	}
	proxy, always := route.proxy, route.hosts
	rest := tr.Proxy
	clone := tr.Clone()
	clone.Proxy = func(req *http.Request) (*url.URL, error) {
		host, err := asciiProxyHost(req.URL.Hostname())
		if err != nil {
			return nil, err
		}
		if _, ok := always[host]; ok {
			return proxy, nil
		}
		if rest == nil {
			return nil, nil
		}
		return rest(req)
	}
	return clone, nil
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
