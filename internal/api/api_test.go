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
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/identity/embedded"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const adminToken = "test-admin-token"

// ─── fakes ─────────────────────────────────────────────────────────────────

type recRecorder struct{ events []types.AuditEvent }

func (r *recRecorder) Record(_ context.Context, ev types.AuditEvent) error {
	r.events = append(r.events, ev)
	return nil
}

type fakeApprovals struct {
	requested  []types.ApprovalRequest
	byID       map[uuid.UUID]types.ApprovalRequest
	decideErr  error
	requestErr error
}

func newFakeApprovals() *fakeApprovals {
	return &fakeApprovals{byID: map[uuid.UUID]types.ApprovalRequest{}}
}

func (f *fakeApprovals) Request(_ context.Context, req types.ApprovalRequest) (types.ApprovalRequest, error) {
	if f.requestErr != nil {
		return types.ApprovalRequest{}, f.requestErr
	}
	if req.ID == uuid.Nil {
		req.ID = uuid.New()
	}
	req.State = types.ApprovalPending
	f.requested = append(f.requested, req)
	f.byID[req.ID] = req
	return req, nil
}

func (f *fakeApprovals) Decide(_ context.Context, id uuid.UUID, approve bool, byType types.ActorType, by, reason string) (types.ApprovalRequest, error) {
	if f.decideErr != nil {
		return types.ApprovalRequest{}, f.decideErr
	}
	ap := f.byID[id]
	ap.ID = id
	if approve {
		ap.State = types.ApprovalApproved
	} else {
		ap.State = types.ApprovalDenied
	}
	ap.DecidedBy = by
	ap.Reason = reason
	f.byID[id] = ap
	return ap, nil
}

func (f *fakeApprovals) Get(_ context.Context, id uuid.UUID) (types.ApprovalRequest, error) {
	ap, ok := f.byID[id]
	if !ok {
		return types.ApprovalRequest{}, errStoreNotFound
	}
	return ap, nil
}

func (f *fakeApprovals) List(_ context.Context, _ types.ApprovalState) ([]types.ApprovalRequest, error) {
	out := make([]types.ApprovalRequest, 0, len(f.byID))
	for _, ap := range f.byID {
		out = append(out, ap)
	}
	return out, nil
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

// ─── test harness ────────────────────────────────────────────────────────────

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
	idp, err := embedded.New(nil, "wardyn.local", embedded.NewMemRevocationStore(), audit)
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

func do(t *testing.T, srv *Server, method, path, bearer, body string) *httptest.ResponseRecorder {
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
	srv.Handler().ServeHTTP(w, r)
	return w
}

// ─── tests ─────────────────────────────────────────────────────────────────

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
	idp, err := embedded.New(nil, "wardyn.local", embedded.NewMemRevocationStore(), &recRecorder{})
	if err != nil {
		t.Fatalf("embedded.New: %v", err)
	}
	return idp
}
