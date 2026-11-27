// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/spiffe/go-spiffe/v2/bundle/jwtbundle"
	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"

	"github.com/cjohnstoniv/wardyn/internal/hoptls"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/identity/svidsource"
)

// runCertSource is a SPIRE source whose SVID a test can swap, as a rotation does.
type runCertSource struct {
	mu     sync.Mutex
	svid   *x509svid.SVID
	closed bool
}

func (s *runCertSource) GetX509SVID() (*x509svid.SVID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.svid, nil
}

func (s *runCertSource) set(v *x509svid.SVID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.svid = v
}

func (*runCertSource) GetX509BundleForTrustDomain(spiffeid.TrustDomain) (*x509bundle.Bundle, error) {
	return nil, errors.New("no bundle in this test")
}

func (*runCertSource) GetJWTBundleForTrustDomain(spiffeid.TrustDomain) (*jwtbundle.Bundle, error) {
	return nil, errors.New("no bundle in this test")
}

func (s *runCertSource) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func runCertKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// runCertIssue signs tmpl with parent's key (self-signed when parent is nil) and returns the certificate.
func runCertIssue(t *testing.T, tmpl *x509.Certificate, key *ecdsa.PrivateKey, parent *x509.Certificate, parentKey *ecdsa.PrivateKey) *x509.Certificate {
	t.Helper()
	tmpl.SerialNumber = big.NewInt(time.Now().UnixNano())
	tmpl.NotBefore, tmpl.NotAfter = time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	if parent == nil {
		parent, parentKey = tmpl, key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, parentKey)
	if err != nil {
		t.Fatal(err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// runCertSVID is a run's X.509-SVID: one URI SAN, the run's SPIFFE ID.
func runCertSVID(t *testing.T, id spiffeid.ID) *x509svid.SVID {
	t.Helper()
	key := runCertKey(t)
	cert := runCertIssue(t, &x509.Certificate{
		URIs:        []*url.URL{id.URL()},
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}, key, nil, nil)
	return &x509svid.SVID{ID: id, Certificates: []*x509.Certificate{cert}, PrivateKey: key}
}

func runCertID(t *testing.T) spiffeid.ID {
	t.Helper()
	return spiffeid.RequireFromString("spiffe://example.org/agent-run/" + uuid.New().String())
}

// runCertServer is a control plane that asks for a client certificate and answers with the URI SAN it was
// shown. pinned is the internal CA PEM the proxy pins: the server's certificate comes from it unless
// foreign is set, when httptest's own certificate (from no internal CA) is served instead.
func runCertServer(t *testing.T, foreign bool) (srv *httptest.Server, pinned string) {
	t.Helper()
	caKey := runCertKey(t)
	ca := runCertIssue(t, &x509.Certificate{
		Subject: pkix.Name{CommonName: "test internal CA"}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}, caKey, nil, nil)
	pinned = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Raw}))
	srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.TLS.PeerCertificates) == 0 || len(r.TLS.PeerCertificates[0].URIs) == 0 {
			_, _ = io.WriteString(w, "none")
			return
		}
		_, _ = io.WriteString(w, r.TLS.PeerCertificates[0].URIs[0].String())
	}))
	srv.TLS = &tls.Config{ClientAuth: tls.RequestClientCert, MinVersion: tls.VersionTLS13}
	if !foreign {
		leafKey := runCertKey(t)
		leaf := runCertIssue(t, &x509.Certificate{
			IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
			KeyUsage:    x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}, leafKey, ca, caKey)
		srv.TLS.Certificates = []tls.Certificate{{Certificate: [][]byte{leaf.Raw}, PrivateKey: leafKey, Leaf: leaf}}
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, pinned
}

// runCertPresented makes one call on a new connection and returns the URI SAN the server saw.
func runCertPresented(t *testing.T, srv *httptest.Server, cp *tls.Config) (string, error) {
	t.Helper()
	c := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: cp, DisableKeepAlives: true}}
	resp, err := c.Get(srv.URL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}

func runCertOpener(src svidsource.Source, err error, gotAddr *string, gotWant *spiffeid.ID) func(context.Context, string, spiffeid.ID) (svidsource.Source, error) {
	return func(_ context.Context, addr string, want spiffeid.ID) (svidsource.Source, error) {
		*gotAddr, *gotWant = addr, want
		return src, err
	}
}

// X1: an empty peer ID leaves the control-plane config as it was and never opens a source.
func TestWithRunCertificate_OffTouchesNothing(t *testing.T) {
	cp, err := hoptls.ClientConfig("")
	if err != nil {
		t.Fatal(err)
	}
	opened := false
	c, err := withRunCertificate(context.Background(), cp, "", func(context.Context, string, spiffeid.ID) (svidsource.Source, error) {
		opened = true
		return nil, errors.New("must not open")
	})
	if c != nil || err != nil || opened || cp.GetClientCertificate != nil {
		t.Fatalf("withRunCertificate(\"\") = %v, %v, opened=%v, GetClientCertificate set=%v; want nil, nil, no open, unset", c, err, opened, cp.GetClientCertificate != nil)
	}
}

// X2: the proxy presents the run's SVID to a control plane that asks, from the agent's Workload API socket,
// and still pins the internal CA: a server whose certificate is not from that CA fails the handshake even
// though the proxy holds an SVID.
func TestWithRunCertificate_PresentsSVIDKeepsPin(t *testing.T) {
	id := runCertID(t)
	src := &runCertSource{svid: runCertSVID(t, id)}
	for _, foreign := range []bool{false, true} {
		srv, pinned := runCertServer(t, foreign)
		cp, err := hoptls.ClientConfig(pinned)
		if err != nil {
			t.Fatal(err)
		}
		var addr string
		var want spiffeid.ID
		closer, err := withRunCertificate(context.Background(), cp, id.String(), runCertOpener(src, nil, &addr, &want))
		if err != nil || closer != io.Closer(src) {
			t.Fatalf("withRunCertificate = %v, %v; want the source, nil", closer, err)
		}
		if addr != identity.ProxyWorkloadAPIAddr || want != id {
			t.Fatalf("opened %q for %v; want %q for %v", addr, want, identity.ProxyWorkloadAPIAddr, id)
		}
		got, err := runCertPresented(t, srv, cp)
		switch {
		case foreign && err == nil:
			t.Fatalf("a server from no internal CA was trusted (it saw %q)", got)
		case foreign && !strings.Contains(err.Error(), "certificate"):
			t.Fatalf("a server from no internal CA failed for another reason: %v", err)
		case !foreign && (err != nil || got != id.String()):
			t.Fatalf("the control plane saw %q, %v; want the run's SPIFFE ID %q", got, err, id)
		}
	}
}

// X3: a source that does not open, by error or by its deadline, fails proxy start, and the error carries
// no certificate bytes.
func TestWithRunCertificate_UnavailableFailsStart(t *testing.T) {
	id := runCertID(t)
	svid := runCertSVID(t, id)
	der := svid.Certificates[0].Raw
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	for name, open := range map[string]func(context.Context, string, spiffeid.ID) (svidsource.Source, error){
		"error": func(context.Context, string, spiffeid.ID) (svidsource.Source, error) {
			return nil, errors.New("svidsource: first X.509-SVID: connection refused")
		},
		"deadline": func(ctx context.Context, _ string, _ spiffeid.ID) (svidsource.Source, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	} {
		cp, err := hoptls.ClientConfig("")
		if err != nil {
			t.Fatal(err)
		}
		c, err := withRunCertificate(ctx, cp, id.String(), open)
		if c != nil || err == nil || !strings.HasPrefix(err.Error(), "control-plane client certificate unavailable: ") || cp.GetClientCertificate != nil {
			t.Fatalf("%s: withRunCertificate = %v, %v; want a refusal and an untouched config", name, c, err)
		}
		if name == "deadline" && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("deadline: %v; want the open's deadline", err)
		}
		msg := err.Error()
		if strings.Contains(msg, "BEGIN") || strings.Contains(msg, base64.StdEncoding.EncodeToString(der[:24])) || strings.Contains(msg, string(der[:24])) {
			t.Fatalf("%s: the start error carries certificate bytes: %q", name, msg)
		}
	}
	// With no deadline of its own, the start still waits at most the open timeout.
	cp, err := hoptls.ClientConfig("")
	if err != nil {
		t.Fatal(err)
	}
	var bounded bool
	_, err = withRunCertificate(context.Background(), cp, id.String(), func(ctx context.Context, _ string, _ spiffeid.ID) (svidsource.Source, error) {
		dl, ok := ctx.Deadline()
		bounded = ok && time.Until(dl) <= runCertificateOpenTimeout
		return nil, errors.New("no agent")
	})
	if err == nil || !bounded {
		t.Fatalf("withRunCertificate = %v, open bounded=%v; want a refusal from an open bounded by %s", err, bounded, runCertificateOpenTimeout)
	}
}

// X4: a peer ID needs an https control-plane URL and a spiffe:// value.
func TestConfig_ControlPlanePeerIDValidation(t *testing.T) {
	id := runCertID(t).String()
	const want = "control_plane_peer_id needs an https control-plane URL and a spiffe:// ID"
	for name, extra := range map[string]map[string]any{
		"http URL":        {"control_plane_url": "http://127.0.0.1:8080", "control_plane_ca_pem": "", "control_plane_peer_id": id},
		"not spiffe":      {"control_plane_peer_id": "https://example.org/agent-run/x"},
		"no trust domain": {"control_plane_peer_id": "spiffe://"},
	} {
		if _, err := LoadConfigBytes(baseConfigJSON(t, extra)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: LoadConfigBytes = %v; want %q", name, err, want)
		}
	}
	cfg, err := LoadConfigBytes(baseConfigJSON(t, map[string]any{"control_plane_peer_id": id}))
	if err != nil || cfg.ControlPlanePeerID != id {
		t.Fatalf("an https URL and a run's SPIFFE ID: %v, %v", cfg, err)
	}
	if cfg, err := LoadConfigBytes(baseConfigJSON(t, nil)); err != nil || cfg.ControlPlanePeerID != "" {
		t.Fatalf("unset: %v, %v; want it empty and accepted", cfg, err)
	}
}

// X5: after the source rotates the SVID, the next handshake presents the new one.
func TestWithRunCertificate_FollowsRotation(t *testing.T) {
	srv, pinned := runCertServer(t, false)
	cp, err := hoptls.ClientConfig(pinned)
	if err != nil {
		t.Fatal(err)
	}
	id := runCertID(t)
	first := runCertSVID(t, id)
	src := &runCertSource{svid: first}
	var addr string
	var want spiffeid.ID
	if _, err := withRunCertificate(context.Background(), cp, id.String(), runCertOpener(src, nil, &addr, &want)); err != nil {
		t.Fatal(err)
	}
	seen := func() *x509.Certificate {
		t.Helper()
		var got *x509.Certificate
		probe := cp.Clone()
		inner := probe.GetClientCertificate
		probe.GetClientCertificate = func(cri *tls.CertificateRequestInfo) (*tls.Certificate, error) {
			c, err := inner(cri)
			if err == nil && c != nil {
				got = c.Leaf
				if got == nil && len(c.Certificate) > 0 {
					got, _ = x509.ParseCertificate(c.Certificate[0])
				}
			}
			return c, err
		}
		if s, err := runCertPresented(t, srv, probe); err != nil || s != id.String() {
			t.Fatalf("the control plane saw %q, %v; want %q", s, err, id)
		}
		return got
	}
	if got := seen(); got == nil || !got.Equal(first.Certificates[0]) {
		t.Fatal("the first handshake did not present the first SVID")
	}
	second := runCertSVID(t, id)
	src.set(second)
	if got := seen(); got == nil || !got.Equal(second.Certificates[0]) {
		t.Fatal("the handshake after the rotation did not present the new SVID")
	}
}
