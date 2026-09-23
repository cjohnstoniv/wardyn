// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// fakeDeviceStore is the in-memory store.DeviceStore behind these tests. It is
// EMBEDDED in authzStore, so TestAuthzMatrix walks the device routes with the
// capability present: a 401 there then comes from deviceAuth refusing the
// credential, never from the capability being absent. Chain verification is
// link-only — recomputing audit_row_hash is Postgres's job, proven by
// internal/store's PG tests.
type fakeDeviceStore struct {
	mu       sync.Mutex
	tokens   map[string]types.DeviceEnrolmentToken // raw enrolment token -> row
	devices  map[uuid.UUID]types.Device
	creds    map[string]uuid.UUID // raw device bearer -> device id
	ingested map[uuid.UUID][]types.FederatedAuditEvent
	// ingestErr, when set, is IngestDeviceAudit's answer: the store failures
	// the handler has to map.
	ingestErr error
}

func newFakeDeviceStore() *fakeDeviceStore {
	return &fakeDeviceStore{
		tokens:   map[string]types.DeviceEnrolmentToken{},
		devices:  map[uuid.UUID]types.Device{},
		creds:    map[string]uuid.UUID{},
		ingested: map[uuid.UUID][]types.FederatedAuditEvent{},
	}
}

var _ store.DeviceStore = (*authzStore)(nil)

func (f *fakeDeviceStore) MintEnrolmentToken(_ context.Context, token string, t types.DeviceEnrolmentToken) (types.DeviceEnrolmentToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, dup := f.tokens[token]; dup {
		return types.DeviceEnrolmentToken{}, store.ErrConflict
	}
	t.CreatedAt = time.Now().UTC()
	f.tokens[token] = t
	return t, nil
}

// ConsumeEnrolmentToken is consume-once under one lock, the in-memory twin of
// the PG store's conditional UPDATE.
func (f *fakeDeviceStore) ConsumeEnrolmentToken(_ context.Context, token string, now time.Time) (types.DeviceEnrolmentToken, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tokens[token]
	if !ok || t.ConsumedAt != nil || !t.ExpiresAt.After(now) {
		return types.DeviceEnrolmentToken{}, false, nil
	}
	t.ConsumedAt = &now
	f.tokens[token] = t
	return t, true, nil
}

func (f *fakeDeviceStore) CreateDevice(_ context.Context, d types.Device, raw string) (types.Device, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, dup := f.creds[raw]; dup {
		return types.Device{}, store.ErrConflict
	}
	sum := sha256.Sum256([]byte(raw))
	d.CredentialSHA256 = hex.EncodeToString(sum[:])
	d.CreatedAt = time.Now().UTC()
	f.devices[d.ID] = d
	f.creds[raw] = d.ID
	return d, nil
}

func (f *fakeDeviceStore) GetDeviceByRaw(_ context.Context, raw string) (types.Device, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.creds[raw]
	if d := f.devices[id]; ok && d.RevokedAt == nil {
		return d, nil
	}
	return types.Device{}, store.ErrNotFound
}

func (f *fakeDeviceStore) TouchDevice(_ context.Context, id uuid.UUID, now time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if d, ok := f.devices[id]; ok {
		d.LastSeenAt = &now
		f.devices[id] = d
	}
	return nil
}

func (f *fakeDeviceStore) ListDevices(context.Context) ([]types.Device, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.SortedFunc(maps.Values(f.devices), func(a, b types.Device) int {
		return b.CreatedAt.Compare(a.CreatedAt)
	}), nil
}

func (f *fakeDeviceStore) RevokeDevice(_ context.Context, id uuid.UUID, now time.Time) (types.Device, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.devices[id]
	if !ok || d.RevokedAt != nil {
		return types.Device{}, store.ErrNotFound
	}
	d.RevokedAt = &now
	f.devices[id] = d
	return d, nil
}

// IngestDeviceAudit mirrors the PG store's answers — ErrDeviceRevoked for a
// revoked device, the peer in place of the claimed source_ip — so the handler
// is tested against the store's contract.
func (f *fakeDeviceStore) IngestDeviceAudit(_ context.Context, id uuid.UUID, peer string, rows []types.FederatedAuditEvent) (store.DeviceIngestResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ingestErr != nil {
		return store.DeviceIngestResult{}, f.ingestErr
	}
	d, ok := f.devices[id]
	if !ok {
		return store.DeviceIngestResult{}, store.ErrNotFound
	}
	if d.RevokedAt != nil {
		return store.DeviceIngestResult{}, store.ErrDeviceRevoked
	}
	start := 0
	for start < len(rows) && rows[start].Seq <= d.LastSeq {
		start++
	}
	fresh := rows[start:]
	if len(fresh) == 0 {
		return store.DeviceIngestResult{}, nil
	}
	reset := false
	if fresh[0].PrevHash == "" {
		reset = d.LastRowHash != ""
	} else if fresh[0].PrevHash != d.LastRowHash {
		return store.DeviceIngestResult{}, store.ErrConflict
	}
	for i := 1; i < len(fresh); i++ {
		if fresh[i].PrevHash != fresh[i-1].RowHash {
			return store.DeviceIngestResult{}, store.ErrConflict
		}
	}
	for _, r := range fresh {
		r.SourceIP = peer
		f.ingested[id] = append(f.ingested[id], r)
	}
	d.LastSeq, d.LastRowHash = fresh[len(fresh)-1].Seq, fresh[len(fresh)-1].RowHash
	f.devices[id] = d
	return store.DeviceIngestResult{Accepted: len(fresh), Reset: reset}, nil
}

func (f *fakeDeviceStore) ingestedFor(id uuid.UUID) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.ingested[id])
}

// The laptop-side half of the capability; the org routes never call it.
func (f *fakeDeviceStore) ListAuditEventsAfterSeq(context.Context, int64, int) ([]types.FederatedAuditEvent, error) {
	return nil, nil
}
func (f *fakeDeviceStore) GetFederationCursor(context.Context) (int64, error) { return 0, nil }
func (f *fakeDeviceStore) SetFederationCursor(context.Context, int64) error   { return nil }

// storeWithoutDevices hides the capability: embedding the Store interface
// exposes Store's methods and nothing else, which is exactly the shape of a
// test double or a future non-Postgres store.
type storeWithoutDevices struct{ store.Store }

// newDeviceTestServer is a server over authzStore (device capability present)
// with a race-safe audit recorder — the concurrent-enrolment test records from
// many goroutines at once.
func newDeviceTestServer(t *testing.T, withOIDC bool) (*Server, *authzStore, *safeRecorder) {
	t.Helper()
	ast := newAuthzStore()
	rec := &safeRecorder{}
	cfg := baseTestConfig(newHarness(t), ast)
	cfg.Audit = rec
	if withOIDC {
		cfg.OIDC = &oidc.Authenticator{}
	}
	return New(cfg), ast, rec
}

// doPeer is do() from a chosen TCP peer — the enrolment limiter keys on it.
func doPeer(t *testing.T, srv *Server, method, path, bearer, body, peer string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	r.RemoteAddr = peer
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	return w
}

// mintEnrolmentToken mints through the real admin route and returns the raw token.
func mintEnrolmentToken(t *testing.T, srv *Server, name string) string {
	t.Helper()
	w := do(t, srv, http.MethodPost, "/api/v1/admin/devices/enrolment-tokens", adminToken, `{"name":`+string(mustJSON(name))+`}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("mint enrolment token: %d %s", w.Code, w.Body.String())
	}
	var tok types.DeviceEnrolmentToken
	if err := json.Unmarshal(w.Body.Bytes(), &tok); err != nil {
		t.Fatal(err)
	}
	return tok.Token
}

// enrolTestDevice runs the whole first-boot exchange through the real routes:
// an admin mints, the laptop enrols. It returns the device and its bearer.
func enrolTestDevice(t *testing.T, srv *Server, name string) (uuid.UUID, string) {
	t.Helper()
	tok := mintEnrolmentToken(t, srv, name)
	w := doPeer(t, srv, http.MethodPost, "/api/v1/devices/enrol", "", `{"token":"`+tok+`"}`, "203.0.113.10:4000")
	if w.Code != http.StatusCreated {
		t.Fatalf("enrol: %d %s", w.Code, w.Body.String())
	}
	var got deviceEnrolResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got.Token, deviceTokenPrefix) || got.DeviceID == uuid.Nil {
		t.Fatalf("enrol response = %+v, want a %s bearer and a device id", got, deviceTokenPrefix)
	}
	return got.DeviceID, got.Token
}

// chainRows returns n federated rows numbered from first, linked from prev.
func chainRows(first int64, n int, prev string) []types.FederatedAuditEvent {
	var rows []types.FederatedAuditEvent
	for i := range n {
		seq := first + int64(i)
		hash := fmt.Sprintf("h%d", seq)
		rows = append(rows, types.FederatedAuditEvent{Seq: seq, AuditEvent: types.AuditEvent{
			ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorHuman, Actor: "dev@corp.example",
			Action: "run.create", Outcome: "success", PrevHash: prev, RowHash: hash,
		}})
		prev = hash
	}
	return rows
}

func ackedSeq(t *testing.T, w *httptest.ResponseRecorder) int64 {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var ack deviceAck
	if err := json.Unmarshal(w.Body.Bytes(), &ack); err != nil {
		t.Fatal(err)
	}
	return ack.AckedSeq
}

func auditRows(rec *safeRecorder, action, outcome string) []types.AuditEvent {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	var out []types.AuditEvent
	for _, ev := range rec.events {
		if ev.Action == action && ev.Outcome == outcome {
			out = append(out, ev)
		}
	}
	return out
}

func auditReason(t *testing.T, ev types.AuditEvent) string {
	t.Helper()
	var data map[string]any
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		t.Fatalf("audit data: %v", err)
	}
	reason, _ := data["reason"].(string)
	return reason
}

// the no-run invariant: a device token authenticates a daemon, never a person

// TestDevices_DeviceTokenIs401OnMe: the device credential must never reach the
// human routes — humanOrAdminAuth republishes a HUMAN through withHumanIdentity,
// and a device token that got there would be a person. Walked with and without
// OIDC, because humanOrAdminAuth is a different chain in each.
func TestDevices_DeviceTokenIs401OnMe(t *testing.T) {
	for _, withOIDC := range []bool{false, true} {
		t.Run(fmt.Sprintf("oidc=%v", withOIDC), func(t *testing.T) {
			srv, _, _ := newDeviceTestServer(t, withOIDC)
			id, tok := enrolTestDevice(t, srv, "alices-laptop")
			// Controls: the token IS live on its own routes, and /me answers a real
			// credential — so the 401 below is about WHICH credential, nothing else.
			ackedSeq(t, do(t, srv, http.MethodPost, "/api/v1/devices/"+id.String()+"/heartbeat", tok, ""))
			if w := do(t, srv, http.MethodGet, "/api/v1/me", adminToken, ""); w.Code != http.StatusOK {
				t.Fatalf("control: admin token on /me = %d, want 200: %s", w.Code, w.Body.String())
			}
			if w := do(t, srv, http.MethodGet, "/api/v1/me", tok, ""); w.Code != http.StatusUnauthorized {
				t.Fatalf("device token on GET /me = %d, want 401: %s", w.Code, w.Body.String())
			}
		})
	}
}

// TestDevices_DeviceTokenIs401OnCreateRun is the invariant the feature must not
// break: a device credential can never create a run. The body is one an admin
// really launches with, and the store is checked, not just the status.
func TestDevices_DeviceTokenIs401OnCreateRun(t *testing.T) {
	const body = `{"agent":"claude-code","repo":"org/repo","task":"escalate"}`
	for _, withOIDC := range []bool{false, true} {
		t.Run(fmt.Sprintf("oidc=%v", withOIDC), func(t *testing.T) {
			srv, st, _ := newDeviceTestServer(t, withOIDC)
			id, tok := enrolTestDevice(t, srv, "alices-laptop")
			ackedSeq(t, do(t, srv, http.MethodPost, "/api/v1/devices/"+id.String()+"/heartbeat", tok, ""))

			if w := do(t, srv, http.MethodPost, "/api/v1/runs", tok, body); w.Code != http.StatusUnauthorized {
				t.Fatalf("device token on POST /runs = %d, want 401: %s", w.Code, w.Body.String())
			}
			st.mu.Lock()
			created := len(st.runs)
			st.mu.Unlock()
			if created != 0 {
				t.Fatalf("a device token's POST /runs left %d run row(s) behind", created)
			}
			// Control: the SAME body from a real credential creates a run, so the
			// 401 above is the credential and nothing else.
			if w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, body); w.Code != http.StatusCreated {
				t.Fatalf("control: admin token on POST /runs = %d, want 201: %s", w.Code, w.Body.String())
			}
		})
	}
}

// TestDevices_DeviceTokenRefusedOnEveryHumanRoute widens the two named routes
// to the whole router: every route outside the device routes and the anonymous
// set answers a live device token 401. routeMatrix is the chi.Walk-checked
// table, so a route added later is swept without an edit here.
func TestDevices_DeviceTokenRefusedOnEveryHumanRoute(t *testing.T) {
	srv, st, _, _ := newAuthzMatrixServer(t)
	devID := uuid.New()
	tok := deviceTokenPrefix + "sweep"
	if _, err := st.CreateDevice(context.Background(), types.Device{ID: devID, Name: "sweep"}, tok); err != nil {
		t.Fatal(err)
	}
	swept := 0
	for key, rc := range routeMatrix {
		if rc.class == classAnonymous || rc.class == classDevice {
			continue
		}
		method, pattern, _ := strings.Cut(key, " ")
		swept++
		t.Run(key, func(t *testing.T) {
			w := do(t, srv, method, buildPath(pattern, uuid.NewString()), tok, bodyFor(method, rc))
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("device token on %s = %d, want 401: %s", key, w.Code, w.Body.String())
			}
		})
	}
	if swept < 90 {
		t.Fatalf("swept only %d routes — the matrix stopped enumerating", swept)
	}
}

// TestDevices_RevokedDeviceIngestIs401: revocation takes effect on the very next
// request, as the 401 the laptop's forwarder reads as "revoked".
func TestDevices_RevokedDeviceIngestIs401(t *testing.T) {
	srv, _, rec := newDeviceTestServer(t, true)
	id, tok := enrolTestDevice(t, srv, "alices-laptop")
	ingest := "/api/v1/devices/" + id.String() + "/audit"
	if got := ackedSeq(t, do(t, srv, http.MethodPost, ingest, tok, string(mustJSON(chainRows(1, 3, ""))))); got != 3 {
		t.Fatalf("acked_seq = %d, want 3", got)
	}
	if w := do(t, srv, http.MethodDelete, "/api/v1/admin/devices/"+id.String(), adminToken, ""); w.Code != http.StatusNoContent {
		t.Fatalf("revoke = %d, want 204: %s", w.Code, w.Body.String())
	}
	if w := do(t, srv, http.MethodPost, ingest, tok, string(mustJSON(chainRows(4, 1, "h3")))); w.Code != http.StatusUnauthorized {
		t.Fatalf("revoked device's next ingest = %d, want 401: %s", w.Code, w.Body.String())
	}
	if w := do(t, srv, http.MethodPost, "/api/v1/devices/"+id.String()+"/heartbeat", tok, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("revoked device's heartbeat = %d, want 401: %s", w.Code, w.Body.String())
	}
	// A second revoke is an act that did not happen: 404 and no second row.
	if w := do(t, srv, http.MethodDelete, "/api/v1/admin/devices/"+id.String(), adminToken, ""); w.Code != http.StatusNotFound {
		t.Fatalf("second revoke = %d, want 404: %s", w.Code, w.Body.String())
	}
	revokes := auditRows(rec, "device.revoke", "success")
	if len(revokes) != 1 || revokes[0].Target != id.String() || revokes[0].Actor != adminTokenPrincipal {
		t.Fatalf("device.revoke rows = %+v, want one naming the device, by the admin", revokes)
	}
}

// TestDevices_EnrolmentTokenDoubleConsumeFails: a token enrols one device, once,
// and a racing second redemption loses. The PG store's conditional UPDATE is
// raced for real in internal/store (TestPG_Devices_EnrolmentToken_ConcurrentConsume).
func TestDevices_EnrolmentTokenDoubleConsumeFails(t *testing.T) {
	srv, _, rec := newDeviceTestServer(t, false)
	tok := mintEnrolmentToken(t, srv, "alices-laptop")
	body := `{"token":"` + tok + `"}`
	if w := doPeer(t, srv, http.MethodPost, "/api/v1/devices/enrol", "", body, "203.0.113.1:1"); w.Code != http.StatusCreated {
		t.Fatalf("first enrol = %d, want 201: %s", w.Code, w.Body.String())
	}
	second := doPeer(t, srv, http.MethodPost, "/api/v1/devices/enrol", "", body, "203.0.113.2:1")
	if second.Code != http.StatusUnauthorized {
		t.Fatalf("second enrol with the same token = %d, want 401: %s", second.Code, second.Body.String())
	}
	// Spent, unknown and expired are one answer — the route is not an oracle.
	unknown := doPeer(t, srv, http.MethodPost, "/api/v1/devices/enrol", "", `{"token":"wde_never-minted"}`, "203.0.113.3:1")
	if unknown.Code != second.Code || unknown.Body.String() != second.Body.String() {
		t.Fatalf("spent token answered %d %q, unknown token %d %q — want byte-identical",
			second.Code, second.Body.String(), unknown.Code, unknown.Body.String())
	}
	if n := len(auditRows(rec, "device.enrol", "success")); n != 1 {
		t.Fatalf("device.enrol success rows = %d, want 1", n)
	}
	fails := auditRows(rec, "device.enrol", "failure")
	if len(fails) == 0 || auditReason(t, fails[0]) != "invalid_enrolment_token" || fails[0].SourceIP == "" {
		t.Fatalf("device.enrol failure rows = %+v, want reason invalid_enrolment_token with the peer", fails)
	}

	t.Run("concurrent", func(t *testing.T) {
		tok := mintEnrolmentToken(t, srv, "bobs-laptop")
		const racers = 16
		codes := make([]int, racers)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range racers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				// A distinct peer each, so the per-peer limiter is not what decides.
				codes[i] = doPeer(t, srv, http.MethodPost, "/api/v1/devices/enrol", "",
					`{"token":"`+tok+`"}`, fmt.Sprintf("198.51.100.%d:1", i+1)).Code
			}()
		}
		close(start)
		wg.Wait()
		won := 0
		for _, c := range codes {
			switch c {
			case http.StatusCreated:
				won++
			case http.StatusUnauthorized:
			default:
				t.Fatalf("racer got %d, want 201 or 401 (codes %v)", c, codes)
			}
		}
		if won != 1 {
			t.Fatalf("%d racers enrolled with one single-use token, want exactly 1 (codes %v)", won, codes)
		}
	})
}

// the rest of the contract

// TestDevices_PathIDMustMatchTheTokensDevice: a live token on any id but its
// own is 404 — never 403, which would confirm the other device exists.
func TestDevices_PathIDMustMatchTheTokensDevice(t *testing.T) {
	srv, _, _ := newDeviceTestServer(t, false)
	_, tokA := enrolTestDevice(t, srv, "a")
	idB, _ := enrolTestDevice(t, srv, "b")
	for _, route := range []string{"heartbeat", "audit"} {
		body := ""
		if route == "audit" {
			body = string(mustJSON(chainRows(1, 1, "")))
		}
		existing := do(t, srv, http.MethodPost, "/api/v1/devices/"+idB.String()+"/"+route, tokA, body)
		missing := do(t, srv, http.MethodPost, "/api/v1/devices/"+uuid.NewString()+"/"+route, tokA, body)
		garbage := do(t, srv, http.MethodPost, "/api/v1/devices/not-a-uuid/"+route, tokA, body)
		for name, w := range map[string]*httptest.ResponseRecorder{"another device": existing, "no device": missing, "not a uuid": garbage} {
			if w.Code != http.StatusNotFound || w.Body.String() != existing.Body.String() {
				t.Errorf("%s, %s: %d %q, want the byte-identical 404", route, name, w.Code, w.Body.String())
			}
		}
	}
}

// TestDevices_FailClosedWithoutTheStoreCapability: mounted unconditionally, the
// routes answer 501 (admin, enrol) or 401 (device routes) on a store without
// store.DeviceStore — never a silent success.
func TestDevices_FailClosedWithoutTheStoreCapability(t *testing.T) {
	cfg := baseTestConfig(newHarness(t), storeWithoutDevices{newAuthzStore()})
	if _, leaks := cfg.Store.(store.DeviceStore); leaks {
		t.Fatal("fixture still carries the device capability")
	}
	srv := New(cfg)
	id := uuid.NewString()
	if w := do(t, srv, http.MethodPost, "/api/v1/devices/enrol", "", `{"token":"wde_x"}`); strings.Contains(strings.ToLower(w.Body.String()), "postgres") {
		t.Errorf("anonymous enrol 501 names the backend: %s", w.Body.String())
	}
	for _, c := range []struct {
		method, path, bearer, body string
		want                       int
	}{
		{http.MethodPost, "/api/v1/admin/devices/enrolment-tokens", adminToken, `{"name":"x"}`, http.StatusNotImplemented},
		{http.MethodGet, "/api/v1/admin/devices", adminToken, "", http.StatusNotImplemented},
		{http.MethodDelete, "/api/v1/admin/devices/" + id, adminToken, "", http.StatusNotImplemented},
		{http.MethodPost, "/api/v1/devices/enrol", "", `{"token":"wde_x"}`, http.StatusNotImplemented},
		{http.MethodPost, "/api/v1/devices/" + id + "/audit", deviceTokenPrefix + "x", "[]", http.StatusUnauthorized},
		{http.MethodPost, "/api/v1/devices/" + id + "/heartbeat", deviceTokenPrefix + "x", "", http.StatusUnauthorized},
	} {
		if w := do(t, srv, c.method, c.path, c.bearer, c.body); w.Code != c.want {
			t.Errorf("%s %s = %d, want %d: %s", c.method, c.path, w.Code, c.want, w.Body.String())
		}
	}
}

// TestDevices_EnrolIsRateLimitedPerPeer: the one anonymous route is bounded per
// TCP peer (an IPv6 peer by its /64), and one peer's exhaustion is not another's.
func TestDevices_EnrolIsRateLimitedPerPeer(t *testing.T) {
	srv, _, rec := newDeviceTestServer(t, false)
	enrol := func(peer string) int {
		return doPeer(t, srv, http.MethodPost, "/api/v1/devices/enrol", "", `{"token":"wde_guess"}`, peer).Code
	}
	for i := range int(enrolBurst) {
		if c := enrol("192.0.2.50:1000"); c != http.StatusUnauthorized {
			t.Fatalf("attempt %d inside the burst = %d, want 401", i+1, c)
		}
	}
	if c := enrol("192.0.2.50:2000"); c != http.StatusTooManyRequests {
		t.Fatalf("attempt past the burst = %d, want 429", c)
	}
	if c := enrol("192.0.2.51:1000"); c != http.StatusUnauthorized {
		t.Fatalf("a second peer = %d, want its own bucket (401)", c)
	}
	for range int(enrolBurst) {
		enrol("[2001:db8:1:2::1]:443")
	}
	if c := enrol("[2001:db8:1:2::ffff]:443"); c != http.StatusTooManyRequests {
		t.Fatalf("another address in the same IPv6 /64 = %d, want 429 (one bucket per /64)", c)
	}
	if n := len(auditRows(rec, "device.enrol", "failure")); n == 0 || n >= 2*int(enrolBurst) {
		t.Fatalf("device.enrol failure rows = %d over %d refusals, want some and rate-bounded", n, 2*int(enrolBurst))
	}
}

// TestPrincipalLimiter_EvictsOneEntryNotAll pins per-entry eviction: a full map
// drops its stalest entry, and a peer that just exhausted its bucket stays
// exhausted when a new peer arrives. The clear-when-full ceiling this replaced
// refilled every bucket at once — fail-open on a route anyone can reach.
func TestPrincipalLimiter_EvictsOneEntryNotAll(t *testing.T) {
	l := principalLimiter{rate: 1, burst: 1, max: 3}
	t0 := time.Unix(1_700_000_000, 0)
	for i, key := range []string{"stale", "b", "hot"} {
		if !l.allow(key, t0.Add(time.Duration(i)*time.Millisecond)) {
			t.Fatalf("%s: first request refused", key)
		}
	}
	if l.allow("hot", t0.Add(3*time.Millisecond)) {
		t.Fatal("hot: second request inside the burst window allowed")
	}
	if !l.allow("newcomer", t0.Add(4*time.Millisecond)) {
		t.Fatal("newcomer refused")
	}
	if len(l.buckets) > 3 {
		t.Fatalf("map grew to %d entries, cap is 3", len(l.buckets))
	}
	if _, kept := l.buckets["stale"]; kept {
		t.Fatal("the stalest entry survived; eviction took a fresher one")
	}
	if l.allow("hot", t0.Add(5*time.Millisecond)) {
		t.Fatal("hot was refilled by a newcomer's arrival — eviction failed open")
	}
}

// TestDevices_IngestContract walks the audit-ingest answers the forwarder acts on.
func TestDevices_IngestContract(t *testing.T) {
	srv, st, rec := newDeviceTestServer(t, false)
	id, tok := enrolTestDevice(t, srv, "alices-laptop")
	path := "/api/v1/devices/" + id.String() + "/audit"
	// Every refusal below must write its row: lift the per-device bucket this
	// walk would otherwise exhaust (TestDevices_IngestFailureRowsAreBoundedPerDevice
	// pins the bucket itself).
	srv.ingestFailureLimiter.burst, srv.ingestFailureLimiter.rate = 1000, 1000
	post := func(body string) *httptest.ResponseRecorder { return do(t, srv, http.MethodPost, path, tok, body) }
	lastFailure := func(t *testing.T) string {
		t.Helper()
		fails := auditRows(rec, "device.audit.ingest", "failure")
		if len(fails) == 0 {
			t.Fatal("no device.audit.ingest failure row")
		}
		ev := fails[len(fails)-1]
		if ev.Target != id.String() || ev.Actor != deviceActor(id) {
			t.Fatalf("failure row = %+v, want it attributed to the device", ev)
		}
		return auditReason(t, ev)
	}

	batch := string(mustJSON(chainRows(1, 3, "")))
	if got := ackedSeq(t, post(batch)); got != 3 {
		t.Fatalf("acked_seq = %d, want 3", got)
	}
	if got := ackedSeq(t, post(batch)); got != 3 || st.ingestedFor(id) != 3 {
		t.Fatalf("idempotent retry: acked %d with %d rows stored, want 3 and 3", got, st.ingestedFor(id))
	}
	if got := ackedSeq(t, post("[]")); got != 3 {
		t.Fatalf("empty batch acked %d, want the cursor (3)", got)
	}
	st.mu.Lock()
	stored := st.ingested[id][0].SourceIP
	st.mu.Unlock()
	if stored != "127.0.0.1:54321" {
		t.Fatalf("store was handed source_ip %q, want the TCP peer the server saw (127.0.0.1:54321), never the claim", stored)
	}

	refusals := []struct {
		name, body, reason string
		want               int
	}{
		{"over 500 rows", string(mustJSON(chainRows(4, maxDeviceIngestRows+1, "h3"))), "batch_too_large", http.StatusRequestEntityTooLarge},
		{"unknown field", `[{"seq":4,"smuggled":true}]`, "invalid_body", http.StatusBadRequest},
		{"not an array", `{}`, "invalid_body", http.StatusBadRequest},
		{"seq not increasing", string(mustJSON(append(chainRows(4, 1, "h3"), chainRows(4, 1, "h4")...))), "invalid_row", http.StatusBadRequest},
		{"actor_type outside the enum", strings.Replace(string(mustJSON(chainRows(4, 1, "h3"))), `"human"`, `"device"`, 1), "invalid_row", http.StatusBadRequest},
		{"does not extend the recorded chain", string(mustJSON(chainRows(4, 1, "forged"))), "chain_mismatch", http.StatusUnprocessableEntity},
	}
	for _, c := range refusals {
		t.Run(c.name, func(t *testing.T) {
			if w := post(c.body); w.Code != c.want {
				t.Fatalf("status = %d, want %d: %s", w.Code, c.want, w.Body.String())
			}
			if got := lastFailure(t); got != c.reason {
				t.Fatalf("failure reason = %q, want %q", got, c.reason)
			}
			if st.ingestedFor(id) != 3 {
				t.Fatalf("a refused batch stored rows (%d, want 3)", st.ingestedFor(id))
			}
		})
	}

	t.Run("genesis after a purge is an audited chain reset", func(t *testing.T) {
		if got := ackedSeq(t, post(string(mustJSON(chainRows(10, 2, ""))))); got != 11 {
			t.Fatalf("acked_seq = %d, want 11", got)
		}
		resets := auditRows(rec, "device.audit.chain_reset", "success")
		if len(resets) != 1 || resets[0].Target != id.String() {
			t.Fatalf("chain_reset rows = %+v, want one naming the device", resets)
		}
		var data map[string]any
		_ = json.Unmarshal(resets[0].Data, &data)
		if data["prior_seq"] != float64(3) || data["prior_row_hash"] != "h3" || data["accepted"] != float64(2) {
			t.Fatalf("chain_reset data = %v, want the prior head (3, h3) and the accepted count", data)
		}
	})

	t.Run("a row under an organisation run is a 422 and a failure row", func(t *testing.T) {
		st.mu.Lock()
		st.ingestErr = store.ErrFederatedOrgRun
		st.mu.Unlock()
		defer func() { st.mu.Lock(); st.ingestErr = nil; st.mu.Unlock() }()
		if w := post(string(mustJSON(chainRows(12, 1, "h11")))); w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422: %s", w.Code, w.Body.String())
		}
		if got := lastFailure(t); got != "org_run" {
			t.Fatalf("failure reason = %q, want org_run", got)
		}
	})

	t.Run("a store outage is a 500 and a failure row", func(t *testing.T) {
		st.mu.Lock()
		st.ingestErr = errors.New("connection reset")
		st.mu.Unlock()
		defer func() { st.mu.Lock(); st.ingestErr = nil; st.mu.Unlock() }()
		if w := post(string(mustJSON(chainRows(12, 1, "h11")))); w.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", w.Code, w.Body.String())
		}
		if got := lastFailure(t); got != "store_error" {
			t.Fatalf("failure reason = %q, want store_error", got)
		}
	})
}

// TestDevices_AdminRoutes pins the admin surface: the enrolment token is shown
// once and prefixed, the inventory never carries a credential hash, and every
// act is audited.
func TestDevices_AdminRoutes(t *testing.T) {
	srv, _, rec := newDeviceTestServer(t, false)
	w := do(t, srv, http.MethodPost, "/api/v1/admin/devices/enrolment-tokens", adminToken, `{"name":"  alices-laptop  "}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("mint = %d: %s", w.Code, w.Body.String())
	}
	var minted types.DeviceEnrolmentToken
	_ = json.Unmarshal(w.Body.Bytes(), &minted)
	if !strings.HasPrefix(minted.Token, enrolmentTokenPrefix) || minted.DeviceName != "alices-laptop" ||
		minted.ExpiresAt.Sub(minted.CreatedAt) > deviceEnrolmentTokenTTL+time.Minute {
		t.Fatalf("minted = %+v, want a %s token for the trimmed name expiring within %s", minted, enrolmentTokenPrefix, deviceEnrolmentTokenTTL)
	}
	creates := auditRows(rec, "device.enrolment_token.create", "success")
	if len(creates) != 1 || creates[0].Target != minted.ID.String() || strings.Contains(string(creates[0].Data), minted.Token) {
		t.Fatalf("device.enrolment_token.create rows = %+v, want one naming the token id and never the token", creates)
	}

	for _, body := range []string{`{"name":""}`, `{"name":"a\u0007b"}`, `{"name":"` + strings.Repeat("x", 201) + `"}`, `{"name":"x","ttl":"1y"}`} {
		if w := do(t, srv, http.MethodPost, "/api/v1/admin/devices/enrolment-tokens", adminToken, body); w.Code != http.StatusBadRequest && w.Code != http.StatusUnprocessableEntity {
			t.Errorf("mint %s = %d, want 400/422", body, w.Code)
		}
	}

	id, tok := enrolTestDevice(t, srv, "bobs-laptop")
	enrols := auditRows(rec, "device.enrol", "success")
	if len(enrols) != 1 || enrols[0].Target != id.String() || strings.Contains(string(enrols[0].Data), tok) {
		t.Fatalf("device.enrol rows = %+v, want one naming the device and never its token", enrols)
	}
	w = do(t, srv, http.MethodGet, "/api/v1/admin/devices", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", w.Code, w.Body.String())
	}
	sum := sha256.Sum256([]byte(tok))
	if strings.Contains(w.Body.String(), hex.EncodeToString(sum[:])) || strings.Contains(w.Body.String(), "credential") {
		t.Fatalf("inventory leaks the credential hash: %s", w.Body.String())
	}
	var listed []types.Device
	_ = json.Unmarshal(w.Body.Bytes(), &listed)
	if len(listed) != 1 || listed[0].ID != id || listed[0].Name != "bobs-laptop" || listed[0].EnrolledBy != adminTokenPrincipal {
		t.Fatalf("inventory = %+v, want the one enrolled device with its minter", listed)
	}
}

// TestDevices_DeviceRoutesNeverTouchHumanIdentity is the structural half of the
// no-run invariant. A device request carries no OIDC human, and isOperator's
// no-human arm reads that as the admin token — so one call from a device
// route to an operator predicate or a request-actor helper would be an admin
// with a device's credential. devices_auth.go holds every device-facing route and
// may name none of them.
func TestDevices_DeviceRoutesNeverTouchHumanIdentity(t *testing.T) {
	forbidden := map[string]bool{
		"withHumanIdentity": true, "withOIDCHuman": true, "withOIDCRole": true, "withLocalPrincipal": true,
		"isOperator": true, "isSecurityOperator": true, "requireOperator": true, "requireSecurityOperator": true,
		"actorFromRequest": true, "actorTypeFromRequest": true, "principalFromRequest": true,
		"humanOrAdminAuth": true, "apiTokenAuth": true, "adminAuth": true, "handleCreateRun": true,
	}
	f, err := parser.ParseFile(token.NewFileSet(), "devices_auth.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var funcs []string
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok {
			funcs = append(funcs, fd.Name.Name)
		}
	}
	for _, want := range []string{"deviceAuth", "handleDeviceEnrol", "handleDeviceAuditIngest", "handleDeviceHeartbeat"} {
		if !slices.Contains(funcs, want) {
			t.Fatalf("%s is not in devices_auth.go (have %v) — the guard would pass over the routes it exists for", want, funcs)
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && forbidden[id.Name] {
			t.Errorf("devices_auth.go names %s — the device routes must never reach a human identity or an operator predicate", id.Name)
		}
		return true
	})
}
