// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

var errMCPRowsFake = errors.New("not held by this fake")

// mcpRowsFake is the run_mcp_sandboxes seam over rows held in memory: the reads and the token rotation the
// MCP door uses; every other write fails.
type mcpRowsFake struct {
	mu   sync.Mutex
	rows []store.RunMCPSandbox
}

func (m *mcpRowsFake) set(f func(*store.RunMCPSandbox)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f(&m.rows[0])
}

func (m *mcpRowsFake) first() store.RunMCPSandbox {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rows[0]
}

func (m *mcpRowsFake) InsertRunMCPSandbox(_ context.Context, row store.RunMCPSandbox) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows = append(m.rows, row)
	return nil
}

func (m *mcpRowsFake) ListRunMCPSandboxes(_ context.Context, runID uuid.UUID) ([]store.RunMCPSandbox, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []store.RunMCPSandbox
	for _, r := range m.rows {
		if r.RunID == runID {
			out = append(out, r)
		}
	}
	return out, nil
}

func (m *mcpRowsFake) RotateMCPSandboxToken(_ context.Context, runID uuid.UUID, ordinal int, oldJTI, newJTI string, oldUntil time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, r := range m.rows {
		if r.RunID == runID && r.Ordinal == ordinal && r.CPTokenJTI == oldJTI && r.State != store.MCPSandboxStopped {
			until := oldUntil
			m.rows[i].PrevCPTokenJTI, m.rows[i].PrevCPTokenUntil, m.rows[i].CPTokenJTI = oldJTI, &until, newJTI
			return true, nil
		}
	}
	return false, nil
}

func (m *mcpRowsFake) MarkMCPSandboxStartedIfStarting(context.Context, uuid.UUID, int, string) (bool, error) {
	return false, errMCPRowsFake
}
func (m *mcpRowsFake) FailMCPSandboxIfUp(context.Context, uuid.UUID, int, string) (bool, error) {
	return false, errMCPRowsFake
}
func (m *mcpRowsFake) MarkMCPSandboxReadyIfLive(context.Context, uuid.UUID, int, string) (bool, error) {
	return false, errMCPRowsFake
}
func (m *mcpRowsFake) ReopenMCPSandboxIfLive(context.Context, uuid.UUID, int, time.Time, string, time.Time) (bool, error) {
	return false, errMCPRowsFake
}
func (m *mcpRowsFake) KeepMCPSandboxStartingIfLive(context.Context, uuid.UUID, int, string) (bool, error) {
	return false, errMCPRowsFake
}
func (m *mcpRowsFake) FailMCPSandboxIfStarting(context.Context, uuid.UUID, int, string) (bool, error) {
	return false, errMCPRowsFake
}
func (m *mcpRowsFake) StopMCPSandboxRow(context.Context, uuid.UUID, int, string) (bool, error) {
	return false, errMCPRowsFake
}
func (m *mcpRowsFake) RecordMCPSandboxCreated(context.Context, uuid.UUID, int, store.MCPSandboxCreated) (bool, error) {
	return false, errMCPRowsFake
}
func (m *mcpRowsFake) RestartMCPSandboxIfLive(context.Context, uuid.UUID, int, store.MCPSandboxRestart) (bool, error) {
	return false, errMCPRowsFake
}

// mcpAuthStore is a live run with MCP server rows.
type mcpAuthStore struct {
	anyRunLive
	*mcpRowsFake
}

var _ store.RunMCPSandboxStore = mcpAuthStore{}

// mcpAuthFixture is a live run whose MCP server files holds grant, with its proxy's token and the agent's.
type mcpAuthFixture struct {
	h          *harness
	srv        *Server
	rows       *mcpRowsFake
	runID      uuid.UUID
	grant      uuid.UUID
	tok, agent string
	tokID      identity.RunIdentity
}

func mcpProxyToken(t *testing.T, h *harness, runID uuid.UUID) identity.RunIdentity {
	t.Helper()
	id, err := h.idp.MintRunIdentity(context.Background(), runID, "alice@example.com", "", mcpSandboxAudience, false)
	if err != nil {
		t.Fatalf("mint an MCP token: %v", err)
	}
	return id
}

func newMCPAuthFixture(t *testing.T) *mcpAuthFixture {
	t.Helper()
	return newMCPAuthFixtureWith(t, nil)
}

// newMCPAuthFixtureWith is newMCPAuthFixture with edit applied to the server's config before New.
func newMCPAuthFixtureWith(t *testing.T, edit func(*Config, *mcpAuthFixture)) *mcpAuthFixture {
	t.Helper()
	h := newHarness(t)
	runID, grant := uuid.New(), uuid.New()
	id := mcpProxyToken(t, h, runID)
	rows := &mcpRowsFake{rows: []store.RunMCPSandbox{{RunID: runID, ServerKey: "files", State: store.MCPSandboxReady,
		CPTokenJTI: id.JTI, GrantIDs: []uuid.UUID{grant}}}}
	cfg := baseTestConfig(h, mcpAuthStore{mcpRowsFake: rows})
	cfg.Approvals, cfg.Broker = h.approvals, h.broker
	cfg.Secrets = getErrStore{getErr: secretstore.ErrNotFound}
	f := &mcpAuthFixture{h: h, rows: rows, runID: runID, grant: grant, tok: id.Token, tokID: id, agent: h.mintRunToken(t, runID)}
	if edit != nil {
		edit(&cfg, f)
	}
	f.srv = New(cfg)
	return f
}

const (
	mcpDecisionsPath = "/api/v1/internal/decisions"
	mcpApprovalsPath = "/api/v1/internal/approvals"
	mcpAllowDecision = `{"request":{"host":"api.files.test","method":"GET"},"decision":"allow","rule_source":"policy:allowlist"}`
	mcpDenyDecision  = `{"request":{"host":"evil.test","method":"GET"},"decision":"deny","rule_source":"policy:denied"}`
	mcpEgressRaise   = `{"kind":"egress_domain","requested_scope":{"host":"pkg.files.test","mode":"wait_for_review"}}`
)

// mcpRowsLabelled is how many of the harness's audit rows carry mcp_sandbox=key.
func mcpRowsLabelled(h *harness, key string) int {
	n := 0
	for _, ev := range h.audit.snapshot() {
		var data map[string]any
		if json.Unmarshal(ev.Data, &data) == nil && data["mcp_sandbox"] == key {
			n++
		}
	}
	return n
}

// mcpActionRows counts the rows of action, and those of them labelled with the MCP server key.
func mcpActionRows(h *harness, action, key string) (n, labelled int) {
	for _, ev := range h.audit.snapshot() {
		if ev.Action != action {
			continue
		}
		n++
		if audit.MCPSandboxOfData(ev.Data) == key {
			labelled++
		}
	}
	return n, labelled
}

func authFailRows(h *harness, reason string) int {
	n := 0
	for _, ev := range h.audit.snapshot() {
		var data map[string]any
		if ev.Action == "auth.fail" && json.Unmarshal(ev.Data, &data) == nil && data["reason"] == reason {
			n++
		}
	}
	return n
}

// mcpSandboxFiveRoutes is the spec's list, written out here so that a sixth entry in mcpSandboxRoutes fails
// a test rather than passing the walk below, which would skip it as served.
var mcpSandboxFiveRoutes = []string{
	"POST /api/v1/internal/decisions",
	"GET /api/v1/internal/injection/{grantID}",
	"POST /api/v1/internal/token/renew",
	"POST /api/v1/internal/approvals",
	"GET /api/v1/internal/approvals/{id}",
}

// TestMCPTokenRefusedOnRunRoutes walks every /internal/ route, inside the internal group and outside it, and
// refuses an MCP proxy's token on each one the MCP door does not serve. The door's table must equal the
// spec's five exactly.
func TestMCPTokenRefusedOnRunRoutes(t *testing.T) {
	five := map[string]bool{}
	for _, route := range mcpSandboxFiveRoutes {
		five[route] = true
		if _, ok := mcpSandboxRoutes[route]; !ok {
			t.Errorf("the MCP door does not serve %s", route)
		}
	}
	if len(mcpSandboxRoutes) != len(five) {
		t.Errorf("the MCP door serves %d routes, want exactly the %d listed", len(mcpSandboxRoutes), len(five))
	}
	f := newMCPAuthFixture(t)
	// With no provider sending identity, gatewayMintOffGate answers the mint path as one no route holds, before
	// any door: the walk turns identity on so that path reaches its door too.
	testGatewayIdentityOn(f.srv)
	routes := map[string]bool{}
	walk := func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if strings.Contains(route, "/internal/") {
			routes[method+" "+route] = true
		}
		return nil
	}
	if err := chi.Walk(f.srv.router, walk); err != nil {
		t.Fatal(err)
	}
	params := regexp.MustCompile(`\{[^}]*\}`)
	refused := 0
	for route := range routes {
		if five[route] {
			continue
		}
		method, pattern, _ := strings.Cut(route, " ")
		path := strings.TrimSuffix(params.ReplaceAllString(pattern, uuid.NewString()), "/*") + "/x"
		if !strings.HasSuffix(pattern, "/*") {
			path = params.ReplaceAllString(pattern, uuid.NewString())
		}
		if w := do(t, f.srv, method, path, f.tok, `{}`); w.Code != http.StatusUnauthorized && w.Code != http.StatusForbidden {
			t.Errorf("%s: an MCP token got %d %s, want a refusal", route, w.Code, w.Body.String())
		}
		refused++
	}
	for _, route := range mcpSandboxFiveRoutes {
		if !routes[route] {
			t.Errorf("the MCP door serves %s, which is not a route", route)
		}
	}
	if refused < 15 {
		t.Errorf("walked %d routes outside the MCP door, want the whole internal surface", refused)
	}
}

// TestMCPTokenServesItsFiveRoutes: an MCP proxy's token reaches the handler of each of its five routes, its
// rows labelled with its server.
func TestMCPTokenServesItsFiveRoutes(t *testing.T) {
	f := newMCPAuthFixture(t)
	if w := do(t, f.srv, http.MethodPost, mcpDecisionsPath, f.tok, mcpDenyDecision); w.Code != http.StatusAccepted {
		t.Fatalf("decisions: %d %s", w.Code, w.Body.String())
	}
	w := do(t, f.srv, http.MethodPost, mcpApprovalsPath, f.tok, mcpEgressRaise)
	if w.Code != http.StatusCreated {
		t.Fatalf("approvals: %d %s", w.Code, w.Body.String())
	}
	var raised types.ApprovalRequest
	if err := json.Unmarshal(w.Body.Bytes(), &raised); err != nil {
		t.Fatal(err)
	}
	if w := do(t, f.srv, http.MethodGet, mcpApprovalsPath+"/"+raised.ID.String(), f.tok, ""); w.Code != http.StatusOK {
		t.Fatalf("approval GET: %d %s", w.Code, w.Body.String())
	}
	// The injection route is the MCP door's: a grant the server does not hold is its refusal, not internalAuth's.
	if w := do(t, f.srv, http.MethodGet, "/api/v1/internal/injection/"+uuid.NewString(), f.tok, ""); w.Code != http.StatusForbidden || reasonOf(t, w) != reasonMCPSandboxUnknown {
		t.Fatalf("injection: %d %s", w.Code, w.Body.String())
	}
	if w := do(t, f.srv, http.MethodPost, "/api/v1/internal/token/renew", f.tok, ""); w.Code != http.StatusOK {
		t.Fatalf("renew: %d %s", w.Code, w.Body.String())
	}
	if n := mcpRowsLabelled(f.h, "files"); n < 3 {
		t.Errorf("rows labelled files = %d, want the decision, the renewal and the refusal", n)
	}
}

// TestMCPTokenResolvesOnlyItsGrants: the injection door resolves a grant only when the server's row holds it.
func TestMCPTokenResolvesOnlyItsGrants(t *testing.T) {
	f := newMCPAuthFixture(t)
	cfg := baseTestConfig(f.h, nil) // no store: the withheld check reads nothing, so the broker is the door
	cfg.Broker, cfg.Secrets = f.h.broker, getErrStore{getErr: secretstore.ErrNotFound}
	s := New(cfg)
	serve := func(grant uuid.UUID) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/internal/injection/"+grant.String(), nil)
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("grantID", grant.String())
		ctx := context.WithValue(contextWithClaims(r.Context(), &identity.Claims{RunID: f.runID, JTI: f.tokID.JTI}), chi.RouteCtxKey, rctx)
		ctx = context.WithValue(audit.WithMCPSandbox(ctx, "files"), mcpSandboxRowKey{}, f.rows.first())
		w := httptest.NewRecorder()
		s.handleMCPSandboxInjection(w, r.WithContext(ctx))
		return w
	}
	if w := serve(uuid.New()); w.Code != http.StatusForbidden || reasonOf(t, w) != reasonMCPSandboxUnknown || f.h.broker.lastCall != nil {
		t.Fatalf("another grant: %d %s, broker asked %v", w.Code, w.Body.String(), f.h.broker.lastCall != nil)
	}
	serve(f.grant)
	if f.h.broker.lastCall == nil || f.h.broker.lastCall.RunID != f.runID {
		t.Fatal("the server's own grant did not reach the broker")
	}
}

// TestMCPAudienceClaimForgedRefused: the run's internal token with its aud rewritten to the MCP audience is
// routed to the MCP door and fails its verification.
func TestMCPAudienceClaimForgedRefused(t *testing.T) {
	f := newMCPAuthFixture(t)
	parts := strings.Split(f.agent, ".")
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatal(err)
	}
	claims["aud"] = mcpSandboxAudience
	forgedPayload, _ := json.Marshal(claims)
	forged := parts[0] + "." + base64.RawURLEncoding.EncodeToString(forgedPayload) + "." + parts[2]
	if w := do(t, f.srv, http.MethodPost, mcpDecisionsPath, forged, mcpAllowDecision); w.Code != http.StatusUnauthorized {
		t.Fatalf("forged aud: %d %s, want 401", w.Code, w.Body.String())
	}
}

// TestMCPSandboxAudienceMatchesProxy ties the server's MCP audience to the one the proxy package declares, so
// the two constants cannot drift apart.
func TestMCPSandboxAudienceMatchesProxy(t *testing.T) {
	if mcpSandboxAudience != proxy.MCPSandboxAudience {
		t.Fatalf("server audience %q, proxy audience %q: want them equal", mcpSandboxAudience, proxy.MCPSandboxAudience)
	}
}

// mcpRequest is one request on the MCP door.
type mcpRequest struct{ method, path, body string }

// fiveRequests is one request on each of the MCP door's five routes, each one the fixture's row would be
// served on.
func (f *mcpAuthFixture) fiveRequests() []mcpRequest {
	return []mcpRequest{
		{http.MethodPost, mcpDecisionsPath, mcpAllowDecision},
		{http.MethodGet, "/api/v1/internal/injection/" + f.grant.String(), ""},
		{http.MethodPost, "/api/v1/internal/token/renew", `{}`},
		{http.MethodPost, mcpApprovalsPath, mcpEgressRaise},
		{http.MethodGet, mcpApprovalsPath + "/" + uuid.NewString(), ""},
	}
}

// mcpRunStateStore is the MCP fixture's store with its run read as run.
type mcpRunStateStore struct {
	mcpAuthStore
	run types.AgentRun
}

func (s mcpRunStateStore) GetRun(_ context.Context, id uuid.UUID) (types.AgentRun, error) {
	r := s.run
	r.ID = id
	return r, nil
}

// TestMCPDoorHoldsRunGates: the MCP door holds its run to internalAuth's liveness and relay gates. On a run
// that ended past its tail grace each of the five routes answers 403 run_terminal, and on a run placed on a
// runner, reached without its relay, 403 relay_required; the same token on the live run is served.
func TestMCPDoorHoldsRunGates(t *testing.T) {
	runner := uuid.New()
	for _, c := range []struct {
		name, reason string
		run          types.AgentRun
	}{
		{"ended", string(authz.ReasonRunTerminal), types.AgentRun{State: types.RunCompleted, UpdatedAt: time.Now().UTC().Add(-time.Hour)}},
		{"relay bound", string(placement.ReasonRelayRequired), types.AgentRun{State: types.RunRunning, Placement: types.PlacementLocal,
			RunnerID: &runner, UpdatedAt: time.Now().UTC()}},
	} {
		f := newMCPAuthFixtureWith(t, func(cfg *Config, _ *mcpAuthFixture) {
			cfg.Store = mcpRunStateStore{mcpAuthStore: cfg.Store.(mcpAuthStore), run: c.run}
		})
		for _, rq := range f.fiveRequests() {
			if w := do(t, f.srv, rq.method, rq.path, f.tok, rq.body); w.Code != http.StatusForbidden || reasonOf(t, w) != c.reason {
				t.Errorf("%s: %s %s = %d %s, want 403 %s", c.name, rq.method, rq.path, w.Code, w.Body.String(), c.reason)
			}
		}
	}
	f := newMCPAuthFixture(t)
	if w := do(t, f.srv, http.MethodPost, mcpDecisionsPath, f.tok, mcpAllowDecision); w.Code != http.StatusAccepted {
		t.Fatalf("control: the live run's decision = %d %s, want 202", w.Code, w.Body.String())
	}
}

// mcpRunReadFailStore is an MCP door store whose run read fails.
type mcpRunReadFailStore struct{ mcpAuthStore }

func (mcpRunReadFailStore) GetRun(context.Context, uuid.UUID) (types.AgentRun, error) {
	return types.AgentRun{}, errors.New("store: get run: connection reset")
}

// TestMCPDoorGateRowsNameTheServer: the rows the MCP door's run gates write at the decision door (the refusal of
// a run that ended or of a post off its relay, and the note of a 503 for a run it could not read) are the
// server's, labelled with its key. The agent's post through the same gate still writes its own, unlabelled.
func TestMCPDoorGateRowsNameTheServer(t *testing.T) {
	runner := uuid.New()
	ended := types.AgentRun{State: types.RunCompleted, UpdatedAt: time.Now().UTC().Add(-time.Hour)}
	bound := types.AgentRun{State: types.RunRunning, Placement: types.PlacementLocal, RunnerID: &runner, UpdatedAt: time.Now().UTC()}
	for _, c := range []struct {
		name, action string
		code         int
		store        func(mcpAuthStore) store.Store
	}{
		{"ended", authz.AuditAction, http.StatusForbidden, func(s mcpAuthStore) store.Store { return mcpRunStateStore{s, ended} }},
		{"relay bound", authz.AuditAction, http.StatusForbidden, func(s mcpAuthStore) store.Store { return mcpRunStateStore{s, bound} }},
		{"run unreadable", runReportLostAction, http.StatusServiceUnavailable, func(s mcpAuthStore) store.Store { return mcpRunReadFailStore{s} }},
	} {
		f := newMCPAuthFixtureWith(t, func(cfg *Config, _ *mcpAuthFixture) { cfg.Store = c.store(cfg.Store.(mcpAuthStore)) })
		if w := do(t, f.srv, http.MethodPost, mcpDecisionsPath, f.tok, mcpAllowDecision); w.Code != c.code {
			t.Fatalf("%s: the MCP proxy's post = %d %s, want %d", c.name, w.Code, w.Body.String(), c.code)
		}
		if n, labelled := mcpActionRows(f.h, c.action, "files"); n != 1 || labelled != 1 {
			t.Errorf("%s: %s rows = %d (%d labelled files), want one, labelled", c.name, c.action, n, labelled)
		}
		if w := do(t, f.srv, http.MethodPost, mcpDecisionsPath, f.agent, mcpAllowDecision); w.Code != c.code {
			t.Fatalf("%s: control: the agent's post = %d %s, want %d", c.name, w.Code, w.Body.String(), c.code)
		}
		if n, labelled := mcpActionRows(f.h, c.action, "files"); n != 2 || labelled != 1 {
			t.Errorf("%s: control: %s rows = %d (%d labelled files), want the agent's, unlabelled, beside it", c.name, c.action, n, labelled)
		}
	}
}

// TestMCPRowWithEmptyKeyRefused: a row whose server key is empty would label its proxy's rows as the agent's,
// so the door refuses its token on every one of the five routes as it refuses an unknown one.
func TestMCPRowWithEmptyKeyRefused(t *testing.T) {
	f := newMCPAuthFixture(t)
	f.rows.set(func(r *store.RunMCPSandbox) { r.ServerKey = "" })
	for _, c := range f.fiveRequests() {
		w := do(t, f.srv, c.method, c.path, f.tok, c.body)
		if w.Code != http.StatusForbidden || reasonOf(t, w) != reasonMCPSandboxUnknown {
			t.Errorf("%s %s with an empty key: %d %s, want 403 %s", c.method, c.path, w.Code, w.Body.String(), reasonMCPSandboxUnknown)
		}
	}
	allows := func() int {
		n := 0
		for _, ev := range f.h.audit.snapshot() {
			if ev.Action == "egress.allow" {
				n++
			}
		}
		return n
	}
	if n := allows(); n != 0 {
		t.Errorf("%d egress.allow rows landed for a row with no key, want none", n)
	}
	f.rows.set(func(r *store.RunMCPSandbox) { r.ServerKey = "files" })
	if w := do(t, f.srv, http.MethodPost, mcpDecisionsPath, f.tok, mcpAllowDecision); w.Code/100 != 2 || allows() != 1 {
		t.Fatalf("control: the same row keyed files got %d %s and %d allow rows, want it served", w.Code, w.Body.String(), allows())
	}
}

// expiringIdentity answers every token in expired as an expired token of runID, and verifies the rest.
type expiringIdentity struct {
	identity.Provider
	runID   uuid.UUID
	expired map[string]bool
}

func (e *expiringIdentity) Verify(ctx context.Context, tok, aud string) (*identity.Claims, error) {
	if e.expired[tok] {
		return nil, &identity.ExpiredTokenError{RunID: e.runID, Expiry: time.Now().Add(-time.Minute), Err: errors.New("token is expired (exp)")}
	}
	return e.Provider.Verify(ctx, tok, aud)
}

// TestMCPExpiredTokenSpendsNoAgentRow: an MCP proxy's expired token is refused with one auth.fail row
// labelled with its server, and writes neither run.identity.expire nor run.report.drop, so the agent's own
// expired token afterwards still gets both.
func TestMCPExpiredTokenSpendsNoAgentRow(t *testing.T) {
	var idp *expiringIdentity
	f := newMCPAuthFixtureWith(t, func(cfg *Config, f *mcpAuthFixture) {
		idp = &expiringIdentity{Provider: cfg.Identity, runID: f.runID, expired: map[string]bool{f.tok: true}}
		cfg.Identity = idp
	})
	actions := func(action string) (n, labelled int) {
		for _, ev := range f.h.audit.snapshot() {
			var data map[string]any
			if ev.Action == action {
				n++
				if json.Unmarshal(ev.Data, &data) == nil && data["mcp_sandbox"] == "files" {
					labelled++
				}
			}
		}
		return n, labelled
	}
	w := do(t, f.srv, http.MethodPost, mcpDecisionsPath, f.tok, mcpAllowDecision)
	if w.Code != http.StatusUnauthorized || reasonOf(t, w) != reasonInvalidRunToken {
		t.Fatalf("expired MCP token: %d %s, want 401 %s", w.Code, w.Body.String(), reasonInvalidRunToken)
	}
	if n, labelled := actions("auth.fail"); n != 1 || labelled != 1 {
		t.Errorf("auth.fail rows = %d (%d labelled files), want one, labelled", n, labelled)
	}
	for _, a := range []string{"run.identity.expire", runReportLostAction} {
		if n, _ := actions(a); n != 0 {
			t.Errorf("the MCP proxy's expired token wrote %d %s rows, want none", n, a)
		}
	}
	idp.expired[f.agent] = true
	if w := do(t, f.srv, http.MethodPost, mcpDecisionsPath, f.agent, mcpAllowDecision); w.Code != http.StatusUnauthorized {
		t.Fatalf("control: the agent's expired token got %d %s, want 401", w.Code, w.Body.String())
	}
	for _, a := range []string{"run.identity.expire", runReportLostAction} {
		if n, labelled := actions(a); n != 1 || labelled != 0 {
			t.Errorf("control: the agent's expired token wrote %d %s rows (%d labelled), want one, unlabelled", n, a, labelled)
		}
	}
}

// TestMCPTokenRelayedRefused: no run reached through a runner has an MCP sandbox, so a relayed MCP token is
// refused.
func TestMCPTokenRelayedRefused(t *testing.T) {
	f := newMCPAuthFixture(t)
	relayed := func(tb testing.TB, h http.Handler) http.Handler {
		inner := panicFails(tb, h)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			inner.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), relayRunnerKey{}, uuid.New())))
		})
	}
	w := doVia(t, relayed, f.srv, http.MethodPost, mcpDecisionsPath, f.tok, mcpAllowDecision)
	if w.Code < 400 || reasonOf(t, w) != string(placement.ReasonRelayRouteRefused) {
		t.Fatalf("relayed: %d %s, want %s", w.Code, w.Body.String(), placement.ReasonRelayRouteRefused)
	}
}

// TestMCPDropLeavesTheAgentsLostPost: an MCP proxy's post the decision door cannot take claims a row of its own
// (claimDecisionRefusal), so the agent's lost post within that claim's span still writes the agent's
// unlabelled run.report.drop, the row that fails its confined replay.
func TestMCPDropLeavesTheAgentsLostPost(t *testing.T) {
	f := newMCPAuthFixture(t)
	for _, tok := range []string{f.tok, f.agent} {
		if w := do(t, f.srv, http.MethodPost, mcpDecisionsPath, tok, `{"request":`); w.Code != http.StatusBadRequest ||
			reasonOf(t, w) != reasonInternalDecisionLogInvalid {
			t.Fatalf("a post that does not decode: %d %s, want 400 %s", w.Code, w.Body.String(), reasonInternalDecisionLogInvalid)
		}
	}
	if n, labelled := mcpActionRows(f.h, runReportLostAction, "files"); n != 2 || labelled != 1 {
		t.Fatalf("%s rows = %d (%d labelled files), want the MCP proxy's, labelled, and the agent's, unlabelled", runReportLostAction, n, labelled)
	}
}

// TestMCPProxyDecisionsLand: the MCP proxy's decision rows are stored on the run, labelled with its server.
func TestMCPProxyDecisionsLand(t *testing.T) {
	f := newMCPAuthFixture(t)
	relay := `{"request":{"host":"api.files.test","method":"POST","path":"/mcp"},"decision":"allow","rule_source":"relay:mcp"}`
	dns := `{"request":{},"dns":{"name":"api.files.test","type":"A","outcome":"refused","via":"proxy"}}`
	for _, body := range []string{mcpDenyDecision, mcpAllowDecision, relay, dns} {
		if w := do(t, f.srv, http.MethodPost, mcpDecisionsPath, f.tok, body); w.Code != http.StatusAccepted {
			t.Fatalf("decision: %d %s", w.Code, w.Body.String())
		}
	}
	landed := map[string]bool{}
	for _, ev := range f.h.audit.snapshot() {
		var data map[string]any
		if json.Unmarshal(ev.Data, &data) == nil && data["mcp_sandbox"] == "files" {
			landed[ev.Action+" "+fmt.Sprint(data["rule_source"])] = true
		}
	}
	for _, want := range []string{"egress.deny policy:denied", "egress.allow relay:mcp", egress.DNSQueryAction + " <nil>"} {
		if !landed[want] {
			t.Errorf("no row %q labelled files; labelled rows: %v", want, landed)
		}
	}
	n := mcpRowsLabelled(f.h, "files")
	if w := do(t, f.srv, http.MethodPost, mcpDecisionsPath, f.agent, mcpDenyDecision); w.Code != http.StatusAccepted {
		t.Fatalf("the agent's decision: %d", w.Code)
	}
	if got := mcpRowsLabelled(f.h, "files"); got != n {
		t.Fatalf("the agent's decision row is labelled with the MCP server")
	}
}

// TestMCPFirstUseHoldReachesApprovals: the MCP proxy's first-use hold is a PENDING egress_domain row labelled
// with its server, read back by its GET; a scope naming mcp_sandbox is refused from either token.
func TestMCPFirstUseHoldReachesApprovals(t *testing.T) {
	f := newMCPAuthFixture(t)
	w := do(t, f.srv, http.MethodPost, mcpApprovalsPath, f.tok, mcpEgressRaise)
	var raised types.ApprovalRequest
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &raised) != nil {
		t.Fatalf("raise: %d %s", w.Code, w.Body.String())
	}
	if raised.State != types.ApprovalPending || raised.Kind != types.ApprovalEgressDomain || mcpSandboxKeyOf(raised) != "files" || approvalHost(raised) != "pkg.files.test" {
		t.Fatalf("raised %+v (%s)", raised, raised.RequestedScope)
	}
	if w := do(t, f.srv, http.MethodGet, mcpApprovalsPath+"/"+raised.ID.String(), f.tok, ""); w.Code != http.StatusOK {
		t.Fatalf("GET: %d %s", w.Code, w.Body.String())
	}
	forged := `{"kind":"egress_domain","requested_scope":{"host":"pkg.files.test","MCP_Sandbox":"files"}}`
	for name, tok := range map[string]string{"internal": f.agent, "mcp": f.tok} {
		before := authFailRows(f.h, reasonReservedScopeKey)
		if w := do(t, f.srv, http.MethodPost, mcpApprovalsPath, tok, forged); w.Code != http.StatusBadRequest || reasonOf(t, w) != reasonReservedScopeKey {
			t.Errorf("%s token naming mcp_sandbox: %d %s", name, w.Code, w.Body.String())
		}
		if authFailRows(f.h, reasonReservedScopeKey) != before+1 {
			t.Errorf("%s token naming mcp_sandbox: no auth.fail row", name)
		}
	}
}

// TestMCPHoldFloodLeavesAgentRaise: an MCP server's holds have a cap of their own, 1024 for its server and
// answered with mcp_sandbox_approval_cap, and are left out of the run's cap, so a server that has filled its
// cap, or the run's, leaves the agent's raise working. The run's own cap still refuses the agent.
func TestMCPHoldFloodLeavesAgentRaise(t *testing.T) {
	f := newMCPAuthFixture(t)
	seed := func(n int, key string) {
		f.h.approvals.mu.Lock()
		defer f.h.approvals.mu.Unlock()
		for range n {
			id := uuid.New()
			f.h.approvals.byID[id] = types.ApprovalRequest{ID: id, RunID: f.runID, Kind: types.ApprovalEgressDomain, State: types.ApprovalPending,
				RequestedScope: json.RawMessage(`{"host":"h` + id.String() + `.test","mcp_sandbox":"` + key + `"}`)}
		}
	}
	raise := func(tok, host string) *httptest.ResponseRecorder {
		return do(t, f.srv, http.MethodPost, mcpApprovalsPath, tok, `{"kind":"egress_domain","requested_scope":{"host":"`+host+`","mode":"wait_for_review"}}`)
	}
	seed(maxMCPHoldsPerServer-1, "files")
	seed(maxMCPHoldsPerServer, "other") // another server's holds are not this one's
	if w := raise(f.tok, "last.files.test"); w.Code != http.StatusCreated {
		t.Fatalf("the server's last free slot: %d %s, want 201", w.Code, w.Body.String())
	}
	if w := raise(f.tok, "over.files.test"); w.Code != http.StatusTooManyRequests || reasonOf(t, w) != reasonMCPSandboxApprovalCap {
		t.Fatalf("past the server's cap: %d %s, want 429 %s", w.Code, w.Body.String(), reasonMCPSandboxApprovalCap)
	}
	seed(maxApprovalsPerRun, "files")
	if w := raise(f.agent, "agent.test"); w.Code != http.StatusCreated {
		t.Fatalf("the agent's raise after the flood: %d %s, want 201", w.Code, w.Body.String())
	}
	f.h.approvals.mu.Lock()
	f.h.approvals.countForRun = maxApprovalsPerRun
	f.h.approvals.mu.Unlock()
	if w := raise(f.agent, "agent2.test"); w.Code != http.StatusTooManyRequests || reasonOf(t, w) != reasonInternalApprovalCapReached {
		t.Fatalf("control: the agent at the run's cap: %d %s, want 429 %s", w.Code, w.Body.String(), reasonInternalApprovalCapReached)
	}
}

// TestMCPTokenRaisesEgressHoldsOnly: no MCP policy holds a hold rule (the finish makes each a deny), and as a
// backstop an MCP token raising any other kind is refused.
func TestMCPTokenRaisesEgressHoldsOnly(t *testing.T) {
	run := mcpPolicyRun() // holds GET a.test/admin with a hold host rule
	if d := types.EvalHostRules(run.HostRules, "a.test", http.MethodGet, "/admin"); d.Effect != types.HostRuleHold {
		t.Fatalf("fixture: the run's policy gives a.test/admin %s, want hold", d.Effect)
	}
	ceiling := governanceCeiling{Spec: types.RunPolicySpec{AllowedDomains: []string{"a.test"}, HostRules: run.HostRules}}
	for _, pol := range []*types.ComponentMCPSandboxPolicy{
		nil,
		{Mode: types.ComponentMCPSandboxNarrower, Overlay: &types.CeilingOverlay{}},
		{Mode: types.ComponentMCPSandboxOwn, Spec: &types.RunPolicySpec{AllowedDomains: []string{"a.test"}}},
	} {
		p := mcpPolicyOf(t, &Server{}, mcpPolicyBase{run: run, ceiling: ceiling, comps: mcpPolicyComps(pol)})
		for _, set := range p.spec.HostRules {
			for _, r := range set.Rules {
				if r.Effect == types.HostRuleHold {
					t.Errorf("%s: a hold rule %+v is left in the MCP policy", p.mode, r)
				}
			}
		}
		if hostRuleEffects(p.spec.HostRules)["a.test"] != types.HostRuleDeny {
			t.Errorf("%s: the hold on a.test is %+v, want deny", p.mode, p.spec.HostRules)
		}
		// What a proxy loaded with this policy decides: the host is on its list, and the rule the proxy evaluates
		// (CompilePolicy keeps the spec's host rules, hostRuleVerdict asks types.EvalHostRules) refuses the
		// request outright. Only a hold would have it raise an approval.
		if v, err := proxy.NewBuiltinEvaluator(p.spec).EvaluateHost(context.Background(), egress.Request{Host: "a.test", Port: 443}); err != nil || v != egress.VerdictAllow {
			t.Errorf("%s: the proxy's host verdict for a.test = %v, %v; want allow", p.mode, v, err)
		}
		if d := types.EvalHostRules(p.spec.HostRules, "a.test", http.MethodGet, "/admin"); d.Effect != types.HostRuleDeny {
			t.Errorf("%s: the proxy's host rule decision for GET a.test/admin = %s, want deny with no raise", p.mode, d.Effect)
		}
	}
	f := newMCPAuthFixture(t)
	for _, body := range []string{
		`{"kind":"tool_call","requested_scope":{"tool":"files.read"}}`,
		`{"kind":"human_takeover","requested_scope":{"reason":"look"}}`,
		`{"kind":"push_content","requested_scope":{"ref":"main"}}`,
	} {
		if w := do(t, f.srv, http.MethodPost, mcpApprovalsPath, f.tok, body); w.Code != http.StatusBadRequest || reasonOf(t, w) != reasonUnsupportedInternalApprovalKind {
			t.Errorf("%s: %d %s", body, w.Code, w.Body.String())
		}
	}
}

// TestMCPHoldRejoinAndGetHeldToLabel: each token rejoins and reads only rows of its own label; a refused
// rejoin stamps nothing.
func TestMCPHoldRejoinAndGetHeldToLabel(t *testing.T) {
	f := newMCPAuthFixture(t)
	raise := func(tok string) types.ApprovalRequest {
		t.Helper()
		w := do(t, f.srv, http.MethodPost, mcpApprovalsPath, tok, mcpEgressRaise)
		var ap types.ApprovalRequest
		if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &ap) != nil {
			t.Fatalf("raise: %d %s", w.Code, w.Body.String())
		}
		return ap
	}
	mcpRow, agentRow := raise(f.tok), raise(f.agent)
	if mcpRow.ID == agentRow.ID {
		t.Fatal("the MCP hold and the agent's hold for one host are one row")
	}
	rejoin := func(id uuid.UUID) string {
		return `{"kind":"egress_domain","requested_scope":{"host":"pkg.files.test","mode":"wait_for_review"},"rejoin_id":"` + id.String() + `"}`
	}
	for _, c := range []struct {
		name   string
		tok    string
		row    types.ApprovalRequest
		wantOK bool
	}{
		{"mcp token, agent row", f.tok, agentRow, false},
		{"internal token, mcp row", f.agent, mcpRow, false},
		{"mcp token, own row", f.tok, mcpRow, true},
		{"internal token, own row", f.agent, agentRow, true},
	} {
		w := do(t, f.srv, http.MethodPost, mcpApprovalsPath, c.tok, rejoin(c.row.ID))
		stamped := f.h.approvals.byID[c.row.ID].LastRejoinedAt != nil
		if c.wantOK != (w.Code == http.StatusOK) || (!c.wantOK && (reasonOf(t, w) != reasonApprovalNotFound || stamped)) {
			t.Errorf("%s: rejoin %d %s, stamped %v", c.name, w.Code, w.Body.String(), stamped)
		}
		g := do(t, f.srv, http.MethodGet, mcpApprovalsPath+"/"+c.row.ID.String(), c.tok, "")
		if c.wantOK != (g.Code == http.StatusOK) || (!c.wantOK && reasonOf(t, g) != reasonApprovalNotFound) {
			t.Errorf("%s: GET %d %s", c.name, g.Code, g.Body.String())
		}
	}
}

// TestMCPRenewKeepsPrevJTIUntilExpiry: a renewal moves the row to the new token and keeps the old one served
// until it lapses; the old token cannot renew again.
func TestMCPRenewKeepsPrevJTIUntilExpiry(t *testing.T) {
	f := newMCPAuthFixture(t)
	w := do(t, f.srv, http.MethodPost, "/api/v1/internal/token/renew", f.tok, "")
	var renewed tokenRenewResponse
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &renewed) != nil || renewed.Token == "" {
		t.Fatalf("renew: %d %s", w.Code, w.Body.String())
	}
	row := f.rows.first()
	if row.CPTokenJTI == f.tokID.JTI || row.PrevCPTokenJTI != f.tokID.JTI || row.PrevCPTokenUntil == nil || row.PrevCPTokenUntil.Unix() != f.tokID.Expiry.Unix() {
		t.Fatalf("row after renew = %+v", row)
	}
	for name, tok := range map[string]string{"old": f.tok, "new": renewed.Token} {
		if w := do(t, f.srv, http.MethodPost, mcpDecisionsPath, tok, mcpAllowDecision); w.Code != http.StatusAccepted {
			t.Errorf("%s token: %d %s", name, w.Code, w.Body.String())
		}
	}
	if w := do(t, f.srv, http.MethodPost, "/api/v1/internal/token/renew", f.tok, ""); w.Code != http.StatusForbidden || reasonOf(t, w) != reasonMCPSandboxUnknown {
		t.Errorf("a renewal with the old token: %d %s", w.Code, w.Body.String())
	}
	past := time.Now().Add(-time.Second)
	f.rows.set(func(r *store.RunMCPSandbox) { r.PrevCPTokenUntil = &past })
	if w := do(t, f.srv, http.MethodPost, mcpDecisionsPath, f.tok, mcpAllowDecision); w.Code != http.StatusForbidden || reasonOf(t, w) != reasonMCPSandboxUnknown {
		t.Errorf("the old token past its until: %d %s", w.Code, w.Body.String())
	}
}

// TestMCPProxyTailRowsLandAfterEnd: a stopped server's proxy still lands its decisions within the tail grace
// of its stop; past it, or on another route, it is refused.
func TestMCPProxyTailRowsLandAfterEnd(t *testing.T) {
	f := newMCPAuthFixture(t)
	stop := func(ago time.Duration) {
		at := time.Now().Add(-ago)
		f.rows.set(func(r *store.RunMCPSandbox) { r.State, r.StoppedAt = store.MCPSandboxStopped, &at })
	}
	stop(time.Minute)
	if w := do(t, f.srv, http.MethodPost, mcpDecisionsPath, f.tok, mcpDenyDecision); w.Code != http.StatusAccepted {
		t.Fatalf("inside the grace: %d %s", w.Code, w.Body.String())
	}
	if mcpRowsLabelled(f.h, "files") == 0 {
		t.Fatal("the tail row is not labelled")
	}
	if w := do(t, f.srv, http.MethodPost, mcpApprovalsPath, f.tok, mcpEgressRaise); w.Code != http.StatusForbidden || reasonOf(t, w) != reasonMCPSandboxUnknown {
		t.Errorf("another route inside the grace: %d %s", w.Code, w.Body.String())
	}
	stop(terminalUploadGrace + time.Minute)
	if w := do(t, f.srv, http.MethodPost, mcpDecisionsPath, f.tok, mcpDenyDecision); w.Code != http.StatusForbidden || reasonOf(t, w) != reasonMCPSandboxUnknown {
		t.Errorf("past the grace: %d %s", w.Code, w.Body.String())
	}
}

// TestMCPTokenServedWithFlagOff: the MCP door does not read the switch, so a running sandbox keeps its trail
// with the switch turned off.
func TestMCPTokenServedWithFlagOff(t *testing.T) {
	t.Setenv(envMCPOwnSandbox, "")
	f := newMCPAuthFixture(t)
	if w := do(t, f.srv, http.MethodPost, mcpDecisionsPath, f.tok, mcpAllowDecision); w.Code != http.StatusAccepted {
		t.Fatalf("switch off: %d %s", w.Code, w.Body.String())
	}
}

// mcpPresenceStore is the pause fixture's store, counting touches, with MCP server rows.
type mcpPresenceStore struct {
	*presenceStore
	*mcpRowsFake
}

// TestMCPSandboxEgressIsNotRunActivity, on one run: the MCP proxy's allowed decision is stored and moves neither
// the idle clock nor the pause's presence clock; the agent's, inside the debounce window, moves both.
func TestMCPSandboxEgressIsNotRunActivity(t *testing.T) {
	var st *mcpPresenceStore
	f := newPauseFixture(t, time.Hour, func(c *Config) {
		st = &mcpPresenceStore{presenceStore: &presenceStore{pauseStore: c.Store.(*pauseStore)}, mcpRowsFake: &mcpRowsFake{}}
		c.Store = st
	})
	id := mcpProxyToken(t, f.h, f.run.ID)
	st.rows = []store.RunMCPSandbox{{RunID: f.run.ID, ServerKey: "files", State: store.MCPSandboxReady, CPTokenJTI: id.JTI}}
	if w := do(t, f.srv, http.MethodPost, mcpDecisionsPath, id.Token, mcpAllowDecision); w.Code != http.StatusAccepted {
		t.Fatalf("MCP decision: %d %s", w.Code, w.Body.String())
	}
	if mcpRowsLabelled(f.h, "files") == 0 {
		t.Fatal("the MCP decision was not stored")
	}
	if f.st.stamps != 0 || st.touched != 0 {
		t.Fatalf("after the MCP decision: stamps %d touches %d, want none", f.st.stamps, st.touched)
	}
	if w := do(t, f.srv, http.MethodPost, mcpDecisionsPath, f.h.mintRunToken(t, f.run.ID), mcpAllowDecision); w.Code != http.StatusAccepted {
		t.Fatalf("agent decision: %d %s", w.Code, w.Body.String())
	}
	if f.st.stamps != 1 || st.touched != 1 {
		t.Fatalf("after the agent's decision: stamps %d touches %d, want one each", f.st.stamps, st.touched)
	}
}

// getErrApprovals is the fake service whose Get fails.
type getErrApprovals struct{ *fakeApprovals }

func (getErrApprovals) Get(context.Context, uuid.UUID) (types.ApprovalRequest, error) {
	return types.ApprovalRequest{}, errors.New("approvals unreadable")
}

// TestInternalTokenUnchangedByMCPAuth: the agent's internal token gets the same answer through the router,
// with the MCP door mounted, as from internalAuth alone, and the raise refusals keep their order, status,
// reason and row.
// TestMCPDoorStepsAsideOnEveryRoute: on every /internal/ route the router has, the MCP door hands the agent's
// internal token to the next handler untouched, and an MCP proxy's token too except on its five routes.
func TestMCPDoorStepsAsideOnEveryRoute(t *testing.T) {
	f := newMCPAuthFixture(t)
	var patterns []string
	walk := func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if strings.Contains(route, "/internal/") {
			patterns = append(patterns, method+" "+route)
		}
		return nil
	}
	if err := chi.Walk(f.srv.router, walk); err != nil {
		t.Fatal(err)
	}
	five := map[string]bool{}
	for _, route := range mcpSandboxFiveRoutes {
		five[route] = true
	}
	for _, route := range patterns {
		method, pattern, _ := strings.Cut(route, " ")
		for name, tok := range map[string]string{"internal": f.agent, "mcp": f.tok} {
			rctx := chi.NewRouteContext()
			rctx.RoutePatterns = []string{pattern}
			r := httptest.NewRequest(method, "/x", strings.NewReader(`{}`))
			r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
			r.Header.Set("Authorization", "Bearer "+tok)
			passed := false
			f.srv.mcpSandboxAuth(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { passed = true })).ServeHTTP(httptest.NewRecorder(), r)
			if want := name == "internal" || !five[route]; passed != want {
				t.Errorf("%s token on %s: handed on %v, want %v", name, route, passed, want)
			}
		}
	}
	if len(patterns) < 20 {
		t.Fatalf("walked %d /internal/ routes, want the whole internal surface", len(patterns))
	}
}

func TestInternalTokenUnchangedByMCPAuth(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	tok := h.mintRunToken(t, runID)
	direct := func(handler http.HandlerFunc, method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		h.srv.internalAuth(handler).ServeHTTP(w, r)
		return w
	}
	for _, c := range []struct {
		name, path, body string
		handler          http.HandlerFunc
	}{
		{"decision", mcpDecisionsPath, mcpAllowDecision, h.srv.handlePostDecision},
		{"renew", "/api/v1/internal/token/renew", `{}`, h.srv.handleInternalTokenRenew},
		{"egress raise", mcpApprovalsPath, mcpEgressRaise, h.srv.handleInternalRequestApproval},
		{"credential raise with a made-up rejoin", mcpApprovalsPath, `{"kind":"credential","requested_scope":{"x":1},"rejoin_id":"` + uuid.NewString() + `"}`, h.srv.handleInternalRequestApproval},
		{"empty scope", mcpApprovalsPath, `{"kind":"egress_domain","rejoin_id":"` + uuid.NewString() + `"}`, h.srv.handleInternalRequestApproval},
		{"lane", mcpApprovalsPath, `{"kind":"egress_domain","requested_scope":{"lane":"ado"},"rejoin_id":"` + uuid.NewString() + `"}`, h.srv.handleInternalRequestApproval},
	} {
		routed := do(t, h.srv, http.MethodPost, c.path, tok, c.body)
		alone := direct(c.handler, http.MethodPost, c.path, c.body)
		// errorReasonOf, not reasonOf: a decision taken is a 202 with no body.
		if r, a := errorReasonOf(routed.Body.String()), errorReasonOf(alone.Body.String()); routed.Code != alone.Code || r != a {
			t.Errorf("%s: routed %d %q, internalAuth alone %d %q", c.name, routed.Code, r, alone.Code, a)
		}
	}
	// A fresh server, so the earlier refusals have not spent the auth.fail limiter's burst of five.
	h = newHarness(t)
	tok = h.mintRunToken(t, runID)
	for body, want := range map[string]string{
		`{"kind":"credential","requested_scope":{"x":1},"rejoin_id":"` + uuid.NewString() + `"}`: reasonUnsupportedInternalApprovalKind,
		`{"kind":"egress_domain","rejoin_id":"` + uuid.NewString() + `"}`:                        reasonMissingRequestedScope,
		`{"kind":"egress_domain","requested_scope":{"lane":"ado"}}`:                              reasonReservedScopeKey,
	} {
		before := authFailRows(h, want)
		if w := do(t, h.srv, http.MethodPost, mcpApprovalsPath, tok, body); w.Code != http.StatusBadRequest || reasonOf(t, w) != want || authFailRows(h, want) != before+1 {
			t.Errorf("%s: %d %s, want 400 %s and one auth.fail row", body, w.Code, w.Body.String(), want)
		}
	}
	h.srv.cfg.Approvals = getErrApprovals{h.approvals}
	rejoin := `{"kind":"egress_domain","requested_scope":{"host":"pkg.files.test"},"rejoin_id":"` + uuid.NewString() + `"}`
	if w := do(t, h.srv, http.MethodPost, mcpApprovalsPath, tok, rejoin); w.Code != http.StatusServiceUnavailable || reasonOf(t, w) != reasonInternalLivenessReadFailed {
		t.Errorf("a rejoin whose read fails: %d %s, want 503", w.Code, w.Body.String())
	}
}

// TestAgentMCPCallApprovalUnchangedByMCPAuth: the agent's approve-each raise for one MCP call, which names
// mcp_server, is admitted with its scope as stored without the MCP door, and gains no mcp_sandbox key.
func TestAgentMCPCallApprovalUnchangedByMCPAuth(t *testing.T) {
	scope := mcpCallScopeJSON(t, "jira", "delete_issue", `{"id":1}`)
	body := `{"kind":"tool_call","requested_scope":` + string(scope) + `}`
	stored := func(routed bool) (int, json.RawMessage) {
		h := newHarness(t)
		tok := h.mintRunToken(t, uuid.New())
		var code int
		if routed {
			code = do(t, h.srv, http.MethodPost, mcpApprovalsPath, tok, body).Code
		} else {
			r := httptest.NewRequest(http.MethodPost, mcpApprovalsPath, strings.NewReader(body))
			r.Header.Set("Authorization", "Bearer "+tok)
			w := httptest.NewRecorder()
			h.srv.internalAuth(http.HandlerFunc(h.srv.handleInternalRequestApproval)).ServeHTTP(w, r)
			code = w.Code
		}
		if len(h.approvals.requested) != 1 {
			t.Fatalf("routed=%v: %d rows, want 1", routed, len(h.approvals.requested))
		}
		return code, h.approvals.requested[0].RequestedScope
	}
	code, routed := stored(true)
	aloneCode, alone := stored(false)
	if code != http.StatusCreated || aloneCode != code || string(routed) != string(alone) {
		t.Fatalf("routed %d %s, without the MCP door %d %s", code, routed, aloneCode, alone)
	}
	if strings.Contains(string(routed), "mcp_sandbox") {
		t.Fatalf("the agent's call scope gained an MCP label: %s", routed)
	}
}
