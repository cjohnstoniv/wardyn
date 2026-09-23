// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/identity/embedded"
	"github.com/cjohnstoniv/wardyn/internal/identity/identitytest"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/version"
)

const adminToken = "test-admin-token"

// fakes

// recRecorder's mutex exists for the detached create-run launch: it records
// audit rows after the 201, while the test is already reading.
type recRecorder struct {
	mu     sync.Mutex
	events []types.AuditEvent
}

func (r *recRecorder) Record(_ context.Context, ev types.AuditEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
	return nil
}

// snapshot is the locked read of events a launch may still be appending to.
func (r *recRecorder) snapshot() []types.AuditEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]types.AuditEvent(nil), r.events...)
}

// waitForRecAudit polls for the run's action/outcome row, which POST /runs'
// detached launch writes after the response.
func waitForRecAudit(t *testing.T, r *recRecorder, runID uuid.UUID, action, outcome string) *types.AuditEvent {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ev := findAudit(r.snapshot(), runID, action, outcome); ev != nil {
			return ev
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("no %s/%s row for run %s after 5s; events=%s", action, outcome, runID, auditDump(r.snapshot(), runID))
	return nil
}

type fakeApprovals struct {
	// mu guards every field: the completion watcher's goroutine calls
	// CancelForRun while a test polls cancelledCalls(), and -race sees it.
	mu         sync.Mutex
	requested  []types.ApprovalRequest
	byID       map[uuid.UUID]types.ApprovalRequest
	decideErr  error
	requestErr error
	cancelErr  error
	cancelled  []cancelCall
	countErr   error
	// countForRun, when > 0, is what CountForRun answers regardless of the map —
	// the per-run cap is 4096 rows and seeding them all would prove nothing the
	// forced count does not.
	countForRun int
}

func newFakeApprovals() *fakeApprovals {
	return &fakeApprovals{byID: map[uuid.UUID]types.ApprovalRequest{}}
}

func (f *fakeApprovals) Request(_ context.Context, req types.ApprovalRequest) (types.ApprovalRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.requestErr != nil {
		return types.ApprovalRequest{}, f.requestErr
	}
	if req.ID == uuid.Nil {
		req.ID = uuid.New()
	}
	req.State = types.ApprovalPending
	// STAMPED, as approval.RequestApproval stamps it. Left unset, every
	// generation check that compares a login run's created_at against this
	// (the credential re-auth's I6) passed VACUOUSLY: any real timestamp is
	// after the zero time, so a sign-in from before the request answered it.
	if req.RequestedAt.IsZero() {
		req.RequestedAt = time.Now().UTC()
	}
	// THE PARTIAL UNIQUE INDEX, MODELLED (migration 0022
	// approvals_pending_noncred_uniq: one PENDING row per
	// (run_id, kind, requested_scope) for every kind but `credential`, which 0064
	// notes now covers credential_reauth too).
	//
	// Without it this double let N concurrent raises for ONE run each insert a
	// row, because approval.RequestApproval's dedup is a LIST-then-INSERT with a
	// real race window between the two — which the database closes and this map
	// did not. Every api-level concurrency assertion of the form "N callers, one
	// question" was therefore decided by goroutine scheduling: it passed most
	// runs and failed some, proving nothing either way (patch-review batch E).
	//
	// Returning the WINNER rather than an error is also what the store+FSM pair
	// does end to end: store.PG.CreateApproval maps the 23505 to
	// ErrDuplicatePending and approval.RequestApproval re-reads and returns the
	// existing row. This is the SERVICE double, so it models that pair's
	// observable answer. The mapping itself is pinned a layer down
	// (internal/approval's TestRequestApproval_DuplicatePending..., and
	// internal/store's PG-gated concurrent-raise test against the real index).
	for _, ap := range f.byID {
		if ap.State == types.ApprovalPending && ap.RunID == req.RunID && ap.Kind == req.Kind &&
			string(ap.RequestedScope) == string(req.RequestedScope) {
			return ap, nil
		}
	}
	f.requested = append(f.requested, req)
	f.byID[req.ID] = req
	return req, nil
}

func (f *fakeApprovals) Decide(_ context.Context, id uuid.UUID, byType types.ActorType, decision types.ApprovalDecision) (types.ApprovalRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.decideErr != nil {
		return types.ApprovalRequest{}, f.decideErr
	}
	ap := f.byID[id]
	ap.ID = id
	ap.State = decision.State
	ap.DecidedBy = decision.DecidedBy
	ap.Reason = decision.Reason
	ap.DecisionScope = decision.Scope
	ap.DecisionExpiresAt = decision.ExpiresAt
	f.byID[id] = ap
	return ap, nil
}

func (f *fakeApprovals) Get(_ context.Context, id uuid.UUID) (types.ApprovalRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ap, ok := f.byID[id]
	if !ok {
		return types.ApprovalRequest{}, errStoreNotFound
	}
	return ap, nil
}

func (f *fakeApprovals) List(_ context.Context, _ types.ApprovalState) ([]types.ApprovalRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]types.ApprovalRequest, 0, len(f.byID))
	for _, ap := range f.byID {
		out = append(out, ap)
	}
	return out, nil
}

// CancelForRun mirrors approval.CancelForRun over the map: only PENDING rows of
// THIS run move, and cancelled records what the handler passed so a test can
// assert the reason the terminal transition supplied.
func (f *fakeApprovals) CancelForRun(_ context.Context, runID uuid.UUID, reason string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cancelErr != nil {
		return 0, f.cancelErr
	}
	n := 0
	for id, ap := range f.byID {
		if ap.RunID != runID || ap.State != types.ApprovalPending {
			continue
		}
		ap.State = types.ApprovalCancelled
		ap.DecidedBy, ap.Reason = "system", reason
		f.byID[id] = ap
		n++
	}
	if n > 0 {
		f.cancelled = append(f.cancelled, cancelCall{RunID: runID, Reason: reason, Count: n})
	}
	return n, nil
}

// CountForRun counts this run's rows in any state (R3-F071's cap reads it).
// cancelledCalls returns a snapshot of what CancelForRun recorded, under the
// lock — the watcher test polls this from the test goroutine.
func (f *fakeApprovals) cancelledCalls() []cancelCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]cancelCall(nil), f.cancelled...)
}

func (f *fakeApprovals) CountForRun(_ context.Context, runID uuid.UUID) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.countErr != nil {
		return 0, f.countErr
	}
	if f.countForRun > 0 {
		return f.countForRun, nil // a test forcing the cap without seeding 4096 rows
	}
	n := 0
	for _, ap := range f.byID {
		if ap.RunID == runID {
			n++
		}
	}
	return n, nil
}

// cancelCall is one CancelForRun that actually moved rows (the emitting case).
type cancelCall struct {
	RunID  uuid.UUID
	Reason string
	Count  int
}

// errStoreNotFound mirrors store.ErrNotFound semantics for the fake (the
// handlers branch on store.ErrNotFound; the fake approval Get path is only used
// by internal handlers that compare run ownership, not the not-found mapping).
var errStoreNotFound = errors.New("store: not found")

type fakeBroker struct {
	minted   broker.Minted
	mintErr  error
	revoked  []uuid.UUID
	lastCall *identity.Claims
}

func (b *fakeBroker) MintForGrant(_ context.Context, caller *identity.Claims, _ uuid.UUID) (broker.Minted, error) {
	b.lastCall = caller
	if b.mintErr != nil {
		return broker.Minted{}, b.mintErr
	}
	return b.minted, nil
}

func (b *fakeBroker) RevokeRun(_ context.Context, runID uuid.UUID) error {
	b.revoked = append(b.revoked, runID)
	return nil
}

// test harness

type harness struct {
	srv       *Server
	idp       *embedded.Provider
	approvals *fakeApprovals
	broker    *fakeBroker
	audit     *recRecorder
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	audit := &recRecorder{}
	idp, err := embedded.New(nil, "wardyn.local", identitytest.NewMemRevocationStore(), audit)
	if err != nil {
		t.Fatalf("embedded.New: %v", err)
	}
	approvals := newFakeApprovals()
	brk := &fakeBroker{}
	srv := New(Config{
		Identity:    idp,
		Approvals:   approvals,
		Broker:      brk,
		Audit:       audit,
		AdminToken:  adminToken,
		TrustDomain: "wardyn.local",
		DefaultPolicy: types.RunPolicySpec{
			AllowedDomains:      []string{"api.anthropic.com"},
			MinConfinementClass: types.CC2,
		},
		ControlPlaneURL: "http://wardynd:8080",
	})
	return &harness{srv: srv, idp: idp, approvals: approvals, broker: brk, audit: audit}
}

// baseTestConfig returns the Config preamble shared by most handler tests
// that build their own Server rather than using newHarness directly: embedded
// identity/audit from h, admin auth, and the fixed trust domain/control-plane
// URL every one of those call sites repeated verbatim. Callers can still
// override any field (Store, DefaultPolicy, ScanAIAdvisor, ...) on the
// returned value before calling New.
func baseTestConfig(h *harness, st store.Store) Config {
	return Config{
		Identity:        h.idp,
		Audit:           h.audit,
		AdminToken:      adminToken,
		TrustDomain:     "wardyn.local",
		ControlPlaneURL: "http://wardynd:8080",
		Store:           st,
	}
}

func (h *harness) mintRunToken(t *testing.T, runID uuid.UUID) string {
	t.Helper()
	id, err := h.idp.MintRunIdentity(context.Background(), runID, "alice@example.com", "", internalAudience)
	if err != nil {
		t.Fatalf("mint run identity: %v", err)
	}
	return id.Token
}

// panicCatcher is a minimal middleware.LogEntry (go-chi/chi/v5/middleware):
// Write is a no-op (nothing in this package reads a request log), and Panic
// records what routes.go's middleware.Recoverer recovered. Recoverer calls
// GetLogEntry(r).Panic(rvr, stack) whenever the request context carries a
// LogEntry INSTEAD OF just printing the stack — a seam chi ships for exactly
// this, that production code never uses (wardynd sets no LogFormatter, so
// GetLogEntry(r) is always nil there; grep WithLogEntry|RequestLogger outside
// _test.go is empty). Guarded by a mutex: httptest.NewServer(panicFails(...))
// serves each request on its OWN connection goroutine (net/http.Server.Serve),
// never the test's own, so the write here and panicFails' read below can race.
type panicCatcher struct {
	mu        sync.Mutex
	recovered bool
	v         any
	stack     []byte
}

func (c *panicCatcher) Write(int, int, http.Header, time.Duration, interface{}) {}

func (c *panicCatcher) Panic(v any, stack []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recovered, c.v, c.stack = true, v, stack
}

func (c *panicCatcher) take() (v any, stack []byte, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.v, c.stack, c.recovered
}

// panicFails wraps h so any panic its router recovers, across every request
// the wrapped handler serves, fails t instead of answering with an
// unremarkable 500 that every assertion still matches (#338). The check runs
// in t.Cleanup rather than inline: a DIRECT h.ServeHTTP(w, r) call (do/doSSO,
// every raw ServeHTTP site in the package) executes on the test's own
// goroutine and could fail immediately, but httptest.NewServer(panicFails(t,
// h)) serves each connection on a goroutine net/http.Server spawns — where
// calling t.FailNow is unsafe (testing.T's own doc comment) — so both forms
// go through the one path that IS always safe. Every srv.Handler().ServeHTTP
// / httptest.NewServer(srv.Handler()) call site in the package wraps its
// handler with this rather than calling it bare, so no test path bypasses
// the check. Nothing about the response or the production logging path
// changes — this only reads what Recoverer already computes.
func panicFails(t testing.TB, h http.Handler) http.Handler {
	t.Helper()
	c := &panicCatcher{}
	t.Cleanup(func() {
		if v, stack, ok := c.take(); ok {
			t.Fatalf("recovered a panic instead of answering it — a recovered panic must fail its test (#338):\n%v\n%s", v, stack)
		}
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, middleware.WithLogEntry(r, c))
	})
}

// panicIsTheFixture is panicFails' one exemption: a handler whose panic IS the
// thing under test — metrics_test.go's scrapePanicStore, the #323
// reintroduction fixture — where a recovered panic is the fixture firing, not
// a defect. The check is INVERTED rather than dropped: the test fails if
// nothing panicked, so an exempted call site cannot quietly stop exercising
// the fixture it was exempted for.
func panicIsTheFixture(t testing.TB, h http.Handler) http.Handler {
	t.Helper()
	c := &panicCatcher{}
	t.Cleanup(func() {
		if _, _, ok := c.take(); !ok {
			t.Fatalf("no panic was recovered, but this call site exists to drive one (#338/#323)")
		}
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, middleware.WithLogEntry(r, c))
	})
}

func do(t *testing.T, srv *Server, method, path, bearer, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doVia(t, panicFails, srv, method, path, bearer, body)
}

// doVia is do's body with the panic-catcher wrapper as a parameter, so the one
// test whose fixture panics on purpose reuses the same request shape.
func doVia(t *testing.T, wrap func(testing.TB, http.Handler) http.Handler, srv *Server, method, path, bearer, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	// FIX #8: the local-mode no-auth surface now requires a loopback Host
	// (DNS-rebinding guard). httptest.NewRequest defaults Host to "example.com",
	// which the guard rejects; real local-mode requests always arrive on a
	// loopback Host (the Compose default publishes 127.0.0.1). Model that here so
	// local-mode tests exercise the allowed path; non-local tests ignore Host.
	r.Host = "127.0.0.1"
	// N1: the local-mode bypass ALSO requires a loopback TCP peer (RemoteAddr).
	// httptest.NewRequest defaults RemoteAddr to "192.0.2.1:1234" (TEST-NET, non-
	// loopback), which the guard rejects; a real local-mode request arrives from a
	// loopback peer. Model that so local-mode tests exercise the allowed path.
	r.RemoteAddr = "127.0.0.1:54321"
	w := httptest.NewRecorder()
	wrap(t, srv.Handler()).ServeHTTP(w, r)
	return w
}

// lastAuditEvent returns the LAST recorded event with the given action (a
// pipeline can record several of the same action across rounds/subtests; the
// last one is the one the current round just wrote). Fails the test if none.
func lastAuditEvent(t *testing.T, events []types.AuditEvent, action string) types.AuditEvent {
	t.Helper()
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Action == action {
			return events[i]
		}
	}
	t.Fatalf("no %q audit event recorded (have %d events)", action, len(events))
	return types.AuditEvent{}
}

// tests

func TestHealthz(t *testing.T) {
	h := newHarness(t)
	w := do(t, h.srv, http.MethodGet, "/healthz", "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("healthz code = %d", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["identity_provider"] != "embedded" {
		t.Errorf("identity_provider = %v, want embedded", body["identity_provider"])
	}
	// The daemon's build must be answerable without a credential (support issues,
	// CLI/server skew after a rolling upgrade).
	if body["version"] != version.Version {
		t.Errorf("version = %v, want %s", body["version"], version.Version)
	}
	// confinement_names lets a scriptable consumer learn the CC-code -> friendly
	// tier name mapping (IFC2) instead of hardcoding the UI's cc-meta.ts labels.
	names, _ := body["confinement_names"].(map[string]any)
	if names["CC1"] != "Fence" || names["CC2"] != "Wall" || names["CC3"] != "Vault" {
		t.Errorf("confinement_names = %v, want CC1/CC2/CC3 -> Fence/Wall/Vault", names)
	}
}

// enforcementWordRunner is a driver that DOES declare the ephemeral-disk
// enforcement word, so the exclusion below is a real assertion rather than a
// tautology over a fake that never had one to leak.
type enforcementWordRunner struct{ runner.Runner }

func (enforcementWordRunner) Name() string { return "k8s" }
func (enforcementWordRunner) Capabilities(context.Context) (runner.Capabilities, error) {
	return runner.Capabilities{
		Driver:                   "k8s",
		ConfinementClasses:       []types.ConfinementClass{types.CC1},
		EphemeralDiskEnforcement: types.StorageEnforcementEviction,
	}, nil
}

// TestHealthz_OmitsTheEnforcementWord pins the half healthz.go states only in a
// comment: the word is OPERATOR DETAIL, redactSetupStatusForMember strips it
// from /setup/status for a member (setup_test.go), and /healthz is ANONYMOUS —
// so it must not carry it at all. handleHealthz composes its body field by
// field precisely so a field added to the setup status never appears here by
// accident; this is the test that notices when one does.
func TestHealthz_OmitsTheEnforcementWord(t *testing.T) {
	srv := New(Config{Runner: enforcementWordRunner{}})
	w := do(t, srv, http.MethodGet, "/healthz", "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("healthz code = %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "ephemeral_disk_enforcement") {
		t.Errorf("/healthz carries the capability field; body = %s", body)
	}
	if strings.Contains(body, string(types.StorageEnforcementEviction)) {
		t.Errorf("/healthz carries the enforcement WORD under some other key; body = %s", body)
	}
}

// TestHealthz_NetworkPolicy proves handleHealthz's "network_policy" field is
// wired to the same k8sNetpolVerdict setupRunnerInfo uses (TestK8sNetpolVerdict,
// setup_test.go, covers the pure grading): a k8s runner's live verdict reaches
// the anonymous /healthz body, a non-k8s driver never sprouts the key at all
// (not even a JSON null — a Docker deployment's /healthz shape must not
// change), and a k8s driver whose Capabilities() call itself errors reports no
// verdict rather than the stronger "enforced" claim.
func TestHealthz_NetworkPolicy(t *testing.T) {
	decode := func(t *testing.T, srv *Server) map[string]any {
		t.Helper()
		w := do(t, srv, http.MethodGet, "/healthz", "", "")
		if w.Code != http.StatusOK {
			t.Fatalf("healthz code = %d", w.Code)
		}
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return body
	}

	t.Run("k8s enforced", func(t *testing.T) {
		srv := New(Config{Runner: k8sRunner{networkPolicy: true}})
		body := decode(t, srv)
		if body["network_policy"] != "enforced" {
			t.Errorf("network_policy = %v, want enforced", body["network_policy"])
		}
	})
	t.Run("k8s unenforced", func(t *testing.T) {
		srv := New(Config{Runner: k8sRunner{}})
		body := decode(t, srv)
		if body["network_policy"] != "unenforced" {
			t.Errorf("network_policy = %v, want unenforced", body["network_policy"])
		}
	})
	t.Run("k8s acknowledged", func(t *testing.T) {
		srv := New(Config{Runner: k8sRunner{networkPolicyAcknowledged: true}})
		body := decode(t, srv)
		if body["network_policy"] != "acknowledged" {
			t.Errorf("network_policy = %v, want acknowledged", body["network_policy"])
		}
	})
	// Negative control: a non-k8s driver must OMIT the key entirely, never
	// report it as "" — a Docker deployment must not sprout this field.
	t.Run("docker driver omits the field", func(t *testing.T) {
		srv := New(Config{Runner: &fakeRunner{}})
		body := decode(t, srv)
		if v, ok := body["network_policy"]; ok {
			t.Errorf("network_policy = %v, want key entirely absent", v)
		}
	})
	// Negative control: a k8s driver whose Capabilities() call itself errors
	// must never report "enforced" (or any verdict) — it reports no field, the
	// same as a non-k8s driver, never a silent false claim of enforcement.
	t.Run("k8s driver, Capabilities() errors: omits the field", func(t *testing.T) {
		srv := New(Config{Runner: k8sRunner{capsErr: errors.New("k8s api unreachable")}})
		body := decode(t, srv)
		if v, ok := body["network_policy"]; ok {
			t.Errorf("network_policy = %v, want key entirely absent on a Capabilities() error", v)
		}
	})
}

// TestHealthz_TokenLoginAndSSOOnly pins the two bits #378/#379 added to the
// anonymous /healthz body: token_login (should the sign-in screen offer the
// admin-token form?) and sso_only (mirrors Config.SSOOnly). token_login is
// computed, never a plain field mirror, precisely so a token that CANNOT work
// as a human sign-in path — sso-only's second front door, or member mode's
// process credential (deploy/desktop/wardyn.env.m-prime.example) — is never
// advertised as one.
func TestHealthz_TokenLoginAndSSOOnly(t *testing.T) {
	decode := func(t *testing.T, srv *Server) map[string]any {
		t.Helper()
		w := do(t, srv, http.MethodGet, "/healthz", "", "")
		if w.Code != http.StatusOK {
			t.Fatalf("healthz code = %d", w.Code)
		}
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return body
	}

	for _, tc := range []struct {
		name           string
		cfg            Config
		wantTokenLogin bool
		wantSSOOnly    bool
	}{
		{
			name:           "plain token deployment: token works, no sso_only",
			cfg:            Config{AdminToken: "tok"},
			wantTokenLogin: true,
		},
		{
			name:           "no token, no sso, no member: nothing to offer (local-mode/misconfigured)",
			cfg:            Config{},
			wantTokenLogin: false,
		},
		{
			name:           "sso-only: token forced off even if a token were somehow set",
			cfg:            Config{AdminToken: "tok", SSOOnly: true},
			wantTokenLogin: false,
			wantSSOOnly:    true,
		},
		{
			name:           "member mode: the token is a process credential, never a human sign-in path",
			cfg:            Config{AdminToken: "tok", MemberMode: true},
			wantTokenLogin: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := decode(t, New(tc.cfg))
			if got := body["token_login"]; got != tc.wantTokenLogin {
				t.Errorf("token_login = %v, want %v", got, tc.wantTokenLogin)
			}
			if got := body["sso_only"]; got != tc.wantSSOOnly {
				t.Errorf("sso_only = %v, want %v", got, tc.wantSSOOnly)
			}
		})
	}
}

func TestAdminAuthRequired(t *testing.T) {
	h := newHarness(t)
	// No token.
	if w := do(t, h.srv, http.MethodGet, "/api/v1/runs", "", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("no token: code = %d, want 401", w.Code)
	}
	// Wrong token.
	if w := do(t, h.srv, http.MethodGet, "/api/v1/runs", "wrong", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("wrong token: code = %d, want 401", w.Code)
	}
}

func TestAdminAuthDisabledWhenNoToken(t *testing.T) {
	srv := New(Config{
		Identity:      mustIDP(t),
		Approvals:     newFakeApprovals(),
		Broker:        &fakeBroker{},
		Audit:         &recRecorder{},
		AdminToken:    "", // disabled
		DefaultPolicy: types.RunPolicySpec{MinConfinementClass: types.CC2},
	})
	// Public route must fail closed (401) when no admin token is configured.
	if w := do(t, srv, http.MethodGet, "/api/v1/runs", "anything", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("disabled admin: code = %d, want 401", w.Code)
	}
	// healthz still works.
	if w := do(t, srv, http.MethodGet, "/healthz", "", ""); w.Code != http.StatusOK {
		t.Errorf("healthz code = %d, want 200", w.Code)
	}
}

func TestInternalAuthRejectsBadToken(t *testing.T) {
	h := newHarness(t)
	body := `{"request":{"host":"x"},"decision":"allow"}`
	// No token.
	if w := do(t, h.srv, http.MethodPost, "/api/v1/internal/decisions", "", body); w.Code != http.StatusUnauthorized {
		t.Errorf("no token: code = %d, want 401", w.Code)
	}
	// Garbage token.
	if w := do(t, h.srv, http.MethodPost, "/api/v1/internal/decisions", "not-a-jwt", body); w.Code != http.StatusUnauthorized {
		t.Errorf("garbage token: code = %d, want 401", w.Code)
	}
	// Admin token must NOT pass internal auth (wrong audience / not a JWT).
	if w := do(t, h.srv, http.MethodPost, "/api/v1/internal/decisions", adminToken, body); w.Code != http.StatusUnauthorized {
		t.Errorf("admin token on internal: code = %d, want 401", w.Code)
	}
}

// TestInternalDecisionAllowIsSuccess pins decisionOutcome's other arm: an allow
// lands as egress.allow with outcome "success" (the deny arm is pinned above;
// without this the collapse of the Allow and default arms was untested).
func TestInternalDecisionAllowIsSuccess(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	tok := h.mintRunToken(t, runID)
	body := `{"request":{"host":"api.example.com","method":"CONNECT"},"decision":"allow","rule_source":"policy"}`
	w := do(t, h.srv, http.MethodPost, "/api/v1/internal/decisions", tok, body)
	if w.Code != http.StatusAccepted {
		t.Fatalf("decision code = %d, want 202", w.Code)
	}
	var found bool
	for _, ev := range h.audit.events {
		if ev.Action == "egress.allow" {
			found = true
			if ev.Outcome != "success" {
				t.Errorf("egress.allow outcome = %s, want success", ev.Outcome)
			}
			if ev.RunID == nil || *ev.RunID != runID {
				t.Errorf("egress audit run id mismatch")
			}
		}
	}
	if !found {
		t.Fatal("no egress.allow audit event recorded")
	}
}

func TestInternalDecisionPersistsAudit(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	tok := h.mintRunToken(t, runID)
	body := `{"request":{"host":"evil.example.com","method":"CONNECT"},"decision":"deny","rule_source":"policy"}`
	w := do(t, h.srv, http.MethodPost, "/api/v1/internal/decisions", tok, body)
	if w.Code != http.StatusAccepted {
		t.Fatalf("decision code = %d, want 202", w.Code)
	}
	// Find the egress.deny audit event attributed to the agent and bound to runID.
	var found bool
	for _, ev := range h.audit.events {
		if ev.Action == "egress.deny" {
			found = true
			if ev.ActorType != types.ActorAgent {
				t.Errorf("egress audit actor_type = %s, want agent", ev.ActorType)
			}
			if ev.Outcome != "denied" {
				t.Errorf("egress.deny outcome = %s, want denied", ev.Outcome)
			}
			if ev.RunID == nil || *ev.RunID != runID {
				t.Errorf("egress audit run id mismatch")
			}
		}
	}
	if !found {
		t.Fatalf("no egress.deny audit event recorded")
	}
}

func TestInternalApprovalRequestBindsRunFromToken(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	tok := h.mintRunToken(t, runID)
	body := `{"kind":"egress_domain","requested_scope":{"host":"pkg.example.com"}}`
	w := do(t, h.srv, http.MethodPost, "/api/v1/internal/approvals", tok, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("internal approval code = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	if len(h.approvals.requested) != 1 {
		t.Fatalf("approvals requested = %d, want 1", len(h.approvals.requested))
	}
	if got := h.approvals.requested[0].RunID; got != runID {
		t.Errorf("approval run id = %s, want %s (must bind from token, not body)", got, runID)
	}
}

// TestInternalApprovalRequestAcceptsToolCall pins the control-plane half of the
// tool-approval gate. The brokered POST /wardyn/v1/approvals (the sandbox's
// tokenless alias, local_routes.go) forwards EXACTLY this body, and the
// approvals screen reads exactly this scope — so a kind quietly dropped from
// the switch above would park every `tool_approvals: hold` run forever.
func TestInternalApprovalRequestAcceptsToolCall(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	tok := h.mintRunToken(t, runID)
	scope := `{"tool":"Bash","cmd":"rm -rf build","env":"AWS_PROFILE"}`
	w := do(t, h.srv, http.MethodPost, "/api/v1/internal/approvals", tok,
		`{"kind":"tool_call","requested_scope":`+scope+`}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("tool_call approval code = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	if len(h.approvals.requested) != 1 {
		t.Fatalf("approvals requested = %d, want 1", len(h.approvals.requested))
	}
	got := h.approvals.requested[0]
	if got.Kind != types.ApprovalToolCall || got.RunID != runID {
		t.Fatalf("stored approval = %s for run %s, want tool_call for %s", got.Kind, got.RunID, runID)
	}
	// Stored verbatim: the UI renders {tool, cmd, env} straight off this field.
	if string(got.RequestedScope) != scope {
		t.Fatalf("requested_scope = %s, want %s", got.RequestedScope, scope)
	}
}

func TestInternalApprovalRequestRejectsCredentialKind(t *testing.T) {
	h := newHarness(t)
	tok := h.mintRunToken(t, uuid.New())
	// A sidecar must not be able to raise a credential approval.
	body := `{"kind":"credential","requested_scope":{"x":1}}`
	w := do(t, h.srv, http.MethodPost, "/api/v1/internal/approvals", tok, body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("credential kind from sidecar: code = %d, want 400", w.Code)
	}
}

func TestInternalGetApprovalCrossRunDenied(t *testing.T) {
	h := newHarness(t)
	ownerRun := uuid.New()
	otherRun := uuid.New()
	// Seed an approval owned by ownerRun.
	ap, _ := h.approvals.Request(context.Background(), types.ApprovalRequest{
		RunID: ownerRun, Kind: types.ApprovalEgressDomain, RequestedScope: json.RawMessage(`{"host":"x"}`),
	})
	// A token for otherRun must not be able to read ownerRun's approval.
	tok := h.mintRunToken(t, otherRun)
	w := do(t, h.srv, http.MethodGet, "/api/v1/internal/approvals/"+ap.ID.String(), tok, "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("cross-run approval read: code = %d, want 404", w.Code)
	}
}

func TestInternalMintForGrant(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	tok := h.mintRunToken(t, runID)
	h.broker.minted = broker.Minted{
		Kind: types.GrantGitHubToken, JTI: "jti-1", Token: "ghs_xxx",
	}
	body := `{"grant_id":"` + uuid.New().String() + `"}`
	w := do(t, h.srv, http.MethodPost, "/api/v1/internal/credentials/mint", tok, body)
	if w.Code != http.StatusOK {
		t.Fatalf("mint code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp mintResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode mint resp: %v", err)
	}
	if resp.Token != "ghs_xxx" || resp.JTI != "jti-1" {
		t.Errorf("mint resp = %+v", resp)
	}
	// The broker must have been called with the run claims from the token.
	if h.broker.lastCall == nil || h.broker.lastCall.RunID != runID {
		t.Errorf("broker caller claims not bound from token")
	}
}

func TestInternalMintApprovalPending(t *testing.T) {
	h := newHarness(t)
	tok := h.mintRunToken(t, uuid.New())
	apID := uuid.New()
	h.broker.mintErr = broker.ErrApprovalPending{ApprovalID: apID}
	body := `{"grant_id":"` + uuid.New().String() + `"}`
	w := do(t, h.srv, http.MethodPost, "/api/v1/internal/credentials/mint", tok, body)
	if w.Code != http.StatusConflict {
		t.Fatalf("pending mint code = %d, want 409", w.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["approval_id"] != apID.String() {
		t.Errorf("approval_id = %v, want %s", resp["approval_id"], apID)
	}
	// The LITERAL, not mintConflictPending: comparing the decoded JSON against
	// the very constant the handler wrote is self-referential — all four wire
	// values could be renamed with the suite green (F134). A wire contract is
	// pinned by its bytes.
	if resp["code"] != "pending" {
		t.Errorf("code = %v, want %q (W19-W19a-2)", resp["code"], "pending")
	}
}

func TestInternalMintScopeMismatchFailsClosed(t *testing.T) {
	h := newHarness(t)
	tok := h.mintRunToken(t, uuid.New())
	h.broker.mintErr = broker.ErrScopeMismatch
	body := `{"grant_id":"` + uuid.New().String() + `"}`
	w := do(t, h.srv, http.MethodPost, "/api/v1/internal/credentials/mint", tok, body)
	if w.Code != http.StatusConflict {
		t.Fatalf("scope mismatch code = %d, want 409", w.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["code"] != "scope_mismatch" { // literal, not the constant — see F134
		t.Errorf("code = %v, want %q (W19-W19a-2)", resp["code"], "scope_mismatch")
	}
}

// TestInternalMintAlreadyMintedCarriesDiscriminatingCode is: three
// distinct fail-closed conditions used to share the bare 409 status with no
// discriminator — the "already minted" (single-use) case decoded in
// cmd/wardyn-git-helper's callMint as an approval-pending 409 with no
// approval_id, surfacing "mint returned 409 without approval_id" for the
// second git operation of an approval-gated run instead of naming single-use
// as the real cause. A "code" field now distinguishes it.
func TestInternalMintAlreadyMintedCarriesDiscriminatingCode(t *testing.T) {
	h := newHarness(t)
	tok := h.mintRunToken(t, uuid.New())
	h.broker.mintErr = broker.ErrAlreadyMinted
	body := `{"grant_id":"` + uuid.New().String() + `"}`
	w := do(t, h.srv, http.MethodPost, "/api/v1/internal/credentials/mint", tok, body)
	if w.Code != http.StatusConflict {
		t.Fatalf("already-minted code = %d, want 409", w.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["code"] != "already_minted" { // literal, not the constant — see F134
		t.Errorf("code = %v, want %q — the git helper cannot otherwise tell this apart from a pending/denied 409", resp["code"], "already_minted")
	}
	if _, hasApprovalID := resp["approval_id"]; hasApprovalID {
		t.Errorf("already-minted response carries approval_id = %v, want none (this is not an approval-flow condition)", resp["approval_id"])
	}
}

func TestInternalMintRequiresSPIRE(t *testing.T) {
	h := newHarness(t)
	tok := h.mintRunToken(t, uuid.New())
	h.broker.mintErr = broker.ErrRequiresSPIRE
	body := `{"grant_id":"` + uuid.New().String() + `"}`
	w := do(t, h.srv, http.MethodPost, "/api/v1/internal/credentials/mint", tok, body)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("cloud_sts mint code = %d, want 422", w.Code)
	}
}

func mustIDP(t *testing.T) *embedded.Provider {
	t.Helper()
	idp, err := embedded.New(nil, "wardyn.local", identitytest.NewMemRevocationStore(), &recRecorder{})
	if err != nil {
		t.Fatalf("embedded.New: %v", err)
	}
	return idp
}
