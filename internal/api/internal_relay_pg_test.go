// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/hoptls"
	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const relayHost = "wardynd.internal"

// relayDecision is a minimal decision log, the call every proxy makes.
const relayDecision = `{"request":{"host":"example.com","port":443,"method":"GET","path":"/"},"decision":"allow","rule_source":"policy"}`

// runToken mints the run's own internal token on srv's identity provider.
func runToken(t *testing.T, srv *Server, runID uuid.UUID) string {
	t.Helper()
	id, err := srv.cfg.Identity.MintRunIdentity(context.Background(), runID, "admission-owner", "", internalAudience, false)
	if err != nil {
		t.Fatal(err)
	}
	return id.Token
}

// hopServingTLS is a real internal CA and a serving config for relayHost: the
// pair a proxy pins and the internal listener serves.
func hopServingTLS(t *testing.T) (caPEM string, server *tls.Config) {
	t.Helper()
	now := time.Now()
	blob, err := hoptls.NewCA(now)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := hoptls.ParseCA(blob)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := ca.ServingCert(relayHost, now)
	if err != nil {
		t.Fatal(err)
	}
	return string(ca.CertPEM), &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13}
}

// runnerRelayClient is a proxy's HTTP client whose connections go through a
// runner-side relay on peer: each dial opens one relay stream and copies bytes
// both ways, keeping a copy of everything sent toward the org in seen.
func runnerRelayClient(t *testing.T, peer *runnerwire.Peer, caPEM string, seen io.Writer) *http.Client {
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
		go func() { _, _ = io.Copy(relaySide, st); _ = relaySide.Close() }()
		return proxySide, nil
	}
	tr := &http.Transport{DialContext: dial, TLSClientConfig: tlsCfg}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: 10 * time.Second}
}

// relayAudit returns the first audit row of action for runID.
func relayAudit(srv *Server, runID uuid.UUID, action string) (types.AuditEvent, bool) {
	a := srv.cfg.Audit.(*syncAudit)
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, ev := range a.events {
		if ev.Action == action && ev.RunID != nil && *ev.RunID == runID {
			return ev, true
		}
	}
	return types.AuditEvent{}, false
}

// TestRelay_EndToEndOverLoopback: a local run's proxy reaches the org's
// internal surface through its runner's relay, over the runner's real admitted
// session, with TLS from the proxy to the org's relay listener. The decision
// lands, its audit row names the relaying runner, and the runner forwarded
// ciphertext only: the run token never crossed it in clear.
func TestRelay_EndToEndOverLoopback(t *testing.T) {
	f := newAdmissionFixture(t)
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
	client := runnerRelayClient(t, peer, caPEM, seen)
	token := runToken(t, f.srv, f.run.ID)
	req, _ := http.NewRequest(http.MethodPost, "https://"+relayHost+":8443/api/v1/internal/decisions", strings.NewReader(relayDecision))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("relayed decision: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode >= 300 {
		t.Fatalf("relayed decision = %d %s", resp.StatusCode, body)
	}
	ev, ok := relayAudit(f.srv, f.run.ID, "egress.allow")
	if !ok {
		t.Fatal("the relayed decision wrote no audit row")
	}
	if got := auditData(t, ev)["relay"]; got != "runner:"+f.rn.ID.String() {
		t.Fatalf("data.relay = %v, want runner:%s", got, f.rn.ID)
	}
	forwarded := seen.String()
	if forwarded == "" {
		t.Fatal("nothing crossed the runner")
	}
	if strings.Contains(forwarded, token) || strings.Contains(forwarded, "Bearer") || strings.Contains(forwarded, "/api/v1/internal") {
		t.Fatal("the runner forwarded the request in clear")
	}
}

// getRunCounter counts the run reads one request makes.
type getRunCounter struct {
	store.PG
	n atomic.Int32
}

func (c *getRunCounter) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	c.n.Add(1)
	return c.PG.GetRun(ctx, id)
}

// bindingFixture is a Server on the admission fixture's database (runner R
// with one RUNNING local run) reading runs through a counter, plus a local run
// on another runner R2 and a remote run.
type bindingFixture struct {
	*admissionFixture
	srv          *Server
	runs         *getRunCounter
	other, cloud types.AgentRun
}

func newBindingFixture(t *testing.T) *bindingFixture {
	t.Helper()
	f := newAdmissionFixture(t)
	b := &bindingFixture{admissionFixture: f, runs: &getRunCounter{PG: f.st}}
	cfg := baseTestConfig(newHarness(t), b.runs)
	cfg.Audit = &syncAudit{}
	cfg.Secrets = getErrStore{getErr: secretstore.ErrNotFound}
	cfg.Broker = &fakeBroker{}
	cfg.OIDC = &oidc.Authenticator{}
	b.srv = New(cfg)
	id2 := uuid.New()
	r2, err := f.st.CreateRunner(f.ctx, types.Runner{ID: id2, Owner: "admission-owner", Name: "laptop-2", PublicKey: make([]byte, 32), KeyFingerprint: id2.String(),
		Version: "0.9.0", OrgURLSHA256: federation.OrgURLSHA256(admissionOrgURL), Limits: roomyRunnerLimits()})
	if err != nil {
		t.Fatal(err)
	}
	b.other, b.cloud = newFinalizeRun(), newFinalizeRun()
	b.other.CreatedBy, b.other.Placement, b.other.RunnerID = "admission-owner", types.PlacementLocal, &r2.ID
	b.cloud.CreatedBy, b.cloud.Placement = "admission-owner", types.PlacementRemote
	for _, run := range []*types.AgentRun{&b.other, &b.cloud} {
		if *run, err = f.st.CreateRun(f.ctx, *run); err != nil {
			t.Fatal(err)
		}
	}
	return b
}

// do sends one internal request with runID's token, relayed by runner when it
// is not Nil (the relay server's own ConnContext and route filter), else
// straight to the internal handler.
func (b *bindingFixture) do(t *testing.T, runner uuid.UUID, method, path string, runID uuid.UUID, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+runToken(t, b.srv, runID))
	h := b.srv.InternalHandler()
	if runner != uuid.Nil {
		req = req.WithContext(relayConnContext(req.Context(), relayedConn{runner: runner}))
		h = b.srv.relayRoutes(h)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// deniedWith reports whether an authz.denied row for runID (any run when nil)
// and target (any when "") carries reason and runnerID.
func (b *bindingFixture) deniedWith(t *testing.T, runID *uuid.UUID, target, reason, runnerID string) bool {
	t.Helper()
	a := b.srv.cfg.Audit.(*syncAudit)
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, ev := range a.events {
		if ev.Action != "authz.denied" || (runID != nil && (ev.RunID == nil || *ev.RunID != *runID)) || (target != "" && ev.Target != target) {
			continue
		}
		data := map[string]any{}
		_ = json.Unmarshal(ev.Data, &data)
		raw, _ := json.Marshal(data)
		if strings.Contains(string(raw), `"`+reason+`"`) && strings.Contains(string(raw), runnerID) {
			return true
		}
	}
	return false
}

// TestRelayBinding_Table is design §4.3's binding table, each row a real
// request through internalAuth: one run read per request.
func TestRelayBinding_Table(t *testing.T) {
	b := newBindingFixture(t)
	R := b.rn.ID
	for _, tc := range []struct {
		name   string
		runner uuid.UUID
		run    types.AgentRun
		want   int
		reason string
	}{
		{"relayed by R, local on R", R, b.run, http.StatusAccepted, ""},
		{"relayed by R, local on another runner", R, b.other, http.StatusForbidden, "relay_runner_mismatch"},
		{"relayed by R, remote", R, b.cloud, http.StatusForbidden, "relay_runner_mismatch"},
		{"not relayed, local", uuid.Nil, b.run, http.StatusForbidden, "relay_required"},
		{"not relayed, remote", uuid.Nil, b.cloud, http.StatusAccepted, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b.runs.n.Store(0)
			w := b.do(t, tc.runner, http.MethodPost, "/api/v1/internal/decisions", tc.run.ID, relayDecision)
			if w.Code != tc.want || (tc.reason != "" && reasonOf(t, w) != tc.reason) {
				t.Fatalf("= %d %s, want %d %q", w.Code, w.Body.String(), tc.want, tc.reason)
			}
			if n := b.runs.n.Load(); n != 1 {
				t.Fatalf("the request read the run %d times, want 1", n)
			}
			if tc.reason != "" && !b.deniedWith(t, &tc.run.ID, "", tc.reason, "runner_id") {
				t.Fatalf("no authz.denied %s row with runner_id", tc.reason)
			}
		})
	}
}

func TestRelay_PendingActionFences(t *testing.T) {
	b := newBindingFixture(t)
	if w := b.do(t, b.rn.ID, http.MethodPost, "/api/v1/internal/decisions", b.run.ID, relayDecision); w.Code != http.StatusAccepted {
		t.Fatalf("before the action: %d %s", w.Code, w.Body.String())
	}
	if _, _, err := b.st.QueueRunnerTeardown(b.ctx, b.rn.ID, types.RunnerActionKill, b.run.SandboxRef); err != nil {
		t.Fatal(err)
	}
	w := b.do(t, b.rn.ID, http.MethodPost, "/api/v1/internal/decisions", b.run.ID, relayDecision)
	if w.Code != http.StatusConflict || reasonOf(t, w) != "runner_action_pending" {
		t.Fatalf("with an open action: %d %s", w.Code, w.Body.String())
	}
	if !b.deniedWith(t, &b.run.ID, "", "runner_action_pending", b.rn.ID.String()) {
		t.Fatal("no authz.denied runner_action_pending row naming the runner")
	}
}

func TestRelay_RefusesUnlistedAndTrustedOutputRoutes(t *testing.T) {
	b := newBindingFixture(t)
	R := b.rn.ID
	for _, tc := range []struct{ method, path string }{
		{http.MethodPut, "/api/v1/internal/scan-results/" + b.run.ID.String()},
		{http.MethodPut, "/api/v1/internal/sso-token/" + b.run.ID.String()},
		{http.MethodPost, "/api/v1/internal/groundtruth"},
		{http.MethodGet, "/api/v1/internal/not-a-route"},
		{http.MethodGet, "/api/v1/runs"},
		{http.MethodGet, "/healthz"},
		{http.MethodDelete, "/api/v1/internal/decisions"},
	} {
		w := b.do(t, R, tc.method, tc.path, b.run.ID, "{}")
		if w.Code != http.StatusForbidden || reasonOf(t, w) != "relay_route_refused" {
			t.Errorf("%s %s = %d %s, want 403 relay_route_refused", tc.method, tc.path, w.Code, w.Body.String())
		}
		if !b.deniedWith(t, nil, tc.path, "relay_route_refused", R.String()) {
			t.Errorf("%s %s: no authz.denied row with runner_id and the path", tc.method, tc.path)
		}
	}
}

// TestEveryInternalRouteIsClassifiedForRelay holds relayedInternalRoutes to
// the router: every /internal/ route is classified, and no row names a route
// that does not exist.
func TestEveryInternalRouteIsClassifiedForRelay(t *testing.T) {
	recordings, err := recording.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := New(Config{AdminToken: adminToken, Secrets: getErrStore{getErr: secretstore.ErrNotFound}, RecordingStore: recordings})
	registered := map[string]bool{}
	walk := func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if strings.Contains(route, "/internal/") {
			registered[method+" "+route] = true
		}
		return nil
	}
	if err := chi.Walk(srv.router, walk); err != nil {
		t.Fatal(err)
	}
	for route := range registered {
		if _, ok := relayedInternalRoutes[route]; !ok {
			t.Errorf("%s is not classified for the relay: add it to relayedInternalRoutes (allow or refuse)", route)
		}
	}
	for route := range relayedInternalRoutes {
		if !registered[route] {
			t.Errorf("relayedInternalRoutes names %s, which no longer exists", route)
		}
	}
}

// TestRelay_InjectionAndMintServeOnlyOwn: through a relay, only a grant
// delivered `own` is resolved or minted, and every grant of a local run is
// persisted `own` (a remote run's carry no delivery).
func TestRelay_InjectionAndMintServeOnlyOwn(t *testing.T) {
	b := newBindingFixture(t)
	grant := func(d types.GrantDelivery) uuid.UUID {
		g, err := b.st.CreateGrant(b.ctx, types.CredentialGrant{ID: uuid.New(), RunID: b.run.ID, CreatedAt: time.Now().UTC(), Delivery: d,
			Spec: types.GrantSpec{Kind: types.GrantAPIKey, Scope: mustJSON(map[string]any{"host": "api.example", "secret_name": "k"})}})
		if err != nil {
			t.Fatal(err)
		}
		return g.ID
	}
	for _, tc := range []struct {
		delivery types.GrantDelivery
		reason   string
	}{
		{types.GrantDeliveryViaOrg, "delivery_via_org"},
		{types.GrantDeliveryResident, "delivery_runner_resident"},
		{types.GrantDeliveryNone, "relay_route_refused"},
		{types.GrantDeliveryOwn, ""},
	} {
		id := grant(tc.delivery)
		inj := b.do(t, b.rn.ID, http.MethodGet, "/api/v1/internal/injection/"+id.String(), b.run.ID, "")
		mint := b.do(t, b.rn.ID, http.MethodPost, "/api/v1/internal/credentials/mint", b.run.ID, `{"grant_id":"`+id.String()+`"}`)
		for name, w := range map[string]*httptest.ResponseRecorder{"injection": inj, "mint": mint} {
			if tc.reason == "" {
				if r := reasonOf(t, w); strings.HasPrefix(r, "delivery_") || r == "relay_route_refused" {
					t.Errorf("%s of an own grant was refused by the delivery rule: %d %s", name, w.Code, w.Body.String())
				}
				continue
			}
			if w.Code != http.StatusForbidden || reasonOf(t, w) != tc.reason {
				t.Errorf("%s of a %q grant = %d %s, want 403 %s", name, tc.delivery, w.Code, w.Body.String(), tc.reason)
			}
		}
	}
	if !b.deniedWith(t, &b.run.ID, "", "delivery_via_org", b.rn.ID.String()) {
		t.Error("no authz.denied delivery_via_org row naming the runner")
	}

	// The writer: a local run's grants are persisted own, a remote run's are not.
	f := newLocalOwnerFixture(t)
	f.putOwn(t, f.alice, "person-secret")
	spec := types.RunPolicySpec{EligibleGrants: localGrantKindsForOwner("person-secret")}
	for _, p := range []types.Placement{types.PlacementLocal, types.PlacementRemote} {
		runID := uuid.New()
		f.createRun(t, runID, "alice", p)
		if _, ok := f.h.srv.persistRunGrants(context.Background(), httptest.NewRecorder(), localMemberRequest("alice"), runID, time.Now().UTC(), spec, p, personalJudgement(storedPolicy(t, f.h.srv))); !ok {
			t.Fatalf("%s grants were refused", p)
		}
		rows, err := f.h.srv.cfg.Store.ListGrantsByRun(context.Background(), runID)
		if err != nil || len(rows) != len(spec.EligibleGrants) {
			t.Fatalf("%s: %d rows, %v", p, len(rows), err)
		}
		want := types.GrantDeliveryNone
		if p == types.PlacementLocal {
			want = types.GrantDeliveryOwn
		}
		for _, g := range rows {
			if g.Delivery != want {
				t.Errorf("%s %s grant persisted delivery %q, want %q", p, g.Spec.Kind, g.Delivery, want)
			}
		}
	}
}

// TestRelay_UnservedRelayIsRefused: with no internal TLS listener (ServeRelay
// never ran) a runner's relay stream is refused, not queued.
func TestRelay_UnservedRelayIsRefused(t *testing.T) {
	srv := New(Config{AdminToken: adminToken})
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	if err := srv.acceptRelay(uuid.NewString(), a); !errors.Is(err, errRelayNotServed) {
		t.Fatalf("an unserved relay was accepted: %v", err)
	}
}

// TestHA_RevokedRunnerOnAnotherReplicaIsToldRevoked: two replicas on one
// database. The runner's session lives on A; the revocation commits on B,
// whose Hub holds nothing to tell. A's session re-reads the stored row and is
// sent REVOKED, and A reads the runner offline.
func TestHA_RevokedRunnerOnAnotherReplicaIsToldRevoked(t *testing.T) {
	f := newAdmissionFixture(t)
	s := f.connect(t, nil, f.liveRow())
	if !f.waitReady(s) || !f.srv.RunnerOnline(f.rn.ID) {
		t.Fatalf("the runner was not admitted on A: %v", s.result())
	}
	cfg := baseTestConfig(newHarness(t), f.st)
	cfg.BaseCtx = f.ctx
	cfg.Runner = &fakeRunner{}
	cfg.Audit = &syncAudit{}
	cfg.RunnerOrgURL = admissionOrgURL
	cfg.RunnerAuditPrepare = f.srv.cfg.RunnerAuditPrepare
	replicaB := New(cfg)
	if _, _, err := replicaB.revokeRunner(f.ctx, f.rn.ID, types.ActorHuman, "security-admin", ""); err != nil {
		t.Fatalf("revoke on B: %v", err)
	}
	select {
	case <-s.done:
	case <-time.After(runnerwire.PingInterval + 10*time.Second):
		t.Fatal("A's session was never told the runner was revoked")
	}
	if !errors.Is(s.err, runnerwire.ErrRevoked) {
		t.Fatalf("A's session ended with %v, want REVOKED", s.err)
	}
	if f.srv.RunnerOnline(f.rn.ID) {
		t.Fatal("A still reads the revoked runner online")
	}
}
