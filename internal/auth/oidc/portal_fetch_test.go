// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc

import (
	"bytes"
	"context"
	"crypto/x509"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

// internalHarness is a harness whose registration names an internal address on a listed host that resolves to
// private space.
func internalHarness(t *testing.T, path, internalPath string, mod ...func(*PortalIssuersOptions)) *portalHarness {
	t.Helper()
	var h *portalHarness
	listed := func(o *PortalIssuersOptions) {
		o.InternalHosts = []string{"idp.internal.test:" + h.idp.port()}
	}
	h = newPortalHarness(t, "example.com", true, path)
	h.reg.InternalIssuer = "https://idp.internal.test:" + h.idp.port() + internalPath
	h.setLookup(func(host string) []net.IP {
		if host == "idp.internal.test" {
			return []net.IP{net.ParseIP("10.1.2.3")}
		}
		return []net.IP{portalPublicIP}
	})
	h.rebuild(append([]func(*PortalIssuersOptions){listed}, mod...)...)
	return h
}

func TestPortalDiscovery_IssuerExactMatch(t *testing.T) {
	variants := func(issuer string) map[string]string {
		return map[string]string{
			"trailing slash":    issuer + "/",
			"scheme-less":       strings.TrimPrefix(issuer, "https://"),
			"upper-case host":   strings.Replace(issuer, "example.com", "EXAMPLE.com", 1),
			"another path":      issuer + "/x",
			"the internal form": "https://idp.internal.test" + strings.TrimPrefix(issuer, "https://example.com"),
		}
	}
	for _, internal := range []bool{false, true} {
		for name, doc := range variants("https://example.com:0/oauth2/a") {
			t.Run(fmt.Sprintf("internal=%v/%s", internal, name), func(t *testing.T) {
				var h *portalHarness
				if internal {
					h = internalHarness(t, "/oauth2/a", "/oauth2/a")
				} else {
					h = newPortalHarness(t, "example.com", true, "/oauth2/a")
				}
				bad := strings.Replace(doc, ":0", ":"+h.idp.port(), 1)
				h.idp.set(func(f *fakeIdP) { f.docIssuer = bad })
				h.wantUnavailable(h.token(h.accessClaims()), portalCauseIssuerMismatch)
				if _, keys := h.idp.hits(); keys != 0 {
					t.Fatal("keys were fetched after a discovery document with another issuer")
				}
			})
		}
		t.Run(fmt.Sprintf("internal=%v/exact", internal), func(t *testing.T) {
			h := newPortalHarness(t, "example.com", true, "/oauth2/a")
			if internal {
				h = internalHarness(t, "/oauth2/a", "/oauth2/a")
			}
			h.mustAdmit(h.token(h.accessClaims()))
		})
	}
}

func TestPortalDiscovery_NoInsecureIssuerURLContext(t *testing.T) {
	files, _ := filepath.Glob("portal_*.go")
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		checked++
		file, err := parser.ParseFile(token.NewFileSet(), f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && (id.Name == "InsecureIssuerURLContext" || id.Name == "NewProvider") {
				t.Errorf("%s names %s: discovery's issuer compare is Wardyn's own", f, id.Name)
			}
			return true
		})
	}
	if checked < 5 {
		t.Fatalf("only %d portal files found", checked)
	}
}

func TestPortalDiscovery_JWKSOffOriginRefused(t *testing.T) {
	for name, uri := range map[string]func(h *portalHarness) string{
		"another host": func(h *portalHarness) string { return "https://other.example.org:" + h.idp.port() + "/oauth2/a/keys" },
		"http":         func(h *portalHarness) string { return "http://example.com:" + h.idp.port() + "/oauth2/a/keys" },
		"another port": func(h *portalHarness) string { return "https://example.com:1234/oauth2/a/keys" },
		"default port": func(h *portalHarness) string { return "https://example.com/oauth2/a/keys" },
		"userinfo":     func(h *portalHarness) string { return "https://u:p@example.com:" + h.idp.port() + "/oauth2/a/keys" },
		"empty":        func(h *portalHarness) string { return "" },
	} {
		t.Run(name, func(t *testing.T) {
			h := newPortalHarness(t, "example.com", true, "/oauth2/a")
			target := uri(h)
			h.idp.set(func(f *fakeIdP) { f.jwksURI = target })
			h.wantUnavailable(h.token(h.accessClaims()), portalCauseKeysOffOrigin)
			if _, keys := h.idp.hits(); keys != 0 {
				t.Fatal("the off-origin keys address was fetched")
			}
			if n := h.dialCount(); n != 1 {
				t.Fatalf("%d connections, want only the discovery one", n)
			}
		})
	}
	// The same origin with the default port spelled out is the same origin.
	h := newPortalHarness(t, "example.com", true, "/oauth2/a")
	h.idp.set(func(f *fakeIdP) { f.jwksURI = h.reg.Issuer + "/keys" })
	h.mustAdmit(h.token(h.accessClaims()))
}

func TestPortalDiscovery_PathPreserved(t *testing.T) {
	t.Run("public", func(t *testing.T) {
		h := newPortalHarness(t, "example.com", true, "/oauth2/default")
		h.mustAdmit(h.token(h.accessClaims()))
		want := []string{
			"example.com:" + h.idp.port() + "/oauth2/default/.well-known/openid-configuration",
			"example.com:" + h.idp.port() + "/oauth2/default/keys",
		}
		if !slices.Equal(h.idp.requests, want) {
			t.Fatalf("requests = %v, want %v", h.idp.requests, want)
		}
	})
	t.Run("trailing slash on the registered address is not doubled", func(t *testing.T) {
		h := internalHarness(t, "/oauth2/default", "/int/default/")
		h.mustAdmit(h.token(h.accessClaims()))
		if got := h.idp.requests[0]; got != "idp.internal.test:"+h.idp.port()+"/int/default/.well-known/openid-configuration" {
			t.Fatalf("discovery request = %q", got)
		}
	})
	t.Run("internal", func(t *testing.T) {
		h := internalHarness(t, "/oauth2/default", "/oauth2/default")
		h.mustAdmit(h.token(h.accessClaims()))
		want := []string{
			"idp.internal.test:" + h.idp.port() + "/oauth2/default/.well-known/openid-configuration",
			"idp.internal.test:" + h.idp.port() + "/oauth2/default/keys",
		}
		if !slices.Equal(h.idp.requests, want) {
			t.Fatalf("requests = %v, want %v", h.idp.requests, want)
		}
	})
}

func TestPortalFetch_SplitHorizonMetadataOtherHostRefused(t *testing.T) {
	// The document fetched at the internal address advertises the internal address as the issuer: refused.
	h := internalHarness(t, "/oauth2/a", "/oauth2/a")
	h.idp.set(func(f *fakeIdP) { f.docIssuer = h.reg.InternalIssuer })
	h.wantUnavailable(h.token(h.accessClaims()), portalCauseIssuerMismatch)
	// Or it advertises keys on the internal host: not the issuer's origin.
	h = internalHarness(t, "/oauth2/a", "/oauth2/a")
	h.idp.set(func(f *fakeIdP) { f.jwksURI = h.reg.InternalIssuer + "/keys" })
	h.wantUnavailable(h.token(h.accessClaims()), portalCauseKeysOffOrigin)
	// A request to any host outside the allowed set never leaves: no lookup, no connection.
	h = internalHarness(t, "/oauth2/a", "/oauth2/a")
	client := newPortalClient(h.pi.net, map[string]bool{"idp.internal.test:" + h.idp.port(): true})
	for _, target := range []string{
		"https://example.com:" + h.idp.port() + "/keys",
		"https://other.example.org:" + h.idp.port() + "/keys",
		"http://idp.internal.test:" + h.idp.port() + "/keys",
	} {
		var out map[string]any
		err := portalGetJSON(context.Background(), client, target, &out)
		if portalFetchCause(err) != portalCauseAddressRefused {
			t.Fatalf("%s: %v", target, err)
		}
	}
	if h.dialCount() != 0 || h.lookups != 0 {
		t.Fatalf("a refused destination was resolved or dialled: %d lookups, %d dials", h.lookups, h.dialCount())
	}
}

func TestPortalFetch_RedirectRefused(t *testing.T) {
	for _, loc := range []string{"/oauth2/a/elsewhere", "https://example.com:%s/oauth2/a/keys", "https://other.example.org:%s/x"} {
		t.Run(loc, func(t *testing.T) {
			h := newPortalHarness(t, "example.com", true, "/oauth2/a")
			target := strings.Replace(loc, "%s", h.idp.port(), 1)
			h.idp.set(func(f *fakeIdP) {
				f.override = func(w http.ResponseWriter, r *http.Request) bool {
					if strings.HasSuffix(r.URL.Path, "/.well-known/openid-configuration") {
						http.Redirect(w, r, target, http.StatusFound)
						return true
					}
					return false
				}
			})
			h.wantUnavailable(h.token(h.accessClaims()), portalCauseAddressRefused)
			if n := len(h.idp.requests); n != 1 || h.dialCount() != 1 {
				t.Fatalf("%d requests and %d dials: the redirect was followed", n, h.dialCount())
			}
		})
	}
}

func TestPortalFetch_PortMismatchRefused(t *testing.T) {
	h := newPortalHarness(t, "example.com", true, "/oauth2/a")
	h.idp.set(func(f *fakeIdP) { f.jwksURI = "https://example.com:1234/oauth2/a/keys" })
	h.wantUnavailable(h.token(h.accessClaims()), portalCauseKeysOffOrigin)
	// And at the transport: the same host on another port is not the allowed authority.
	client := newPortalClient(h.pi.net, map[string]bool{"example.com:" + h.idp.port(): true})
	var out map[string]any
	err := portalGetJSON(context.Background(), client, "https://example.com:1234/oauth2/a/keys", &out)
	if portalFetchCause(err) != portalCauseAddressRefused {
		t.Fatalf("err = %v", err)
	}
	for _, d := range h.dials {
		if strings.HasSuffix(d, ":1234") {
			t.Fatalf("dialled the mismatched port: %v", h.dials)
		}
	}
}

func TestPortalFetch_PrivateAddressOnlyForListedInternalHost(t *testing.T) {
	private := func(string) []net.IP { return []net.IP{net.ParseIP("10.0.0.5")} }
	t.Run("public issuer host on private space", func(t *testing.T) {
		h := newPortalHarness(t, "example.com", true, "/oauth2/a")
		h.setLookup(private)
		h.wantUnavailable(h.token(h.accessClaims()), portalCauseAddressRefused)
		if h.dialCount() != 0 {
			t.Fatal("a private address was dialled for a public issuer")
		}
	})
	t.Run("one private answer among public ones", func(t *testing.T) {
		h := newPortalHarness(t, "example.com", true, "/oauth2/a")
		h.setLookup(func(string) []net.IP { return []net.IP{portalPublicIP, net.ParseIP("192.168.1.1")} })
		h.wantUnavailable(h.token(h.accessClaims()), portalCauseAddressRefused)
		if h.dialCount() != 0 {
			t.Fatal("an address list holding a private address was dialled")
		}
	})
	t.Run("internal host not listed", func(t *testing.T) {
		h := internalHarness(t, "/oauth2/a", "/oauth2/a", func(o *PortalIssuersOptions) { o.InternalHosts = nil })
		h.wantUnavailable(h.token(h.accessClaims()), portalCauseAddressRefused)
		if h.dialCount() != 0 {
			t.Fatal("an unlisted internal host was dialled")
		}
	})
	t.Run("another port of a listed host", func(t *testing.T) {
		h := internalHarness(t, "/oauth2/a", "/oauth2/a", func(o *PortalIssuersOptions) { o.InternalHosts = []string{"idp.internal.test:1234"} })
		h.wantUnavailable(h.token(h.accessClaims()), portalCauseAddressRefused)
	})
	t.Run("listed host", func(t *testing.T) {
		h := internalHarness(t, "/oauth2/a", "/oauth2/a")
		h.mustAdmit(h.token(h.accessClaims()))
		if !slices.Contains(h.dials, "10.1.2.3:"+h.idp.port()) {
			t.Fatalf("dials = %v, want the checked private address", h.dials)
		}
	})
	t.Run("listed host never reaches loopback or metadata", func(t *testing.T) {
		for _, ip := range []string{"127.0.0.1", "::1", "169.254.169.254", "0.0.0.0", "224.0.0.1", "fd00:ec2::254"} {
			h := internalHarness(t, "/oauth2/a", "/oauth2/a")
			h.setLookup(func(string) []net.IP { return []net.IP{net.ParseIP(ip)} })
			h.wantUnavailable(h.token(h.accessClaims()), portalCauseAddressRefused)
			if h.dialCount() != 0 {
				t.Fatalf("%s was dialled", ip)
			}
		}
	})
}

func TestPortalFetch_RebindingDialsCheckedAddress(t *testing.T) {
	h := newPortalHarness(t, "example.com", true, "/oauth2/a")
	var calls atomic.Int32
	h.setLookup(func(string) []net.IP {
		if calls.Add(1) == 1 {
			return []net.IP{portalPublicIP}
		}
		return []net.IP{net.ParseIP("127.0.0.1")}
	})
	h.idp.set(func(f *fakeIdP) {
		f.override = func(w http.ResponseWriter, r *http.Request) bool {
			w.Header().Set("Connection", "close") // the keys request must dial again
			return false
		}
	})
	// The first resolution is public: that exact address is dialled, never a second resolution of the name.
	// The second resolution is loopback: it is refused and never dialled.
	h.wantUnavailable(h.token(h.accessClaims()), portalCauseKeysUnreachable)
	want := []string{"8.8.8.8:" + h.idp.port()}
	if !slices.Equal(h.dials, want) {
		t.Fatalf("dials = %v, want %v", h.dials, want)
	}
	if h.lookups != 2 {
		t.Fatalf("%d resolutions, want one per connection", h.lookups)
	}
}

func TestPortalFetch_ProxyEnvironmentIgnored(t *testing.T) {
	proxy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	var hit atomic.Int32
	go func() {
		for {
			c, err := proxy.Accept()
			if err != nil {
				return
			}
			hit.Add(1)
			_ = c.Close()
		}
	}()
	for _, k := range []string{"HTTPS_PROXY", "HTTP_PROXY", "ALL_PROXY", "https_proxy", "http_proxy"} {
		t.Setenv(k, "http://"+proxy.Addr().String())
	}
	t.Setenv("NO_PROXY", "")
	h := newPortalHarness(t, "example.com", true, "/oauth2/a")
	h.mustAdmit(h.token(h.accessClaims()))
	if hit.Load() != 0 {
		t.Fatal("a request went to the proxy named by the environment")
	}
	client := newPortalClient(h.pi.net, map[string]bool{"example.com:1": true})
	if client.Transport.(*portalTransport).base.Proxy != nil {
		t.Fatal("the portal transport has a proxy function")
	}
}

func TestPortalFetch_BodyCapAndNoBodyInErrors(t *testing.T) {
	const secret = "SECRET-BODY-9f3a"
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(prev)
	cases := map[string]struct {
		handler func(w http.ResponseWriter)
		detail  string
	}{
		"over the cap":   {func(w http.ResponseWriter) { _, _ = w.Write(bytes.Repeat([]byte("a"), portalFetchBodyCap+1)) }, "body too large"},
		"server error":   {func(w http.ResponseWriter) { w.WriteHeader(500); _, _ = w.Write([]byte(secret)) }, "status 5xx"},
		"malformed json": {func(w http.ResponseWriter) { _, _ = w.Write([]byte(`{"issuer": "` + secret)) }, "malformed json"},
		"wrong type":     {func(w http.ResponseWriter) { _, _ = w.Write([]byte(`["` + secret + `"]`)) }, "malformed json"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h := newPortalHarness(t, "example.com", true, "/oauth2/a")
			h.idp.set(func(f *fakeIdP) {
				f.override = func(w http.ResponseWriter, r *http.Request) bool { c.handler(w); return true }
			})
			client := newPortalClient(h.pi.net, map[string]bool{"example.com:" + h.idp.port(): true})
			var out struct{ Issuer string }
			err := portalGetJSON(context.Background(), client, h.reg.Issuer+"/x", &out)
			if err == nil || !strings.Contains(err.Error(), c.detail) || strings.Contains(err.Error(), secret) {
				t.Fatalf("error = %v, want %q and no body", err, c.detail)
			}
			_, r := h.verify(h.token(h.accessClaims()))
			if r.Reason != PortalIssuerUnavailable || strings.Contains(fmt.Sprintf("%+v", r), secret) {
				t.Fatalf("refusal = %+v", r)
			}
		})
	}
	if strings.Contains(logs.String(), secret) {
		t.Fatalf("an upstream body reached a log line: %s", logs.String())
	}
}

func TestPortalFetch_EmbeddedIPv4FormsRefused(t *testing.T) {
	refused := []string{
		"::10.0.0.1", "::127.0.0.1", "::8.8.8.8", // IPv4-compatible
		"2002:a00:1::", "2002:7f00:1::", "2002:808:808::", // 6to4
		"64:ff9b::a00:1", "64:ff9b::7f00:1", "64:ff9b::808:808", // NAT64
		"::ffff:10.0.0.1", // IPv4-mapped private
	}
	h := internalHarness(t, "/oauth2/a", "/oauth2/a")
	for _, listed := range []bool{false, true} {
		host := "example.com"
		if listed {
			host = "idp.internal.test"
		}
		client := newPortalClient(h.pi.net, map[string]bool{host + ":" + h.idp.port(): true})
		for _, s := range refused {
			if listed && s == "::ffff:10.0.0.1" {
				continue // a listed internal host may sit on private space, mapped or not
			}
			ip := net.ParseIP(s)
			h.setLookup(func(string) []net.IP { return []net.IP{ip} })
			var out map[string]any
			err := portalGetJSON(context.Background(), client, "https://"+host+":"+h.idp.port()+"/x", &out)
			if portalFetchCause(err) != portalCauseAddressRefused {
				t.Fatalf("%s (listed host %v): %v", s, listed, err)
			}
		}
	}
	if h.dialCount() != 0 {
		t.Fatalf("%d connections for refused address forms: %v", h.dialCount(), h.dials)
	}
	// The control: a public IPv4-mapped address is dialled.
	client := newPortalClient(h.pi.net, map[string]bool{"example.com:" + h.idp.port(): true})
	h.setLookup(func(string) []net.IP { return []net.IP{net.ParseIP("::ffff:8.8.8.8")} })
	var out map[string]any
	_ = portalGetJSON(context.Background(), client, "https://example.com:"+h.idp.port()+"/x", &out)
	if h.dialCount() != 1 {
		t.Fatal("the control address was not dialled, so the zero counts above prove nothing")
	}
}

func TestPortalFetch_InternalCAOnlyForListedHost(t *testing.T) {
	noPublicTrust := func(o *PortalIssuersOptions) {
		o.roots = x509.NewCertPool()
		o.InternalCAPEM = portalCAPEM
	}
	// The listed internal host verifies against the operator's bundle.
	h := internalHarness(t, "/oauth2/a", "/oauth2/a", noPublicTrust)
	h.mustAdmit(h.token(h.accessClaims()))
	// The same bundle does not vouch for the issuer's own host, nor for an unlisted one.
	pub := newPortalHarness(t, "example.com", true, "/oauth2/a", func(o *PortalIssuersOptions) {
		noPublicTrust(o)
		o.InternalHosts = []string{"idp.internal.test:1"}
	})
	pub.wantUnavailable(pub.token(pub.accessClaims()), portalCauseUnreachable)
	unlisted := internalHarness(t, "/oauth2/a", "/oauth2/a", noPublicTrust, func(o *PortalIssuersOptions) { o.InternalHosts = nil })
	unlisted.setLookup(func(string) []net.IP { return []net.IP{portalPublicIP} })
	unlisted.wantUnavailable(unlisted.token(unlisted.accessClaims()), portalCauseUnreachable)
	// A bundle with no certificate is refused at construction.
	if _, err := NewPortalIssuers(PortalIssuersOptions{Reserved: func(string) bool { return false }, InternalCAPEM: []byte("not pem")}); err == nil {
		t.Fatal("a bundle holding no certificate was accepted")
	}
	if _, err := NewPortalIssuers(PortalIssuersOptions{}); err == nil {
		t.Fatal("a verifier set with no reserved-name check was built")
	}
}
