// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
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
	// sets WARDYN_DAEMON_PROXY_URL — see awssso_refresh_test.go's own pin.
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
	cases := []string{"not a url at all", "ftp://proxy.corp.example", "http://", "://bad"}
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
