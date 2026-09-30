// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// bedrock-api-key is no longer a model credential: a run's Bedrock key is its
// provider's (wardyn-provider-<uid>-key). The sink refuses every grant naming
// the old name, and dispatch strips every injection naming it. Each test seeds
// the OPERATOR's row with a distinguishable value, so "the key was served" is
// an assertion about the body rather than about an error.

const (
	bedrockGuardOperatorBearer = "operator-only-bedrock-bearer-must-not-leak"
	bedrockGuardMemberBearer   = "alice-own-bearer-xyz"
	bedrockGuardMember         = "alice@example.com" // mintRunToken's subject
)

// bearerGuardStore is the minimal store.Store the injection route reads: the
// run and its grants.
// store.Store is embedded so any OTHER method the sink starts reading panics in
// test rather than silently reading a zero value.
//
// GetRun is called from TWO places on this request path: refuseTerminalRun (the
// /internal/* liveness middleware, internal_live_run.go) reads it once before
// the handler runs at all, and the sink reads it again itself.
// failRunFromCall lets a test make the SECOND read fail while the first
// succeeds — the middleware already fails closed (503/403) on a first-read
// error, so a run_unreadable case that failed on call 1 would never reach the
// sink at all and would be proving the middleware, not this arm.
type bearerGuardStore struct {
	store.Store
	mu              sync.Mutex
	run             types.AgentRun
	runCalls        int
	failRunFromCall int // GetRun returns runErr starting at this 1-indexed call; 0 = never fail
	runErr          error
	site            types.SiteConfig
	siteErr         error
	grants          []types.CredentialGrant
}

func (s *bearerGuardStore) GetRun(context.Context, uuid.UUID) (types.AgentRun, error) {
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

func (s *bearerGuardStore) ListGrantsByRun(context.Context, uuid.UUID) ([]types.CredentialGrant, error) {
	return s.grants, nil
}

// recordBearerGrant adds a grant naming bedrock-api-key whose scope records the
// (owner, credential_source) namespace, as dispatch's does, and returns its id.
// Spelled as JSON rather than through the production constructor so a record a
// policy author could hand-write is what the sink is tested against.
func (s *bearerGuardStore) recordBearerGrant(owner, source string) uuid.UUID {
	id := uuid.New()
	scope, _ := json.Marshal(map[string]any{
		"host": "bedrock-runtime.us-east-1.amazonaws.com", "header": "Authorization",
		"format": "Bearer %s", "secret_name": bedrockAPIKeySecret,
		"snapshot": map[string]string{"owner_subject": owner, "credential_source": source},
	})
	s.grants = append(s.grants, types.CredentialGrant{ID: id, RunID: s.run.ID,
		Spec: types.GrantSpec{Kind: types.GrantAPIKey, Scope: scope}})
	return id
}

// bedrockBearerGrant is the minted shape of the grant dispatch authors.
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
// whose OPERATOR row for bedrock-api-key is bedrockGuardOperatorBearer.
func bearerGuardHarness(t *testing.T, st *bearerGuardStore) (*harness, *memSecrets) {
	t.Helper()
	h, sec := newSecretsHarness(t)
	sec.m[bedrockAPIKeySecret] = []byte(bedrockGuardOperatorBearer)
	h.srv.cfg.Store = st
	h.srv.router = h.srv.routes()
	return h, sec
}

func resolveBearerGrant(t *testing.T, h *harness, runID, grantID uuid.UUID) (int, string) {
	t.Helper()
	h.broker.minted = bedrockBearerGrant("jti-" + grantID.String())
	rr := do(t, h.srv, http.MethodGet, "/api/v1/internal/injection/"+grantID.String(), h.mintRunToken(t, runID), "")
	return rr.Code, rr.Body.String()
}

// TestBedrockBearerSink_RefusesEveryGrant: a grant naming bedrock-api-key is
// refused whatever it records — the shape a pre-conversion dispatch authored,
// a per-person record, or none at all — and neither the operator's key nor the
// member's own reaches the body. The generic sink's owner-fallback read must
// never see the name.
func TestBedrockBearerSink_RefusesEveryGrant(t *testing.T) {
	for _, tc := range []struct{ name, owner, source string }{
		{"a shared record", "", "shared"},
		{"a per-person record", bedrockGuardMember, "per_user"},
		{"no record", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &bearerGuardStore{run: types.AgentRun{ID: uuid.New(), Agent: "claude-code", CreatedBy: bedrockGuardMember}}
			h, sec := bearerGuardHarness(t, st)
			if err := sec.For(bedrockGuardMember).Put(context.Background(), bedrockAPIKeySecret, []byte(bedrockGuardMemberBearer)); err != nil {
				t.Fatal(err)
			}
			grantID := st.recordBearerGrant(tc.owner, tc.source)
			code, body := resolveBearerGrant(t, h, st.run.ID, grantID)
			if code != http.StatusForbidden || !strings.Contains(body, reasonMissingScopeSnapshot) {
				t.Fatalf("resolve = %d %s, want 403 %s", code, body, reasonMissingScopeSnapshot)
			}
			for _, key := range []string{bedrockGuardOperatorBearer, bedrockGuardMemberBearer} {
				if strings.Contains(body, key) {
					t.Fatalf("the refusal served a Bedrock key: %s", body)
				}
			}
			ev := lastAuditEvent(t, h.audit.events, "secret.read")
			if ev.Outcome != "failure" || ev.Target != bedrockAPIKeySecret {
				t.Errorf("secret.read = %s %s, want a failure row naming %s", ev.Outcome, ev.Target, bedrockAPIKeySecret)
			}
		})
	}
}

// TestDispatch_StripsEveryBedrockKeyInjection: dispatch authors no grant
// naming bedrock-api-key, and one a stored or recorded policy carried is
// dropped and audited — with no model provider block at all, the shape an
// install that never configured one dispatches with.
func TestDispatch_StripsEveryBedrockKeyInjection(t *testing.T) {
	st := &bearerGuardStore{run: types.AgentRun{ID: uuid.New(), Agent: "claude-code", Task: "t", CreatedBy: bedrockGuardMember}}
	h, _ := bearerGuardHarness(t, st)
	stale := runner.InjectionGrant{GrantID: uuid.New(), Rule: egress.InjectionRule{
		Host: "bedrock-runtime.us-east-1.amazonaws.com", Header: "Authorization",
		SecretName: bedrockAPIKeySecret, Format: "Bearer %s"}}
	captured := &captureGrantStore{}
	h.srv.cfg.Store = captured
	policy := types.RunPolicySpec{}
	plan, ok := h.srv.resolveLLMInjections(context.Background(), st.run, dispatchParams{},
		&policy, map[string]string{}, []runner.InjectionGrant{stale}, "", artifactRedirectPlan{}, false,
		types.SiteConfig{}, true, false, bedrockCredUngraded())
	if !ok {
		t.Fatal("dispatch refused a run no provider serves; it launches with no model credential")
	}
	for _, ig := range plan.injections {
		if ig.Rule.SecretName == bedrockAPIKeySecret {
			t.Fatalf("an injection naming %s survived dispatch: %+v", bedrockAPIKeySecret, ig)
		}
	}
	if len(captured.grants) != 0 {
		t.Errorf("dispatch authored %d grant(s) for a run no provider serves", len(captured.grants))
	}
	ev := lastAuditEvent(t, h.audit.events, "run.injection.drop")
	if ev.Target != stale.GrantID.String() || ev.Outcome != "denied" ||
		!strings.Contains(string(ev.Data), "model_credential_not_provider_authored") {
		t.Fatalf("drop audit = target %s outcome %s data %s, want the stale grant %s denied with its reason",
			ev.Target, ev.Outcome, ev.Data, stale.GrantID)
	}
}
