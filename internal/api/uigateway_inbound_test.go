// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// uiTokenStore adds the API-token table to the gateway's fake store: the bearer arm resolves through it.
type uiTokenStore struct {
	*uiMemStore
	mu     sync.Mutex
	tokens map[string]types.APIToken
	// lookupErr, when set, fails every lookup (failed counts them): the store cannot answer.
	lookupErr error
	failed    int
	// hang, when set, holds every lookup until it is closed (hung counts them): the store does not answer.
	hang chan struct{}
	hung int
	// runErr and policyErr, when set, fail every read of the run and of its policy's audit events.
	runErr, policyErr error
}

func (s *uiTokenStore) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	s.mu.Lock()
	err := s.runErr
	s.mu.Unlock()
	if err != nil {
		return types.AgentRun{}, err
	}
	return s.uiMemStore.GetRun(ctx, id)
}

func (s *uiTokenStore) QueryAuditEvents(ctx context.Context, runID uuid.UUID, limit int) ([]types.AuditEvent, error) {
	s.mu.Lock()
	err := s.policyErr
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return s.uiMemStore.QueryAuditEvents(ctx, runID, limit)
}

// failReads makes every read of the run and of its policy fail with err (nil answers again).
func (s *uiTokenStore) failReads(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runErr, s.policyErr = err, err
}

// GetAPITokenByRaw answers as the store's lookup does: a revoked or expired token is no token.
func (s *uiTokenStore) GetAPITokenByRaw(_ context.Context, raw string) (types.APIToken, error) {
	s.mu.Lock()
	if hang := s.hang; hang != nil {
		s.hung++
		s.mu.Unlock()
		<-hang
		s.mu.Lock()
	}
	defer s.mu.Unlock()
	if s.lookupErr != nil {
		s.failed++
		return types.APIToken{}, s.lookupErr
	}
	t, ok := s.tokens[raw]
	if !ok || t.RevokedAt != nil || (t.ExpiresAt != nil && !time.Now().Before(*t.ExpiresAt)) {
		return types.APIToken{}, store.ErrNotFound
	}
	return t, nil
}

// failLookups makes every lookup fail with err (nil answers again) and returns how many have failed so far.
func (s *uiTokenStore) failLookups(err error) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lookupErr = err
	return s.failed
}

// failedLookups is how many lookups have failed.
func (s *uiTokenStore) failedLookups() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failed
}

// hangLookups holds every lookup from now until the test ends.
func (s *uiTokenStore) hangLookups(t *testing.T) {
	hang := make(chan struct{})
	t.Cleanup(func() { close(hang) })
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hang = hang
}

// hungLookups is how many lookups have been held.
func (s *uiTokenStore) hungLookups() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hung
}

// update changes the stored token raw, as a revoke or an expiry would.
func (s *uiTokenStore) update(raw string, change func(*types.APIToken)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.tokens[raw]
	change(&t)
	s.tokens[raw] = t
}

func (s *uiTokenStore) TouchAPIToken(context.Context, uuid.UUID, time.Time) error { return nil }

// inboundHarness is newUIHarness whose app "code" is declared inbound with callers, whose backend echoes
// the headers it was sent, and whose store holds an inbound token for the owner ("wdn_alice"), one for
// bob ("wdn_bob"), one for carol ("wdn_carol") and an API token for the owner ("wdn_api").
func inboundHarness(t *testing.T, callers ...string) (*uiHarness, chan http.Header) {
	t.Helper()
	seen := make(chan http.Header, 64)
	return inboundHarnessOn(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Clone()
		_, _ = io.WriteString(w, "sandbox app")
	}), callers...), seen
}

// inboundHarnessOn is inboundHarness with backend as the app.
func inboundHarnessOn(t *testing.T, backend http.Handler, callers ...string) *uiHarness {
	t.Helper()
	h := newUIHarness(t, backend)
	h.store.putEffectivePolicy(h.run.ID, types.RunPolicySpec{MinConfinementClass: types.CC2,
		UIApps: []types.UIApp{{Name: "code", Port: uiTestPort, Inbound: true, Callers: callers}}})
	token := func(principal, kind string) types.APIToken {
		return types.APIToken{ID: uuid.New(), Principal: principal, Email: principal + "@example.com", Kind: kind,
			CreatedAt: time.Now().Add(-time.Hour)}
	}
	h.srv.cfg.Store = &uiTokenStore{uiMemStore: h.store, tokens: map[string]types.APIToken{
		"wdn_alice": token(h.owner, types.APITokenKindInbound),
		"wdn_bob":   token("bob", types.APITokenKindInbound),
		"wdn_carol": token("carol", types.APITokenKindInbound),
		"wdn_api":   token(h.owner, types.APITokenKindAPI),
	}}
	return h
}

// call is one bearer request to the app, with mutate applied.
func (h *uiHarness) call(token string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.relay("/api/health?x=1", nil, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+token)
		if mutate != nil {
			mutate(r)
		}
	})
}

func respondedReason(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body errorBody
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return body.Reason
}

// TestUIGateway_InboundToken: an inbound token calls an inbound app as its owner or a named caller; every
// other bearer is the one 401, and a live token with no reach here the one 404.
func TestUIGateway_InboundToken(t *testing.T) {
	t.Run("the owner's token calls, audited without the query", func(t *testing.T) {
		h, _ := inboundHarness(t)
		if rec := h.call("wdn_alice", nil); rec.Code != http.StatusOK || rec.Body.String() != "sandbox app" {
			t.Fatalf("call = %d %s, want 200", rec.Code, rec.Body.String())
		}
		rows := h.audit.auditRows("inbound.allow", "success")
		if len(rows) != 1 || rows[0].Actor != h.owner || rows[0].Target != "code" {
			t.Fatalf("inbound.allow rows = %+v, want one by %s on code", rows, h.owner)
		}
		var data map[string]any
		_ = json.Unmarshal(rows[0].Data, &data)
		if data["via"] != "token" || data["caller"] != "owner" || data["path"] != "/api/health" || data["method"] != http.MethodGet || data["token_id"] == nil {
			t.Errorf("inbound.allow data = %v, want via token, caller owner, path /api/health, a token_id", data)
		}
	})

	t.Run("a named caller calls; the sandbox learns who, never the token", func(t *testing.T) {
		h, seen := inboundHarness(t, "user:bob")
		rec := h.call("wdn_bob", func(r *http.Request) {
			r.Header.Set("X-Wardyn-Caller", "alice")
			r.Header.Set("X-Wardyn-Run", "spoof")
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("call = %d %s, want 200", rec.Code, rec.Body.String())
		}
		got := <-seen
		if got.Get("Authorization") != "" || got.Get("X-Wardyn-Run") != "" {
			t.Errorf("the token or a client X-Wardyn-* header reached the sandbox: %v", got)
		}
		if got.Get("X-Wardyn-Caller") != "bob" || got.Get("X-Wardyn-Caller-Email") != "bob@example.com" {
			t.Errorf("caller headers = %q / %q, want bob", got.Get("X-Wardyn-Caller"), got.Get("X-Wardyn-Caller-Email"))
		}
		if !h.audit.hasDataValue("caller", "user:bob") {
			t.Errorf("no inbound.allow names the caller entry; rows: %v", h.audit.actions())
		}
	})

	for _, tc := range []struct{ name, token string }{
		{"an unknown token", "wdn_nobody"},
		{"an API token", "wdn_api"},
	} {
		t.Run(tc.name+" is 401", func(t *testing.T) {
			h, _ := inboundHarness(t)
			rec := h.call(tc.token, nil)
			if rec.Code != http.StatusUnauthorized || respondedReason(t, rec) != reasonInboundCredentialInvalid || rec.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Fatalf("call = %d %s %v, want 401 %s with WWW-Authenticate", rec.Code, rec.Body.String(), rec.Header(), reasonInboundCredentialInvalid)
			}
			if !h.audit.hasDataValue("reason", inboundDenyCredentialInvalid) {
				t.Errorf("no inbound.deny credential_invalid; reasons: %s", h.audit.dataReasons())
			}
		})
	}

	t.Run("no reach is one 404", func(t *testing.T) {
		answer := func(h *uiHarness, path string) (int, string) {
			rec := h.relay("", nil, func(r *http.Request) {
				r.URL.Path = path
				r.Header.Set("Authorization", "Bearer wdn_carol")
			})
			return rec.Code, rec.Body.String()
		}
		h, _ := inboundHarness(t, "user:bob")
		code, body := answer(h, uiRelayPrefix(h.run.ID, "code")+"/")
		for name, path := range map[string]string{
			"no run":    uiRelayPrefix(uuid.New(), "code") + "/",
			"no app":    uiRelayPrefix(h.run.ID, "other") + "/",
			"not yours": uiRelayPrefix(h.run.ID, "code") + "/",
		} {
			if c, b := answer(h, path); c != http.StatusNotFound || c != code || b != body {
				t.Errorf("%s = %d %s, want the one 404 %d %s", name, c, b, code, body)
			}
		}
		plain, _ := inboundHarness(t)
		plain.store.putEffectivePolicy(plain.run.ID, types.RunPolicySpec{MinConfinementClass: types.CC2,
			UIApps: []types.UIApp{{Name: "code", Port: uiTestPort}}})
		if c, _ := answer(plain, uiRelayPrefix(plain.run.ID, "code")+"/"); c != http.StatusNotFound {
			t.Errorf("an app that is not inbound = %d, want 404", c)
		}
		if !h.audit.hasDataValue("reason", inboundDenyNotFound) {
			t.Errorf("no inbound.deny not_found; reasons: %s", h.audit.dataReasons())
		}
	})

	t.Run("a background run's inbound app answers a token", func(t *testing.T) {
		h, _ := inboundHarness(t)
		h.run.Experience = types.ExperienceBackground
		h.store.putRun(h.run)
		if rec := h.call("wdn_alice", nil); rec.Code != http.StatusOK {
			t.Fatalf("call = %d %s, want 200", rec.Code, rec.Body.String())
		}
	})
}

// TestUIGateway_InboundRefusalsAreInboundDenies: a call to a run that is not running, or is kept, is refused
// before it is admitted (inbound.deny not_running, never inbound.allow); a profile that denies interactive
// sessions is inbound.deny unit_denies; and a bearer that proved no one is recorded with no actor.
func TestUIGateway_InboundRefusalsAreInboundDenies(t *testing.T) {
	for name, end := range map[string]func(run *types.AgentRun){
		"completed": func(run *types.AgentRun) { run.State = types.RunCompleted },
		"kept":      func(run *types.AgentRun) { at := time.Now(); run.LostAt = &at },
	} {
		t.Run("a "+name+" run is not_running", func(t *testing.T) {
			h, _ := inboundHarness(t)
			end(&h.run)
			h.store.putRun(h.run)
			if rec := h.call("wdn_alice", nil); rec.Code != http.StatusConflict || respondedReason(t, rec) != reasonUIGatewayNotRunning {
				t.Fatalf("call = %d %s, want 409 %s", rec.Code, rec.Body.String(), reasonUIGatewayNotRunning)
			}
			if rows := h.audit.auditRows("inbound.deny", "denied"); len(rows) != 1 || rows[0].Actor != h.owner || !h.audit.hasDataValue("reason", inboundDenyNotRunning) {
				t.Errorf("inbound.deny rows = %+v, want one not_running by %s", rows, h.owner)
			}
			if rows := h.audit.auditRows("inbound.allow", "success"); len(rows) != 0 {
				t.Errorf("inbound.allow rows = %+v, want none", rows)
			}
		})
	}

	t.Run("a profile denying interactive sessions is unit_denies", func(t *testing.T) {
		h, _ := inboundHarness(t)
		unit := types.GovernanceUnit{ID: uuid.New(), Name: "no-interactive", Limits: types.GovernanceLimits{DenyInteractive: true}}
		h.store.profiles = []types.GovernanceUnit{unit}
		h.run.UnitID = &unit.ID
		h.store.putRun(h.run)
		if rec := h.call("wdn_alice", nil); rec.Code != http.StatusForbidden {
			t.Fatalf("call = %d %s, want 403", rec.Code, rec.Body.String())
		}
		if !h.audit.hasDataValue("reason", inboundDenyUnitDenies) {
			t.Errorf("no inbound.deny unit_denies; reasons: %s", h.audit.dataReasons())
		}
		if n := len(h.audit.auditRows("inbound.allow", "success")) + len(h.audit.auditRows("ui.authorize", "denied")); n != 0 {
			t.Errorf("%d inbound.allow or ui.authorize rows beside the inbound.deny; actions: %v", n, h.audit.actions())
		}
	})

	// A store that cannot answer is no verdict on the caller: 503 run_unreadable, and no inbound.deny row.
	t.Run("a run or policy that cannot be read is 503 and no row", func(t *testing.T) {
		h, _ := inboundHarness(t)
		h.srv.cfg.Store.(*uiTokenStore).failReads(errors.New("connection refused"))
		if rec := h.call("wdn_alice", nil); rec.Code != http.StatusServiceUnavailable || respondedReason(t, rec) != reasonRunUnreadable {
			t.Fatalf("call = %d %s, want 503 %s", rec.Code, rec.Body.String(), reasonRunUnreadable)
		}
		if rows := h.audit.auditRows("inbound.deny", "denied"); len(rows) != 0 {
			t.Errorf("inbound.deny rows = %+v, want none", rows)
		}
		h.srv.cfg.Store.(*uiTokenStore).failReads(nil)
		if rec := h.call("wdn_alice", nil); rec.Code != http.StatusOK {
			t.Fatalf("call after the store answers = %d %s, want 200", rec.Code, rec.Body.String())
		}
	})

	t.Run("a run the store does not hold is 404 inbound_not_found", func(t *testing.T) {
		h, _ := inboundHarness(t)
		h.srv.cfg.Store.(*uiTokenStore).runErr = store.ErrNotFound
		if rec := h.call("wdn_alice", nil); rec.Code != http.StatusNotFound || respondedReason(t, rec) != reasonInboundNotFound {
			t.Fatalf("call = %d %s, want 404 %s", rec.Code, rec.Body.String(), reasonInboundNotFound)
		}
		if !h.audit.hasDataValue("reason", inboundDenyNotFound) {
			t.Errorf("no inbound.deny not_found; reasons: %s", h.audit.dataReasons())
		}
	})

	t.Run("a bearer that proved no one has no actor", func(t *testing.T) {
		h, _ := inboundHarness(t)
		if rec := h.call("wdn_nobody", nil); rec.Code != http.StatusUnauthorized {
			t.Fatalf("call = %d %s, want 401", rec.Code, rec.Body.String())
		}
		if rows := h.audit.auditRows("inbound.deny", "denied"); len(rows) != 1 || rows[0].Actor != "" {
			t.Errorf("inbound.deny rows = %+v, want one with actor \"\"", rows)
		}
	})
}

// TestUIGateway_InboundRateLimit: past the run's bucket a call is 429 with Retry-After, and audited.
func TestUIGateway_InboundRateLimit(t *testing.T) {
	h, _ := inboundHarness(t)
	for range int(inboundBurst) {
		h.srv.uiInbound.take(h.run.ID, "someone", h.clock.now(), "")
	}
	rec := h.call("wdn_alice", nil)
	if rec.Code != http.StatusTooManyRequests || respondedReason(t, rec) != reasonInboundRateLimited || rec.Header().Get("Retry-After") != "1" {
		t.Fatalf("call = %d %s, want 429 %s with Retry-After 1", rec.Code, rec.Body.String(), reasonInboundRateLimited)
	}
	if !h.audit.hasDataValue("reason", inboundDenyRateLimited) {
		t.Errorf("no inbound.deny rate_limited; reasons: %s", h.audit.dataReasons())
	}
	h.clock.advance(time.Second)
	if rec := h.call("wdn_alice", nil); rec.Code != http.StatusOK {
		t.Fatalf("a second later = %d %s, want 200", rec.Code, rec.Body.String())
	}
}

// TestUIGateway_InboundCallerRateLimit: a caller who spreads calls over many runs is bound by their own
// bucket, however full each run's is; another caller, and the run's own bucket, are untouched by it.
func TestUIGateway_InboundCallerRateLimit(t *testing.T) {
	h, _ := inboundHarness(t, "user:bob")
	for range int(inboundBurst) {
		if ok, _ := h.srv.uiInbound.take(uuid.New(), "bob", h.clock.now(), ""); !ok {
			t.Fatal("a caller's first calls, each on its own run, were refused")
		}
	}
	rec := h.call("wdn_bob", nil)
	if rec.Code != http.StatusTooManyRequests || respondedReason(t, rec) != reasonInboundRateLimited || rec.Header().Get("Retry-After") != "1" {
		t.Fatalf("bob's call = %d %s, want 429 %s with Retry-After 1", rec.Code, rec.Body.String(), reasonInboundRateLimited)
	}
	if !h.audit.hasDataValue("reason", inboundDenyRateLimited) {
		t.Errorf("no inbound.deny rate_limited; reasons: %s", h.audit.dataReasons())
	}
	if rec := h.call("wdn_alice", nil); rec.Code != http.StatusOK {
		t.Fatalf("alice's call = %d %s, want 200: bob's bucket is not hers", rec.Code, rec.Body.String())
	}
	h.clock.advance(time.Second)
	if rec := h.call("wdn_bob", nil); rec.Code != http.StatusOK {
		t.Fatalf("bob a second later = %d %s, want 200", rec.Code, rec.Body.String())
	}
}

// TestUIGateway_HostModeCrossSite: in host mode a session request from another site is refused unless it
// is a top-level navigation, and an Origin must be the request's own; path mode is unchanged.
func TestUIGateway_HostModeCrossSite(t *testing.T) {
	other := "https://" + uuid.NewString() + ".ui.example.com"
	for _, tc := range []struct {
		name    string
		host    bool
		headers map[string]string
		want    int
	}{
		{"a sibling run's WebSocket", true, map[string]string{"Sec-Fetch-Site": "same-site", "Sec-Fetch-Mode": "websocket", "Origin": other}, http.StatusForbidden},
		{"a sibling run's fetch", true, map[string]string{"Sec-Fetch-Site": "same-site", "Sec-Fetch-Mode": "cors", "Sec-Fetch-Dest": "empty"}, http.StatusForbidden},
		{"a foreign Origin with no fetch metadata", true, map[string]string{"Origin": other}, http.StatusForbidden},
		{"a top-level navigation from a link", true, map[string]string{"Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document"}, http.StatusOK},
		{"its own page's fetch", true, map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "https://{run}.ui.example.com"}, http.StatusOK},
		{"a client with no fetch metadata", true, nil, http.StatusOK},
		{"path mode is unchanged", false, map[string]string{"Sec-Fetch-Site": "same-site", "Sec-Fetch-Mode": "websocket", "Origin": other}, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newUIHarness(t, okBackend())
			host := "ui.example.com"
			if tc.host {
				h.srv.cfg.UIOriginTemplate = "https://{run}.ui.example.com"
				host = h.run.ID.String() + ".ui.example.com"
			}
			now := time.Now()
			cookie := &http.Cookie{Name: uiCookieName, Value: h.srv.encodeUISession(uiSession{Run: h.run.ID, App: "code", Port: uiTestPort,
				Principal: h.owner, Expires: now.Add(time.Hour).Unix(), IssuedAt: now.Unix(), AuthorizedAt: now.Unix()})}
			rec := h.relay("/", cookie, func(r *http.Request) {
				r.Host = host
				for k, v := range tc.headers {
					r.Header.Set(k, strings.ReplaceAll(v, "{run}", h.run.ID.String()))
				}
			})
			if rec.Code != tc.want {
				t.Fatalf("relay = %d %s, want %d", rec.Code, rec.Body.String(), tc.want)
			}
			if tc.want == http.StatusForbidden && (respondedReason(t, rec) != reasonUIGatewayCrossSite || !h.audit.hasDataValue("reason", inboundDenyCrossSite)) {
				t.Errorf("refusal = %s, reasons %s; want %s audited cross_site", rec.Body.String(), h.audit.dataReasons(), reasonUIGatewayCrossSite)
			}
		})
	}
}

// TestUIGateway_RunnerOfflineIs503: an exec the runner cannot take because it is offline is a retryable
// 503 runner_offline, not a 502.
func TestUIGateway_RunnerOfflineIs503(t *testing.T) {
	h := newUIHarness(t, okBackend())
	cookie := h.openSession()
	h.runner.execFn = func(spec runner.ExecSpec) (*runner.ExecSession, error) {
		if spec.Argv[0] == "sh" {
			return &runner.ExecSession{Wait: func() (int, error) { return 0, nil }}, nil
		}
		return nil, runner.ErrRunnerOffline
	}
	rec := h.relay("/", cookie, nil)
	if rec.Code != http.StatusServiceUnavailable || respondedReason(t, rec) != string(placement.ReasonRunnerOffline) {
		t.Fatalf("relay = %d %s, want 503 %s", rec.Code, rec.Body.String(), placement.ReasonRunnerOffline)
	}
}

// TestUIGateway_HealthzPublishesInboundTemplate: an API client is told where to call an inbound app.
func TestUIGateway_HealthzPublishesInboundTemplate(t *testing.T) {
	h := newUIHarness(t, okBackend())
	if got := h.srv.uiSandboxHealthz()["inbound_url_template"]; got != "https://ui.example.com/r/{run}/{app}/" {
		t.Fatalf("inbound_url_template = %v", got)
	}
	h.srv.cfg.UIOriginTemplate = "https://{run}.ui.example.com"
	if got := h.srv.uiSandboxHealthz()["inbound_url_template"]; got != "https://{run}.ui.example.com/r/{run}/{app}/" {
		t.Fatalf("host-mode inbound_url_template = %v", got)
	}
}

// uiSponsorGate is a revocation store that also answers whose identity is deactivated
// (oidc.IdentityRevocations), as the sponsor check of a service identity's token asks it.
type uiSponsorGate struct {
	mu          sync.Mutex
	deactivated string
}

func (g *uiSponsorGate) IsSessionRevoked(context.Context, string, string, time.Time) (bool, error) {
	return false, nil
}
func (g *uiSponsorGate) RevokeSub(context.Context, string) error { return nil }
func (g *uiSponsorGate) RevokeAll(context.Context) error         { return nil }
func (g *uiSponsorGate) SessionStatus(_ context.Context, sub, _ string, _ oidc.Issue, _ int64) (oidc.SessionStatus, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if sub == g.deactivated {
		return oidc.SessionDeactivated, nil
	}
	return oidc.SessionLive, nil
}

func (g *uiSponsorGate) deactivate(sub string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.deactivated = sub
}

// uiServiceTokenStore is uiTokenStore with one service identity, si; only GetServiceIdentity is asked of it.
type uiServiceTokenStore struct {
	*uiTokenStore
	store.ServiceIdentityStore
	si types.ServiceIdentity
}

func (s *uiServiceTokenStore) GetServiceIdentity(context.Context, string) (types.ServiceIdentity, error) {
	return s.si, nil
}

// uiDenyGate is safeRecorder whose inbound.deny writes, once held, wait until the test ends: an audit sink
// that has stopped answering, which a stream's close must not wait on.
type uiDenyGate struct {
	*safeRecorder
	mu   sync.Mutex
	held chan struct{}
}

func (g *uiDenyGate) Record(ctx context.Context, ev types.AuditEvent) error {
	g.mu.Lock()
	held := g.held
	g.mu.Unlock()
	if held != nil && ev.Action == "inbound.deny" {
		<-held
	}
	return g.safeRecorder.Record(ctx, ev)
}

// hold holds every inbound.deny write from now until the test ends.
func (g *uiDenyGate) hold(t *testing.T) {
	held := make(chan struct{})
	t.Cleanup(func() { close(held) })
	g.mu.Lock()
	defer g.mu.Unlock()
	g.held = held
}

// awaitDenies is h's inbound.deny rows once there is one, or after 5 s: a stream writes its refusal's row
// after it closes.
func awaitDenies(h *uiHarness) []types.AuditEvent {
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		if rows := h.audit.auditRows("inbound.deny", "denied"); len(rows) > 0 || time.Now().After(deadline) {
			return rows
		}
	}
}

// openInboundStream upgrades a connection to h's inbound app "code" with token and returns it with its
// reader, past the 101.
func openInboundStream(t *testing.T, h *uiHarness, gw *httptest.Server, token string) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.Dial("tcp", gw.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial gateway: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	req, err := http.NewRequest(http.MethodGet, gw.URL+uiRelayPrefix(h.run.ID, "code")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Authorization", "Bearer "+token)
	if err := req.Write(conn); err != nil {
		t.Fatalf("write upgrade request: %v", err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		t.Fatalf("read upgrade response: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade = %d, want 101", resp.StatusCode)
	}
	return conn, br
}

// echoes reports whether the stream still carries a line both ways.
func echoes(conn net.Conn, br *bufio.Reader) bool {
	if _, err := conn.Write([]byte("ping\n")); err != nil {
		return false
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := br.ReadString('\n')
	return err == nil && line == "ping\n"
}

// closes reports whether the gateway closes the stream within 5 s, sent nothing.
func closes(conn net.Conn, br *bufio.Reader) bool {
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err := br.ReadString('\n')
	var ne net.Error
	return err != nil && !(errors.As(err, &ne) && ne.Timeout())
}

// TestUIGateway_InboundStreamIsReadmitted: a stream an inbound token upgraded is admitted again on an
// interval, so the token's revoke or expiry, or its service identity's sponsor being deactivated, closes it
// with the inbound.deny row a refused request gets. A token that keeps its authority keeps its stream.
func TestUIGateway_InboundStreamIsReadmitted(t *testing.T) {
	const every = 20 * time.Millisecond
	open := func(t *testing.T) (*uiHarness, *httptest.Server) {
		held := make(chan struct{})
		t.Cleanup(func() { close(held) })
		h := inboundHarnessOn(t, echoAfterUpgradeBackend(t, held), "service:payments")
		h.srv.uiInboundStreamEvery = every
		gw := httptest.NewServer(h.gateway)
		t.Cleanup(gw.Close)
		return h, gw
	}
	tokens := func(h *uiHarness) *uiTokenStore { return h.srv.cfg.Store.(*uiTokenStore) }

	for name, end := range map[string]func(h *uiHarness){
		"revoked": func(h *uiHarness) {
			tokens(h).update("wdn_alice", func(tok *types.APIToken) { at := time.Now(); tok.RevokedAt = &at })
		},
		"expired": func(h *uiHarness) {
			tokens(h).update("wdn_alice", func(tok *types.APIToken) { at := time.Now(); tok.ExpiresAt = &at })
		},
	} {
		t.Run("a stream whose token is "+name+" closes", func(t *testing.T) {
			h, gw := open(t)
			conn, br := openInboundStream(t, h, gw, "wdn_alice")
			if !echoes(conn, br) {
				t.Fatal("the stream carried nothing before the token ended")
			}
			tok, err := tokens(h).GetAPITokenByRaw(context.Background(), "wdn_alice")
			if err != nil {
				t.Fatalf("read the token before it ends: %v", err)
			}
			end(h)
			if !closes(conn, br) {
				t.Fatal("the stream outlived its token")
			}
			// One row, naming who the stream was open for, though the token no longer proves it.
			rows := awaitDenies(h)
			if len(rows) != 1 || rows[0].Actor != h.owner || !h.audit.hasDataValue("reason", inboundDenyCredentialInvalid) ||
				!h.audit.hasDataValue("token_id", tok.ID.String()) {
				t.Errorf("inbound.deny rows = %+v, want one credential_invalid by %s for token %s", rows, h.owner, tok.ID)
			}
		})
	}

	t.Run("a service identity's stream closes when its sponsor is deactivated", func(t *testing.T) {
		h, gw := open(t)
		gate := &uiSponsorGate{}
		h.srv.cfg.SessionRevocations = gate
		ts := tokens(h)
		ts.update("wdn_svc", func(tok *types.APIToken) {
			*tok = types.APIToken{ID: uuid.New(), Principal: types.ServicePrincipalPrefix + "payments",
				Kind: types.APITokenKindInbound, CreatedAt: time.Now().Add(-time.Hour)}
		})
		h.srv.cfg.Store = &uiServiceTokenStore{uiTokenStore: ts, si: types.ServiceIdentity{
			Name: "payments", Sponsor: "dana", SponsorSince: time.Now().Add(-2 * time.Hour)}}
		conn, br := openInboundStream(t, h, gw, "wdn_svc")
		if !echoes(conn, br) {
			t.Fatal("the stream carried nothing before the sponsor left")
		}
		gate.deactivate("dana")
		if !closes(conn, br) {
			t.Fatal("the stream outlived its sponsor")
		}
		if awaitDenies(h); !h.audit.hasDataValue("reason", inboundDenyCredentialInvalid) {
			t.Errorf("no inbound.deny credential_invalid; reasons: %s", h.audit.dataReasons())
		}
	})

	// The profile arm of a request's re-assert reaches an open stream too: a profile changed to deny UI apps,
	// or interactive sessions, closes it at the next tick with the inbound.deny row a refused call gets.
	for name, limits := range map[string]types.GovernanceLimits{
		"deny_ui_apps":     {DenyUIApps: true},
		"deny_interactive": {DenyInteractive: true},
	} {
		t.Run("a stream closes when its run's profile comes to "+name, func(t *testing.T) {
			h, gw := open(t)
			unit := types.GovernanceUnit{ID: uuid.New(), Name: "walled"}
			setUnit := func(u types.GovernanceUnit) {
				h.store.sshMemStore.mu.Lock()
				defer h.store.sshMemStore.mu.Unlock()
				h.store.profiles = []types.GovernanceUnit{u}
			}
			setUnit(unit)
			h.run.UnitID = &unit.ID
			h.store.putRun(h.run)
			conn, br := openInboundStream(t, h, gw, "wdn_alice")
			if !echoes(conn, br) {
				t.Fatal("the stream carried nothing under a profile that allows inbound")
			}
			unit.Limits = limits
			setUnit(unit)
			if !closes(conn, br) {
				t.Fatal("the stream outlived the profile that now denies it")
			}
			if rows := awaitDenies(h); len(rows) != 1 || rows[0].Actor != h.owner ||
				!h.audit.hasDataValue("reason", inboundDenyUnitDenies) {
				t.Errorf("inbound.deny rows = %+v, want one unit_denies by %s", rows, h.owner)
			}
		})
	}

	// A lookup that cannot be made is no verdict on the token: the stream rides it out, and is closed, with a
	// row, only once the re-admission has been unanswerable for the staleness bound.
	lookupsFail := func(t *testing.T, h *uiHarness) {
		t.Helper()
		ts := tokens(h)
		from := ts.failLookups(errors.New("connection refused"))
		for deadline := time.Now().Add(5 * time.Second); ts.failedLookups() < from+2; time.Sleep(every) { // two: the first is recorded
			if time.Now().After(deadline) {
				t.Fatal("no re-admission was attempted while the store failed")
			}
		}
	}
	t.Run("a transient lookup failure keeps the stream", func(t *testing.T) {
		h, gw := open(t)
		conn, br := openInboundStream(t, h, gw, "wdn_alice")
		lookupsFail(t, h)
		tokens(h).failLookups(nil)
		time.Sleep(5 * every)
		if !echoes(conn, br) {
			t.Fatal("one unanswerable re-admission closed the stream")
		}
		if rows := h.audit.auditRows("inbound.deny", "denied"); len(rows) != 0 {
			t.Errorf("inbound.deny rows = %+v, want none", rows)
		}
	})
	t.Run("a failed read of the run keeps the stream", func(t *testing.T) {
		h, gw := open(t)
		conn, br := openInboundStream(t, h, gw, "wdn_alice")
		tokens(h).failReads(errors.New("connection refused"))
		time.Sleep(5 * every)
		tokens(h).failReads(nil)
		time.Sleep(5 * every)
		if !echoes(conn, br) {
			t.Fatal("a store blip on the run read closed the stream")
		}
		if rows := h.audit.auditRows("inbound.deny", "denied"); len(rows) != 0 {
			t.Errorf("inbound.deny rows = %+v, want none", rows)
		}
	})
	t.Run("reads of the run failing past the bound close the stream with a run_unreadable row", func(t *testing.T) {
		h, gw := open(t)
		conn, br := openInboundStream(t, h, gw, "wdn_alice")
		tokens(h).failReads(errors.New("connection refused"))
		time.Sleep(5 * every) // the first unanswerable re-admission is recorded, and rides out
		if !echoes(conn, br) {
			t.Fatal("one unanswerable re-admission closed the stream")
		}
		h.clock.advance(inboundResolveTTL)
		if !closes(conn, br) {
			t.Fatal("the stream outlived re-admissions unanswerable past the bound")
		}
		if rows := awaitDenies(h); len(rows) != 1 || rows[0].Actor != h.owner ||
			!h.audit.hasDataValue("reason", reasonRunUnreadable) || h.audit.hasDataValue("reason", inboundDenyNotFound) {
			t.Errorf("inbound.deny rows = %+v, want one run_unreadable by %s and no not_found", rows, h.owner)
		}
	})
	t.Run("lookups failing past the bound close the stream with a row", func(t *testing.T) {
		h, gw := open(t)
		conn, br := openInboundStream(t, h, gw, "wdn_alice")
		lookupsFail(t, h)
		h.clock.advance(inboundResolveTTL)
		if !closes(conn, br) {
			t.Fatal("the stream outlived re-admissions unanswerable past the bound")
		}
		if rows := awaitDenies(h); len(rows) != 1 || rows[0].Actor != h.owner ||
			!h.audit.hasDataValue("reason", reasonTokenLookupUnavailable) {
			t.Errorf("inbound.deny rows = %+v, want one token_lookup_unavailable by %s", rows, h.owner)
		}
	})

	// The bound runs from the last check that confirmed the stream, here the lookup of the session it opened
	// on (cached), not from the first that failed.
	t.Run("lookups failing close the stream a bound after its last good check, not after the first failure", func(t *testing.T) {
		h, gw := open(t)
		if _, ok := h.srv.inboundTokenSession(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, uiRelayPrefix(h.run.ID, "code")+"/", nil),
			h.run.ID, "code", "wdn_alice"); !ok {
			t.Fatal("the token was not admitted")
		}
		h.clock.advance(inboundResolveTTL / 2)
		ts := tokens(h)
		from := ts.failLookups(errors.New("connection refused"))
		conn, br := openInboundStream(t, h, gw, "wdn_alice") // on the cached session
		for deadline := time.Now().Add(5 * time.Second); ts.failedLookups() < from+1; time.Sleep(every) {
			if time.Now().After(deadline) {
				t.Fatal("no re-admission was attempted while the store failed")
			}
		}
		if !echoes(conn, br) {
			t.Fatal("a failure inside the bound closed the stream")
		}
		h.clock.advance(inboundResolveTTL / 2) // the bound after the good check; half of it after the first failure
		if !closes(conn, br) {
			t.Fatal("the stream outlived its last good check by the bound")
		}
		if rows := awaitDenies(h); len(rows) != 1 || !h.audit.hasDataValue("reason", reasonTokenLookupUnavailable) {
			t.Errorf("inbound.deny rows = %+v, want one token_lookup_unavailable", rows)
		}
	})

	// A check that never answers holds no stream past the bound: the tick closes it with the check in flight.
	t.Run("a token lookup that never answers closes the stream at the bound", func(t *testing.T) {
		h, gw := open(t)
		conn, br := openInboundStream(t, h, gw, "wdn_alice")
		if !echoes(conn, br) {
			t.Fatal("the stream carried nothing before the store hung")
		}
		ts := tokens(h)
		ts.hangLookups(t)
		for deadline := time.Now().Add(5 * time.Second); ts.hungLookups() == 0; time.Sleep(every) {
			if time.Now().After(deadline) {
				t.Fatal("no re-admission was attempted while the store hung")
			}
		}
		time.Sleep(5 * every)
		if !echoes(conn, br) {
			t.Fatal("a check in flight inside the bound closed the stream")
		}
		h.clock.advance(inboundResolveTTL)
		if !closes(conn, br) {
			t.Fatal("the stream outlived a re-admission that never answered")
		}
		if rows := awaitDenies(h); len(rows) != 1 || rows[0].Actor != h.owner ||
			!h.audit.hasDataValue("reason", reasonTokenLookupUnavailable) {
			t.Errorf("inbound.deny rows = %+v, want one token_lookup_unavailable by %s", rows, h.owner)
		}
	})

	t.Run("a refusal closes the stream though the write of its row hangs", func(t *testing.T) {
		h, gw := open(t)
		gate := &uiDenyGate{safeRecorder: h.audit}
		h.srv.cfg.Audit = gate
		conn, br := openInboundStream(t, h, gw, "wdn_alice")
		if !echoes(conn, br) {
			t.Fatal("the stream carried nothing before the token was revoked")
		}
		gate.hold(t)
		tokens(h).update("wdn_alice", func(tok *types.APIToken) { at := time.Now(); tok.RevokedAt = &at })
		if !closes(conn, br) {
			t.Fatal("the stream's close waited on its refusal's audit write")
		}
	})

	t.Run("a stream whose token keeps its authority stays open", func(t *testing.T) {
		h, gw := open(t)
		conn, br := openInboundStream(t, h, gw, "wdn_alice")
		for range 4 { // two bounds' worth of time: each check that admits the stream moves its bound on
			h.clock.advance(inboundResolveTTL / 2)
			time.Sleep(5 * every)
		}
		if !echoes(conn, br) {
			t.Fatal("a stream with a live token closed across the re-admissions")
		}
		if reasons := h.audit.dataReasons(); strings.Contains(reasons, inboundDenyCredentialInvalid) {
			t.Errorf("a live token was refused: %s", reasons)
		}
	})
}
