// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestTrustedProxyChainRule(t *testing.T) {
	v4 := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	v6 := []netip.Prefix{netip.MustParsePrefix("2001:db8:1::/48")}
	for _, c := range []struct {
		name    string
		peer    string
		xff     []string
		trusted []netip.Prefix
		want    string // "" keeps the peer
	}{
		{"direct spoofing from an untrusted peer", "203.0.113.9:4000", []string{"198.51.100.7"}, v4, ""},
		{"no list trusts nobody", "10.0.0.1:4000", []string{"198.51.100.7"}, nil, ""},
		{"trusted peer, no header", "10.0.0.1:4000", nil, v4, ""},
		{"one trusted hop", "10.0.0.1:4000", []string{"198.51.100.7"}, v4, "198.51.100.7"},
		{"untrusted intermediary is the client", "10.0.0.1:4000", []string{"198.51.100.7, 192.0.2.50"}, v4, "192.0.2.50"},
		{"attacker-prefixed chain", "10.0.0.1:4000", []string{"10.0.0.9, 1.2.3.4, 198.51.100.7"}, v4, "198.51.100.7"},
		{"attacker prefix naming a trusted address", "10.0.0.1:4000", []string{"10.0.0.9, 198.51.100.7"}, v4, "198.51.100.7"},
		{"multiple trusted hops", "10.0.0.7:4000", []string{"198.51.100.7, 10.0.0.5, 10.0.0.6"}, v4, "198.51.100.7"},
		{"repeated headers join in order", "10.0.0.1:4000", []string{"1.2.3.4", "198.51.100.7"}, v4, "198.51.100.7"},
		{"hop with a port", "10.0.0.1:4000", []string{"198.51.100.7:51000"}, v4, "198.51.100.7"},
		{"only trusted addresses keeps the peer", "10.0.0.1:4000", []string{"10.0.0.5, 10.0.0.6"}, v4, ""},
		{"IPv6 trusted hops", "[2001:db8:1::1]:443", []string{"2001:db8:ffff::5, 2001:db8:1::2"}, v6, "2001:db8:ffff::5"},
		{"IPv6 hop with a port", "[2001:db8:1::1]:443", []string{"[2001:db8:ffff::5]:443"}, v6, "2001:db8:ffff::5"},
		{"IPv6 peer outside the list", "[2001:db8:2::1]:443", []string{"2001:db8:ffff::5"}, v6, ""},
		{"IPv4-mapped peer", "[::ffff:10.0.0.1]:4000", []string{"198.51.100.7"}, v4, "198.51.100.7"},
		{"malformed hop", "10.0.0.1:4000", []string{"garbage, 198.51.100.7"}, v4, ""},
		{"unknown hop", "10.0.0.1:4000", []string{"unknown"}, v4, ""},
		{"empty hop", "10.0.0.1:4000", []string{"198.51.100.7,,"}, v4, ""},
		{"zoned hop", "10.0.0.1:4000", []string{"fe80::1%eth0"}, v4, ""},
		{"loopback hop", "10.0.0.5:1", []string{"127.0.0.1"}, v4, ""},
		{"loopback hop right of a client", "10.0.0.5:1", []string{"198.51.100.7, 127.0.0.1"}, v4, ""},
		{"IPv4-mapped loopback hop", "10.0.0.5:1", []string{"::ffff:127.0.0.1"}, v4, ""},
		{"IPv6 loopback hop with a port", "10.0.0.5:1", []string{"[::1]:443"}, v4, ""},
		{"unspecified hop", "10.0.0.5:1", []string{"0.0.0.0"}, v4, ""},
		{"IPv6 unspecified hop", "10.0.0.5:1", []string{"::"}, v4, ""},
		{"too many hops", "10.0.0.1:4000", []string{strings.Repeat("198.51.100.7,", maxForwardedHops) + "198.51.100.7"}, v4, ""},
		{"too many bytes", "10.0.0.1:4000", []string{"198.51.100.7," + strings.Repeat(" ", maxForwardedBytes) + "198.51.100.8"}, v4, ""},
	} {
		got, ok := forwardedClient(c.peer, c.xff, c.trusted)
		if got != c.want || ok != (c.want != "") {
			t.Errorf("%s: forwardedClient = %q, %v; want %q", c.name, got, ok, c.want)
		}
	}
}

func TestParseTrustedProxies(t *testing.T) {
	got, err := ParseTrustedProxies(" 10.1.2.3/8, 2001:db8::/32 ,")
	if err != nil || len(got) != 2 || got[0].String() != "10.0.0.0/8" || got[1].String() != "2001:db8::/32" {
		t.Fatalf("ParseTrustedProxies = %v, %v", got, err)
	}
	if got, err := ParseTrustedProxies(""); err != nil || got != nil {
		t.Fatalf("empty list = %v, %v; want nil, nil", got, err)
	}
	for _, bad := range []string{"0.0.0.0/0", "::/0", "10.0.0.1", "10.0.0.0/33", "proxy.example", "::ffff:10.0.0.0/104", "::ffff:0:0/96"} {
		if _, err := ParseTrustedProxies(bad); err == nil {
			t.Errorf("ParseTrustedProxies(%q) accepted", bad)
		}
	}
}

// postDecisionVia posts one egress decision through a real listener serving
// the handler serve picks, with the given X-Forwarded-For, and returns the
// egress.deny row's source_ip and data.
func postDecisionVia(t *testing.T, serve func(*Server) http.Handler, trusted []netip.Prefix, xff string) (string, map[string]any) {
	t.Helper()
	h := newHarness(t)
	h.srv.cfg.TrustedProxies = trusted
	ts := httptest.NewServer(panicFails(t, serve(h.srv)))
	defer ts.Close()
	body := `{"request":{"host":"evil.example.com","method":"CONNECT"},"decision":"deny","rule_source":"policy"}`
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/api/v1/internal/decisions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+h.mintRunToken(t, uuid.New()))
	req.Header.Set("X-Forwarded-For", xff)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("decision status = %d, want 202", resp.StatusCode)
	}
	for _, ev := range h.audit.events {
		if ev.Action == "egress.deny" {
			data := map[string]any{}
			_ = json.Unmarshal(ev.Data, &data)
			return ev.SourceIP, data
		}
	}
	t.Fatal("no egress.deny audit event recorded")
	return "", nil
}

func TestTrustedProxySourceIPKeepsPeer(t *testing.T) {
	loopback := []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}
	for _, xff := range []string{"198.51.100.7", "1.2.3.4, 198.51.100.7"} {
		src, data := postDecisionVia(t, (*Server).Handler, loopback, xff)
		peer, _ := data["proxy_peer"].(string)
		host, _, _ := net.SplitHostPort(peer)
		if src != "198.51.100.7" || !isLoopback(host) {
			t.Fatalf("xff %q: source_ip %q, proxy_peer %q; want the client and the loopback peer", xff, src, peer)
		}
	}
	// A list that does not hold the peer reads nothing: the anti-spoof pin.
	src, data := postDecisionVia(t, (*Server).Handler, []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}, "198.51.100.7")
	if strings.Contains(src, "198.51.100.7") || data["proxy_peer"] != nil {
		t.Fatalf("untrusted peer: source_ip %q, proxy_peer %v; want the TCP peer and no stamp", src, data["proxy_peer"])
	}
}

// The proxy-facing listener is dialled by runs directly, never through the
// operator's proxy, so even with the loopback peer trusted it keeps the TCP
// peer and stamps nothing.
func TestTrustedProxyInternalHandlerKeepsPeer(t *testing.T) {
	src, data := postDecisionVia(t, (*Server).InternalHandler, loopbackProxies, "198.51.100.7")
	if host, _, _ := net.SplitHostPort(src); !isLoopback(host) || data["proxy_peer"] != nil {
		t.Fatalf("internal listener: source_ip %q, proxy_peer %v; want the loopback peer and no stamp", src, data["proxy_peer"])
	}
}

// A trusted proxy cannot hand a caller a loopback source_ip, so the local-mode
// loopback-peer gate (REST and the attach WebSocket) still refuses it.
func TestTrustedProxyCannotSatisfyLocalModeLoopbackGate(t *testing.T) {
	srv := New(Config{
		Identity:       mustIDP(t),
		Approvals:      newFakeApprovals(),
		Broker:         &fakeBroker{},
		Audit:          &recRecorder{},
		LocalMode:      true,
		LocalOperator:  "local:tester",
		DefaultPolicy:  types.RunPolicySpec{MinConfinementClass: types.CC2},
		TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
	})
	for _, path := range []string{"/api/v1/me", "/api/v1/runs/" + uuid.NewString() + "/attach"} {
		for _, xff := range []string{"127.0.0.1", "::1", "::ffff:127.0.0.1", "0.0.0.0", "198.51.100.7, 127.0.0.1"} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Host, req.RemoteAddr = "127.0.0.1", "10.0.0.5:1"
			req.Header.Set("X-Forwarded-For", xff)
			w := httptest.NewRecorder()
			panicFails(t, srv.Handler()).ServeHTTP(w, req)
			if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), reasonLocalModePeerNotLoopback) {
				t.Errorf("%s via trusted peer, X-Forwarded-For %q: %d %s; want 403 %s", path, xff, w.Code, w.Body.String(), reasonLocalModePeerNotLoopback)
			}
		}
	}
}
