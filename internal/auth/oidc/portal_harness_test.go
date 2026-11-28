// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// The harness for the issuer verifier's tests: a TLS identity provider on a loopback listener that Wardyn
// reaches only through the dial seam, a fake clock, and a token minter.

var (
	portalCertOnce sync.Once
	portalCACert   *x509.Certificate
	portalCAPEM    []byte
	portalLeaf     tls.Certificate

	portalKeyOnce sync.Once
	portalKeys    []*rsa.PrivateKey
)

// portalTestHosts are the names the test server's certificate carries.
var portalTestHosts = []string{"example.com", "idp.internal.test", "other.example.org", "accounts.google.com", "login.corp.example"}

func portalTestCerts(t testing.TB) {
	t.Helper()
	portalCertOnce.Do(func() {
		caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "portal test CA"},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(48 * time.Hour),
			IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
		caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
		if err != nil {
			panic(err)
		}
		portalCACert, _ = x509.ParseCertificate(caDER)
		portalCAPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
		leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		leafTmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "portal test leaf"},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(48 * time.Hour),
			DNSNames: portalTestHosts, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
		leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, portalCACert, &leafKey.PublicKey, caKey)
		if err != nil {
			panic(err)
		}
		portalLeaf = tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}
	})
}

func portalTestKeys(t testing.TB) []*rsa.PrivateKey {
	t.Helper()
	portalKeyOnce.Do(func() {
		for i := 0; i < 4; i++ {
			k, err := rsa.GenerateKey(rand.Reader, 2048)
			if err != nil {
				panic(err)
			}
			portalKeys = append(portalKeys, k)
		}
	})
	return portalKeys
}

// portalClock is a settable clock that starts at the real time, since go-oidc's expiry check reads the real one.
type portalClock struct {
	mu  sync.Mutex
	now time.Time
}

func newPortalClock() *portalClock { return &portalClock{now: time.Now().Truncate(time.Second)} }

func (c *portalClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *portalClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// fakeIdP is the identity provider: a discovery document and a key document, both counted and both bendable.
type fakeIdP struct {
	srv *httptest.Server

	mu         sync.Mutex
	docIssuer  string // the issuer the discovery document states
	jwksURI    string
	kids       []string // published kids, in keys order
	keyIdx     map[string]int
	discHits   int
	keyHits    int
	requests   []string // host + path of every request
	discStatus int
	keyStatus  int
	keyDelay   time.Duration
	override   func(w http.ResponseWriter, r *http.Request) bool
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	portalTestCerts(t)
	f := &fakeIdP{keyIdx: map[string]int{}}
	f.srv = httptest.NewUnstartedServer(f)
	f.srv.TLS = &tls.Config{Certificates: []tls.Certificate{portalLeaf}}
	f.srv.StartTLS()
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeIdP) port() string {
	_, p, _ := net.SplitHostPort(f.srv.Listener.Addr().String())
	return p
}

// publish makes keys[idx] the published key for kid.
func (f *fakeIdP) publish(t testing.TB, kid string, idx int) {
	_ = portalTestKeys(t)
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.keyIdx[kid]; !ok {
		f.kids = append(f.kids, kid)
	}
	f.keyIdx[kid] = idx
}

func (f *fakeIdP) unpublish(kid string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.keyIdx, kid)
	for i, k := range f.kids {
		if k == kid {
			f.kids = append(f.kids[:i], f.kids[i+1:]...)
			break
		}
	}
}

func (f *fakeIdP) set(fn func(f *fakeIdP)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeIdP) hits() (disc, keys int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.discHits, f.keyHits
}

func (f *fakeIdP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.Host+r.URL.Path)
	override := f.override
	f.mu.Unlock()
	if override != nil && override(w, r) {
		return
	}
	switch {
	case strings.HasSuffix(r.URL.Path, "/.well-known/openid-configuration"):
		f.mu.Lock()
		f.discHits++
		status, doc := f.discStatus, map[string]string{"issuer": f.docIssuer, "jwks_uri": f.jwksURI}
		f.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		_ = json.NewEncoder(w).Encode(doc)
	case strings.HasSuffix(r.URL.Path, "/keys"):
		f.mu.Lock()
		f.keyHits++
		status, delay := f.keyStatus, f.keyDelay
		var keys []jose.JSONWebKey
		for _, kid := range f.kids {
			keys = append(keys, jose.JSONWebKey{Key: &portalKeys[f.keyIdx[kid]].PublicKey, KeyID: kid, Algorithm: "RS256", Use: "sig"})
		}
		f.mu.Unlock()
		if delay > 0 {
			time.Sleep(delay)
		}
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: keys})
	default:
		http.NotFound(w, r)
	}
}

// portalHarness wires a fake provider, a clock and a PortalIssuers whose network seams reach only the fake.
type portalHarness struct {
	t     *testing.T
	idp   *fakeIdP
	clock *portalClock
	pi    *PortalIssuers
	reg   PortalRegistration

	mu       sync.Mutex
	lookupFn func(host string) []net.IP // what names resolve to
	lookups  int
	dials    []string // the address of every dial that was approved and reached the seam
	dialErr  error
}

var portalPublicIP = net.ParseIP("8.8.8.8")

// newPortalHarness starts a provider whose issuer is https://<host>[:<port>]<path> and an access-token portal
// registration for it, with key "k1" published.
func newPortalHarness(t *testing.T, host string, withPort bool, path string, mod ...func(*PortalIssuersOptions)) *portalHarness {
	t.Helper()
	h := &portalHarness{t: t, idp: newFakeIdP(t), clock: newPortalClock()}
	h.lookupFn = func(string) []net.IP { return []net.IP{portalPublicIP} }
	issuer := "https://" + host
	if withPort {
		issuer += ":" + h.idp.port()
	}
	issuer += path
	h.idp.publish(t, "k1", 0)
	h.idp.set(func(f *fakeIdP) { f.docIssuer, f.jwksURI = issuer, issuer+"/keys" })
	h.reg = PortalRegistration{
		Issuer: issuer, TokenProfile: PortalProfileAccessToken, ClientID: "client-1", Audience: "api://wardyn",
		RequiredScopes: []string{"wardyn.run"}, MaxSubjectTokenAge: 300 * time.Second,
	}
	h.rebuild(mod...)
	return h
}

func (h *portalHarness) rebuild(mod ...func(*PortalIssuersOptions)) {
	h.t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(portalCACert)
	opts := PortalIssuersOptions{
		Reserved: func(p string) bool {
			p = strings.ToLower(strings.TrimSpace(p))
			return strings.HasPrefix(p, "local:") || strings.HasPrefix(p, "service:") || strings.HasPrefix(p, "delegate:") || p == "admin-token"
		},
		clock: h.clock.Now, roots: pool,
		lookup: func(_ context.Context, host string) ([]net.IP, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.lookups++
			return h.lookupFn(host), nil
		},
		dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			h.mu.Lock()
			h.dials = append(h.dials, addr)
			err := h.dialErr
			h.mu.Unlock()
			if err != nil {
				return nil, err
			}
			return (&net.Dialer{}).DialContext(ctx, network, h.idp.srv.Listener.Addr().String())
		},
	}
	for _, m := range mod {
		m(&opts)
	}
	pi, err := NewPortalIssuers(opts)
	if err != nil {
		h.t.Fatal(err)
	}
	h.pi = pi
}

func (h *portalHarness) dialCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.dials)
}

func (h *portalHarness) setLookup(fn func(host string) []net.IP) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lookupFn = fn
}

// accessClaims are the claims of a valid access token, issued at the harness clock.
func (h *portalHarness) accessClaims() map[string]any {
	now := h.clock.Now()
	return map[string]any{
		"iss": h.reg.Issuer, "sub": "alice@example.org", "uid": "00u1abcd", "cid": "client-1", "aud": "api://wardyn",
		"scp": []string{"wardyn.run", "other"}, "iat": now.Unix(), "exp": now.Add(10 * time.Minute).Unix(),
	}
}

// idClaims are the claims of a valid ID token for an ID-token registration.
func (h *portalHarness) idClaims() map[string]any {
	now := h.clock.Now()
	return map[string]any{
		"iss": h.reg.Issuer, "sub": "00u1abcd", "aud": "client-1", "iat": now.Unix(), "exp": now.Add(10 * time.Minute).Unix(),
		"email": "alice@example.org", "email_verified": true,
	}
}

func (h *portalHarness) idRegistration() PortalRegistration {
	r := h.reg
	r.TokenProfile = PortalProfileIDToken
	r.Audience, r.RequiredScopes = "", nil
	return r
}

// mint signs claims with key index idx under kid, with the given extra header entries.
func (h *portalHarness) mint(kid string, idx int, claims map[string]any, hdr map[jose.HeaderKey]any) string {
	h.t.Helper()
	payload, _ := json.Marshal(claims)
	return h.mintRaw(kid, idx, payload, hdr)
}

// mintRaw signs a payload as given, so member order and spelling are the test's.
func (h *portalHarness) mintRaw(kid string, idx int, payload []byte, hdr map[jose.HeaderKey]any) string {
	h.t.Helper()
	keys := portalTestKeys(h.t)
	opts := &jose.SignerOptions{}
	for k, v := range hdr {
		opts.WithHeader(k, v)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: keys[idx], KeyID: kid}}, opts)
	if err != nil {
		h.t.Fatal(err)
	}
	obj, err := signer.Sign(payload)
	if err != nil {
		h.t.Fatal(err)
	}
	s, err := obj.CompactSerialize()
	if err != nil {
		h.t.Fatal(err)
	}
	return s
}

// mintHeader signs claims under a protected header written out by the test, so a member go-jose would drop (a
// JSON null) is on the wire as the test wrote it.
func (h *portalHarness) mintHeader(idx int, claims map[string]any, headerJSON string) string {
	h.t.Helper()
	payload, _ := json.Marshal(claims)
	signed := base64.RawURLEncoding.EncodeToString([]byte(headerJSON)) + "." + base64.RawURLEncoding.EncodeToString(payload)
	sum := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, portalTestKeys(h.t)[idx], crypto.SHA256, sum[:])
	if err != nil {
		h.t.Fatal(err)
	}
	return signed + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func (h *portalHarness) token(claims map[string]any) string { return h.mint("k1", 0, claims, nil) }

// verify runs one exchange for the harness registration with the matching declared type.
func (h *portalHarness) verify(raw string) (PortalSubject, PortalRefusal) {
	return h.verifyReg(h.reg, raw)
}

func (h *portalHarness) verifyReg(reg PortalRegistration, raw string) (PortalSubject, PortalRefusal) {
	return h.pi.VerifyDetail(context.Background(), reg, raw, portalProfileType(reg.TokenProfile))
}

func (h *portalHarness) mustAdmit(raw string) PortalSubject {
	h.t.Helper()
	subj, r := h.verify(raw)
	if !r.OK() {
		h.t.Fatalf("token refused: %+v", r)
	}
	return subj
}

func (h *portalHarness) wantRefusal(raw, reason string) PortalRefusal {
	h.t.Helper()
	_, r := h.verify(raw)
	if r.Reason != reason {
		h.t.Fatalf("refusal = %+v, want reason %q", r, reason)
	}
	return r
}

func (h *portalHarness) wantUnavailable(raw, cause string) PortalRefusal {
	h.t.Helper()
	_, r := h.verify(raw)
	if r.Reason != PortalIssuerUnavailable || r.Cause != cause || r.Class != PortalRefusedUnavailable {
		h.t.Fatalf("refusal = %+v, want 503 cause %q", r, cause)
	}
	return r
}
