// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestValidateUpstreamProxyNoProxy(t *testing.T) {
	if err := validateUpstreamProxyNoProxy([]string{"vpce.amazonaws.com", ".corp.internal", "100.64.0.0/10"}); err != nil {
		t.Fatalf("valid entries rejected: %v", err)
	}
	if err := validateUpstreamProxyNoProxy(nil); err != nil {
		t.Fatalf("empty list rejected: %v", err)
	}
	for _, bad := range []string{"*", "*.corp.internal", "http://proxy.corp:8080", "corp internal", ""} {
		err := validateUpstreamProxyNoProxy([]string{bad})
		if err == nil {
			t.Errorf("entry %q must be refused at write time — the proxy DROPS what it cannot compile, so it would silently stay proxied", bad)
			continue
		}
		if !strings.Contains(err.Error(), "upstream_proxy_no_proxy[0]") {
			t.Errorf("entry %q: error %q must name the offending index", bad, err)
		}
	}
}

// TestRedirectProbeTo pins gap 4's decision: WHICH url probe 1 requests and
// whether it swaps the TCP target. A hostname To must stay byte-identical to
// the pre-fix probe.
func TestRedirectProbeTo(t *testing.T) {
	cases := []struct {
		name          string
		red           types.EgressRedirect
		wantURL       string
		wantConnectTo string
	}{
		{
			name:    "hostname To is unchanged (no swap, no regression)",
			red:     types.EgressRedirect{From: "pypi.org", To: "https://mirror.corp.internal/simple"},
			wantURL: "https://mirror.corp.internal/simple",
		},
		{
			name:          "literal-IP To presents the From hostname for TLS",
			red:           types.EgressRedirect{From: "pypi.org", To: "https://100.64.5.7"},
			wantURL:       "https://pypi.org",
			wantConnectTo: "pypi.org:443:100.64.5.7:443",
		},
		{
			name:          "literal-IP To keeps To's own port on the wire",
			red:           types.EgressRedirect{From: "https://pypi.org/simple", To: "https://100.64.5.7:8443"},
			wantURL:       "https://pypi.org/simple",
			wantConnectTo: "pypi.org:443:100.64.5.7:8443",
		},
		{
			// From's SCHEME is never borrowed: --connect-to decides the
			// connection, the URL decides the protocol spoken inside it, and only
			// To knows which protocol the mirror serves. PORT1 follows the
			// requested URL's scheme for the same reason -- it is what curl
			// matches the swap on.
			name:          "an http From still asks for To's https, and matches on 443",
			red:           types.EgressRedirect{From: "http://mirror.example.com", To: "https://10.40.1.5"},
			wantURL:       "https://mirror.example.com",
			wantConnectTo: "mirror.example.com:443:10.40.1.5:443",
		},
		{
			name:          "a plain-http To is spoken as http, and matches on 80",
			red:           types.EgressRedirect{From: "https://mirror.example.com/simple", To: "http://10.40.1.5:8080"},
			wantURL:       "http://mirror.example.com/simple",
			wantConnectTo: "mirror.example.com:80:10.40.1.5:8080",
		},
		{
			name:          "From's EXPLICIT port is PORT1, never the scheme default",
			red:           types.EgressRedirect{From: "registry.corp.example:8443", To: "https://10.40.1.5"},
			wantURL:       "https://registry.corp.example:8443",
			wantConnectTo: "registry.corp.example:8443:10.40.1.5:443",
		},
		{
			name:    "no usable From host: no swap rather than a guess",
			red:     types.EgressRedirect{From: "", To: "https://100.64.5.7"},
			wantURL: "https://100.64.5.7",
		},
		{
			// PORT2 follows the SCHEME To spells, not a hard-coded 443: a
			// plain-http mirror addressed by literal IP serves 80, and dialing
			// its 443 made curl fail and the operator read "could not reach the
			// mirror" for a redirect real runs use successfully. The expected
			// port is written literally here, never derived from redirectPort,
			// so the pin cannot move with the code it pins.
			name:          "a plain-http To with no port is dialed on 80, not 443",
			red:           types.EgressRedirect{From: "pypi.org", To: "http://10.40.1.5"},
			wantURL:       "http://pypi.org",
			wantConnectTo: "pypi.org:80:10.40.1.5:80",
		},
		{
			// A query/fragment ends the authority just as '/' does. Cutting only
			// at '/' handed strconv "8443?repo=npm", which failed, and PORT2
			// silently became 443 — the probe dialing a port the operator never
			// configured, for a To that validateSiteConfig accepts.
			name:          "a query-bearing To keeps its own port",
			red:           types.EgressRedirect{From: "registry.npmjs.org", To: "https://10.40.2.11:8443?repo=npm-remote"},
			wantURL:       "https://registry.npmjs.org",
			wantConnectTo: "registry.npmjs.org:443:10.40.2.11:8443",
		},
		{
			name:          "a fragment-bearing To keeps its own port",
			red:           types.EgressRedirect{From: "registry.npmjs.org", To: "https://10.40.2.11:8443#frag"},
			wantURL:       "https://registry.npmjs.org",
			wantConnectTo: "registry.npmjs.org:443:10.40.2.11:8443",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotURL, gotConnect := redirectProbeTo(c.red, hostOfForTest(c.red.To), hostOfForTest(c.red.From))
			if gotURL != c.wantURL {
				t.Errorf("url = %q, want %q", gotURL, c.wantURL)
			}
			if gotConnect != c.wantConnectTo {
				t.Errorf("connect-to = %q, want %q", gotConnect, c.wantConnectTo)
			}
		})
	}
}

// TestRedirectProbeScriptKeepsItsSentinels: the two-probe shape and its
// load-bearing exit codes survive the gap-4 edit — probe 1 still propagates
// curl's own code, probe 2 still exits 250 (redirectProbeBypassCode) and 251
// stays the proxy probe's alone.
func TestRedirectProbeScriptKeepsItsSentinels(t *testing.T) {
	if !strings.Contains(redirectProbeScript, "exit 250") {
		t.Error("probe 2's explicit bypass sentinel (250) is gone")
	}
	if strings.Contains(redirectProbeScript, "251") {
		t.Error("251 is the proxy probe's sentinel and must not appear here")
	}
	if !strings.Contains(redirectProbeScript, "|| exit $?") {
		t.Error("probe 1 must still propagate curl's own exit code")
	}
	if !strings.Contains(redirectProbeScript, "--connect-to \"$WARDYN_PROBE_TO_CONNECT\"") {
		t.Error("the literal-IP shape must dial To while presenting From for TLS")
	}
	if !strings.Contains(redirectProbeScript, "--noproxy '*'") {
		t.Error("probe 2 must still be a direct dial that bypasses the proxy's policy")
	}
}

// TestRedirectProbe_LiteralIPCertIsScopedToFromHost is the behavioral proof of
// the false negative gap 4 removes, and of the mechanism that removes it.
//
// A private endpoint presents a certificate for the PUBLIC (From) hostname —
// the normal, correct shape. Curling the literal To address directly makes the
// ADDRESS the name to verify, and verification fails (curl 60), so the probe
// reported a correct configuration as broken. Dialing the same address while
// presenting the From hostname — what --connect-to does, and what the data
// path already does — verifies cleanly. httptest + a cert scoped to the From
// host only stands in for the endpoint.
func TestRedirectProbe_LiteralIPCertIsScopedToFromHost(t *testing.T) {
	const fromHost = "mirror.example.test"
	cert, pool := certForHost(t, fromHost)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "https://")
	toAddr, _, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split %q: %v", addr, err)
	}

	// What the OLD probe did: dial the To address, so the address is the name.
	if _, err := tls.Dial("tcp", addr, &tls.Config{RootCAs: pool, ServerName: toAddr}); err == nil {
		t.Fatal("dialing the literal To address must FAIL cert verification — otherwise this test proves nothing")
	}
	// What --connect-to does: same TCP target, From's hostname for TLS.
	conn, err := tls.Dial("tcp", addr, &tls.Config{RootCAs: pool, ServerName: fromHost})
	if err != nil {
		t.Fatalf("dialing the To address while presenting %q must verify: %v", fromHost, err)
	}
	_ = conn.Close()

	// And that is exactly the swap redirectProbeTo asks the probe to make.
	_, connectTo := redirectProbeTo(
		types.EgressRedirect{From: fromHost, To: "https://" + addr}, toAddr, fromHost)
	if !strings.HasPrefix(connectTo, fromHost+":443:"+toAddr+":") {
		t.Fatalf("connect-to = %q, want the From host mapped onto the To address", connectTo)
	}
}

// hostOfForTest mirrors the handler's own host extraction so the table above
// exercises redirectProbeTo with the arguments it really receives.
func hostOfForTest(raw string) string {
	s := raw
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/:"); i >= 0 {
		s = s[:i]
	}
	return strings.ToLower(s)
}

// certForHost mints a self-signed leaf valid ONLY for host — no IP SANs, which
// is the whole point: a private endpoint's certificate names the public
// hostname, never the address it answers on.
func certForHost(t *testing.T, host string) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: host},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:              []string{host},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("cert: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}
