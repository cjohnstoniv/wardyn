// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/x509"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// freshTransport returns a *http.Transport with the SAME zero-value shape
// http.DefaultTransport starts with (Proxy: ProxyFromEnvironment), so tests
// can assert "untouched" by identity of the function value's behavior rather
// than a literal pointer comparison (function values are not comparable).
func freshTransport() *http.Transport {
	return &http.Transport{Proxy: http.ProxyFromEnvironment}
}

func TestInstallDaemonProxy_UnsetLeavesTransportUntouched(t *testing.T) {
	tr := freshTransport()
	before := tr.Proxy
	eff, err := installDaemonProxy(tr, "", "")
	if err != nil {
		t.Fatalf("installDaemonProxy(unset) error: %v", err)
	}
	if eff != "" {
		t.Fatalf("effective = %q, want empty", eff)
	}
	// http.ProxyFromEnvironment and any func we might assign do not compare
	// with ==, so prove untouched by checking Proxy is STILL the exact value
	// it started as (fresh clone identity, not a new closure).
	got := tr.Proxy
	wantSame := reflectSameFunc(before, got)
	if !wantSame {
		t.Fatalf("tr.Proxy was reassigned on an unset WARDYN_DAEMON_PROXY_URL")
	}
}

func TestInstallDaemonProxy_ValidURLProxiesExternalBypassesListed(t *testing.T) {
	tr := freshTransport()
	eff, err := installDaemonProxy(tr, "http://proxy.corp.example:3128", "internal.example,.svc.cluster.local")
	if err != nil {
		t.Fatalf("installDaemonProxy: %v", err)
	}
	if eff != "http://proxy.corp.example:3128" {
		t.Fatalf("effective = %q, want the normalized proxy URL", eff)
	}
	if tr.Proxy == nil {
		t.Fatal("tr.Proxy is nil, want a proxy func")
	}

	extReq := &http.Request{URL: mustParseURL(t, "https://sso-oidc.us-east-1.amazonaws.com/token")}
	got, err := tr.Proxy(extReq)
	if err != nil {
		t.Fatalf("Proxy(external): %v", err)
	}
	if got == nil || got.Host != "proxy.corp.example:3128" {
		t.Fatalf("Proxy(external) = %v, want the configured proxy", got)
	}

	bypassReq := &http.Request{URL: mustParseURL(t, "https://internal.example/api")}
	got, err = tr.Proxy(bypassReq)
	if err != nil {
		t.Fatalf("Proxy(bypassed): %v", err)
	}
	if got != nil {
		t.Fatalf("Proxy(bypassed) = %v, want nil (NO_PROXY match)", got)
	}
}

func TestInstallDaemonProxy_KubernetesServiceHostAutoBypassed(t *testing.T) {
	tr := freshTransport()
	_, err := installDaemonProxy(tr, "http://proxy.corp.example:3128", "", "10.96.0.1")
	if err != nil {
		t.Fatalf("installDaemonProxy: %v", err)
	}
	req := &http.Request{URL: mustParseURL(t, "https://10.96.0.1:443/api")}
	got, err := tr.Proxy(req)
	if err != nil {
		t.Fatalf("Proxy(k8s host): %v", err)
	}
	if got != nil {
		t.Fatalf("Proxy(k8s host) = %v, want nil (auto-bypassed)", got)
	}
}

func TestInstallDaemonProxy_SSOOverrideHostAutoBypassed(t *testing.T) {
	tr := freshTransport()
	ssoHost := daemonProxyBypassHost("http://wardyn-awsssofake:8090")
	if ssoHost != "wardyn-awsssofake" {
		t.Fatalf("daemonProxyBypassHost = %q, want the bare host", ssoHost)
	}
	_, err := installDaemonProxy(tr, "http://proxy.corp.example:3128", "", ssoHost)
	if err != nil {
		t.Fatalf("installDaemonProxy: %v", err)
	}
	req := &http.Request{URL: mustParseURL(t, "http://wardyn-awsssofake:8090/token")}
	got, err := tr.Proxy(req)
	if err != nil {
		t.Fatalf("Proxy(sso override host): %v", err)
	}
	if got != nil {
		t.Fatalf("Proxy(sso override host) = %v, want nil (auto-bypassed)", got)
	}
}

func TestInstallDaemonProxy_LoopbackExemptByDefault(t *testing.T) {
	// Pin the fact prose alone cannot guarantee: httpproxy exempts loopback
	// even with NO EXPLICIT bypass entry, which is why existing
	// awssso_refresh_test.go httptest-loopback tests keep passing once a lane
	// sets WARDYN_DAEMON_PROXY_URL. THIS is that pin — awssso_refresh_test.go
	// itself carries no daemon-proxy-specific test; its httptest-loopback
	// callers simply never trip the bypass this test proves exists.
	tr := freshTransport()
	if _, err := installDaemonProxy(tr, "http://proxy.corp.example:3128", ""); err != nil {
		t.Fatalf("installDaemonProxy: %v", err)
	}
	req := &http.Request{URL: mustParseURL(t, "http://127.0.0.1:54321/token")}
	got, err := tr.Proxy(req)
	if err != nil {
		t.Fatalf("Proxy(loopback): %v", err)
	}
	if got != nil {
		t.Fatalf("Proxy(loopback) = %v, want nil (loopback is exempt by construction)", got)
	}
}

func TestInstallDaemonProxy_UserinfoRefused(t *testing.T) {
	tr := freshTransport()
	_, err := installDaemonProxy(tr, "http://alice:s3cr3t-token@proxy.corp.example:3128", "")
	if err == nil {
		t.Fatal("installDaemonProxy(userinfo) succeeded, want a refusal")
	}
	if err.Error() != daemonProxyUserinfoRefusal {
		t.Fatalf("err = %q, want the exact daemonProxyUserinfoRefusal constant", err.Error())
	}
	if strings.Contains(err.Error(), "alice") || strings.Contains(err.Error(), "s3cr3t-token") {
		t.Fatalf("refusal echoes the raw credential: %q", err.Error())
	}
}

func TestInstallDaemonProxy_GarbageRefusesBoot(t *testing.T) {
	// "http://:3128" is the "missing a host" branch specifically: it parses
	// clean (Scheme "http", Host ":3128" is non-empty) and the scheme is
	// supported, so only Hostname() == "" catches it — the other three cases
	// all fail earlier (empty scheme or empty Host).
	cases := []string{"not a url at all", "ftp://proxy.corp.example", "http://", "://bad", "http://:3128"}
	for _, raw := range cases {
		tr := freshTransport()
		_, err := installDaemonProxy(tr, raw, "")
		if err == nil {
			t.Fatalf("installDaemonProxy(%q) succeeded, want a refusal", raw)
		}
		if strings.Contains(err.Error(), raw) {
			t.Fatalf("refusal for %q echoes the raw value: %q", raw, err.Error())
		}
	}
}

func TestDaemonProxyBypassHost_EmptyAndUnparseable(t *testing.T) {
	if got := daemonProxyBypassHost(""); got != "" {
		t.Fatalf("daemonProxyBypassHost(\"\") = %q, want empty", got)
	}
	// "://bad" fails url.Parse outright (empty scheme before "://"); degrades
	// to "" rather than a second boot failure over the same typo another
	// validator (ValidateAWSSSOEndpointOverride) already owns.
	if got := daemonProxyBypassHost("://bad"); got != "" {
		t.Fatalf("daemonProxyBypassHost(\"://bad\") = %q, want empty", got)
	}
}

func TestInstallDaemonProxy_NoProxyMatrix(t *testing.T) {
	t.Run("dot-suffix", func(t *testing.T) {
		tr := freshTransport()
		if _, err := installDaemonProxy(tr, "http://proxy.corp.example:3128", ".svc.cluster.local"); err != nil {
			t.Fatalf("installDaemonProxy: %v", err)
		}
		if got, _ := tr.Proxy(&http.Request{URL: mustParseURL(t, "https://wardynd.default.svc.cluster.local/api")}); got != nil {
			t.Fatalf("Proxy(subdomain of the suffix) = %v, want nil (bypassed)", got)
		}
		if got, _ := tr.Proxy(&http.Request{URL: mustParseURL(t, "https://example.com/api")}); got == nil {
			t.Fatal("Proxy(unrelated host) = nil, want the configured proxy")
		}
	})
	t.Run("CIDR", func(t *testing.T) {
		tr := freshTransport()
		if _, err := installDaemonProxy(tr, "http://proxy.corp.example:3128", "10.0.0.0/8"); err != nil {
			t.Fatalf("installDaemonProxy: %v", err)
		}
		if got, _ := tr.Proxy(&http.Request{URL: mustParseURL(t, "https://10.1.2.3/api")}); got != nil {
			t.Fatalf("Proxy(in-CIDR) = %v, want nil (bypassed)", got)
		}
		if got, _ := tr.Proxy(&http.Request{URL: mustParseURL(t, "https://8.8.8.8/api")}); got == nil {
			t.Fatal("Proxy(out-of-CIDR) = nil, want the configured proxy")
		}
	})
	t.Run("asterisk disables proxying entirely", func(t *testing.T) {
		tr := freshTransport()
		if _, err := installDaemonProxy(tr, "http://proxy.corp.example:3128", "*"); err != nil {
			t.Fatalf("installDaemonProxy: %v", err)
		}
		if got, _ := tr.Proxy(&http.Request{URL: mustParseURL(t, "https://sso-oidc.us-east-1.amazonaws.com/token")}); got != nil {
			t.Fatalf("Proxy(any host) = %v, want nil — \"*\" means no proxying at all", got)
		}
	})
	t.Run("IPv6 literal", func(t *testing.T) {
		tr := freshTransport()
		if _, err := installDaemonProxy(tr, "http://proxy.corp.example:3128", "2001:db8::1"); err != nil {
			t.Fatalf("installDaemonProxy: %v", err)
		}
		if got, _ := tr.Proxy(&http.Request{URL: mustParseURL(t, "https://[2001:db8::1]/api")}); got != nil {
			t.Fatalf("Proxy(IPv6 literal) = %v, want nil (bypassed)", got)
		}
		if got, _ := tr.Proxy(&http.Request{URL: mustParseURL(t, "https://[2001:db8::2]/api")}); got == nil {
			t.Fatal("Proxy(a DIFFERENT IPv6 literal) = nil, want the configured proxy")
		}
	})
}

func TestInstallDaemonProxy_UnreachableProxyErrorNamesHostPort(t *testing.T) {
	// Bind then immediately close: nothing listens at addr any more, so a
	// dial there refuses the connection — the CreateToken-shaped failure mode
	// (10s client timeout stands; this fails much faster via ECONNREFUSED).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	tr := freshTransport()
	if _, err := installDaemonProxy(tr, "http://"+addr, ""); err != nil {
		t.Fatalf("installDaemonProxy: %v", err)
	}
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	resp, err := client.Get("https://sso-oidc.us-east-1.amazonaws.com/token")
	if err == nil {
		resp.Body.Close()
		t.Fatal("request through an unreachable proxy succeeded, want a dial error")
	}
	if !strings.Contains(err.Error(), addr) {
		t.Fatalf("error %q does not name the proxy host:port %q", err.Error(), addr)
	}
}

func TestInstallDaemonProxy_ProxyReceivesAbsoluteURIRequest(t *testing.T) {
	var gotRequestURI string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRequestURI = r.RequestURI
		w.WriteHeader(http.StatusOK)
	}))
	defer proxy.Close()

	tr := freshTransport()
	if _, err := installDaemonProxy(tr, proxy.URL, ""); err != nil {
		t.Fatalf("installDaemonProxy: %v", err)
	}
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	resp, err := client.Get("http://sso.example.invalid/token")
	if err != nil {
		t.Fatalf("client.Get through the proxy: %v", err)
	}
	resp.Body.Close()

	// A plain-http destination through a proxy is a single GET carrying the
	// ABSOLUTE URI on the request line (never CONNECT, which is https-only) —
	// this is what proves the proxy actually saw where the request was going,
	// not merely that SOME request arrived.
	if !strings.HasPrefix(gotRequestURI, "http://sso.example.invalid/token") {
		t.Fatalf("proxy received RequestURI %q, want an absolute-URI naming http://sso.example.invalid/token", gotRequestURI)
	}
}

// testBootFlags builds a *bootFlags carrying only the fields
// bootDaemonProxy/installBootTransport read — same construction pattern as
// bedrock_plaintext_warn_test.go's capture helper.
func testBootFlags(proxyURL, noProxy, ssoOverride, oidcInternalIss string) *bootFlags {
	return testBootFlagsWithSecret(proxyURL, "", noProxy, ssoOverride, oidcInternalIss)
}

// testBootFlagsWithSecret is testBootFlags plus WARDYN_DAEMON_PROXY_SECRET,
// for the tests that exercise the credentialed file path or the
// both-set refusal.
func testBootFlagsWithSecret(proxyURL, secretFile, noProxy, ssoOverride, oidcInternalIss string) *bootFlags {
	return &bootFlags{
		daemonProxyURL:         &proxyURL,
		daemonProxySecretFile:  &secretFile,
		daemonNoProxy:          &noProxy,
		awsSSOEndpointOverride: &ssoOverride,
		oidcInternalIss:        &oidcInternalIss,
	}
}

// writeProxySecretFile writes contents to a fresh file under t.TempDir() at
// the given mode, returning its path — the WARDYN_DAEMON_PROXY_SECRET shape
// every credentialed-boot test below points at.
func writeProxySecretFile(t *testing.T, contents string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "proxy-secret")
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		t.Fatalf("os.WriteFile(%q): %v", path, err)
	}
	// os.WriteFile's mode is masked by umask; force the exact mode the test
	// asks for so a permissive umask on the CI host can't produce a false pass.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("os.Chmod(%q, %v): %v", path, mode, err)
	}
	return path
}

func TestBootDaemonProxy_UnsetIsNoop(t *testing.T) {
	tr := freshTransport()
	before := tr.Proxy
	f := testBootFlags("", "", "", "")
	if err := bootDaemonProxy(tr, f); err != nil {
		t.Fatalf("bootDaemonProxy(unset): %v", err)
	}
	if !reflectSameFunc(before, tr.Proxy) {
		t.Fatal("bootDaemonProxy reassigned tr.Proxy on an unset WARDYN_DAEMON_PROXY_URL")
	}
}

func TestBootDaemonProxy_ConfiguredWiresAndLogsBypassList(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	tr := freshTransport()
	f := testBootFlags("http://proxy.corp.example:3128", "internal.example", "http://wardyn-awsssofake:8090", "http://dex:5556")
	if err := bootDaemonProxy(tr, f); err != nil {
		t.Fatalf("bootDaemonProxy: %v", err)
	}

	// Wired: an external host is proxied, the SSO override host is not.
	got, err := tr.Proxy(&http.Request{URL: mustParseURL(t, "https://sso-oidc.us-east-1.amazonaws.com/token")})
	if err != nil || got == nil || got.Host != "proxy.corp.example:3128" {
		t.Fatalf("Proxy(external) = %v, %v, want the configured proxy", got, err)
	}
	if got, _ := tr.Proxy(&http.Request{URL: mustParseURL(t, "http://wardyn-awsssofake:8090/token")}); got != nil {
		t.Fatalf("Proxy(sso override host) = %v, want nil (auto-bypassed)", got)
	}

	// Logged: the proxy host and every bypass entry, never a raw credential
	// (there is none here, but the log must still name what it bypassed).
	logged := buf.String()
	for _, want := range []string{"proxy.corp.example:3128", "internal.example", "wardyn-awsssofake", "dex"} {
		if !strings.Contains(logged, want) {
			t.Fatalf("boot log %q missing %q", logged, want)
		}
	}
}

func TestInstallBootTransport_NonTransportIsNoop(t *testing.T) {
	f := testBootFlags("http://proxy.corp.example:3128", "", "", "")
	if err := installBootTransport(stubRoundTripper{}, nil, f); err != nil {
		t.Fatalf("installBootTransport(non-*http.Transport): %v, want nil (silent no-op)", err)
	}
}

func TestInstallBootTransport_WiresTrustedCAAndProxy(t *testing.T) {
	// A NON-nil pool: installTrustedCA early-returns on nil (see its own doc
	// comment), so a nil pool here would prove nothing about the RootCAs wire
	// — only about installDaemonProxy, which the separate Proxy assertion
	// below already covers.
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(genTestCert(t, "Corp Root Test")) {
		t.Fatal("AppendCertsFromPEM(genTestCert) failed")
	}

	tr := freshTransport()
	f := testBootFlags("http://proxy.corp.example:3128", "", "", "")
	if err := installBootTransport(tr, pool, f); err != nil {
		t.Fatalf("installBootTransport: %v", err)
	}
	if tr.TLSClientConfig == nil || tr.TLSClientConfig.RootCAs != pool {
		t.Fatal("installBootTransport did not wire the trusted CA pool onto tr.TLSClientConfig.RootCAs")
	}
	if tr.Proxy == nil {
		t.Fatal("tr.Proxy is nil, want installDaemonProxy to have wired it")
	}
	got, err := tr.Proxy(&http.Request{URL: mustParseURL(t, "https://sso-oidc.us-east-1.amazonaws.com/token")})
	if err != nil || got == nil || got.Host != "proxy.corp.example:3128" {
		t.Fatalf("Proxy(external) = %v, %v, want the configured proxy", got, err)
	}
}

func TestBootDaemonProxy_KubernetesServiceHostEnvAutoBypassed(t *testing.T) {
	// t.Setenv on the REAL var name, through bootDaemonProxy's own
	// os.Getenv("KUBERNETES_SERVICE_HOST") read — every other test in this
	// file passes the k8s host as an explicit autoBypass string straight to
	// installDaemonProxy, which would pass even if bootDaemonProxy's env-var
	// NAME were typo'd.
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.96.0.1")
	tr := freshTransport()
	f := testBootFlags("http://proxy.corp.example:3128", "", "", "")
	if err := bootDaemonProxy(tr, f); err != nil {
		t.Fatalf("bootDaemonProxy: %v", err)
	}
	got, err := tr.Proxy(&http.Request{URL: mustParseURL(t, "https://10.96.0.1/api")})
	if err != nil {
		t.Fatalf("Proxy(k8s host): %v", err)
	}
	if got != nil {
		t.Fatalf("Proxy(k8s host) = %v, want nil — bypassed via the real KUBERNETES_SERVICE_HOST env read", got)
	}
}

func TestInstallBootTransport_MalformedProxyRefusesBoot(t *testing.T) {
	tr := freshTransport()
	f := testBootFlags("not a url at all", "", "", "")
	if err := installBootTransport(tr, nil, f); err == nil {
		t.Fatal("installBootTransport(malformed proxy) succeeded, want a refusal")
	}
}

// ─── WARDYN_DAEMON_PROXY_SECRET (credentialed file path) ────────────────────

func TestInstallDaemonProxySecret_UnsetLeavesTransportUntouched(t *testing.T) {
	tr := freshTransport()
	before := tr.Proxy
	eff, err := installDaemonProxySecret(tr, "", "")
	if err != nil {
		t.Fatalf("installDaemonProxySecret(unset) error: %v", err)
	}
	if eff != "" {
		t.Fatalf("effective = %q, want empty", eff)
	}
	if !reflectSameFunc(before, tr.Proxy) {
		t.Fatal("tr.Proxy was reassigned on an unset WARDYN_DAEMON_PROXY_SECRET")
	}
}

func TestInstallDaemonProxySecret_CredentialedURLWired(t *testing.T) {
	path := writeProxySecretFile(t, "http://alice:s3cr3t-token@proxy.corp.example:3128\n", 0o600)
	tr := freshTransport()
	eff, err := installDaemonProxySecret(tr, path, "")
	if err != nil {
		t.Fatalf("installDaemonProxySecret: %v", err)
	}
	if eff != "http://alice:s3cr3t-token@proxy.corp.example:3128" {
		t.Fatalf("effective = %q, want the full credentialed URL (userinfo allowed here)", eff)
	}
	got, err := tr.Proxy(&http.Request{URL: mustParseURL(t, "https://sso-oidc.us-east-1.amazonaws.com/token")})
	if err != nil || got == nil || got.Host != "proxy.corp.example:3128" {
		t.Fatalf("Proxy(external) = %v, %v, want the configured proxy", got, err)
	}
	if got.User == nil || got.User.Username() != "alice" {
		t.Fatalf("Proxy(external).User = %v, want alice's credential preserved", got.User)
	}
}

func TestInstallDaemonProxySecret_MissingFileRefusesBoot(t *testing.T) {
	tr := freshTransport()
	_, err := installDaemonProxySecret(tr, filepath.Join(t.TempDir(), "does-not-exist"), "")
	if err == nil {
		t.Fatal("installDaemonProxySecret(missing file) succeeded, want a refusal")
	}
}

func TestInstallDaemonProxySecret_WideModeRefusesBoot(t *testing.T) {
	path := writeProxySecretFile(t, "http://alice:s3cr3t-token@proxy.corp.example:3128\n", 0o644)
	tr := freshTransport()
	_, err := installDaemonProxySecret(tr, path, "")
	if err == nil {
		t.Fatal("installDaemonProxySecret(mode 0644) succeeded, want a refusal")
	}
	if strings.Contains(err.Error(), "alice") || strings.Contains(err.Error(), "s3cr3t-token") {
		t.Fatalf("refusal echoes the raw credential: %q", err.Error())
	}
}

func TestInstallDaemonProxySecret_EmptyFileRefusesBoot(t *testing.T) {
	path := writeProxySecretFile(t, "\n  \n", 0o600)
	tr := freshTransport()
	_, err := installDaemonProxySecret(tr, path, "")
	if err == nil {
		t.Fatal("installDaemonProxySecret(empty file) succeeded, want a refusal")
	}
}

func TestInstallDaemonProxySecret_GarbageContentRefusesBoot(t *testing.T) {
	path := writeProxySecretFile(t, "not a url at all", 0o600)
	tr := freshTransport()
	_, err := installDaemonProxySecret(tr, path, "")
	if err == nil {
		t.Fatal("installDaemonProxySecret(garbage content) succeeded, want a refusal")
	}
	if strings.Contains(err.Error(), "not a url at all") {
		t.Fatalf("refusal echoes the raw file content: %q", err.Error())
	}
}

func TestBootDaemonProxy_BothSetRefusesBoot(t *testing.T) {
	path := writeProxySecretFile(t, "http://alice:s3cr3t@proxy.corp.example:3128", 0o600)
	tr := freshTransport()
	f := testBootFlagsWithSecret("http://proxy.corp.example:3128", path, "", "", "")
	err := bootDaemonProxy(tr, f)
	if err == nil {
		t.Fatal("bootDaemonProxy(both WARDYN_DAEMON_PROXY_URL and WARDYN_DAEMON_PROXY_SECRET set) succeeded, want a refusal")
	}
	if err.Error() != daemonProxyBothSetRefusal {
		t.Fatalf("err = %q, want the exact daemonProxyBothSetRefusal constant", err.Error())
	}
}

func TestBootDaemonProxy_SecretFileWiresCredentialedProxyAndLogsNoCredential(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	path := writeProxySecretFile(t, "http://alice:s3cr3t-token@proxy.corp.example:3128", 0o600)
	tr := freshTransport()
	f := testBootFlagsWithSecret("", path, "", "", "")
	if err := bootDaemonProxy(tr, f); err != nil {
		t.Fatalf("bootDaemonProxy: %v", err)
	}

	got, err := tr.Proxy(&http.Request{URL: mustParseURL(t, "https://sso-oidc.us-east-1.amazonaws.com/token")})
	if err != nil || got == nil || got.Host != "proxy.corp.example:3128" || got.User == nil || got.User.Username() != "alice" {
		t.Fatalf("Proxy(external) = %v, %v, want the configured credentialed proxy", got, err)
	}

	logged := buf.String()
	if strings.Contains(logged, "alice") || strings.Contains(logged, "s3cr3t-token") {
		t.Fatalf("boot log leaked the credential: %q", logged)
	}
	if !strings.Contains(logged, "proxy.corp.example:3128") {
		t.Fatalf("boot log %q missing the proxy host", logged)
	}
	if !strings.Contains(logged, "WARDYN_DAEMON_PROXY_SECRET") {
		t.Fatalf("boot log %q does not name WARDYN_DAEMON_PROXY_SECRET as the source", logged)
	}
}

// stubRoundTripper is a http.RoundTripper that is deliberately NOT a
// *http.Transport, proving installBootTransport's type-assertion no-op path
// without touching the process-global http.DefaultTransport.
type stubRoundTripper struct{}

func (stubRoundTripper) RoundTrip(*http.Request) (*http.Response, error) { return nil, nil }

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", raw, err)
	}
	return u
}

// reflectSameFunc reports whether two func values point at the same
// underlying function — good enough to prove installDaemonProxy did not
// reassign tr.Proxy on an unset knob, without relying on func == (which the
// language refuses to compile).
func reflectSameFunc(a, b func(*http.Request) (*url.URL, error)) bool {
	return reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer()
}
