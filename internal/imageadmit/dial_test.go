// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package imageadmit

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDefaultGuardRefusesLoopbackAndMetadataKeepsRFC1918(t *testing.T) {
	g := defaultGuard()
	for _, h := range []string{"127.0.0.1", "::1", "169.254.169.254", "169.254.170.2", "fd00:ec2::254", "64:ff9b::a9fe:a9fe", "0.0.0.0", "224.0.0.1"} {
		if err := g.check(context.Background(), h, false); !errors.Is(err, ErrRefusedTarget) {
			t.Errorf("check(%q) = %v, want ErrRefusedTarget", h, err)
		}
	}
	for _, h := range []string{"10.1.2.3", "172.16.0.9", "192.168.5.5", "93.184.216.34", "2606:4700::1111"} {
		if err := g.check(context.Background(), h, false); err != nil {
			t.Errorf("check(%q) = %v, want allowed", h, err)
		}
	}
}

func TestGuardResolvesNamesLocallyAndRefusesAnyForbiddenAnswer(t *testing.T) {
	g := testGuard()
	g.lookup = func(_ context.Context, h string) ([]net.IP, error) {
		switch h {
		case "meta.internal":
			return []net.IP{net.ParseIP("10.0.0.5"), net.ParseIP("169.254.169.254")}, nil
		case "fine.internal":
			return []net.IP{net.ParseIP("10.0.0.5")}, nil
		}
		return nil, &net.DNSError{Err: "no such host", Name: h, IsNotFound: true}
	}
	if err := g.check(context.Background(), "meta.internal", false); !errors.Is(err, ErrRefusedTarget) {
		t.Errorf("a name with one metadata answer: %v", err)
	}
	if err := g.check(context.Background(), "fine.internal", false); err != nil {
		t.Errorf("an RFC1918 name: %v", err)
	}
	if err := g.check(context.Background(), "nx.internal", false); err == nil {
		t.Error("without a proxy an unresolvable name must fail")
	}
	if err := g.check(context.Background(), "nx.internal", true); err != nil {
		t.Errorf("behind a forward proxy the proxy resolves a name this host cannot: %v", err)
	}
}

func TestLinkLocalRealmRefusedAtDial(t *testing.T) {
	reg := bearerRegistry("https://169.254.169.254/token", "tok", nil)
	defer reg.Close()
	c := newTestClient(t, mapAuth{host(reg): {Username: "u", Password: "p"}}, testGuard(), nil, reg)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := c.Manifest(ctx, mustRef(t, host(reg)+"/team/app:1.0"))
	var un *Unavailable
	if !errors.As(err, &un) || un.Cause != CauseRefused || !errors.Is(err, ErrRefusedTarget) {
		t.Fatalf("err = %v, want Unavailable{refused} wrapping ErrRefusedTarget", err)
	}
	if un.Host != "169.254.169.254" {
		t.Errorf("Host = %q, want the realm host", un.Host)
	}
}

func TestLinkLocalRegistryRefusedAtDial(t *testing.T) {
	c := newTestClient(t, nil, testGuard(), nil)
	_, err := c.Manifest(context.Background(), mustRef(t, "169.254.169.254/x/y:z"))
	if !errors.Is(err, ErrRefusedTarget) {
		t.Fatalf("err = %v, want ErrRefusedTarget", err)
	}
}

// fakeProxy is a forward proxy that records every target it is handed. With tunnel set it connects CONNECT
// requests to that address, so a permitted target is reachable through it.
type fakeProxy struct {
	srv    *httptest.Server
	mu     sync.Mutex
	seen   []string
	tunnel string
}

func newFakeProxy(tunnel string) *fakeProxy {
	p := &fakeProxy{tunnel: tunnel}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.seen = append(p.seen, r.Method+" "+r.Host)
		p.mu.Unlock()
		if r.Method != http.MethodConnect || p.tunnel == "" {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		up, err := net.Dial("tcp", p.tunnel)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		cc, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		_, _ = cc.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
		go func() { _, _ = io.Copy(up, cc); _ = up.Close() }()
		go func() { _, _ = io.Copy(cc, up); _ = cc.Close() }()
	}))
	return p
}

func (p *fakeProxy) hits() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.seen...)
}
func (p *fakeProxy) fn() func(*http.Request) (*url.URL, error) {
	u, _ := url.Parse(p.srv.URL)
	return http.ProxyURL(u)
}

func TestForbiddenTargetsNeverReachForwardProxy(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	t.Run("registry literal", func(t *testing.T) {
		for _, h := range []string{"169.254.169.254", "127.0.0.1:5000"} {
			p := newFakeProxy("")
			defer p.srv.Close()
			c := newTestClient(t, nil, defaultGuard(), p.fn())
			_, err := c.Manifest(ctx, mustRef(t, h+"/x/y:z"))
			if !errors.Is(err, ErrRefusedTarget) {
				t.Errorf("%s: err = %v, want ErrRefusedTarget", h, err)
			}
			if got := p.hits(); len(got) != 0 {
				t.Errorf("%s reached the proxy: %v", h, got)
			}
		}
	})
	t.Run("name that resolves to metadata", func(t *testing.T) {
		p := newFakeProxy("")
		defer p.srv.Close()
		g := testGuard()
		g.lookup = func(context.Context, string) ([]net.IP, error) { return []net.IP{net.ParseIP("169.254.169.254")}, nil }
		_, err := newTestClient(t, nil, g, p.fn()).Manifest(ctx, mustRef(t, "meta.example.com/x/y:z"))
		if !errors.Is(err, ErrRefusedTarget) || len(p.hits()) != 0 {
			t.Fatalf("err = %v, proxy hits %v", err, p.hits())
		}
	})
	t.Run("realm and redirect targets", func(t *testing.T) {
		for _, kind := range []string{"realm", "redirect"} {
			p := newFakeProxy("")
			defer p.srv.Close()
			var reg *httptest.Server
			if kind == "realm" {
				reg = bearerRegistry("https://169.254.169.254/token", "tok", nil)
			} else {
				reg = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					http.Redirect(w, r, "https://169.254.169.254/blob", http.StatusFound)
				}))
			}
			defer reg.Close()
			// The registry is reached directly (NoProxy-style: the proxy fn skips it); only the forbidden hop would use the proxy.
			via := func(r *http.Request) (*url.URL, error) {
				if r.URL.Hostname() == "169.254.169.254" {
					return p.fn()(r)
				}
				return nil, nil
			}
			c := newTestClient(t, nil, testGuard(), via, reg)
			_, err := c.Manifest(ctx, mustRef(t, host(reg)+"/team/app:1.0"))
			if !errors.Is(err, ErrRefusedTarget) {
				t.Errorf("%s: err = %v, want ErrRefusedTarget", kind, err)
			}
			if got := p.hits(); len(got) != 0 {
				t.Errorf("%s target reached the proxy: %v", kind, got)
			}
		}
	})
}

func TestPermittedTargetGoesThroughForwardProxy(t *testing.T) {
	reg := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveManifest(w, imageBody, ociManifestType, "")
	}))
	defer reg.Close()
	p := newFakeProxy(strings.TrimPrefix(reg.URL, "https://"))
	defer p.srv.Close()
	// Loopback is refused for registries, and the proxy itself is on loopback: the operator's proxy address is
	// theirs to choose, only the targets are checked. The name resolves to an RFC1918 address nothing listens on.
	g := defaultGuard()
	g.lookup = func(context.Context, string) ([]net.IP, error) { return []net.IP{net.ParseIP("10.9.9.9")}, nil }
	c := newTestClient(t, nil, g, p.fn(), reg)
	m, err := c.Manifest(context.Background(), mustRef(t, "example.com/team/app:1.0"))
	if err != nil {
		t.Fatal(err)
	}
	if m.Digest != sum(imageBody) {
		t.Fatalf("digest = %s", m.Digest)
	}
	if got := p.hits(); len(got) != 1 || got[0] != "CONNECT example.com:443" {
		t.Fatalf("proxy saw %v, want one CONNECT example.com:443", got)
	}
}

func TestUnresolvableNameIsLeftToTheForwardProxy(t *testing.T) {
	p := newFakeProxy("")
	defer p.srv.Close()
	g := testGuard()
	g.lookup = func(_ context.Context, h string) ([]net.IP, error) {
		return nil, &net.DNSError{Err: "no such host", Name: h}
	}
	c := newTestClient(t, nil, g, p.fn())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := c.Manifest(ctx, mustRef(t, "internal-only.corp/x/y:z"))
	if err == nil {
		t.Fatal("expected the proxy's 502 to fail the call")
	}
	if got := p.hits(); len(got) != 1 || got[0] != "CONNECT internal-only.corp:443" {
		t.Fatalf("proxy saw %v", got)
	}
}

// The proxy exemption belongs to the request that selected the proxy, not to an address remembered
// for later: a proxy used once must not make the same host:port reachable by a direct dial.
func TestProxyAddressIsNotRememberedForDirectDials(t *testing.T) {
	reg := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveManifest(w, imageBody, ociManifestType, "")
	}))
	defer reg.Close()
	p := newFakeProxy(strings.TrimPrefix(reg.URL, "https://"))
	defer p.srv.Close()
	proxyHost := strings.TrimPrefix(p.srv.URL, "http://")
	viaProxy := p.fn()
	// example.com goes through the proxy; the proxy's own address is NO_PROXY (direct).
	proxy := func(r *http.Request) (*url.URL, error) {
		if r.URL.Host == proxyHost {
			return nil, nil
		}
		return viaProxy(r)
	}
	g := defaultGuard()
	g.lookup = func(_ context.Context, h string) ([]net.IP, error) {
		if ip := net.ParseIP(h); ip != nil {
			return []net.IP{ip}, nil
		}
		return []net.IP{net.ParseIP("10.9.9.9")}, nil
	}
	c := newTestClient(t, nil, g, proxy, reg)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := c.Manifest(ctx, mustRef(t, "example.com/team/app:1.0")); err != nil {
		t.Fatalf("first, proxied: %v", err)
	}
	before := len(p.hits())
	_, err := c.Manifest(ctx, mustRef(t, proxyHost+"/admin/x:1"))
	if !errors.Is(err, ErrRefusedTarget) {
		t.Fatalf("direct dial of the proxy's address: err = %v, want ErrRefusedTarget", err)
	}
	if got := p.hits(); len(got) != before {
		t.Fatalf("the proxy's address was connected to directly: %v", got[before:])
	}
}

func TestRedirectToProxyAddressDirectIsRefused(t *testing.T) {
	var proxyHost atomic.Value
	reg := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://"+proxyHost.Load().(string)+"/admin", http.StatusFound)
	}))
	defer reg.Close()
	p := newFakeProxy(strings.TrimPrefix(reg.URL, "https://"))
	defer p.srv.Close()
	proxyHost.Store(strings.TrimPrefix(p.srv.URL, "http://"))
	viaProxy := p.fn()
	proxy := func(r *http.Request) (*url.URL, error) {
		if r.URL.Host == proxyHost.Load().(string) {
			return nil, nil
		}
		return viaProxy(r)
	}
	g := defaultGuard()
	g.lookup = func(_ context.Context, h string) ([]net.IP, error) {
		if ip := net.ParseIP(h); ip != nil {
			return []net.IP{ip}, nil
		}
		return []net.IP{net.ParseIP("10.9.9.9")}, nil
	}
	c := newTestClient(t, nil, g, proxy, reg)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := c.Manifest(ctx, mustRef(t, "example.com/team/app:1.0"))
	if !errors.Is(err, ErrRefusedTarget) {
		t.Fatalf("err = %v, want ErrRefusedTarget", err)
	}
	for _, h := range p.hits() {
		if !strings.HasPrefix(h, "CONNECT example.com") {
			t.Errorf("the proxy saw %q", h)
		}
	}
}

// A direct dial asks the resolver once, and connects to the address it checked.
func TestDirectDialResolvesTheNameOnce(t *testing.T) {
	reg := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveManifest(w, imageBody, ociManifestType, "")
	}))
	defer reg.Close()
	g := testGuard()
	var lookups atomic.Int32
	inner := g.lookup
	g.lookup = func(ctx context.Context, h string) ([]net.IP, error) {
		lookups.Add(1)
		return inner(ctx, h)
	}
	c := newTestClient(t, nil, g, nil, reg)
	if _, err := c.Manifest(context.Background(), mustRef(t, host(reg)+"/team/app:1.0")); err != nil {
		t.Fatal(err)
	}
	if n := lookups.Load(); n != 1 {
		t.Errorf("lookups = %d, want 1", n)
	}
}
