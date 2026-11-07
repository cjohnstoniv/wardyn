// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package imageadmit

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// wildcardServer is a TLS server whose certificate also covers *.example.com, so a test can redirect to a
// subdomain of the registry's host.
func wildcardServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: "example.com"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, IsCA: true,
		DNSNames: []string{"example.com", "*.example.com"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewUnstartedServer(h)
	s.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	s.StartTLS()
	return s
}

// hopLog is a two-hop redirect target: /path1 redirects to the relative /path2, which serves the blob.
type hopLog struct {
	srv *httptest.Server
	mu  sync.Mutex
	got map[string]string // path -> Authorization
}

func newHopLog(t *testing.T, wildcard bool) *hopLog {
	l := &hopLog{got: map[string]string{}}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l.mu.Lock()
		l.got[r.URL.Path] = r.Header.Get("Authorization")
		l.mu.Unlock()
		if r.URL.Path == "/path1" {
			http.Redirect(w, r, "/path2", http.StatusFound)
			return
		}
		_, _ = w.Write(layerBytes)
	})
	if wildcard {
		l.srv = wildcardServer(t, h)
	} else {
		l.srv = httptest.NewTLSServer(h)
	}
	return l
}

func (l *hopLog) auth(path string) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a, ok := l.got[path]
	return a, ok
}

// chainRegistry challenges until an Authorization arrives, with the scheme the source uses, then redirects
// the blob read to dest. It returns the credential strings that must never reach a redirect target.
func chainRegistry(t *testing.T, source string, dest string) (reg *httptest.Server, cred RegistryAuth, secrets []string, cleanup func()) {
	realm := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "RTOKEN"})
	}))
	reg = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			if source == "basic" {
				w.Header().Set("WWW-Authenticate", `Basic realm="r"`)
			} else {
				w.Header().Set("WWW-Authenticate", `Bearer realm="https://127.0.0.1:`+port(realm)+`/token",service="r"`)
			}
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, dest, http.StatusFound)
	}))
	switch source {
	case "identity token":
		cred = RegistryAuth{IdentityToken: "RSECRET-REFRESH"}
		secrets = []string{"RSECRET-REFRESH", "Bearer RTOKEN"}
	default:
		cred = RegistryAuth{Username: "alice", Password: "RSECRET"}
		secrets = []string{"Basic " + base64.StdEncoding.EncodeToString([]byte("alice:RSECRET")), "Bearer RTOKEN"}
	}
	return reg, cred, secrets, func() { reg.Close(); realm.Close() }
}

func TestRedirectChainNeverLeaksTheRegistryCredential(t *testing.T) {
	for _, source := range []string{"basic", "bearer", "identity token"} {
		for _, dest := range []string{"subdomain", "other port"} {
			t.Run(source+"/"+dest, func(t *testing.T) {
				hop := newHopLog(t, dest == "subdomain")
				defer hop.srv.Close()
				// Registered before the registry exists, so build the Location first.
				loc := "https://127.0.0.1:" + port(hop.srv) + "/path1"
				switch dest {
				case "subdomain":
					loc = "https://cdn.example.com:" + port(hop.srv) + "/path1"
				case "other port":
					loc = "https://example.com:" + port(hop.srv) + "/path1"
				}
				reg, cred, secrets, done := chainRegistry(t, source, loc)
				defer done()
				// Pool trust every server involved: realm and registry use the default test certificate.
				c := newTestClient(t, mapAuth{host(reg): cred}, testGuard(), nil, reg, hop.srv)
				c.hc.Transport.(*guardedRT).tr.TLSClientConfig.RootCAs = poolOf(reg, hop.srv)
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				b, err := c.Blob(ctx, mustRef(t, host(reg)+"/team/app"), layerDesc())
				if err != nil || string(b) != string(layerBytes) {
					t.Fatalf("blob = %q, err = %v", b, err)
				}
				for _, p := range []string{"/path1", "/path2"} {
					a, ok := hop.auth(p)
					if !ok {
						t.Fatalf("%s was never requested", p)
					}
					for _, s := range secrets {
						if strings.Contains(a, s) {
							t.Errorf("%s received the registry's credential %q", p, a)
						}
					}
					if a != "" {
						t.Errorf("%s received Authorization %q, none is configured for that origin", p, a)
					}
				}
			})
		}
	}
}

func TestRedirectChainGivesTheCDNItsOwnCredentialOnEveryHop(t *testing.T) {
	for _, source := range []string{"basic", "bearer", "identity token"} {
		t.Run(source, func(t *testing.T) {
			cdn := newHopLog(t, false)
			defer cdn.srv.Close()
			reg, cred, secrets, done := chainRegistry(t, source, "https://127.0.0.1:"+port(cdn.srv)+"/path1")
			defer done()
			cdnKey := "127.0.0.1:" + port(cdn.srv)
			want := "Basic " + base64.StdEncoding.EncodeToString([]byte("cdn:cdnpw"))
			c := newTestClient(t, mapAuth{host(reg): cred, cdnKey: {Username: "cdn", Password: "cdnpw"}}, testGuard(), nil, reg, cdn.srv)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := c.Blob(ctx, mustRef(t, host(reg)+"/team/app"), layerDesc()); err != nil {
				t.Fatal(err)
			}
			for _, p := range []string{"/path1", "/path2"} {
				a, _ := cdn.auth(p)
				if a != want {
					t.Errorf("%s Authorization = %q, want the CDN's own %q", p, a, want)
				}
				for _, s := range secrets {
					if strings.Contains(a, s) {
						t.Errorf("%s received the registry's credential", p)
					}
				}
			}
		})
	}
}

func TestSameOriginRedirectKeepsTheRegistryToken(t *testing.T) {
	var hits []string
	var mu sync.Mutex
	realm := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "RTOKEN"})
	}))
	defer realm.Close()
	reg := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer RTOKEN" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="https://127.0.0.1:`+port(realm)+`/token"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mu.Lock()
		hits = append(hits, r.URL.Path)
		mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/blobs/"+layerDesc().Digest) {
			http.Redirect(w, r, "/storage/blob", http.StatusFound)
			return
		}
		_, _ = w.Write(layerBytes)
	}))
	defer reg.Close()
	c := newTestClient(t, nil, testGuard(), nil, reg, realm)
	b, err := c.Blob(context.Background(), mustRef(t, host(reg)+"/team/app"), layerDesc())
	if err != nil || string(b) != string(layerBytes) {
		t.Fatalf("blob = %q, err = %v (hits %v)", b, err, hits)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(hits) != 2 || hits[1] != "/storage/blob" {
		t.Fatalf("authorized hits = %v, want the blob read and then /storage/blob, both carrying our Bearer", hits)
	}
}

func TestCheckRedirectNeverTrustsTheCopiedHeader(t *testing.T) {
	c := newTestClient(t, nil, testGuard(), nil)
	prev, _ := http.NewRequest("GET", "https://reg.example.com/a", nil)
	req, _ := http.NewRequest("GET", "https://reg.example.com/b", nil)
	req.Header.Set("Authorization", "Bearer copied-by-go")
	if err := c.checkRedirect(req, []*http.Request{prev}); err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("Authorization"); got != "" {
		t.Fatalf("a same-origin hop with no grant kept %q", got)
	}
}
