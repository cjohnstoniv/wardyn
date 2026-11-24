// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const reuseHost = "reuse.example"

// reuseResolver answers every host with one address it can change, and counts the lookups.
type reuseResolver struct {
	lookups atomic.Int64
	mu      sync.Mutex
	ip      net.IP
}

func newReuseResolver(ip string) *reuseResolver { return &reuseResolver{ip: net.ParseIP(ip)} }

func (r *reuseResolver) LookupIP(string) ([]net.IP, error) {
	r.lookups.Add(1)
	r.mu.Lock()
	defer r.mu.Unlock()
	return []net.IP{r.ip}, nil
}

func (r *reuseResolver) answer(ip string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ip = net.ParseIP(ip)
}

// reuseUpstream is an HTTP/1.1 TLS upstream that counts the connections it accepts and the handshakes that
// did not resume a session.
type reuseUpstream struct {
	srv          *httptest.Server
	accepts      atomic.Int64
	fullHandshks atomic.Int64
}

func newReuseUpstream(tb testing.TB, closeEach bool) *reuseUpstream {
	tb.Helper()
	u := &reuseUpstream{}
	u.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if closeEach {
			w.Header().Set("Connection", "close")
		}
		_, _ = io.WriteString(w, "ok")
	}))
	u.srv.Config.ConnState = func(_ net.Conn, st http.ConnState) {
		if st == http.StateNew {
			u.accepts.Add(1)
		}
	}
	u.srv.TLS = &tls.Config{VerifyConnection: func(cs tls.ConnectionState) error {
		if !cs.DidResume {
			u.fullHandshks.Add(1)
		}
		return nil
	}}
	u.srv.StartTLS()
	tb.Cleanup(u.srv.Close)
	return u
}

// reuseProxy is a run's proxy that terminates reuseHost and sends it to up, resolving through res.
func reuseProxy(up *reuseUpstream, res resolver, now func() time.Time) *Proxy {
	return newProxy(Options{
		RunID:           uuid.New(),
		Policy:          CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{reuseHost}}),
		Sink:            &decisionSink{out: &bytes.Buffer{}, ch: make(chan egress.DecisionLog, 4096)},
		MITMHosts:       []string{reuseHost},
		Resolver:        res,
		Now:             now,
		TLSClientConfig: testInsecureTLSConfig,
		Dial:            redirectDial(upstreamAddr(up.srv)),
	})
}

// reuseGet sends one inner request for reuseHost through p's terminated-tunnel path.
func reuseGet(tb testing.TB, p *Proxy) {
	rec := httptest.NewRecorder()
	p.serveMITMRequest(rec, httptest.NewRequest(http.MethodGet, "https://"+reuseHost+"/x", nil), reuseHost, 443)
	if rec.Code != http.StatusOK {
		tb.Errorf("status %d body %q, want 200", rec.Code, rec.Body.String())
	}
}

// TestProxyResolvesAHostOncePerCacheTime: ten requests to one host in a row cost one lookup.
func TestProxyResolvesAHostOncePerCacheTime(t *testing.T) {
	up := newReuseUpstream(t, false)
	res := newReuseResolver("93.184.216.34")
	p := reuseProxy(up, res, nil)
	for range 10 {
		reuseGet(t, p)
	}
	if n := res.lookups.Load(); n != 1 {
		t.Errorf("%d lookups for 10 requests to one host, want 1", n)
	}
}

// TestProxyCachedAnswerIsVettedOnEveryUse: the cache keeps a lookup's answer, never the guard's verdict. A
// private answer is refused on every use, and an answer that changes to a private address is refused once
// the cached one has run out.
func TestProxyCachedAnswerIsVettedOnEveryUse(t *testing.T) {
	var mu sync.Mutex
	clock := time.Now()
	now := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return clock
	}
	advance := func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		clock = clock.Add(d)
	}
	t.Run("a private answer is refused on every use", func(t *testing.T) {
		res := newReuseResolver("10.0.0.5")
		p := reuseProxy(newReuseUpstream(t, false), res, now)
		for i := range 3 {
			if g := p.vetHost(reuseHost); !g.Denied {
				t.Fatalf("use %d: the private answer was admitted: %+v", i, g)
			}
		}
		if n := res.lookups.Load(); n != 1 {
			t.Errorf("%d lookups, want 1: the answer is cached and vetted again on each use", n)
		}
	})
	t.Run("a rebound answer is refused once the cached one runs out", func(t *testing.T) {
		res := newReuseResolver("93.184.216.34")
		p := reuseProxy(newReuseUpstream(t, false), res, now)
		if g := p.vetHost(reuseHost); g.Denied {
			t.Fatalf("public answer refused: %s", g.Reason)
		}
		res.answer("10.0.0.5")
		advance(31 * time.Second)
		if g := p.vetHost(reuseHost); !g.Denied {
			t.Fatalf("a rebound private answer was admitted after the cache time: %+v", g)
		}
	})
}

// TestUpstreamTLSSessionResumes: a second connection to an upstream resumes the first one's TLS session
// instead of paying a full handshake.
func TestUpstreamTLSSessionResumes(t *testing.T) {
	up := newReuseUpstream(t, true) // each answer closes its connection, so each request dials anew
	p := reuseProxy(up, newReuseResolver("93.184.216.34"), nil)
	for range 3 {
		reuseGet(t, p)
	}
	if a, f := up.accepts.Load(), up.fullHandshks.Load(); a != 3 || f != 1 {
		t.Errorf("%d connections and %d full handshakes, want 3 and 1: later connections resume the session", a, f)
	}
}

// TestGatewayCertDialNeverResumes: a dial that offers the run's client certificate resumes no session, so
// every handshake proves the certificate held now.
func TestGatewayCertDialNeverResumes(t *testing.T) {
	p := &Proxy{gatewayCert: &gatewayCertHolder{}, gatewayRoutes: []GatewayIdentityRoute{{Host: "gw.example", Port: 443}}}
	cfg := &tls.Config{ClientSessionCache: tls.NewLRUClientSessionCache(1)}
	p.offerGatewayClientCert(cfg, "gw.example:443")
	if cfg.GetClientCertificate == nil {
		t.Fatal("no client certificate offered")
	}
	if cfg.ClientSessionCache != nil {
		t.Error("a dial offering the client certificate keeps a session cache")
	}
}

// ticketCounter is a client session cache that counts the tickets a server sends.
type ticketCounter struct{ puts atomic.Int64 }

func (c *ticketCounter) Get(string) (*tls.ClientSessionState, bool) { return nil, false }
func (c *ticketCounter) Put(_ string, cs *tls.ClientSessionState) {
	if cs != nil {
		c.puts.Add(1)
	}
}

// TestMITMTunnelSendsNoSessionTicket: a terminated tunnel sends the client no session ticket, which it could
// never redeem.
func TestMITMTunnelSendsNoSessionTicket(t *testing.T) {
	up := newReuseUpstream(t, false)
	certPEM, keyPEM := genTestCA(t)
	ca, err := newCertAuthority(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	p := newProxy(Options{
		RunID:  uuid.New(),
		Policy: CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{reuseHost}}),
		Sink:   &decisionSink{out: &bytes.Buffer{}, ch: make(chan egress.DecisionLog, 8)},
		CA:     ca, MITMHosts: []string{reuseHost}, Resolver: publicResolver{},
		TLSClientConfig: testInsecureTLSConfig, Dial: redirectDial(upstreamAddr(up.srv)),
	})
	proxySrv := httptest.NewServer(p)
	t.Cleanup(proxySrv.Close)
	conn, status := connectThrough(t, proxySrv.URL, reuseHost+":443")
	t.Cleanup(func() { _ = conn.Close() })
	if !strings.Contains(status, "200") {
		t.Fatalf("CONNECT status = %q, want 200", status)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(certPEM)
	tickets := &ticketCounter{}
	tc := tls.Client(conn, &tls.Config{ServerName: reuseHost, RootCAs: pool, ClientSessionCache: tickets})
	req, _ := http.NewRequest(http.MethodGet, "https://"+reuseHost+"/x", nil)
	if err := req.Write(tc); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(tc), req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	if n := tickets.puts.Load(); n != 0 {
		t.Errorf("the tunnel sent %d session tickets, want none", n)
	}
}

// TestHTTP2UpstreamsArePinged: both HTTP/2 upstream lanes ping a silent connection and close it when the
// ping goes unanswered.
func TestHTTP2UpstreamsArePinged(t *testing.T) {
	p := reuseProxy(newReuseUpstream(t, false), publicResolver{}, nil)
	if h := p.transport.HTTP2; h == nil || h.SendPingTimeout <= 0 || h.PingTimeout <= 0 {
		t.Errorf("egress transport HTTP2 config %+v, want a ping after silence and a ping timeout", h)
	}
	//lint:ignore SA1019 the fallback's transport is h2Fallback's deprecated http2.Transport (see h2Fallback)
	idle, ping := p.h2.transport.ReadIdleTimeout, p.h2.transport.PingTimeout
	if idle <= 0 || ping <= 0 {
		t.Errorf("h2 fallback ReadIdleTimeout %v PingTimeout %v, want both set", idle, ping)
	}
}

// BenchmarkProxyUpstreamReuse sends b.N inner requests for one host from 8 workers at once through the
// terminated-tunnel path to a local HTTP/1.1 TLS upstream, and reports per request the lookups, upstream
// connections and full TLS handshakes.
func BenchmarkProxyUpstreamReuse(b *testing.B) {
	up := newReuseUpstream(b, false)
	res := newReuseResolver("93.184.216.34")
	p := reuseProxy(up, res, nil)
	var next atomic.Int64
	b.ResetTimer()
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for next.Add(1) <= int64(b.N) {
				reuseGet(b, p)
			}
		}()
	}
	wg.Wait()
	b.StopTimer()
	n := float64(b.N)
	b.ReportMetric(float64(res.lookups.Load())/n, "lookups/op")
	b.ReportMetric(float64(up.accepts.Load())/n, "conns/op")
	b.ReportMetric(float64(up.fullHandshks.Load())/n, "handshakes/op")
}
