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

// httpproxy drops an HTTPS_PROXY it cannot parse and then answers "direct", so a listed host would
// bypass the proxy it was listed for. Boot refuses instead, and never prints the value.
func TestDaemonProxyAlways_RefusesUnparsableEnvironmentProxy(t *testing.T) {
	for _, bad := range []string{"http://bad host:3128", "://proxy", "%zz", "http://proxy:notaport", "ftp://proxy.corp.example:21"} {
		clearProxyEnv(t)
		t.Setenv("HTTPS_PROXY", bad)
		grant, err := bootDaemonProxy(freshTransport(), alwaysFlags("", "", "login.corp.example"))
		if err == nil || !strings.Contains(err.Error(), "HTTPS_PROXY") || !strings.Contains(err.Error(), "WARDYN_DAEMON_PROXY_ALWAYS") {
			t.Errorf("HTTPS_PROXY=%q: bootDaemonProxy = %v, %v; want a refusal naming HTTPS_PROXY", bad, grant, err)
		} else if strings.Contains(err.Error(), bad) {
			t.Errorf("HTTPS_PROXY=%q: the refusal echoes the value: %v", bad, err)
		}
	}
	// Lower case is the same variable to the environment reader.
	clearProxyEnv(t)
	t.Setenv("https_proxy", "http://bad host:3128")
	if _, err := bootDaemonProxy(freshTransport(), alwaysFlags("", "", "login.corp.example")); err == nil {
		t.Error("https_proxy=http://bad host: bootDaemonProxy succeeded, want a refusal")
	}
	// The same value with the list empty changes nothing: no refusal, no grant transport.
	grant, err := bootDaemonProxy(freshTransport(), alwaysFlags("", "", ""))
	if err != nil || grant != nil {
		t.Errorf("empty list with HTTPS_PROXY=bad: %v, %v; want nil, nil", grant, err)
	}
}

// The host:port shorthand httpproxy accepts (a value with no scheme) keeps working.
func TestDaemonProxyAlways_AcceptsHostPortShorthand(t *testing.T) {
	for raw, want := range map[string]string{
		"proxy.corp.example:3128":           "http://proxy.corp.example:3128",
		"localhost:3128":                    "http://localhost:3128",
		"10.0.0.9:8080":                     "http://10.0.0.9:8080",
		"https://proxy.corp.example:3129":   "https://proxy.corp.example:3129",
		"socks5://proxy.corp.example:1080":  "socks5://proxy.corp.example:1080",
		"socks5h://proxy.corp.example:1080": "socks5h://proxy.corp.example:1080",
	} {
		clearProxyEnv(t)
		t.Setenv("HTTPS_PROXY", raw)
		grant, err := bootDaemonProxy(freshTransport(), alwaysFlags("", "", "login.corp.example"))
		if err != nil {
			t.Errorf("HTTPS_PROXY=%q: %v", raw, err)
			continue
		}
		if got := proxyFor(t, grant, "https://login.corp.example/token"); got != want {
			t.Errorf("HTTPS_PROXY=%q: grant transport sends login.corp.example to %q, want %q", raw, got, want)
		}
	}
}

// In every configuration that boots, a listed host's Proxy function answers a URL, never nil:
// not under NO_PROXY=*, not with the daemon proxy bypassing it, not with the standard
// library's own function (which answers nil for an unusable value) as the shared one.
func TestDaemonProxyAlways_ListedHostNeverDirect(t *testing.T) {
	envFn := func(r *http.Request) (*url.URL, error) { return httpproxy.FromEnvironment().ProxyFunc()(r.URL) }
	cases := map[string]struct {
		env     map[string]string
		proxy   string
		noProxy string
		shared  func(*http.Request) (*url.URL, error)
	}{
		"daemon proxy, bypassed":       {proxy: "http://proxy.corp.example:3128", noProxy: "corp.example", shared: freshTransport().Proxy},
		"daemon proxy, no_proxy star":  {proxy: "http://proxy.corp.example:3128", noProxy: "*", shared: freshTransport().Proxy},
		"environment, listed in NO":    {env: map[string]string{"HTTPS_PROXY": "http://e.corp.example:3128", "NO_PROXY": "login.corp.example"}, shared: envFn},
		"environment, no_proxy star":   {env: map[string]string{"https_proxy": "e.corp.example:3128", "no_proxy": "*"}, shared: envFn},
		"environment, nil shared func": {env: map[string]string{"HTTPS_PROXY": "http://e.corp.example:3128"}, shared: nil},
	}
	for name, c := range cases {
		clearProxyEnv(t)
		for k, v := range c.env {
			t.Setenv(k, v)
		}
		grant, err := bootDaemonProxy(&http.Transport{Proxy: c.shared}, alwaysFlags(c.proxy, c.noProxy, "login.corp.example,Idp.Corp.Example.,xn--bcher-kva.corp.example"))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		for _, target := range []string{"https://login.corp.example/t", "https://login.corp.example:8443/t", "https://LOGIN.corp.example./t", "http://idp.corp.example/t",
			// net/http dials the IDNA-ASCII form, so these spellings are the listed hosts too.
			"https://\uff4cogin.corp.example/token", "https://login\u3002corp.example/token", "https://b\u00fccher.corp.example/token"} {
			if got := proxyFor(t, grant, target); got == "" {
				t.Errorf("%s: a listed host, %s, goes direct", name, target)
			}
		}
	}
	// A host that cannot be converted is an error, never a direct request.
	clearProxyEnv(t)
	grant, err := bootDaemonProxy(freshTransport(), alwaysFlags("http://proxy.corp.example:3128", "", "login.corp.example"))
	if err != nil {
		t.Fatal(err)
	}
	if u, err := grant.(*http.Transport).Proxy(&http.Request{URL: &url.URL{Scheme: "https", Host: "a\u2028b.example"}}); err == nil {
		t.Errorf("a host that does not convert to ASCII got proxy %v and no error", u)
	}
	// And the configuration that would send it direct does not boot.
	clearProxyEnv(t)
	t.Setenv("HTTPS_PROXY", "http://bad host")
	if grant, err := bootDaemonProxy(freshTransport(), alwaysFlags("", "", "login.corp.example")); err == nil {
		t.Errorf("unusable HTTPS_PROXY booted with a grant transport %v", grant)
	}
}

// The refusal is written to the daemon log, so it names an entry by position and reason, never by
// its text: a proxy URL pasted into the list carries a credential.
func TestDaemonProxyAlways_RefusalOmitsEntryText(t *testing.T) {
	clearProxyEnv(t)
	cases := []struct{ list, wantPosition string }{
		{"http://user:MARKER-7f3a@proxy.example:3128", "entry 1 "},
		{"login.corp.example, http://user:MARKER-7f3a@proxy.example:3128", "entry 2 "},
		{"login.corp.example,*.MARKER-7f3a.example", "entry 2 "},
		{"login.corp.example,,LocalHost", "entry 3 "},
		{"MARKER-7f3a@host.example", "entry 1 "},
	}
	for _, c := range cases {
		_, err := bootDaemonProxy(freshTransport(), alwaysFlags("http://proxy.corp.example:3128", "", c.list))
		if err == nil {
			t.Errorf("%q: boot succeeded, want a refusal", c.list)
			continue
		}
		msg := err.Error()
		if !strings.Contains(msg, "WARDYN_DAEMON_PROXY_ALWAYS") || !strings.Contains(msg, c.wantPosition) {
			t.Errorf("%q: refusal %q does not name the variable and %q", c.list, msg, c.wantPosition)
		}
		for _, leak := range []string{"MARKER-7f3a", "user:", "proxy.example", "LocalHost"} {
			if strings.Contains(msg, leak) {
				t.Errorf("%q: refusal %q carries entry text %q", c.list, msg, leak)
			}
		}
	}
	// The unusable-proxy refusal names the variable, not its value.
	t.Setenv("HTTPS_PROXY", "http://user:MARKER-7f3a@bad host:3128")
	if _, err := bootDaemonProxy(freshTransport(), alwaysFlags("", "", "login.corp.example")); err == nil || strings.Contains(err.Error(), "MARKER-7f3a") {
		t.Errorf("unusable HTTPS_PROXY: err = %v, want a refusal without the value", err)
	}
}

// An entry that is not a whole host name can never match a request host, and its text could be
// private: boot refuses it by position and reason, and the log line only ever carries validated names.
func TestDaemonProxyAlways_RefusesNonHostnameEntry(t *testing.T) {
	clearProxyEnv(t)
	long := strings.Repeat("a", 64) + ".example"
	for _, entry := range []string{
		"login.corp.example?MARKER-q1", "login.corp.example#MARKER-f1", "login corp.example", "login..corp.example",
		"-bad.example", "bad-.example", long, strings.Repeat("a.", 130) + "example", "log\u00efn.example", "under_score.example", ".",
		"127.1", "2130706433", "0x7f.0.0.1",
	} {
		_, err := bootDaemonProxy(freshTransport(), alwaysFlags("http://proxy.corp.example:3128", "", "ok.example,"+entry))
		if err == nil {
			t.Errorf("%.30q: boot succeeded, want a refusal", entry)
			continue
		}
		msg := err.Error()
		if !strings.Contains(msg, "WARDYN_DAEMON_PROXY_ALWAYS entry 2 ") || strings.Contains(msg, "MARKER") || strings.Contains(msg, entry) {
			t.Errorf("%.30q: refusal %q must name entry 2 and carry no entry text", entry, msg)
		}
	}
	// Valid names, including a single label and punycode, boot.
	grant, err := bootDaemonProxy(freshTransport(), alwaysFlags("http://proxy.corp.example:3128", "", "login.corp.example,idp,xn--lgin-6qa.example,a-1.b-2.example."))
	if err != nil || grant == nil {
		t.Errorf("valid names: %v, %v; want a grant transport", grant, err)
	}
}
