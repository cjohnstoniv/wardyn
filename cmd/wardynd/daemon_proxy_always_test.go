// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/net/http/httpproxy"
)

// alwaysFlags is testBootFlags plus WARDYN_DAEMON_PROXY_ALWAYS.
func alwaysFlags(proxyURL, noProxy, always string) *bootFlags {
	f := testBootFlags(proxyURL, noProxy, "", "")
	f.daemonProxyAlways = &always
	return f
}

// proxyFor is what a transport's Proxy function answers for a request to rawURL ("" is direct).
func proxyFor(t *testing.T, rt http.RoundTripper, rawURL string) string {
	t.Helper()
	tr, ok := rt.(*http.Transport)
	if !ok {
		t.Fatalf("%T is not a *http.Transport", rt)
	}
	u, err := tr.Proxy(&http.Request{URL: mustParseURL(t, rawURL)})
	if err != nil {
		t.Fatalf("Proxy(%s): %v", rawURL, err)
	}
	if u == nil {
		return ""
	}
	return u.String()
}

func clearProxyEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "NO_PROXY", "no_proxy", "KUBERNETES_SERVICE_HOST"} {
		t.Setenv(k, "")
	}
}

// A host the daemon transport bypasses (WARDYN_DAEMON_NO_PROXY) still goes to the proxy on the
// grant's transport, whatever the case or trailing dot of the request's host, and with a port.
func TestDaemonProxyAlways_ForcesProxyForBypassedHost(t *testing.T) {
	clearProxyEnv(t)
	tr := freshTransport()
	grant, err := bootDaemonProxy(tr, alwaysFlags("http://proxy.corp.example:3128", "corp.example", "Login.Corp.Example."))
	if err != nil {
		t.Fatal(err)
	}
	if got := proxyFor(t, tr, "https://login.corp.example/token"); got != "" {
		t.Fatalf("the shared transport sends login.corp.example to %q, want it bypassed", got)
	}
	for _, target := range []string{"https://login.corp.example/token", "https://LOGIN.corp.example./token", "https://login.corp.example:8443/token"} {
		if got := proxyFor(t, grant, target); got != "http://proxy.corp.example:3128" {
			t.Errorf("grant transport Proxy(%s) = %q, want the daemon proxy", target, got)
		}
	}
	if got := proxyFor(t, grant, "https://other.example/token"); got != "http://proxy.corp.example:3128" {
		t.Errorf("grant transport Proxy(other.example) = %q, want the daemon transport's own answer (the proxy)", got)
	}
}

// With only HTTPS_PROXY set the daemon transport is the standard environment function; the grant
// transport sends the listed host to that proxy even when NO_PROXY covers it, and every other
// host where the environment function sends it.
func TestDaemonProxyAlways_WrapsEnvironmentProxy(t *testing.T) {
	clearProxyEnv(t)
	t.Setenv("HTTPS_PROXY", "http://envproxy.corp.example:3128")
	t.Setenv("NO_PROXY", "login.corp.example,direct.example")
	env := func(r *http.Request) (*url.URL, error) { return httpproxy.FromEnvironment().ProxyFunc()(r.URL) }
	tr := &http.Transport{Proxy: env}
	grant, err := bootDaemonProxy(tr, alwaysFlags("", "", "login.corp.example"))
	if err != nil {
		t.Fatal(err)
	}
	if got := proxyFor(t, tr, "https://login.corp.example/token"); got != "" {
		t.Fatalf("the environment function sends login.corp.example to %q, want it bypassed", got)
	}
	cases := map[string]string{
		"https://login.corp.example/token": "http://envproxy.corp.example:3128",
		"https://direct.example/":          "",
		"https://elsewhere.example/":       "http://envproxy.corp.example:3128",
	}
	for target, want := range cases {
		if got := proxyFor(t, grant, target); got != want {
			t.Errorf("grant transport Proxy(%s) = %q, want %q", target, got, want)
		}
	}
}

func TestDaemonProxyAlways_RefusesBootWithoutProxy(t *testing.T) {
	clearProxyEnv(t)
	grant, err := bootDaemonProxy(freshTransport(), alwaysFlags("", "", "login.corp.example"))
	if err == nil || !strings.Contains(err.Error(), "WARDYN_DAEMON_PROXY_ALWAYS") {
		t.Fatalf("bootDaemonProxy(list set, no proxy) = %v, %v; want a refusal naming WARDYN_DAEMON_PROXY_ALWAYS", grant, err)
	}
	// An empty list needs no proxy, and returns the shared transport (nil).
	grant, err = bootDaemonProxy(freshTransport(), alwaysFlags("", "", " , "))
	if err != nil || grant != nil {
		t.Fatalf("bootDaemonProxy(empty list, no proxy) = %v, %v; want nil, nil", grant, err)
	}
}

// Only the listed name: a subdomain of it, and its parent, stay on the daemon transport's answer,
// which here is a bypass.
func TestDaemonProxyAlways_ExactHostOnly(t *testing.T) {
	clearProxyEnv(t)
	grant, err := bootDaemonProxy(freshTransport(), alwaysFlags("http://proxy.corp.example:3128", "corp.example", "login.corp.example"))
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"https://login.corp.example/":   "http://proxy.corp.example:3128",
		"https://x.login.corp.example/": "",
		"https://corp.example/":         "",
	}
	for target, want := range cases {
		if got := proxyFor(t, grant, target); got != want {
			t.Errorf("grant transport Proxy(%s) = %q, want %q", target, got, want)
		}
	}
}

// An entry that is an address, localhost, or a host wardynd always reaches directly refuses boot,
// as does one that is not an exact host name.
func TestDaemonProxyAlways_RefusesProtectedHosts(t *testing.T) {
	clearProxyEnv(t)
	t.Setenv("KUBERNETES_SERVICE_HOST", "kube.cluster.example")
	cases := map[string]string{
		"an IPv4 literal":      "10.1.2.3",
		"an IPv6 literal":      "[::1]",
		"localhost":            "LocalHost",
		"the cluster API host": "kube.cluster.example.",
		"the internal issuer":  "idp.internal.example",
		"the AWS SSO override": "sso.internal.example",
		"a link-local host":    "169.254.169.254",
		"a link-local IPv6":    "fd00:ec2::254",
		"a wildcard":           "*.corp.example",
		"a suffix":             ".corp.example",
		"a host with a port":   "login.corp.example:443",
		"one bad among good":   "login.corp.example,localhost",
	}
	for name, entry := range cases {
		f := alwaysFlags("http://proxy.corp.example:3128", "", entry)
		*f.oidcInternalIss = "https://idp.internal.example"
		*f.awsSSOEndpointOverride = "https://sso.internal.example:4566"
		grant, err := bootDaemonProxy(freshTransport(), f)
		if err == nil || !strings.Contains(err.Error(), "WARDYN_DAEMON_PROXY_ALWAYS") {
			t.Errorf("%s (%q): bootDaemonProxy = %v, %v; want a refusal naming WARDYN_DAEMON_PROXY_ALWAYS", name, entry, grant, err)
		}
	}
}

// With the list set, http.DefaultTransport routes every host exactly as it does without it, and
// the grant's transport is a different transport.
func TestDaemonProxyAlways_OtherCallsUnchanged(t *testing.T) {
	clearProxyEnv(t)
	hosts := []string{
		"login.corp.example", "x.login.corp.example", "corp.example", "other.example",
		"kube.cluster.example", "idp.internal.example", "169.254.169.254", "localhost", "10.1.2.3",
	}
	t.Setenv("KUBERNETES_SERVICE_HOST", "kube.cluster.example")
	route := func(always string) (map[string]string, http.RoundTripper) {
		defer swapDefaultTransport()()
		f := alwaysFlags("http://proxy.corp.example:3128", "corp.example", always)
		*f.oidcInternalIss = "https://idp.internal.example"
		grant, err := installBootTransport(http.DefaultTransport, nil, f)
		if err != nil {
			t.Fatalf("installBootTransport(always=%q): %v", always, err)
		}
		out := map[string]string{}
		for _, h := range hosts {
			out[h] = proxyFor(t, http.DefaultTransport, "https://"+h+"/")
		}
		return out, grant
	}
	without, g0 := route("")
	with, g1 := route("login.corp.example")
	if g0 != nil {
		t.Errorf("an empty list returned a transport %T, want nil (the shared transport)", g0)
	}
	if g1 == nil {
		t.Fatal("a listed host returned no transport")
	}
	for _, h := range hosts {
		if with[h] != without[h] {
			t.Errorf("http.DefaultTransport routes %s to %q with the list and %q without it", h, with[h], without[h])
		}
	}
}
