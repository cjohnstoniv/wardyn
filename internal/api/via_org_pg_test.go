// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The organisation's via_org egress (H12, design 0.9 §7.3) against a real Postgres: the route serves
// only this run's via_org grants, to the grant's own destination, under the per-run bounds, and every
// open stream ends with the run or its runner. The end-to-end test runs over a real runner session and
// H5's relay.
//
// Guarded by WARDYN_TEST_PG (throwawayPGPool): skipped cleanly when unset, must PASS when set.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/hoptls"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	viaOrgAPIHost  = "api.corp.example"
	viaOrgAPIValue = "ORG-SECRET-VALUE-9f3c"
)

// viaOrgUpstream is the internet the organisation dials: TLS for any name asked, under a test CA
// the server trusts (Config.TrustedCAPEM). hold, when set, keeps a request open until released.
type viaOrgUpstream struct {
	srv   *httptest.Server
	caPEM string
	mu    sync.Mutex
	hits  int
	auth  []string
	hosts []string
	open  atomic.Int32
	hold  chan struct{}
	big   int // bytes the upstream answers with, when set
}

func newViaOrgUpstream(t *testing.T) *viaOrgUpstream {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "via-org test CA"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), IsCA: true, KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(der)
	leaf := func(name string) (*tls.Certificate, error) {
		lk, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		lt := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
		ld, err := x509.CreateCertificate(rand.Reader, lt, ca, &lk.PublicKey, key)
		return &tls.Certificate{Certificate: [][]byte{ld}, PrivateKey: lk}, err
	}
	u := &viaOrgUpstream{caPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
	u.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.hits++
		u.auth, u.hosts = append(u.auth, r.Header.Get("Authorization")), append(u.hosts, r.Host)
		hold := u.hold
		u.mu.Unlock()
		_, _ = io.Copy(io.Discard, r.Body)
		if hold != nil {
			w.WriteHeader(http.StatusOK)
			http.NewResponseController(w).Flush()
			u.open.Add(1)
			select {
			case <-hold:
			case <-r.Context().Done():
			}
			u.open.Add(-1)
			return
		}
		if u.big > 0 {
			_, _ = w.Write(bytes.Repeat([]byte("x"), u.big))
			return
		}
		_, _ = io.WriteString(w, "upstream-ok")
	}))
	u.srv.TLS = &tls.Config{GetCertificate: func(h *tls.ClientHelloInfo) (*tls.Certificate, error) { return leaf(h.ServerName) }}
	u.srv.StartTLS()
	t.Cleanup(u.srv.Close)
	return u
}

func (u *viaOrgUpstream) seen() (int, []string, []string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hits, append([]string(nil), u.auth...), append([]string(nil), u.hosts...)
}

// viaOrgAPI wires srv for via_org: a run config key, the org secret, a broker answering the api_key
// grant, the upstream's CA, and every dial landing on the upstream.
func viaOrgAPI(t *testing.T, srv *Server, up *viaOrgUpstream, online *atomic.Bool) {
	t.Helper()
	srv.cfg.RunConfigKey = bytes.Repeat([]byte{7}, 32)
	srv.cfg.Secrets = getErrStore{val: []byte(viaOrgAPIValue)}
	rule := egress.InjectionRule{Host: viaOrgAPIHost, Header: "Authorization", Format: "Bearer %s", SecretName: "corp-secret"}
	srv.cfg.Broker = &fakeBroker{minted: broker.Minted{Kind: types.GrantAPIKey, JTI: "j", Injection: &rule}}
	srv.cfg.TrustedCAPEM = up.caPEM
	if online != nil {
		srv.cfg.RunnerOnline = func(uuid.UUID) bool { return online.Load() }
	}
	srv.viaOrg.dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(up.srv.URL, "https://"))
	}
	srv.viaOrg.lookupIP = func(string) ([]net.IP, error) { return []net.IP{net.ParseIP("93.184.216.34")}, nil }
}

// viaOrgGrantFor writes a grant of run and returns its id.
func viaOrgGrantFor(t *testing.T, st interface {
	CreateGrant(context.Context, types.CredentialGrant) (types.CredentialGrant, error)
}, runID uuid.UUID, kind types.GrantKind, d types.GrantDelivery,
) uuid.UUID {
	t.Helper()
	scope := mustJSON(map[string]any{"host": viaOrgAPIHost, "secret_name": "corp-secret", "header": "Authorization", "format": "Bearer %s"})
	if kind == types.GrantGitHubToken {
		scope = mustJSON(map[string]any{"repos": []string{"acme/app"}})
	}
	g, err := st.CreateGrant(context.Background(), types.CredentialGrant{ID: uuid.New(), RunID: runID, CreatedAt: time.Now().UTC(), Delivery: d,
		Spec: types.GrantSpec{Kind: kind, Scope: scope}})
	if err != nil {
		t.Fatal(err)
	}
	return g.ID
}

// storeViaOrgConfig stores run's laptop config, as local dispatch stores it, with these routes.
func storeViaOrgConfig(t *testing.T, srv *Server, runID uuid.UUID, routes []proxy.ViaOrgRoute, mutate ...func(*proxy.Config)) {
	t.Helper()
	caPEM, _ := hopServingTLS(t)
	cfg := proxy.Config{RunID: runID, ControlPlaneURL: "https://" + relayHost + ":8443", ControlPlaneCAPEM: caPEM, RunToken: "stored-token", Listen: ":3128",
		Policy: types.RunPolicySpec{AllowedDomains: []string{viaOrgAPIHost}}, MITMHosts: []string{viaOrgAPIHost}, ViaOrgRoutes: routes}
	for _, m := range mutate {
		m(&cfg)
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.storeRunProxyConfig(context.Background(), runID, b); err != nil {
		t.Fatal(err)
	}
}

type viaOrgAPIFixture struct {
	*bindingFixture
	up     *viaOrgUpstream
	grant  uuid.UUID
	online atomic.Bool
}

func newViaOrgAPIFixture(t *testing.T) *viaOrgAPIFixture {
	t.Helper()
	f := &viaOrgAPIFixture{bindingFixture: newBindingFixture(t), up: newViaOrgUpstream(t)}
	f.online.Store(true)
	viaOrgAPI(t, f.srv, f.up, &f.online)
	f.grant = viaOrgGrantFor(t, f.st, f.run.ID, types.GrantAPIKey, types.GrantDeliveryViaOrg)
	storeViaOrgConfig(t, f.srv, f.run.ID, []proxy.ViaOrgRoute{{GrantID: f.grant, Host: viaOrgAPIHost}})
	return f
}

// forward sends what a runner's proxy sends for one sandbox request, relayed by runner.
func (f *viaOrgAPIFixture) forward(t *testing.T, runner, runID, grant uuid.UUID, rest, authority string) *httptest.ResponseRecorder {
	t.Helper()
	return f.forwardBody(t, runner, runID, grant, rest, authority, `{"q":1}`)
}

func (f *viaOrgAPIFixture) forwardBody(t *testing.T, runner, runID, grant uuid.UUID, rest, authority, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, proxy.ViaOrgPathPrefix+grant.String()+rest, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+runToken(t, f.srv, runID))
	req.Header.Set(proxy.ViaOrgAuthorityHeader, authority)
	req.Header.Set(proxy.ViaOrgAuthorizationHeader, "Bearer sandbox-own")
	h := f.srv.InternalHandler()
	if runner != uuid.Nil {
		req = req.WithContext(relayConnContext(req.Context(), relayedConn{runner: runner}))
		h = f.srv.relayRoutes(h)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func setViaOrgSeams(t *testing.T, idle, watch time.Duration, byteCap int64) {
	t.Helper()
	oi, ow, ob := viaOrgIdle, viaOrgWatchEvery, viaOrgByteCap
	viaOrgIdle, viaOrgWatchEvery, viaOrgByteCap = idle, watch, byteCap
	t.Cleanup(func() { viaOrgIdle, viaOrgWatchEvery, viaOrgByteCap = oi, ow, ob })
}

// TestViaOrg_RefusesWrongGrantRunOrDelivery: only a grant of this run, recorded via_org and dispatched
// via_org, is served; everything else is delivery_not_via_org, audited with the relaying runner.
func TestViaOrg_RefusesWrongGrantRunOrDelivery(t *testing.T) {
	f := newViaOrgAPIFixture(t)
	R := f.rn.ID
	notDispatched := viaOrgGrantFor(t, f.st, f.run.ID, types.GrantAPIKey, types.GrantDeliveryViaOrg)
	for name, grant := range map[string]uuid.UUID{
		"unknown grant":          uuid.New(),
		"another run's grant":    viaOrgGrantFor(t, f.st, f.other.ID, types.GrantAPIKey, types.GrantDeliveryViaOrg),
		"an own grant":           viaOrgGrantFor(t, f.st, f.run.ID, types.GrantAPIKey, types.GrantDeliveryOwn),
		"no recorded delivery":   viaOrgGrantFor(t, f.st, f.run.ID, types.GrantAPIKey, types.GrantDeliveryNone),
		"a resident grant":       viaOrgGrantFor(t, f.st, f.run.ID, types.GrantAPIKey, types.GrantDeliveryResident),
		"not dispatched via_org": notDispatched,
	} {
		if w := f.forward(t, R, f.run.ID, grant, "/v1/x", viaOrgAPIHost); w.Code != http.StatusForbidden || reasonOf(t, w) != string(placement.ReasonDeliveryNotViaOrg) {
			t.Errorf("%s = %d %s, want 403 delivery_not_via_org", name, w.Code, w.Body)
		}
	}
	// The run's own via_org grant, relayed by another runner or not relayed at all, never reaches it.
	if w := f.forward(t, uuid.New(), f.run.ID, f.grant, "/v1/x", viaOrgAPIHost); w.Code != http.StatusForbidden {
		t.Errorf("another runner's relay = %d %s", w.Code, w.Body)
	}
	if w := f.forward(t, uuid.Nil, f.run.ID, f.grant, "/v1/x", viaOrgAPIHost); w.Code != http.StatusForbidden || reasonOf(t, w) != "relay_required" {
		t.Errorf("unrelayed = %d %s", w.Code, w.Body)
	}
	if hits, _, _ := f.up.seen(); hits != 0 {
		t.Fatalf("a refused request reached the destination %d times", hits)
	}
	if !f.deniedWith(t, &f.run.ID, "", string(placement.ReasonDeliveryNotViaOrg), R.String()) {
		t.Error("no authz.denied delivery_not_via_org row naming the runner")
	}
	// The grant itself is served.
	if w := f.forward(t, R, f.run.ID, f.grant, "/v1/x", viaOrgAPIHost); w.Code != http.StatusOK || w.Body.String() != "upstream-ok" {
		t.Fatalf("the run's via_org grant = %d %s", w.Code, w.Body)
	}
}

// TestViaOrg_DestinationFromTheRuleNeverTheRequest: the organisation dials the grant's own host; a
// request naming another authority or port, or a broker grant asked for anything but its route, is
// via_org_destination_refused and nothing is dialed.
func TestViaOrg_DestinationFromTheRuleNeverTheRequest(t *testing.T) {
	f := newViaOrgAPIFixture(t)
	R := f.rn.ID
	for _, authority := range []string{"evil.example", viaOrgAPIHost + ".evil.example", viaOrgAPIHost + ":8443", viaOrgAPIHost + ":80", ""} {
		if w := f.forward(t, R, f.run.ID, f.grant, "/v1/x", authority); w.Code != http.StatusForbidden || reasonOf(t, w) != "via_org_destination_refused" {
			t.Errorf("authority %q = %d %s, want via_org_destination_refused", authority, w.Code, w.Body)
		}
	}
	gh := viaOrgGrantFor(t, f.st, f.run.ID, types.GrantGitHubToken, types.GrantDeliveryViaOrg)
	if w := f.forward(t, R, f.run.ID, gh, "/repos/acme/app", "github.com"); w.Code != http.StatusForbidden || reasonOf(t, w) != "via_org_destination_refused" {
		t.Errorf("a GitHub grant outside /wardyn/gh/ = %d %s", w.Code, w.Body)
	}
	if hits, _, _ := f.up.seen(); hits != 0 {
		t.Fatalf("a refused destination was dialed %d times", hits)
	}
	if w := f.forward(t, R, f.run.ID, f.grant, "/v1/x", "API.Corp.Example.:443"); w.Code != http.StatusOK {
		t.Fatalf("the grant's own destination = %d %s", w.Code, w.Body)
	}
	if _, auth, hosts := f.up.seen(); len(hosts) != 1 || hosts[0] != viaOrgAPIHost || auth[0] != "Bearer "+viaOrgAPIValue {
		t.Fatalf("upstream saw host %v auth %v, want the rule's host and the org credential", hosts, auth)
	}
}

// TestViaOrg_GitHubBrokerThroughTheOrg: a via_org GitHub grant's /wardyn/gh/ request is served by the
// org's git broker, which mints the installation token through the org's own mint route and presents
// it to github.com itself.
func TestViaOrg_GitHubBrokerThroughTheOrg(t *testing.T) {
	f := newViaOrgAPIFixture(t)
	f.srv.cfg.Broker = &fakeBroker{minted: broker.Minted{Kind: types.GrantGitHubToken, Token: "gh-inst-token-7b1e", JTI: "j", ExpiresAt: time.Now().Add(time.Hour)}}
	gh := viaOrgGrantFor(t, f.st, f.run.ID, types.GrantGitHubToken, types.GrantDeliveryViaOrg)
	storeViaOrgConfig(t, f.srv, f.run.ID, []proxy.ViaOrgRoute{{GrantID: gh, Host: "github.com"}},
		func(c *proxy.Config) { c.GitGrants = map[string]uuid.UUID{"acme/app": gh}; c.MITMHosts = nil })
	req := httptest.NewRequest(http.MethodGet, proxy.ViaOrgPathPrefix+gh.String()+"/wardyn/gh/acme/app.git/info/refs?service=git-upload-pack", nil)
	req.Header.Set("Authorization", "Bearer "+runToken(t, f.srv, f.run.ID))
	req.Header.Set(proxy.ViaOrgAuthorizationHeader, "Basic c2FuZGJveDpzYW5kYm94")
	req = req.WithContext(relayConnContext(req.Context(), relayedConn{runner: f.rn.ID}))
	w := httptest.NewRecorder()
	f.srv.relayRoutes(f.srv.InternalHandler()).ServeHTTP(w, req)
	if w.Code != http.StatusOK || w.Body.String() != "upstream-ok" {
		t.Fatalf("via_org git broker = %d %s", w.Code, w.Body)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:gh-inst-token-7b1e"))
	if _, auth, hosts := f.up.seen(); len(auth) != 1 || auth[0] != want || hosts[0] != "github.com" {
		t.Fatalf("github saw %v %v, want the org-minted installation token", auth, hosts)
	}
	if strings.Contains(w.Body.String(), "gh-inst-token-7b1e") {
		t.Fatal("the installation token came back toward the runner")
	}
}

// TestViaOrg_PerRunCaps: the 17th concurrent stream of a run is 429 via_org_cap_exceeded; a run past
// its byte total is refused; an idle stream is closed.
func TestViaOrg_PerRunCaps(t *testing.T) {
	setViaOrgSeams(t, viaOrgIdleTimeout, 20*time.Millisecond, viaOrgMaxBytes)
	f := newViaOrgAPIFixture(t)
	R := f.rn.ID
	f.up.hold = make(chan struct{})
	var wg sync.WaitGroup
	for range viaOrgMaxStreams {
		wg.Add(1)
		go func() { defer wg.Done(); f.forward(t, R, f.run.ID, f.grant, "/v1/stream", viaOrgAPIHost) }()
	}
	waitFor(t, "16 open streams", func() bool { return f.up.open.Load() == viaOrgMaxStreams })
	seventeenth := make(chan *httptest.ResponseRecorder, 1)
	go func() { seventeenth <- f.forward(t, R, f.run.ID, f.grant, "/v1/x", viaOrgAPIHost) }()
	select {
	case w := <-seventeenth:
		if w.Code != http.StatusTooManyRequests || reasonOf(t, w) != "via_org_cap_exceeded" {
			t.Fatalf("17th stream = %d %s, want 429 via_org_cap_exceeded", w.Code, w.Body)
		}
	case <-time.After(3 * time.Second):
		close(f.up.hold)
		t.Fatal("the 17th stream was admitted")
	}
	close(f.up.hold)
	wg.Wait()
	f.up.hold = nil
	if w := f.forward(t, R, f.run.ID, f.grant, "/v1/x", viaOrgAPIHost); w.Code != http.StatusOK {
		t.Fatalf("after the streams closed = %d %s", w.Code, w.Body)
	}

	// Bytes: a run past its total is refused.
	setViaOrgSeams(t, viaOrgIdleTimeout, 20*time.Millisecond, 8)
	g := newViaOrgAPIFixture(t)
	g.forward(t, g.rn.ID, g.run.ID, g.grant, "/v1/x", viaOrgAPIHost)
	if w := g.forward(t, g.rn.ID, g.run.ID, g.grant, "/v1/x", viaOrgAPIHost); w.Code != http.StatusTooManyRequests || reasonOf(t, w) != "via_org_cap_exceeded" {
		t.Fatalf("past the byte total = %d %s, want 429", w.Code, w.Body)
	}

	// The total is the run's: an idle release and a runner reconnect keep it spent.
	setViaOrgSeams(t, 150*time.Millisecond, 20*time.Millisecond, 8)
	k := newViaOrgAPIFixture(t)
	exhausted := func(stage string) {
		t.Helper()
		if w := k.forward(t, k.rn.ID, k.run.ID, k.grant, "/v1/x", viaOrgAPIHost); w.Code != http.StatusTooManyRequests || reasonOf(t, w) != "via_org_cap_exceeded" {
			t.Fatalf("%s = %d %s, want 429 via_org_cap_exceeded", stage, w.Code, w.Body)
		}
	}
	k.forward(t, k.rn.ID, k.run.ID, k.grant, "/v1/x", viaOrgAPIHost)
	exhausted("past the total")
	waitFor(t, "the idle release", func() bool { return viaOrgEngineReleased(k.srv, k.run.ID) })
	exhausted("after the idle release")
	k.srv.RecordRunnerDisconnected(context.Background(), k.rn.ID, nil)
	waitFor(t, "the disconnect release", func() bool { return viaOrgEngineReleased(k.srv, k.run.ID) })
	exhausted("after the runner reconnects")
	if hits, _, _ := k.up.seen(); hits != 1 {
		t.Fatalf("the spent run reached the destination %d times, want 1", hits)
	}

	// Idle: a stream that moves nothing is closed.
	setViaOrgSeams(t, 150*time.Millisecond, 20*time.Millisecond, viaOrgMaxBytes)
	h := newViaOrgAPIFixture(t)
	h.up.hold = make(chan struct{})
	defer close(h.up.hold)
	done := make(chan struct{})
	go func() { h.forward(t, h.rn.ID, h.run.ID, h.grant, "/v1/stream", viaOrgAPIHost); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("an idle stream was not closed")
	}
}

// TestViaOrg_StreamsCloseOnRunEndAndRunnerOffline: an open stream ends when the run ends and when its
// runner goes offline (the session's Disconnected, or the hub no longer reporting it online).
func TestViaOrg_StreamsCloseOnRunEndAndRunnerOffline(t *testing.T) {
	setViaOrgSeams(t, viaOrgIdleTimeout, 20*time.Millisecond, viaOrgMaxBytes)
	for _, tc := range []struct {
		name string
		end  func(f *viaOrgAPIFixture)
	}{
		{"run end", func(f *viaOrgAPIFixture) {
			if ok, err := f.st.UpdateRunStateIf(context.Background(), f.run.ID, f.run.State, types.RunKilled); err != nil || !ok {
				t.Fatalf("end the run: %v %v", ok, err)
			}
		}},
		{"runner disconnected", func(f *viaOrgAPIFixture) { f.srv.RecordRunnerDisconnected(context.Background(), f.rn.ID, nil) }},
		{"runner offline", func(f *viaOrgAPIFixture) { f.online.Store(false) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newViaOrgAPIFixture(t)
			f.up.hold = make(chan struct{})
			defer close(f.up.hold)
			done := make(chan struct{})
			go func() { f.forward(t, f.rn.ID, f.run.ID, f.grant, "/v1/stream", viaOrgAPIHost); close(done) }()
			waitFor(t, "an open stream", func() bool { return f.up.open.Load() == 1 })
			tc.end(f)
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("the stream outlived its run or runner")
			}
			waitFor(t, "the stream closed upstream", func() bool { return f.up.open.Load() == 0 })
		})
	}
}

// TestViaOrg_EndToEndThroughTheRelay: a runner's proxy reaches the organisation's via_org route over
// the runner's real session and H5's relay; the org injects its credential on its own TLS leg, the
// decision row is written org-side with data.relay, and no byte the runner side sees carries the value.
func TestViaOrg_EndToEndThroughTheRelay(t *testing.T) {
	f := newAdmissionFixture(t)
	up := newViaOrgUpstream(t)
	viaOrgAPI(t, f.srv, up, nil)
	f.srv.cfg.RunnerOnline = func(uuid.UUID) bool { return true }
	grant := viaOrgGrantFor(t, f.st, f.run.ID, types.GrantAPIKey, types.GrantDeliveryViaOrg)
	storeViaOrgConfig(t, f.srv, f.run.ID, []proxy.ViaOrgRoute{{GrantID: grant, Host: viaOrgAPIHost}})
	caPEM, serving := hopServingTLS(t)
	relaySrv := f.srv.ServeRelay(serving)
	t.Cleanup(func() { _ = relaySrv.Close() })
	s := f.connect(t, nil, f.liveRow())
	if !f.waitReady(s) {
		t.Fatalf("the runner was not admitted: %v", s.result())
	}
	s.mu.Lock()
	peer := s.peer
	s.mu.Unlock()

	seen := &lockedBuffer{}
	client := viaOrgRelayClient(t, peer, caPEM, seen)
	req, _ := http.NewRequest(http.MethodPost, "https://"+relayHost+":8443"+proxy.ViaOrgPathPrefix+grant.String()+"/v1/messages", strings.NewReader(`{"q":1}`))
	req.Header.Set("Authorization", "Bearer "+runToken(t, f.srv, f.run.ID))
	req.Header.Set(proxy.ViaOrgAuthorityHeader, viaOrgAPIHost)
	req.Header.Set(proxy.ViaOrgAuthorizationHeader, "Bearer sandbox-own")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "upstream-ok" {
		t.Fatalf("relayed via_org = %d %s", resp.StatusCode, body)
	}
	if _, auth, hosts := up.seen(); len(auth) != 1 || auth[0] != "Bearer "+viaOrgAPIValue || hosts[0] != viaOrgAPIHost {
		t.Fatalf("upstream saw %v %v, want the org credential on the rule's host", auth, hosts)
	}
	for k, vs := range resp.Header {
		if strings.Contains(k+strings.Join(vs, ","), viaOrgAPIValue) {
			t.Fatal("the response carried the credential back toward the runner")
		}
	}
	if strings.Contains(seen.String(), viaOrgAPIValue) || strings.Contains(string(body), viaOrgAPIValue) {
		t.Fatal("the credential value crossed the runner")
	}

	// The org-side decision rows flush with the engine; they carry the relaying runner.
	f.srv.endViaOrgRunner(f.rn.ID)
	waitFor(t, "an org-side decision row with data.relay", func() bool {
		ev, ok := relayAudit(f.srv, f.run.ID, "egress.allow")
		return ok && auditDataOf(ev)["relay"] == "runner:"+f.rn.ID.String()
	})
}

// viaOrgRelayClient is runnerRelayClient keeping both directions of every relayed byte.
func viaOrgRelayClient(t *testing.T, peer *runnerwire.Peer, caPEM string, seen io.Writer) *http.Client {
	t.Helper()
	tlsCfg, err := hoptls.ClientConfig(caPEM)
	if err != nil {
		t.Fatal(err)
	}
	tlsCfg.ServerName = relayHost
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		st, err := peer.Open(ctx, runnerwire.Open{Kind: runnerwire.KindRelay})
		if err != nil {
			return nil, err
		}
		proxySide, relaySide := net.Pipe()
		go func() {
			if _, err := io.Copy(io.MultiWriter(st, seen), relaySide); err == nil {
				_ = st.CloseWrite()
			} else {
				_ = st.Close()
			}
		}()
		go func() { _, _ = io.Copy(io.MultiWriter(relaySide, seen), st); _ = relaySide.Close() }()
		return proxySide, nil
	}
	tr := &http.Transport{DialContext: dial, TLSClientConfig: tlsCfg}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: 20 * time.Second}
}

func auditDataOf(ev types.AuditEvent) map[string]any {
	d := map[string]any{}
	_ = json.Unmarshal(ev.Data, &d)
	return d
}

// viaOrgEngineReleased reports whether the run holds no engine (or no state at all).
func viaOrgEngineReleased(s *Server, runID uuid.UUID) bool {
	s.viaOrg.mu.Lock()
	run, ok := s.viaOrg.runs[runID]
	s.viaOrg.mu.Unlock()
	if !ok {
		return true
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	return run.engine == nil
}

// A sandbox path is never a door of this server: a terminal run's via_org request is refused whatever
// path follows the grant id, by internalAuth and by the route itself, and nothing is dialed.
func TestViaOrg_TerminalRunRefusedWhateverThePath(t *testing.T) {
	f := newViaOrgAPIFixture(t)
	R := f.rn.ID
	if ok, err := f.st.UpdateRunStateIf(context.Background(), f.run.ID, f.run.State, types.RunKilled); err != nil || !ok {
		t.Fatalf("end the run: %v %v", ok, err)
	}
	for _, rest := range []string{"/internal/token/renew", "/api/v1/internal/token/renew", "/internal/recordings/x", "/v1/x"} {
		if w := f.forward(t, R, f.run.ID, f.grant, rest, viaOrgAPIHost); w.Code != http.StatusForbidden || reasonOf(t, w) != "run_terminal" {
			t.Errorf("%s on a killed run = %d %s, want 403 run_terminal", rest, w.Code, w.Body)
		}
	}
	// The route's own check, without internalAuth in front of it.
	rt := chi.NewRouter()
	rt.Route("/api/v1", func(r chi.Router) { f.srv.mountViaOrgRoute(r) })
	req := httptest.NewRequest(http.MethodPost, proxy.ViaOrgPathPrefix+f.grant.String()+"/v1/x", strings.NewReader(`{}`))
	req.Header.Set(proxy.ViaOrgAuthorityHeader, viaOrgAPIHost)
	req = req.WithContext(contextWithClaims(relayConnContext(req.Context(), relayedConn{runner: R}), &identity.Claims{RunID: f.run.ID}))
	w := httptest.NewRecorder()
	rt.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden || reasonOf(t, w) != "run_terminal" {
		t.Errorf("the route served a killed run past internalAuth: %d %s", w.Code, w.Body)
	}
	if hits, _, _ := f.up.seen(); hits != 0 {
		t.Fatalf("a killed run's request reached the destination %d times", hits)
	}
}

// The terminal-run gate reads the door, never the sandbox's path after a via_org grant id.
func TestInternalGateMatchesTheDoorNotTheSandboxPath(t *testing.T) {
	via := proxy.ViaOrgPathPrefix + uuid.NewString()
	for _, tc := range []struct {
		path     string
		segments []string
		want     bool
	}{
		{"/api/v1/internal/token/renew", internalSelfGatedRoutes, true},
		{"/api/v1/internal/recordings/" + uuid.NewString(), terminalGraceRoutes, true},
		{via + "/internal/token/renew", internalSelfGatedRoutes, false},
		{via + "/internal/recordings/x", terminalGraceRoutes, false},
		{via + "/x/internal/sso-token/y", terminalGraceRoutes, false},
	} {
		if got := pathHasAny(tc.path, tc.segments); got != tc.want {
			t.Errorf("pathHasAny(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// The org engine inspects via_org traffic with the run's dispatch-time workspace-secret corpus, which
// the run's sealed org-side config keeps (the laptop copy never carries it).
func TestViaOrg_OrgInspectsWithTheDeclaredCorpus(t *testing.T) {
	f := newViaOrgAPIFixture(t)
	storeViaOrgConfig(t, f.srv, f.run.ID, []proxy.ViaOrgRoute{{GrantID: f.grant, Host: viaOrgAPIHost}}, func(c *proxy.Config) {
		c.Policy.LLMInspection = &types.LLMInspectionSpec{Mode: "block", DetectSecrets: true, InspectForwardEgress: true,
			WorkspaceSecretNames: []string{"ws-deploy-key"}, WorkspaceSecretValues: []string{"WS-DECLARED-VALUE-31c7"}}
	})
	R := f.rn.ID
	if w := f.forwardBody(t, R, f.run.ID, f.grant, "/v1/x", viaOrgAPIHost, `{"note":"WS-DECLARED-VALUE-31c7"}`); w.Code != http.StatusForbidden {
		t.Fatalf("a declared workspace secret crossed the org egress: %d %s", w.Code, w.Body)
	}
	if hits, _, _ := f.up.seen(); hits != 0 {
		t.Fatalf("the refused body reached the destination %d times", hits)
	}
	if w := f.forwardBody(t, R, f.run.ID, f.grant, "/v1/x", viaOrgAPIHost, `{"note":"nothing secret"}`); w.Code != http.StatusOK {
		t.Fatalf("an ordinary body = %d %s", w.Code, w.Body)
	}
}

// A request that took the run's engine just before a non-final release (idle, or the runner away)
// is never served through the retired engine: it takes the live replacement, and the org-side
// decision row of its traffic is written.
func TestViaOrg_RetiredEngineIsNeverServed(t *testing.T) {
	f := newViaOrgAPIFixture(t)
	R := f.rn.ID
	acquired, proceed := make(chan struct{}), make(chan struct{})
	var once sync.Once
	viaOrgAfterEngine = func() { once.Do(func() { close(acquired); <-proceed }) }
	t.Cleanup(func() { viaOrgAfterEngine = nil })
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- f.forward(t, R, f.run.ID, f.grant, "/v1/x", viaOrgAPIHost) }()
	<-acquired
	f.srv.viaOrg.mu.Lock()
	run := f.srv.viaOrg.runs[f.run.ID]
	f.srv.viaOrg.mu.Unlock()
	run.mu.Lock()
	old := run.engine
	run.mu.Unlock()
	f.srv.releaseViaOrgRun(run, false, false)
	waitFor(t, "the retired engine closes", func() bool { return old.engine.Closed() })
	close(proceed)
	w := <-done
	if w.Code != http.StatusOK || w.Body.String() != "upstream-ok" {
		t.Fatalf("a request that took a retired engine = %d %s, want the live replacement's answer", w.Code, w.Body)
	}
	run.mu.Lock()
	cur := run.engine
	run.mu.Unlock()
	if cur == nil || cur == old {
		t.Fatal("the request was not served by a live replacement engine")
	}
	if _, auth, _ := f.up.seen(); len(auth) != 1 || auth[0] != "Bearer "+viaOrgAPIValue {
		t.Fatalf("upstream saw %v, want one request with the org credential", auth)
	}
	f.srv.endViaOrgRunner(R)
	waitFor(t, "the request's org-side decision row", func() bool {
		ev, ok := relayAudit(f.srv, f.run.ID, "egress.allow")
		return ok && auditDataOf(ev)["relay"] == "runner:"+R.String()
	})
}

// blockingWriter holds the first response write until released: a producer still in flight
// whatever its context says.
type blockingWriter struct {
	*httptest.ResponseRecorder
	writing chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingWriter) Write(p []byte) (int, error) {
	b.once.Do(func() { close(b.writing); <-b.release })
	return b.ResponseRecorder.Write(p)
}

// A retired engine closes only after every request admitted to it has returned: its decision sink
// and masking corpus stay with its last producer.
func TestViaOrg_RetiredEngineOutlivesItsProducers(t *testing.T) {
	f := newViaOrgAPIFixture(t)
	R := f.rn.ID
	req := httptest.NewRequest(http.MethodPost, proxy.ViaOrgPathPrefix+f.grant.String()+"/v1/x", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+runToken(t, f.srv, f.run.ID))
	req.Header.Set(proxy.ViaOrgAuthorityHeader, viaOrgAPIHost)
	req = req.WithContext(relayConnContext(req.Context(), relayedConn{runner: R}))
	bw := &blockingWriter{ResponseRecorder: httptest.NewRecorder(), writing: make(chan struct{}), release: make(chan struct{})}
	done := make(chan struct{})
	go func() { f.srv.relayRoutes(f.srv.InternalHandler()).ServeHTTP(bw, req); close(done) }()
	<-bw.writing
	f.srv.viaOrg.mu.Lock()
	run := f.srv.viaOrg.runs[f.run.ID]
	f.srv.viaOrg.mu.Unlock()
	run.mu.Lock()
	old := run.engine
	run.mu.Unlock()
	f.srv.releaseViaOrgRun(run, false, false)
	time.Sleep(200 * time.Millisecond)
	if old.engine.Closed() {
		t.Fatal("the retired engine closed under a request still in flight")
	}
	close(bw.release)
	<-done
	waitFor(t, "the retired engine closes after its last producer", func() bool { return old.engine.Closed() })
}

// A peer that stops reading cannot keep a retired engine open: the release cancels the stream, and
// the blocked write to that peer returns, so the stream ends and the engine closes. The peer is a
// runner's real relay stream, so the write deadline has to take effect on the relay connection itself.
func TestViaOrg_StalledPeerDoesNotPinARetiredEngine(t *testing.T) {
	f := newAdmissionFixture(t)
	up := newViaOrgUpstream(t)
	up.big = 64 << 20
	viaOrgAPI(t, f.srv, up, nil)
	f.srv.cfg.RunnerOnline = func(uuid.UUID) bool { return true }
	grant := viaOrgGrantFor(t, f.st, f.run.ID, types.GrantAPIKey, types.GrantDeliveryViaOrg)
	storeViaOrgConfig(t, f.srv, f.run.ID, []proxy.ViaOrgRoute{{GrantID: grant, Host: viaOrgAPIHost}})
	caPEM, serving := hopServingTLS(t)
	relaySrv := f.srv.ServeRelay(serving)
	t.Cleanup(func() { _ = relaySrv.Close() })
	s := f.connect(t, nil, f.liveRow())
	if !f.waitReady(s) {
		t.Fatalf("the runner was not admitted: %v", s.result())
	}
	s.mu.Lock()
	peer := s.peer
	s.mu.Unlock()

	client := viaOrgRelayClient(t, peer, caPEM, io.Discard)
	req, _ := http.NewRequest(http.MethodPost, "https://"+relayHost+":8443"+proxy.ViaOrgPathPrefix+grant.String()+"/v1/x", strings.NewReader(`{"q":1}`))
	req.Header.Set("Authorization", "Bearer "+runToken(t, f.srv, f.run.ID))
	req.Header.Set(proxy.ViaOrgAuthorityHeader, viaOrgAPIHost)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	// The body is never read: the relay fills and the org's write blocks.
	t.Cleanup(func() { _ = resp.Body.Close() })
	var run *viaOrgRun
	waitFor(t, "the stream is open and its response has started", func() bool {
		f.srv.viaOrg.mu.Lock()
		run = f.srv.viaOrg.runs[f.run.ID]
		f.srv.viaOrg.mu.Unlock()
		return run != nil && run.bytes.Load() > 64<<10
	})
	// Blocked: the bytes written stop moving.
	for last := int64(-1); last != run.bytes.Load(); {
		last = run.bytes.Load()
		time.Sleep(300 * time.Millisecond)
	}
	run.mu.Lock()
	old := run.engine
	run.mu.Unlock()
	f.srv.releaseViaOrgRun(run, false, false)
	waitFor(t, "the retired engine closes although its peer stopped reading", func() bool { return old.engine.Closed() })
}
