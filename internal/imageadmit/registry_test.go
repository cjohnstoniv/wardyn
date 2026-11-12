// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package imageadmit

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/ipguard"
)

// testGuard allows loopback (the httptest servers) and refuses everything else the production guard does.
// Every host name resolves to 127.0.0.1 so a test can give one server several host names.
func testGuard() *targetGuard {
	return &targetGuard{
		refuse: func(ip net.IP) bool { return !ip.IsLoopback() && ipguard.GatewayIPRefused(ip) },
		lookup: func(_ context.Context, host string) ([]net.IP, error) {
			if ip := net.ParseIP(host); ip != nil {
				return []net.IP{ip}, nil
			}
			return []net.IP{net.IPv4(127, 0, 0, 1)}, nil
		},
	}
}

type mapAuth map[string]RegistryAuth

func (m mapAuth) Lookup(host string) (RegistryAuth, bool, error) { a, ok := m[host]; return a, ok, nil }

func poolOf(srvs ...*httptest.Server) *x509.CertPool {
	p := x509.NewCertPool()
	for _, s := range srvs {
		p.AddCert(s.Certificate())
	}
	return p
}

func newTestClient(t *testing.T, auth CredentialSource, g *targetGuard, proxy func(*http.Request) (*url.URL, error), srvs ...*httptest.Server) *RegistryClient {
	t.Helper()
	base := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: poolOf(srvs...)}, Proxy: proxy}
	return newRegistryClient(RegistryOptions{Base: base, Auth: auth}, g)
}

func port(s *httptest.Server) string { u, _ := url.Parse(s.URL); return u.Port() }

// host is the name the tests use for a server: example.com is in the httptest certificate.
func host(s *httptest.Server) string { return "example.com:" + port(s) }

func sum(b []byte) string { h := sha256.Sum256(b); return "sha256:" + hex.EncodeToString(h[:]) }

func mustRef(t *testing.T, s string) Ref {
	t.Helper()
	r, err := ParseRef(s)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", s, err)
	}
	return r
}

const (
	ociManifestType = "application/vnd.oci.image.manifest.v1+json"
	ociIndexType    = "application/vnd.oci.image.index.v1+json"
)

var imageBody = []byte(`{"schemaVersion":2,"mediaType":"` + ociManifestType + `","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:` + strings.Repeat("a", 64) + `","size":2},"layers":[]}`)

func serveManifest(w http.ResponseWriter, body []byte, mediaType, headerDigest string) {
	w.Header().Set("Content-Type", mediaType)
	if headerDigest != "" {
		w.Header().Set("Docker-Content-Digest", headerDigest)
	}
	_, _ = w.Write(body)
}

func TestResolveTagComputesDigestFromBytes(t *testing.T) {
	var accept string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/team/app/manifests/1.0" {
			http.NotFound(w, r)
			return
		}
		accept = r.Header.Get("Accept")
		serveManifest(w, imageBody, ociManifestType, "")
	}))
	defer srv.Close()
	c := newTestClient(t, nil, testGuard(), nil, srv)
	m, err := c.Manifest(context.Background(), mustRef(t, host(srv)+"/team/app:1.0"))
	if err != nil {
		t.Fatal(err)
	}
	if m.Digest != sum(imageBody) {
		t.Fatalf("digest = %s, want %s (computed from the bytes, no header was sent)", m.Digest, sum(imageBody))
	}
	for _, want := range []string{ociIndexType, ociManifestType, "application/vnd.docker.distribution.manifest.list.v2+json", "application/vnd.docker.distribution.manifest.v2+json"} {
		if !strings.Contains(accept, want) {
			t.Errorf("Accept %q lacks %s", accept, want)
		}
	}
}

func TestResolveRefusesHeaderBodyMismatch(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveManifest(w, imageBody, ociManifestType, sum([]byte("some other manifest")))
	}))
	defer srv.Close()
	c := newTestClient(t, nil, testGuard(), nil, srv)
	_, err := c.Manifest(context.Background(), mustRef(t, host(srv)+"/team/app:1.0"))
	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("err = %v, want ErrDigestMismatch", err)
	}
}

func TestResolveByDigestRefusesWrongBytes(t *testing.T) {
	want := sum(imageBody)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveManifest(w, []byte(`{"schemaVersion":2,"layers":[]}`), ociManifestType, want)
	}))
	defer srv.Close()
	c := newTestClient(t, nil, testGuard(), nil, srv)
	_, err := c.Manifest(context.Background(), mustRef(t, host(srv)+"/team/app@"+want))
	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("err = %v, want ErrDigestMismatch", err)
	}
}

func TestManifestOverBoundRefused(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveManifest(w, []byte(strings.Repeat("x", MaxManifestBytes+1)), ociManifestType, "")
	}))
	defer srv.Close()
	c := newTestClient(t, nil, testGuard(), nil, srv)
	_, err := c.Manifest(context.Background(), mustRef(t, host(srv)+"/team/app:1.0"))
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
}

// bearerRegistry challenges every request without the token and records what the realm saw.
func bearerRegistry(realmURL string, token string, seen *atomic.Value) *httptest.Server {
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s",service="reg.test",scope="repository:team/app:pull"`, realmURL))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		serveManifest(w, imageBody, ociManifestType, "")
	}))
}

func TestBearerChallengeFlowSendsBasicToHTTPSRealm(t *testing.T) {
	var realmReq atomic.Value
	realm := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		realmReq.Store(r.Clone(context.Background()))
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "tok1", "expires_in": 300})
	}))
	defer realm.Close()
	reg := bearerRegistry("https://127.0.0.1:"+port(realm)+"/token", "tok1", nil)
	defer reg.Close()
	auth := mapAuth{host(reg): {Username: "u", Password: "p"}}
	c := newTestClient(t, auth, testGuard(), nil, reg, realm)
	m, err := c.Manifest(context.Background(), mustRef(t, host(reg)+"/team/app:1.0"))
	if err != nil {
		t.Fatal(err)
	}
	if m.Digest != sum(imageBody) {
		t.Fatalf("digest = %s", m.Digest)
	}
	r := realmReq.Load().(*http.Request)
	if u, p, ok := r.BasicAuth(); !ok || u != "u" || p != "p" {
		t.Errorf("realm Authorization = %q, want Basic u:p", r.Header.Get("Authorization"))
	}
	if q := r.URL.Query(); q.Get("service") != "reg.test" || q.Get("scope") != "repository:team/app:pull" {
		t.Errorf("realm query = %v", q)
	}
}

func TestAnonymousBearerSendsNoCredential(t *testing.T) {
	var realmAuth atomic.Value
	realm := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		realmAuth.Store(r.Header.Get("Authorization"))
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok2"})
	}))
	defer realm.Close()
	reg := bearerRegistry("https://127.0.0.1:"+port(realm)+"/token", "tok2", nil)
	defer reg.Close()
	c := newTestClient(t, nil, testGuard(), nil, reg, realm)
	if _, err := c.Manifest(context.Background(), mustRef(t, host(reg)+"/team/app:1.0")); err != nil {
		t.Fatal(err)
	}
	if got := realmAuth.Load(); got != "" {
		t.Errorf("anonymous pull sent Authorization %q to the realm", got)
	}
}

func TestBasicChallengeRetriesWithConfiguredCredential(t *testing.T) {
	reg := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "u" || p != "p" {
			w.Header().Set("WWW-Authenticate", `Basic realm="reg"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		serveManifest(w, imageBody, ociManifestType, "")
	}))
	defer reg.Close()
	c := newTestClient(t, mapAuth{host(reg): {Username: "u", Password: "p"}}, testGuard(), nil, reg)
	if _, err := c.Manifest(context.Background(), mustRef(t, host(reg)+"/team/app:1.0")); err != nil {
		t.Fatal(err)
	}
}

func TestCredentialsNeverSentToPlainHTTPRealm(t *testing.T) {
	var hits atomic.Int32
	realm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer realm.Close()
	reg := bearerRegistry(realm.URL+"/token", "tok", nil)
	defer reg.Close()
	c := newTestClient(t, mapAuth{host(reg): {Username: "u", Password: "p"}}, testGuard(), nil, reg)
	_, err := c.Manifest(context.Background(), mustRef(t, host(reg)+"/team/app:1.0"))
	var un *Unavailable
	if !errors.As(err, &un) || un.Cause != CauseRefused {
		t.Fatalf("err = %v, want Unavailable{refused}", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("plain-http realm got %d requests, want 0", hits.Load())
	}
	if strings.Contains(err.Error(), "dTpw") { // base64("u:p") begins dTpw
		t.Errorf("error leaks credentials: %v", err)
	}
}

// blobRegistry answers any request without Basic credentials with a Basic challenge, then redirects blob reads.
func blobRegistry(location *atomic.Value) *httptest.Server {
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, _, ok := r.BasicAuth(); !ok {
			w.Header().Set("WWW-Authenticate", `Basic realm="reg"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, location.Load().(string), http.StatusFound)
	}))
}

var layerBytes = []byte("signature bytes")

func layerDesc() Descriptor {
	return Descriptor{MediaType: "application/vnd.dev.cosign.simplesigning.v1+json", Digest: sum(layerBytes), Size: int64(len(layerBytes))}
}

func TestRedirectToAnotherOriginDropsAuthorization(t *testing.T) {
	cases := []struct {
		name       string
		dest       func(target *httptest.Server) string // the Location
		destCred   func(target *httptest.Server) mapAuth
		wantAuthzn bool
	}{
		{"other host", func(s *httptest.Server) string { return "https://127.0.0.1:" + port(s) + "/blob" }, nil, false},
		{"other port, same host", func(s *httptest.Server) string { return "https://" + host(s) + "/blob" }, nil, false},
		{"credential configured for the destination is re-added", func(s *httptest.Server) string { return "https://127.0.0.1:" + port(s) + "/blob" },
			func(s *httptest.Server) mapAuth {
				return mapAuth{"127.0.0.1:" + port(s): {Username: "cdn", Password: "cdnpw"}}
			}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotAuth atomic.Value
			gotAuth.Store("unset")
			target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotAuth.Store(r.Header.Get("Authorization"))
				_, _ = w.Write(layerBytes)
			}))
			defer target.Close()
			var loc atomic.Value
			loc.Store(tc.dest(target))
			reg := blobRegistry(&loc)
			defer reg.Close()
			auth := mapAuth{host(reg): {Username: "u", Password: "p"}}
			if tc.destCred != nil {
				for k, v := range tc.destCred(target) {
					auth[k] = v
				}
			}
			c := newTestClient(t, auth, testGuard(), nil, reg, target)
			b, err := c.Blob(context.Background(), mustRef(t, host(reg)+"/team/app:1.0"), layerDesc())
			if err != nil {
				t.Fatal(err)
			}
			if string(b) != string(layerBytes) {
				t.Fatalf("blob = %q", b)
			}
			got := gotAuth.Load().(string)
			if tc.wantAuthzn && !strings.HasPrefix(got, "Basic ") {
				t.Errorf("destination Authorization = %q, want the destination's own Basic credential", got)
			}
			if !tc.wantAuthzn && got != "" {
				t.Errorf("destination Authorization = %q, want none", got)
			}
			if tc.wantAuthzn && strings.Contains(got, "dTpw") { // base64("u:p") begins dTpw
				t.Errorf("the registry's credential reached the destination")
			}
		})
	}
}

func TestCheckRedirectOrigins(t *testing.T) {
	c := newTestClient(t, nil, testGuard(), nil)
	mk := func(u string) *http.Request {
		r, _ := http.NewRequest("GET", u, nil)
		r.Header.Set("Authorization", "Bearer secret")
		return r
	}
	cases := []struct {
		name, from, to string
		keep           bool
		refused        bool
	}{
		{"same origin keeps", "https://reg.example.com/v2/a", "https://reg.example.com/v2/b", true, false},
		{"default port is the same origin", "https://reg.example.com/a", "https://reg.example.com:443/b", true, false},
		{"subdomain drops", "https://reg.example.com/a", "https://cdn.reg.example.com/b", false, false},
		{"parent domain drops", "https://cdn.reg.example.com/a", "https://reg.example.com/b", false, false},
		{"changed port drops", "https://reg.example.com/a", "https://reg.example.com:8443/b", false, false},
		{"other host drops", "https://reg.example.com/a", "https://blob.example.net/b", false, false},
		{"downgrade to http refused", "https://reg.example.com/a", "http://reg.example.com/b", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := mk(tc.to)
			// The client grants its header to the origin it was set for; the copy Go made is never the source.
			fromURL, _ := url.Parse(tc.from)
			req = req.WithContext(context.WithValue(req.Context(), authzGrantKey{}, authzGrant{origin: originOf(fromURL), header: "Bearer secret"}))
			err := c.checkRedirect(req, []*http.Request{mk(tc.from)})
			if tc.refused {
				if !errors.Is(err, ErrRefusedTarget) {
					t.Fatalf("err = %v, want ErrRefusedTarget", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if kept := req.Header.Get("Authorization") != ""; kept != tc.keep {
				t.Errorf("Authorization kept = %v, want %v", kept, tc.keep)
			}
		})
	}
}

func TestRedirectLimit(t *testing.T) {
	var self atomic.Value
	reg := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, self.Load().(string)+"/next", http.StatusFound)
	}))
	defer reg.Close()
	self.Store("https://" + host(reg))
	c := newTestClient(t, nil, testGuard(), nil, reg)
	_, err := c.Manifest(context.Background(), mustRef(t, host(reg)+"/team/app:1.0"))
	if err == nil {
		t.Fatal("an endless redirect chain was followed")
	}
}

func indexJSON(t *testing.T, n int, artifactType string) []byte {
	t.Helper()
	var ms []Descriptor
	for i := 0; i < n; i++ {
		at := artifactType
		if i%2 == 1 {
			at = "application/other"
		}
		ms = append(ms, Descriptor{MediaType: ociManifestType, Digest: sum([]byte{byte(i)}), Size: 10, ArtifactType: at})
	}
	b, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": ociIndexType, "manifests": ms})
	return b
}

func TestReferrersAPIAndArtifactTypeFilter(t *testing.T) {
	subject := sum(imageBody)
	var gotQuery, gotAccept string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/team/app/referrers/"+subject {
			http.NotFound(w, r)
			return
		}
		gotQuery, gotAccept = r.URL.Query().Get("artifactType"), r.Header.Get("Accept")
		serveManifest(w, indexJSON(t, 6, "application/vnd.dev.sigstore.bundle.v0.3+json"), ociIndexType, "")
	}))
	defer srv.Close()
	c := newTestClient(t, nil, testGuard(), nil, srv)
	got, err := c.Referrers(context.Background(), mustRef(t, host(srv)+"/team/app"), subject, "application/vnd.dev.sigstore.bundle.v0.3+json")
	if err != nil {
		t.Fatal(err)
	}
	if gotQuery != "application/vnd.dev.sigstore.bundle.v0.3+json" {
		t.Errorf("artifactType query = %q", gotQuery)
	}
	if !strings.Contains(gotAccept, ociIndexType) || strings.Contains(gotAccept, "docker") {
		t.Errorf("referrers Accept = %q, want the OCI index only", gotAccept)
	}
	if len(got) != 3 {
		t.Fatalf("got %d referrers, want the 3 of the requested type (client-side filter when the registry ignores the query)", len(got))
	}
}

func TestReferrersFallsBackToTagSchemaOn404(t *testing.T) {
	subject := sum(imageBody)
	tag := "sha256-" + strings.TrimPrefix(subject, "sha256:")
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/team/app/manifests/" + tag:
			serveManifest(w, indexJSON(t, 4, "application/vnd.dev.sigstore.bundle.v0.3+json"), ociIndexType, "")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := newTestClient(t, nil, testGuard(), nil, srv)
	got, err := c.Referrers(context.Background(), mustRef(t, host(srv)+"/team/app"), subject, "application/vnd.dev.sigstore.bundle.v0.3+json")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d, want 2 after the artifactType filter", len(got))
	}
}

func TestReferrersNoneAnywhereIsEmptyNotError(t *testing.T) {
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	defer srv.Close()
	c := newTestClient(t, nil, testGuard(), nil, srv)
	got, err := c.Referrers(context.Background(), mustRef(t, host(srv)+"/team/app"), sum(imageBody), "")
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v; want empty and nil", got, err)
	}
}

func TestReferrersReadsAtMost64(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveManifest(w, indexJSON(t, 130, "a"), ociIndexType, "")
	}))
	defer srv.Close()
	c := newTestClient(t, nil, testGuard(), nil, srv)
	got, err := c.Referrers(context.Background(), mustRef(t, host(srv)+"/team/app"), sum(imageBody), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != MaxReferrers {
		t.Fatalf("got %d referrers, want the cap %d", len(got), MaxReferrers)
	}
}

func TestLegacyTagReturnsLayersAndAnnotations(t *testing.T) {
	subject := sum(imageBody)
	hexd := strings.TrimPrefix(subject, "sha256:")
	for _, suffix := range []string{".sig", ".att"} {
		t.Run(suffix, func(t *testing.T) {
			body, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": ociManifestType,
				"config": map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": "sha256:" + strings.Repeat("b", 64), "size": 2},
				"layers": []any{map[string]any{"mediaType": "application/vnd.dev.cosign.simplesigning.v1+json", "digest": sum(layerBytes), "size": len(layerBytes),
					"annotations": map[string]string{"dev.cosignproject.cosign/signature": "MEUC", "dev.sigstore.cosign/certificate": "-----BEGIN CERTIFICATE-----"}}}})
			var accept string
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v2/team/app/manifests/sha256-"+hexd+suffix {
					http.NotFound(w, r)
					return
				}
				accept = r.Header.Get("Accept")
				serveManifest(w, body, ociManifestType, "")
			}))
			defer srv.Close()
			c := newTestClient(t, nil, testGuard(), nil, srv)
			m, err := c.LegacyTag(context.Background(), mustRef(t, host(srv)+"/team/app"), subject, suffix)
			if err != nil {
				t.Fatal(err)
			}
			if len(m.Layers) != 1 || m.Layers[0].Annotations["dev.cosignproject.cosign/signature"] != "MEUC" || m.Layers[0].Digest != sum(layerBytes) {
				t.Fatalf("layers = %+v", m.Layers)
			}
			if !strings.Contains(accept, ociManifestType) {
				t.Errorf("Accept = %q", accept)
			}
		})
	}
}

func TestLegacyTagRejectsUnknownSuffix(t *testing.T) {
	c := newTestClient(t, nil, testGuard(), nil)
	if _, err := c.LegacyTag(context.Background(), mustRef(t, "example.com/team/app"), sum(imageBody), "/../x"); err == nil {
		t.Fatal("a suffix other than .sig and .att was accepted")
	}
}

func TestBlobDigestMismatchAndBoundsRefused(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("tampered bytes!"))
	}))
	defer srv.Close()
	c := newTestClient(t, nil, testGuard(), nil, srv)
	ref := mustRef(t, host(srv)+"/team/app")
	if _, err := c.Blob(context.Background(), ref, layerDesc()); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("tampered blob: err = %v, want ErrDigestMismatch", err)
	}
	big := layerDesc()
	big.Size = MaxBlobBytes + 1
	if _, err := c.Blob(context.Background(), ref, big); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversize descriptor: err = %v, want ErrTooLarge", err)
	}
}

func TestUnavailableCauses(t *testing.T) {
	status := func(code int, challenge string) *httptest.Server {
		return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if challenge != "" {
				w.Header().Set("WWW-Authenticate", challenge)
			}
			w.WriteHeader(code)
		}))
	}
	t.Run("401 with no credential", func(t *testing.T) {
		srv := status(401, `Basic realm="r"`)
		defer srv.Close()
		un := mustUnavailable(t, newTestClient(t, nil, testGuard(), nil, srv), host(srv))
		if un.Cause != CauseUnauthorized || un.HasCredential || un.Host != host(srv) {
			t.Errorf("got %+v", un)
		}
	})
	t.Run("401 with a credential", func(t *testing.T) {
		srv := status(401, `Basic realm="r"`)
		defer srv.Close()
		un := mustUnavailable(t, newTestClient(t, mapAuth{host(srv): {Username: "u", Password: "p"}}, testGuard(), nil, srv), host(srv))
		if un.Cause != CauseUnauthorized || !un.HasCredential {
			t.Errorf("got %+v", un)
		}
	})
	t.Run("403", func(t *testing.T) {
		srv := status(403, "")
		defer srv.Close()
		if un := mustUnavailable(t, newTestClient(t, nil, testGuard(), nil, srv), host(srv)); un.Cause != CauseForbidden {
			t.Errorf("got %+v", un)
		}
	})
	t.Run("503", func(t *testing.T) {
		srv := status(503, "")
		defer srv.Close()
		if un := mustUnavailable(t, newTestClient(t, nil, testGuard(), nil, srv), host(srv)); un.Cause != CauseServer {
			t.Errorf("got %+v", un)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		// The handler answers only once the client has returned. Waiting on r.Context() instead answered 200 with an
		// empty body as soon as the client's cancel closed the connection, and net/http hands back a response that
		// races the context's end, so the client could read "not JSON" at the deadline.
		release := make(chan struct{})
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
		defer srv.Close()
		defer close(release)
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		_, err := newTestClient(t, nil, testGuard(), nil, srv).Manifest(ctx, mustRef(t, host(srv)+"/team/app:1.0"))
		var un *Unavailable
		if !errors.As(err, &un) || un.Cause != CauseTimeout {
			t.Fatalf("err = %v, want Unavailable{timeout}", err)
		}
	})
	t.Run("untrusted TLS", func(t *testing.T) {
		srv := httptest.NewTLSServer(http.NotFoundHandler())
		defer srv.Close()
		c := newTestClient(t, nil, testGuard(), nil) // empty pool: the server's certificate is unknown
		if un := mustUnavailable(t, c, host(srv)); un.Cause != CauseTLS {
			t.Errorf("got %+v", un)
		}
	})
	t.Run("unreachable", func(t *testing.T) {
		l, _ := net.Listen("tcp", "127.0.0.1:0")
		addr := l.Addr().String()
		_ = l.Close()
		_, p, _ := net.SplitHostPort(addr)
		c := newTestClient(t, nil, testGuard(), nil)
		if un := mustUnavailable(t, c, "example.com:"+p); un.Cause != CauseUnreachable {
			t.Errorf("got %+v", un)
		}
	})
}

func mustUnavailable(t *testing.T, c *RegistryClient, hostPort string) *Unavailable {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := c.Manifest(ctx, mustRef(t, hostPort+"/team/app:1.0"))
	var un *Unavailable
	if !errors.As(err, &un) {
		t.Fatalf("err = %v, want *Unavailable", err)
	}
	if strings.Contains(un.Error(), "Basic ") || strings.Contains(un.Error(), "Bearer ") {
		t.Errorf("error text carries an Authorization value: %v", un)
	}
	return un
}
