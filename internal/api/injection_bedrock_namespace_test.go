// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// errTransientStoreFailure is a generic (non-ErrNotFound) read failure: the
// shape a Postgres blip takes, as opposed to a genuinely missing row.
var errTransientStoreFailure = errors.New("store: transient failure")

// bedrockBearerNamespaceOK's three REFUSAL arms (run_unreadable,
// roster_unreadable, per_user_bearer_absent) were exercised only incidentally,
// via the pass-through arms in injection_test.go — nothing asserted that an
// unreadable run or roster, or an absent per-user row, actually stops the
// OPERATOR's bedrock-api-key from being injected in their place. That
// substitution is exactly what per_user exists to refuse (see the guard's doc
// comment), so it is the thing worth proving.
//
// Each test below seeds the OPERATOR's row with a distinguishable value and
// asserts three things together: a non-200 status, that value absent from the
// response body, and no successful secret.read audit event — "refused", not
// merely "errored".

const bedrockGuardOperatorBearer = "operator-only-bedrock-bearer-must-not-leak"

// bearerGuardStore is the minimal store.Store bedrockBearerNamespaceOK reads:
// GetRun (for the run's agent) and GetSiteConfig (for the roster's per_user
// row). store.Store is embedded so any OTHER method this guard starts reading
// in the future panics in test rather than silently reading a zero value.
//
// GetRun is called from TWO places on this request path: refuseTerminalRun (the
// /internal/* liveness middleware, internal_live_run.go) reads it once before
// the handler runs at all, and bedrockBearerNamespaceOK reads it again itself.
// failRunFromCall lets a test make the SECOND read fail while the first
// succeeds — the middleware already fails closed (503/403) on a first-read
// error, so a run_unreadable case that failed on call 1 would never reach the
// guard at all and would be proving the middleware, not this arm.
type bearerGuardStore struct {
	store.Store
	mu              sync.Mutex
	run             types.AgentRun
	runCalls        int
	failRunFromCall int // GetRun returns runErr starting at this 1-indexed call; 0 = never fail
	runErr          error
	site            types.SiteConfig
	siteErr         error
}

func (s *bearerGuardStore) GetRun(_ context.Context, _ uuid.UUID) (types.AgentRun, error) {
	s.mu.Lock()
	s.runCalls++
	n := s.runCalls
	s.mu.Unlock()
	if s.failRunFromCall > 0 && n >= s.failRunFromCall {
		return types.AgentRun{}, s.runErr
	}
	return s.run, nil
}

func (s *bearerGuardStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	if s.siteErr != nil {
		return types.SiteConfig{}, s.siteErr
	}
	return s.site, nil
}

// bedrockBearerGrant is the api_key grant resolveAWSSSOInjection ignores (its
// secret name is bedrockAPIKeySecret, not types.AWSSSOAccessTokenSecret) and
// bedrockBearerNamespaceOK gates before the generic sink would resolve it.
func bedrockBearerGrant(jti string) broker.Minted {
	return broker.Minted{
		Kind: types.GrantAPIKey,
		JTI:  jti,
		Injection: &egress.InjectionRule{
			Host: "bedrock-runtime.us-east-1.amazonaws.com", Header: "Authorization",
			SecretName: bedrockAPIKeySecret, Format: "Bearer %s",
		},
	}
}

// bearerGuardHarness wires a secrets-enabled harness whose store is st and
// whose OPERATOR row for bedrock-api-key is the distinguishable sentinel
// value every test below checks does NOT leak.
func bearerGuardHarness(t *testing.T, st *bearerGuardStore) (*harness, *memSecrets) {
	t.Helper()
	h, sec := newSecretsHarness(t)
	sec.m[bedrockAPIKeySecret] = []byte(bedrockGuardOperatorBearer)
	h.srv.cfg.Store = st
	h.srv.router = h.srv.routes()
	return h, sec
}

// assertBedrockBearerRefused is the shared assertion: the credential was
// REFUSED, not merely errored — the operator's bearer never appears in the
// body, and no secret.read success was audited.
func assertBedrockBearerRefused(t *testing.T, rr *httptest.ResponseRecorder, h *harness, wantStatus int, wantBody, wantReason string) {
	t.Helper()
	if rr.Code != wantStatus {
		t.Fatalf("status = %d, want %d; body=%s", rr.Code, wantStatus, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), wantBody) {
		t.Fatalf("body = %q, want it to contain %q", rr.Body.String(), wantBody)
	}
	if strings.Contains(rr.Body.String(), bedrockGuardOperatorBearer) {
		t.Fatalf("the operator's bearer leaked into the refusal body: %s", rr.Body.String())
	}
	for _, ev := range h.audit.events {
		if ev.Action == "secret.read" && ev.Outcome == "success" {
			t.Fatalf("a successful secret.read was recorded despite the refusal: %+v", ev)
		}
	}
	ev := lastAuditEvent(t, h.audit.events, "secret.read")
	if !strings.Contains(string(ev.Data), wantReason) {
		t.Fatalf("audit data = %s, want reason %q", ev.Data, wantReason)
	}
}

// TestBedrockBearerNamespaceOK_RefusesUnreadableRun pins the run_unreadable
// arm: the guard cannot ask a roster whose row is which without first knowing
// the run's agent, so a store that cannot read the run must fail CLOSED rather
// than let awsSSOScopeFor treat a zero-value run (empty Agent) as "no per_user
// row for this agent" and fall through to the operator's row.
//
// The run's FIRST read (by refuseTerminalRun, the /internal/* liveness
// middleware) succeeds — a run that middleware itself could not read never
// reaches this handler, so that is a different refusal, not this arm. Only the
// guard's OWN read (the second GetRun on this path) fails, the shape a
// transient store blip between the two takes.
func TestBedrockBearerNamespaceOK_RefusesUnreadableRun(t *testing.T) {
	runID := uuid.New()
	st := &bearerGuardStore{
		run:             types.AgentRun{ID: runID, Agent: "claude-code", State: types.RunRunning},
		failRunFromCall: 2,
		runErr:          errTransientStoreFailure,
	}
	h, _ := bearerGuardHarness(t, st)
	token := h.mintRunToken(t, runID)
	h.broker.minted = bedrockBearerGrant("jti-run-unreadable")

	rr := do(t, h.srv, http.MethodGet, "/api/v1/internal/injection/"+uuid.NewString(), token, "")
	assertBedrockBearerRefused(t, rr, h, http.StatusServiceUnavailable, credentialReauthRunUnreadableBody, "run_unreadable")
	if st.runCalls < 2 {
		t.Fatalf("GetRun called %d times, want >=2 (middleware + guard) — the guard's own read was never reached", st.runCalls)
	}
}

// TestBedrockBearerNamespaceOK_RefusesUnreadableRoster pins the
// roster_unreadable arm: the run reads fine (agent = claude-code), but the
// roster does not, so the guard cannot tell "no per_user row" from "the row
// that says per_user could not be read" — both share the zero SiteConfig — and
// must refuse rather than treat the read failure as the former.
func TestBedrockBearerNamespaceOK_RefusesUnreadableRoster(t *testing.T) {
	runID := uuid.New()
	st := &bearerGuardStore{
		run:     types.AgentRun{ID: runID, Agent: "claude-code"},
		siteErr: errStoreNotFound,
	}
	h, _ := bearerGuardHarness(t, st)
	token := h.mintRunToken(t, runID)
	h.broker.minted = bedrockBearerGrant("jti-roster-unreadable")

	rr := do(t, h.srv, http.MethodGet, "/api/v1/internal/injection/"+uuid.NewString(), token, "")
	assertBedrockBearerRefused(t, rr, h, http.StatusServiceUnavailable, credentialReauthStoreErrorBody, "roster_unreadable")
}

// TestBedrockBearerNamespaceOK_RefusesAbsentPerUserBearer pins the
// per_user_bearer_absent arm: both reads succeed, the roster genuinely says
// per_user for this agent, and the run's OWN principal (alice, from
// mintRunToken) has no bedrock-api-key row of her own — she had one at
// dispatch and does not now, or never did. The generic fallback below this
// guard (Store.For(sub).Get) would otherwise serve the OPERATOR's row, billed
// to the org and attributed to nobody; the guard exists to refuse that
// specific substitution.
func TestBedrockBearerNamespaceOK_RefusesAbsentPerUserBearer(t *testing.T) {
	runID := uuid.New()
	st := &bearerGuardStore{
		run:  types.AgentRun{ID: runID, Agent: "claude-code"},
		site: agentRoster(types.AgentProvider{ID: "claude-code", CredentialSource: types.CredentialSourcePerUser}),
	}
	h, _ := bearerGuardHarness(t, st)
	token := h.mintRunToken(t, runID) // sub = "alice@example.com" — owns no bedrock-api-key row
	h.broker.minted = bedrockBearerGrant("jti-per-user-absent")

	rr := do(t, h.srv, http.MethodGet, "/api/v1/internal/injection/"+uuid.NewString(), token, "")
	assertBedrockBearerRefused(t, rr, h, http.StatusFailedDependency, bedrockBearerNamespaceNotOwn, "per_user_bearer_absent")
}

// TestBedrockBearerNamespaceOK_PerUserOwnRowPasses is the positive control for
// the arm above: alice's OWN bedrock-api-key row resolves, not the operator's —
// proving the refusal above is about the absent row specifically, not about
// per_user routing generally.
func TestBedrockBearerNamespaceOK_PerUserOwnRowPasses(t *testing.T) {
	runID := uuid.New()
	st := &bearerGuardStore{
		run:  types.AgentRun{ID: runID, Agent: "claude-code"},
		site: agentRoster(types.AgentProvider{ID: "claude-code", CredentialSource: types.CredentialSourcePerUser}),
	}
	h, sec := bearerGuardHarness(t, st)
	if err := sec.For("alice@example.com").Put(context.Background(), bedrockAPIKeySecret, []byte("alice-own-bedrock-bearer")); err != nil {
		t.Fatalf("seed alice's row: %v", err)
	}
	token := h.mintRunToken(t, runID)
	h.broker.minted = bedrockBearerGrant("jti-per-user-own")

	rr := do(t, h.srv, http.MethodGet, "/api/v1/internal/injection/"+uuid.NewString(), token, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), bedrockGuardOperatorBearer) {
		t.Fatalf("operator bearer resolved instead of alice's own: %s", rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "alice-own-bedrock-bearer") {
		t.Fatalf("alice's own bearer did not resolve: %s", rr.Body.String())
	}
}
