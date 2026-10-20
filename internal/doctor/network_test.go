// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package doctor

import (
	"context"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// org is an httptest TLS server whose certificate no system root signed, with the pool that holds it.
type org struct {
	srv  *httptest.Server
	pool *x509.CertPool
}

func newOrg(t *testing.T, h http.HandlerFunc) org {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return org{srv, pool}
}

func healthy(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/healthz":
		_, _ = w.Write([]byte(`{"status":"ok","version":"0.10.0"}`))
	case "/api/v1/me":
		if r.Header.Get("Authorization") != "Bearer tok-secret-1234" {
			http.Error(w, `{"error":"sign in"}`, http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"method":"token","role":"admin","principal":"someone","email":"a@b.c"}`))
	default:
		http.NotFound(w, r)
	}
}

func direct(*http.Request) (*url.URL, error) { return nil, nil }

func viaProxy(t *testing.T, raw string) func(*http.Request) (*url.URL, error) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return http.ProxyURL(u)
}

func resolves(string) ([]string, error) { return []string{"203.0.113.7"}, nil }

func resolver(f func(string) ([]string, error)) func(context.Context, string) ([]string, error) {
	return func(_ context.Context, host string) ([]string, error) { return f(host) }
}

func byName(t *testing.T, stages []Stage, name string) Stage {
	t.Helper()
	for _, s := range stages {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no %q stage in %+v", name, stages)
	return Stage{}
}

func text(t *testing.T, stages []Stage) string {
	t.Helper()
	var b strings.Builder
	if err := (Report{Stages: stages}).WriteText(&b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// base is a CLI-shaped run against o: its certificate is unknown unless a test adds the pool.
func base(o org) Network {
	return Network{OrgURL: o.srv.URL, Source: "WARDYN_URL", OrgSetting: "WARDYN_URL", Identity: true, Token: "tok-secret-1234", TokenSource: "WARDYN_TOKEN",
		LookupHost: resolver(func(string) ([]string, error) { return resolves("") }), ProxyFor: direct, ExtraCASource: "WARDYN_TRUSTED_CA_FILE"}
}

// proxyThatAnswers is a CONNECT proxy that refuses every request with status.
func proxyThatAnswers(t *testing.T, status int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status == http.StatusProxyAuthRequired {
			w.Header().Set("Proxy-Authenticate", `Basic realm="corp"`)
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func closedAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// TestEveryStageNamesItsSetting fakes one failure per stage and checks that the failing line names the setting
// that fixes it, in the plain output as well as in the stage.
func TestEveryStageNamesItsSetting(t *testing.T) {
	good := newOrg(t, healthy)
	notWardyn := newOrg(t, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	portal := newOrg(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>sign in to the wifi</html>"))
	})
	moved := newOrg(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://elsewhere.example.com/healthz", http.StatusFound)
	})
	locked := newOrg(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			healthy(w, r)
			return
		}
		http.Error(w, "no", http.StatusForbidden)
	})

	for _, tc := range []struct {
		name    string
		net     func() Network
		stage   string
		setting string
		in      string // text the failing line must also carry
	}{
		{"unusable URL", func() Network { n := base(good); n.OrgURL = "ftp://wardyn.example.com"; return n }, "org-url", "WARDYN_URL", ""},
		{"no URL at all", func() Network { n := base(good); n.OrgURL = ""; return n }, "org-url", "WARDYN_URL", "no organisation address"},
		{"runner URL must be https", func() Network { n := base(good); n.OrgURL = "http://wardyn.example.com"; n.Strict = true; return n }, "org-url", "WARDYN_URL", "https://"},
		{"name does not resolve", func() Network {
			n := base(good)
			n.OrgURL = "https://wardyn.example.test"
			n.LookupHost = resolver(func(string) ([]string, error) { return nil, errors.New("no such host") })
			return n
		}, "dns", "WARDYN_URL", "wardyn.example.test does not resolve"},
		{"proxy name does not resolve", func() Network {
			n := base(good)
			n.OrgURL = "https://wardyn.example.test"
			n.ProxyFor = viaProxy(t, "http://proxy.example.test:3128")
			n.LookupHost = resolver(func(host string) ([]string, error) {
				if host == "proxy.example.test" {
					return nil, errors.New("no such host")
				}
				return resolves(host)
			})
			return n
		}, "dns", "HTTPS_PROXY", "proxy proxy.example.test does not resolve"},
		{"proxy settings unreadable", func() Network {
			n := base(good)
			n.ProxyFor = func(*http.Request) (*url.URL, error) { return nil, errors.New("invalid proxy") }
			return n
		}, "proxy", "HTTPS_PROXY", "not repeated"},
		{"proxy wants authentication", func() Network {
			n := base(good)
			n.ProxyFor = viaProxy(t, proxyThatAnswers(t, http.StatusProxyAuthRequired))
			return n
		}, "tls", "HTTPS_PROXY", "407"},
		{"proxy forbids the destination", func() Network {
			n := base(good)
			n.ProxyFor = viaProxy(t, proxyThatAnswers(t, http.StatusForbidden))
			return n
		}, "tls", "HTTPS_PROXY", "403"},
		{"proxy not listening", func() Network {
			n := base(good)
			n.ProxyFor = viaProxy(t, "http://"+closedAddr(t))
			return n
		}, "tls", "HTTPS_PROXY", "did not accept the connection"},
		{"unknown authority", func() Network { return base(good) }, "tls", "WARDYN_TRUSTED_CA_FILE", "Acme Co"},
		{"certificate for another name", func() Network {
			n := base(good)
			n.OrgURL = strings.Replace(good.srv.URL, "127.0.0.1", "localhost", 1)
			n.SystemRoots = good.pool
			return n
		}, "tls", "WARDYN_URL", "does not cover localhost"},
		{"bundle the CLI does not read", func() Network { n := base(good); n.ExtraCA = good.pool; return n }, "tls", "SSL_CERT_FILE", "WARDYN_TRUSTED_CA_FILE"},
		{"nothing listening", func() Network { n := base(good); n.OrgURL = "https://" + closedAddr(t); return n }, "tls", "WARDYN_URL", "no connection to"},
		{"not a Wardyn address", func() Network { n := base(notWardyn); n.SystemRoots = notWardyn.pool; return n }, "healthz", "WARDYN_URL", "answered 404"},
		{"a portal answers", func() Network { n := base(portal); n.SystemRoots = portal.pool; return n }, "healthz", "WARDYN_URL", "not Wardyn's health report"},
		{"redirected", func() Network { n := base(moved); n.SystemRoots = moved.pool; return n }, "healthz", "WARDYN_URL", "elsewhere.example.com"},
		{"token refused", func() Network { n := base(good); n.SystemRoots = good.pool; n.Token = "wrong-token-9999"; return n }, "identity", "WARDYN_TOKEN", "was refused"},
		{"no token", func() Network { n := base(good); n.SystemRoots = good.pool; n.Token = ""; return n }, "identity", "WARDYN_TOKEN", "no token was sent"},
		{"me forbidden", func() Network { n := base(locked); n.SystemRoots = locked.pool; return n }, "identity", "WARDYN_TOKEN", "403"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := tc.net()
			stages := n.Run(context.Background())
			s := byName(t, stages, tc.stage)
			if s.Status != StatusFail || s.Setting != tc.setting {
				t.Fatalf("%s = %s setting %q, want FAIL naming %s\n%s", tc.stage, s.Status, s.Setting, tc.setting, text(t, stages))
			}
			line := ""
			for _, l := range strings.Split(text(t, stages), "\n") {
				if strings.Contains(l, tc.stage+" ") && strings.HasPrefix(l, "FAIL") {
					line = l
				}
			}
			if !strings.Contains(line, "set "+tc.setting+" to") || !strings.Contains(line, tc.in) {
				t.Errorf("the plain line %q must say %q and carry %q", line, "set "+tc.setting+" to", tc.in)
			}
		})
	}
}

func TestHealthyRunIsAllOKAndNamesTheTrustStore(t *testing.T) {
	o := newOrg(t, healthy)
	for _, tc := range []struct {
		name  string
		set   func(*Network)
		store string
	}{
		{"system", func(n *Network) { n.SystemRoots = o.pool }, "system trust store"},
		{"the runner's bundle", func(n *Network) { n.ExtraCA, n.ExtraCATrusted = o.pool, true }, "WARDYN_TRUSTED_CA_FILE trust store"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := base(o)
			tc.set(&n)
			stages := n.Run(context.Background())
			if !(Report{Stages: stages}).OK() {
				t.Fatalf("not all ok:\n%s", text(t, stages))
			}
			for _, s := range stages {
				if s.Status != StatusOK {
					t.Errorf("%s = %s, want ok:\n%s", s.Name, s.Status, text(t, stages))
				}
			}
			if tls := byName(t, stages, "tls"); !strings.Contains(tls.Detail, tc.store) || !strings.Contains(tls.Detail, "Acme Co") {
				t.Errorf("tls = %q, want the issuer and %q", tls.Detail, tc.store)
			}
			if n.ServerVersion != "0.10.0" || !strings.Contains(byName(t, stages, "healthz").Detail, "0.10.0") {
				t.Errorf("server version = %q, healthz = %q", n.ServerVersion, byName(t, stages, "healthz").Detail)
			}
			if id := byName(t, stages, "identity"); !strings.Contains(id.Detail, "method token, role admin") {
				t.Errorf("identity = %q", id.Detail)
			}
		})
	}
}

func TestFailureSkipsWhatNeedsIt(t *testing.T) {
	n := base(newOrg(t, healthy))
	stages := n.Run(context.Background())
	for _, name := range []string{"healthz", "identity"} {
		if s := byName(t, stages, name); s.Status != StatusSkip {
			t.Errorf("%s = %s after a failed handshake, want skip", name, s.Status)
		}
	}
}

func TestRunnerNetworkHasNoIdentityStage(t *testing.T) {
	o := newOrg(t, healthy)
	n := base(o)
	n.Identity, n.Token, n.SystemRoots = false, "", o.pool
	for _, s := range n.Run(context.Background()) {
		if s.Name == "identity" {
			t.Fatalf("a runner's doctor must not open an identity stage: %+v", s)
		}
	}
}

func TestPlainHTTPHasNoHandshakeToCheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(healthy))
	t.Cleanup(srv.Close)
	n := base(org{srv: srv})
	stages := n.Run(context.Background())
	if s := byName(t, stages, "tls"); s.Status != StatusSkip {
		t.Errorf("tls = %s on http://, want skip", s.Status)
	}
	if s := byName(t, stages, "healthz"); s.Status != StatusOK {
		t.Errorf("healthz = %s on http://, want ok", s.Status)
	}
}
