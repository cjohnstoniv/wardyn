// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/gatewayidentity"
)

// gwcertRig is the gateway identity rig with a gateway that asks for a client certificate as
// clientAuth says, trusting a real gateway client CA, and a control plane that certifies with it.
type gwcertRig struct {
	*gwidRig
	ca     *gatewayidentity.Authority
	certs  atomic.Int64 // certificate mints
	status atomic.Int64 // a status the certificate mint answers instead; 0 certifies
}

func newGwcertRig(t *testing.T, clientAuth tls.ClientAuthType) *gwcertRig {
	t.Helper()
	g := &gwcertRig{gwidRig: newGwidRig(t, gwidRoutes(), false)}
	raw, err := gatewayidentity.RotateAuthority(nil, false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if g.ca, err = gatewayidentity.NewAuthority(raw); err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(g.ca.BundlePEM()) {
		t.Fatal("empty bundle")
	}
	g.gw = newGwcertUpstream(t, &tls.Config{ClientAuth: clientAuth, ClientCAs: pool})
	g.other = newGwcertUpstream(t, &tls.Config{ClientAuth: tls.RequestClientCert})
	g.mint.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { g.answer(t, w, r) }))
	t.Cleanup(g.mint.srv.Close)
	return g
}

// newGwcertUpstream is newGwidUpstream with its own server TLS config.
func newGwcertUpstream(t *testing.T, cfg *tls.Config) *gwidUpstream {
	t.Helper()
	u := &gwidUpstream{respond: gwidEcho}
	u.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.seen = append(u.seen, r.Clone(context.Background()))
		u.mu.Unlock()
		gwidEcho(w, r)
	}))
	u.srv.TLS = cfg
	u.srv.StartTLS()
	t.Cleanup(u.srv.Close)
	return u
}

// answer is the fake control plane: a token for a provider, a certificate for a public key.
func (g *gwcertRig) answer(t *testing.T, w http.ResponseWriter, r *http.Request) {
	var body struct {
		ProviderID string `json:"provider_id"`
		PublicKey  string `json:"public_key"`
	}
	if r.URL.Path != "/api/v1/internal/gateway-identity" || r.Header.Get("Authorization") != "Bearer RUNTOK" || json.NewDecoder(r.Body).Decode(&body) != nil {
		t.Errorf("bad mint request %s %s", r.Method, r.URL.Path)
	}
	if body.PublicKey == "" {
		tok := gwidToken(gwidAudience, g.now().Add(10*time.Minute), g.mint.hits.Add(1))
		_ = json.NewEncoder(w).Encode(map[string]any{"token": tok, "expires_at": g.now().Add(10 * time.Minute)})
		return
	}
	if body.ProviderID != "" {
		t.Errorf("a certificate mint named a provider: %q", body.ProviderID)
	}
	g.certs.Add(1)
	if s := g.status.Load(); s != 0 {
		w.WriteHeader(int(s))
		return
	}
	der, err := base64.StdEncoding.DecodeString(body.PublicKey)
	if err != nil {
		t.Errorf("public key is not base64: %v", err)
	}
	pub, err := x509.ParsePKIXPublicKey(der)
	ec, ok := pub.(*ecdsa.PublicKey)
	if err != nil || !ok {
		t.Errorf("public key is not ECDSA: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	leaf, exp, err := g.ca.Issue(ec, "spiffe://wardyn.local/agent-run/"+g.p.runID.String(), 0)
	if err != nil {
		t.Errorf("issue: %v", err)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"certificate": string(leaf), "expires_at": exp})
}

// presented is the client certificate the gateway saw on its last request, or nil.
func (g *gwcertRig) presented(t *testing.T) *x509.Certificate {
	t.Helper()
	seen := g.gw.last(t)
	if seen.TLS == nil || len(seen.TLS.PeerCertificates) == 0 {
		return nil
	}
	return seen.TLS.PeerCertificates[0]
}

func (g *gwcertRig) newConnections() {
	g.p.transport.CloseIdleConnections()
	//lint:ignore SA1019 the fallback's transport is h2Fallback's deprecated http2.Transport (see h2Fallback)
	g.p.h2.transport.CloseIdleConnections()
}

// A gateway that requires a client certificate gets the run's, on both lanes, beside the token;
// its one URI SAN is the run's SPIFFE ID. REVERT PROOF: dropping the offerGatewayClientCert call
// in offerHTTP2 fails this test at the status.
func TestGatewayClientCert_PresentedWhenRequired(t *testing.T) {
	for _, lane := range []string{"brokered", "mitm"} {
		t.Run(lane, func(t *testing.T) {
			g := newGwcertRig(t, tls.RequireAndVerifyClientCert)
			rec := gwidLanes(g.gwidRig, t)[lane]()
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body)
			}
			leaf := g.presented(t)
			if leaf == nil || len(leaf.URIs) != 1 {
				t.Fatalf("the gateway saw certificate %v", leaf)
			}
			if got, want := leaf.URIs[0].String(), "spiffe://wardyn.local/agent-run/"+g.p.runID.String(); got != want {
				t.Errorf("URI SAN %q, want %q", got, want)
			}
			if g.gw.last(t).Header.Get(gatewayIdentityHeader) == "" {
				t.Error("the token was not sent beside the certificate")
			}
			if n := g.certs.Load(); n != 1 {
				t.Errorf("%d certificate mints, want 1", n)
			}
		})
	}
}

// A gateway that does not ask costs no mint; a host that is not an identity route is never
// offered the certificate, even when it asks.
func TestGatewayClientCert_OnlyIdentityRoutesThatAsk(t *testing.T) {
	g := newGwcertRig(t, tls.NoClientCert)
	if rec := gwidLanes(g.gwidRig, t)["brokered"](); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if g.presented(t) != nil || g.certs.Load() != 0 {
		t.Fatalf("a gateway that did not ask: certificate %v, %d mints", g.presented(t), g.certs.Load())
	}
	rec := httptest.NewRecorder()
	g.p.serveMITMRequest(rec, httptest.NewRequest(http.MethodGet, "https://"+gwidOther+"/x", nil), gwidOther, 443)
	if rec.Code != http.StatusOK {
		t.Fatalf("other host: status %d: %s", rec.Code, rec.Body)
	}
	if seen := g.other.last(t); seen.TLS == nil || len(seen.TLS.PeerCertificates) != 0 || g.certs.Load() != 0 {
		t.Fatalf("a host off the identity routes was offered the certificate (%d mints)", g.certs.Load())
	}
}

// From half-life a new connection gets a new certificate. A renewal that fails but may pass
// keeps the held one serving and is not asked again on the next connection.
func TestGatewayClientCert_RenewedAtHalfLife(t *testing.T) {
	g := newGwcertRig(t, tls.RequireAndVerifyClientCert)
	send := gwidLanes(g.gwidRig, t)["brokered"]
	if rec := send(); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	first := g.presented(t)
	g.advance(6 * time.Minute)
	g.status.Store(http.StatusServiceUnavailable)
	g.newConnections()
	if rec := send(); rec.Code != http.StatusOK {
		t.Fatalf("held certificate during an outage: status %d: %s", rec.Code, rec.Body)
	}
	if got := g.presented(t); got == nil || got.SerialNumber.Cmp(first.SerialNumber) != 0 || g.certs.Load() != 2 {
		t.Fatalf("during an outage: certificate %v, %d mints; want the held one after 2", got, g.certs.Load())
	}
	g.newConnections()
	if rec := send(); rec.Code != http.StatusOK || g.certs.Load() != 2 {
		t.Fatalf("a failed renewal was asked again at once: status %d, %d mints", rec.Code, g.certs.Load())
	}
	g.status.Store(0)
	g.advance(lastGoodRetry)
	g.newConnections()
	if rec := send(); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if got := g.presented(t); got == nil || got.SerialNumber.Cmp(first.SerialNumber) == 0 || g.certs.Load() != 3 {
		t.Fatalf("after the outage: certificate %v, %d mints; want a new one after 3", got, g.certs.Load())
	}
}

// A refused certificate sends none, so a gateway that requires one refuses the call before
// anything reaches it, and the refusal is not asked again on the next call.
func TestGatewayClientCert_RefusalFailsTheCall(t *testing.T) {
	g := newGwcertRig(t, tls.RequireAndVerifyClientCert)
	g.status.Store(http.StatusForbidden)
	send := gwidLanes(g.gwidRig, t)["brokered"]
	if rec := send(); rec.Code < 400 || g.gw.count() != 0 {
		t.Fatalf("status %d, gateway hits %d", rec.Code, g.gw.count())
	}
	g.newConnections()
	if rec := send(); rec.Code < 400 || g.certs.Load() != 1 {
		t.Fatalf("second call: status %d, %d mints; want a refusal after 1", rec.Code, g.certs.Load())
	}
}

// A TLS front that asks without requiring Wardyn's CA gets no certificate and lets the call
// through: one that names only another CA, and one that asks while the mint fails. REVERT
// PROOF: returning get's certificate and error unchecked fails both at the status.
func TestGatewayClientCert_OptionalFrontGetsNone(t *testing.T) {
	for name, c := range map[string]struct {
		auth   tls.ClientAuthType
		status int
	}{
		"another CA":      {tls.VerifyClientCertIfGiven, 0},
		"mint that fails": {tls.RequestClientCert, http.StatusServiceUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			g := newGwcertRig(t, tls.NoClientCert)
			another := x509.NewCertPool()
			another.AddCert(g.other.srv.Certificate())
			g.gw = newGwcertUpstream(t, &tls.Config{ClientAuth: c.auth, ClientCAs: another})
			g.status.Store(int64(c.status))
			if rec := gwidLanes(g.gwidRig, t)["brokered"](); rec.Code != http.StatusOK || g.gw.count() != 1 {
				t.Fatalf("status %d, gateway hits %d: %s", rec.Code, g.gw.count(), rec.Body)
			}
			if g.presented(t) != nil || g.certs.Load() != 1 {
				t.Fatalf("certificate %v after %d mints; want none after 1", g.presented(t), g.certs.Load())
			}
		})
	}
}

// A definite refusal at renewal drops the held certificate, so a cut run's next connection to
// a gateway that requires one is refused. REVERT PROOF: keeping the held one on a refusal
// fails this test at the second status.
func TestGatewayClientCert_RefusalDropsTheHeldOne(t *testing.T) {
	g := newGwcertRig(t, tls.RequireAndVerifyClientCert)
	send := gwidLanes(g.gwidRig, t)["brokered"]
	if rec := send(); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	g.advance(6 * time.Minute)
	g.status.Store(http.StatusForbidden)
	g.newConnections()
	if rec := send(); rec.Code < 400 || g.gw.count() != 1 || g.certs.Load() != 2 {
		t.Fatalf("after a refusal: status %d, gateway hits %d, %d mints", rec.Code, g.gw.count(), g.certs.Load())
	}
}

// A held certificate is never presented past its expiry, neither while a failed renewal waits
// to be asked again nor when that renewal fails again. REVERT PROOF: judging "held" without
// the expiry fails the first refusal; serving the held one on a failed mint without the
// expiry fails the second.
func TestGatewayClientCert_NeverServedPastExpiry(t *testing.T) {
	g := newGwcertRig(t, tls.RequireAndVerifyClientCert)
	send := gwidLanes(g.gwidRig, t)["brokered"]
	if rec := send(); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	g.advance(9*time.Minute + 50*time.Second)
	g.status.Store(http.StatusServiceUnavailable)
	g.newConnections()
	if rec := send(); rec.Code != http.StatusOK || g.certs.Load() != 2 {
		t.Fatalf("before expiry: status %d, %d mints", rec.Code, g.certs.Load())
	}
	for i, step := range []time.Duration{15 * time.Second, lastGoodRetry} {
		g.advance(step)
		g.newConnections()
		if rec := send(); rec.Code < 400 || g.gw.count() != 2 {
			t.Fatalf("past expiry, step %d: status %d, gateway hits %d", i, rec.Code, g.gw.count())
		}
	}
	if n := g.certs.Load(); n != 3 {
		t.Errorf("%d mints, want 3", n)
	}
}

// The port counts: the route's host on another port is not offered the certificate, even when
// it asks. REVERT PROOF: matching the host alone fails this test.
func TestGatewayClientCert_OtherPortGetsNone(t *testing.T) {
	g := newGwcertRig(t, tls.RequireAndVerifyClientCert)
	rec := httptest.NewRecorder()
	g.p.serveMITMRequest(rec, httptest.NewRequest(http.MethodGet, "https://"+gwidHost+":8443/v1/x", nil), gwidHost, 8443)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if seen := g.other.last(t); seen.TLS == nil || len(seen.TLS.PeerCertificates) != 0 || g.certs.Load() != 0 {
		t.Fatalf("another port was offered the certificate (%d mints)", g.certs.Load())
	}
}

// The HTTP/2 fallback's dial presents the certificate too, on its own copy of the config.
// REVERT PROOF: dropping the offerGatewayClientCert call in the fallback's DialTLSContext fails
// this test at the response.
func TestGatewayClientCert_H2FallbackPresents(t *testing.T) {
	g := newGwcertRig(t, tls.RequireAndVerifyClientCert)
	//lint:ignore SA1019 see h2Fallback
	cfg := g.p.h2.transport.TLSClientConfig.Clone()
	cfg.ServerName = gwidHost
	ctx := context.WithValue(context.Background(), vettedIPKey{}, "93.184.216.10:443")
	//lint:ignore SA1019 see h2Fallback
	conn, err := g.p.h2.transport.DialTLSContext(ctx, "tcp", gwidHost+":443", cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if cfg.GetClientCertificate != nil {
		t.Error("the fallback set the hook on the config it was handed")
	}
	req := httptest.NewRequest(http.MethodGet, "https://"+gwidHost+"/v1/x", nil)
	if err := req.Write(conn); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		t.Fatalf("the gateway refused the fallback's connection: %v", err)
	}
	_ = resp.Body.Close()
	if g.presented(t) == nil || g.certs.Load() != 1 {
		t.Fatalf("certificate %v after %d mints", g.presented(t), g.certs.Load())
	}
}

// check refuses a leaf for another key, one naming another run, an expired one, and anything
// that is not one PEM certificate.
func TestGatewayClientCert_CheckRefuses(t *testing.T) {
	raw, err := gatewayidentity.RotateAuthority(nil, false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ca, err := gatewayidentity.NewAuthority(raw)
	if err != nil {
		t.Fatal(err)
	}
	ours, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	theirs, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	h := &gatewayCertHolder{run: "11111111-1111-1111-1111-111111111111"}
	issue := func(pub *ecdsa.PublicKey, run string) string {
		leaf, _, err := ca.Issue(pub, "spiffe://wardyn.local/agent-run/"+run, 0)
		if err != nil {
			t.Fatal(err)
		}
		return string(leaf)
	}
	now := time.Now()
	good := issue(&ours.PublicKey, h.run)
	if _, exp, err := h.check(good, now.Add(time.Hour), &ours.PublicKey, now); err != nil || exp.After(now.Add(10*time.Minute)) {
		t.Fatalf("a good leaf: exp %v, err %v", exp, err)
	}
	for name, c := range map[string]struct {
		pem string
		exp time.Time
	}{
		"another key":   {issue(&theirs.PublicKey, h.run), now.Add(time.Minute)},
		"another run":   {issue(&ours.PublicKey, "22222222-2222-2222-2222-222222222222"), now.Add(time.Minute)},
		"expired":       {good, now},
		"two blocks":    {good + good, now.Add(time.Minute)},
		"not a PEM":     {"certificate", now.Add(time.Minute)},
		"empty":         {"", now.Add(time.Minute)},
		"trailing junk": {good + "junk", now.Add(time.Minute)},
	} {
		if _, _, err := h.check(c.pem, c.exp, &ours.PublicKey, now); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// A plain-lane call to a TLS route on port 80, sent as cleartext with no port, is offered the certificate:
// the dial address is the port the proxy judged, not the outgoing scheme's default.
func TestGatewayClientCert_PlainLaneJudgedPort(t *testing.T) {
	g := newGwcertRig(t, tls.RequireAndVerifyClientCert)
	g.p.gatewayRoutes[0].Port = 80 // the rig dials every address but the gateway host's 443 to g.other, which asks for a certificate
	rec := httptest.NewRecorder()
	g.p.servePlain(rec, httptest.NewRequest(http.MethodGet, "http://"+gwidHost+"/v1/x", nil))
	seen, certs := g.other.last(t), 0
	if seen.TLS != nil {
		certs = len(seen.TLS.PeerCertificates)
	}
	if rec.Code != http.StatusOK || certs != 1 || seen.Header.Get(gatewayIdentityHeader) == "" {
		t.Fatalf("status %d: the port-80 gateway saw %d certificates and identity %q", rec.Code, certs, seen.Header.Get(gatewayIdentityHeader))
	}
}
