// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// headConn serves the bytes the preamble listener did not take off the front.
type headConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *headConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// headListener records the first 34 bytes of each connection and strips them when they are a relay
// preamble (first byte 0x57), so the TLS server behind it sees a plain stream either way.
type headListener struct {
	net.Listener
	mu    sync.Mutex
	heads [][]byte
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func newHeadListener(t *testing.T) *headListener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h := &headListener{Listener: ln, conns: make(chan net.Conn, 16), done: make(chan struct{})}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				br := bufio.NewReaderSize(c, 4096)
				head, err := br.Peek(34)
				if err != nil {
					_ = c.Close()
					return
				}
				h.mu.Lock()
				h.heads = append(h.heads, append([]byte(nil), head...))
				h.mu.Unlock()
				if head[0] == relayPreambleMagic {
					_, _ = br.Discard(34)
				}
				h.conns <- &headConn{Conn: c, r: br}
			}()
		}
	}()
	return h
}

func (h *headListener) Accept() (net.Conn, error) {
	select {
	case c := <-h.conns:
		return c, nil
	case <-h.done:
		return nil, net.ErrClosed
	}
}

func (h *headListener) Close() error {
	h.once.Do(func() { close(h.done) })
	return h.Listener.Close()
}

func (h *headListener) recorded() [][]byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([][]byte(nil), h.heads...)
}

// tlsControlPlane starts a TLS server behind a headListener; its certificate is trusted by the returned config.
func tlsControlPlane(t *testing.T, h http.Handler) (*httptest.Server, *headListener, *tls.Config) {
	t.Helper()
	hl := newHeadListener(t)
	srv := httptest.NewUnstartedServer(h)
	srv.Listener = hl
	srv.StartTLS()
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return srv, hl, &tls.Config{RootCAs: pool, ServerName: "example.com", MinVersion: tls.VersionTLS12}
}

func testRelayKey() []byte {
	k := make([]byte, relayKeyLen)
	for i := range k {
		k[i] = byte(0xA0 + i)
	}
	return k
}

func get(t *testing.T, c *http.Client, url string) *http.Response {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	_ = resp.Body.Close()
	c.CloseIdleConnections()
	return resp
}

func TestRelayPreambleSentToControlPlaneBeforeTLS(t *testing.T) {
	srv, hl, tlsCfg := tlsControlPlane(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	key := testRelayKey()
	relay := &relayPreamble{addr: strings.TrimPrefix(srv.URL, "https://"), key: key}
	get(t, controlPlaneClient(nil, tlsCfg, relay), srv.URL)

	heads := hl.recorded()
	if len(heads) != 1 {
		t.Fatalf("connections = %d, want 1", len(heads))
	}
	want := append([]byte{0x57, 0x01}, key...)
	if !bytes.Equal(heads[0], want) {
		t.Fatalf("first 34 bytes = %x, want %x", heads[0], want)
	}
}

func TestRelayPreambleAbsentWithoutKeyIsClientHello(t *testing.T) {
	srv, hl, tlsCfg := tlsControlPlane(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	get(t, controlPlaneClient(nil, tlsCfg, nil), srv.URL)
	heads := hl.recorded()
	if len(heads) != 1 || heads[0][0] != 0x16 {
		t.Fatalf("first bytes = %x, want one connection opening with the 0x16 ClientHello", heads)
	}
}

func TestRelayPreambleOnlyToControlPlaneAddress(t *testing.T) {
	srv, hl, tlsCfg := tlsControlPlane(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	// The control-plane address is another one; the server dialled here is not it.
	relay := &relayPreamble{addr: "cp.invalid:443", key: testRelayKey()}
	get(t, controlPlaneClient(nil, tlsCfg, relay), srv.URL)
	heads := hl.recorded()
	if len(heads) != 1 || heads[0][0] != 0x16 {
		t.Fatalf("first bytes to a non-control-plane address = %x, want the 0x16 ClientHello and no preamble", heads)
	}
}

func TestRelayPreambleIgnoresEnvProxy(t *testing.T) {
	srv, hl, tlsCfg := tlsControlPlane(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	envProxy := newHeadListener(t)
	t.Setenv("HTTPS_PROXY", "http://"+envProxy.Addr().String())
	t.Setenv("https_proxy", "http://"+envProxy.Addr().String())
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	// A name, not a loopback address: the environment proxy function never proxies loopback.
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "https://"))
	cpAddr := net.JoinHostPort("wardynd.test", port)
	relay := &relayPreamble{addr: cpAddr, key: testRelayKey(),
		dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if addr == cpAddr {
				addr = net.JoinHostPort("127.0.0.1", port)
			}
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		}}
	client := controlPlaneClient(nil, tlsCfg, relay)
	// The environment is read once per process, so a run that already used it would hide a regression
	// from the request below; the transport's own setting does not.
	if tr := client.Transport.(*http.Transport); tr.Proxy != nil {
		t.Fatal("control-plane transport has a Proxy function; it must ignore HTTPS_PROXY")
	}
	get(t, client, "https://"+cpAddr+"/")

	if got := envProxy.recorded(); len(got) != 0 {
		t.Fatalf("the environment proxy took %d connection(s); the control-plane client must not use it", len(got))
	}
	heads := hl.recorded()
	if len(heads) != 1 || heads[0][0] != relayPreambleMagic || heads[0][1] != relayPreambleVersion {
		t.Fatalf("control-plane connection opened with %x, want the preamble", heads)
	}
}

func TestRelayPreambleNotCarriedByRedirect(t *testing.T) {
	other, otherHL, tlsCfg := tlsControlPlane(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	srv, hl, _ := tlsControlPlane(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/elsewhere", http.StatusFound)
	}))
	relay := &relayPreamble{addr: strings.TrimPrefix(srv.URL, "https://"), key: testRelayKey()}
	get(t, controlPlaneClient(nil, tlsCfg, relay), srv.URL)

	if h := hl.recorded(); len(h) != 1 || h[0][0] != relayPreambleMagic {
		t.Fatalf("control plane saw %x, want the preamble", h)
	}
	if h := otherHL.recorded(); len(h) != 1 || h[0][0] != 0x16 {
		t.Fatalf("redirect target saw %x, want the 0x16 ClientHello and no preamble", h)
	}
}

func relayKeyConfig(key string) []byte {
	field := ""
	if key != "" {
		field = `,"relay_key":"` + key + `"`
	}
	return []byte(`{"run_id":"` + uuid.NewString() + `","control_plane_url":"http://127.0.0.1:8080","run_token":"t"` + field + `}`)
}

func TestConfigRelayKeyValidated(t *testing.T) {
	good := base64.StdEncoding.EncodeToString(testRelayKey())
	for name, tc := range map[string]struct {
		key     string
		refused bool
	}{
		"absent":         {"", false},
		"32 bytes":       {good, false},
		"31 bytes":       {base64.StdEncoding.EncodeToString(testRelayKey()[:31]), true},
		"33 bytes":       {base64.StdEncoding.EncodeToString(append(testRelayKey(), 1)), true},
		"not base64":     {"not*base64*SECRETISH", true},
		"url-safe chars": {strings.NewReplacer("+", "-", "/", "_").Replace(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xfb}, 32))), true},
	} {
		cfg, err := LoadConfigBytes(relayKeyConfig(tc.key))
		if tc.refused {
			if err == nil {
				t.Errorf("%s: config accepted", name)
			} else if tc.key != "" && strings.Contains(err.Error(), tc.key) {
				t.Errorf("%s: error text carries the key: %v", name, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: refused: %v", name, err)
		} else if tc.key == "" && cfg.RelayKey != "" {
			t.Errorf("%s: key set", name)
		}
	}
}

func TestConfigRelayKeyNeverPrints(t *testing.T) {
	good := base64.StdEncoding.EncodeToString(testRelayKey())
	cfg, err := LoadConfigBytes(relayKeyConfig(good))
	if err != nil {
		t.Fatal(err)
	}
	relay, err := relayPreambleFor(cfg)
	if err != nil || relay == nil {
		t.Fatalf("relayPreambleFor = %v, %v", relay, err)
	}
	var logs bytes.Buffer
	lg := slog.New(slog.NewTextHandler(&logs, nil))
	lg.Info("cfg", slog.Any("config", cfg), slog.Any("key", cfg.RelayKey), slog.Any("relay", relay))
	var out strings.Builder
	for _, v := range []any{cfg, *cfg, cfg.RelayKey, relay, *relay} {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
			fmt.Fprintf(&out, verb+"\n", v)
		}
	}
	out.WriteString(logs.String())
	raw := fmt.Sprintf("%x", testRelayKey())
	for _, leak := range []string{good, string(testRelayKey()), raw, fmt.Sprint(testRelayKey())} {
		if strings.Contains(out.String(), leak) {
			t.Fatalf("a dump of the config carries the relay key (%q)", leak[:8])
		}
	}
}
