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

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The Bedrock bearer SINK resolves bedrock-api-key from the namespace dispatch
// recorded on its own grant, and from nowhere else. Each test below seeds the
// OPERATOR's row with a distinguishable value, so "the wrong key was injected"
// is an assertion about which namespace was read rather than about an error.

const (
	bedrockGuardOperatorBearer = "operator-only-bedrock-bearer-must-not-leak"
	bedrockGuardMemberBearer   = "alice-own-bearer-xyz"
	bedrockGuardMember         = "alice@example.com" // mintRunToken's subject
)

// bearerGuardStore is the minimal store.Store the sink reads: the run (for its
// agent), the roster, and the run's grants (for the recorded namespace).
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

func bearerRow(source types.CredentialSource) types.SiteConfig {
	return agentRoster(types.AgentProvider{ID: "claude-code", Mechanism: types.AgentMechanismBedrockBearer,
		CredentialSource: source})
}

// dispatchBearerGrant runs the REAL dispatch phase (resolveLLMInjections) for a
// run of alice's on h.srv and returns its plan and the bedrock-api-key grant it
// authored. The store is swapped for a capturing one for the duration and the
// grant is then handed to st, which is what the sink reads.
func dispatchBearerGrant(t *testing.T, h *harness, st *bearerGuardStore, injections []runner.InjectionGrant) (dispatchLLMPlan, types.CredentialGrant) {
	t.Helper()
	h.srv.cfg.BedrockRegion, h.srv.cfg.BedrockModel = "us-east-1", "us.anthropic.claude-sonnet-4-5-20250929-v1:0"
	captured := &captureGrantStore{}
	h.srv.cfg.Store = captured
	defer func() { h.srv.cfg.Store = st }()
	policy := types.RunPolicySpec{}
	plan, ok := h.srv.resolveLLMInjections(context.Background(), st.run, dispatchParams{},
		&policy, map[string]string{}, injections, "", artifactRedirectPlan{}, false, st.site, true, false, bedrockCredUngraded())
	if !ok || !plan.llm.injectBedrockBearer || len(captured.grants) != 1 {
		t.Fatalf("dispatch: ok=%v injectBedrockBearer=%v grants=%d, want a bearer run with exactly one grant",
			ok, plan.llm.injectBedrockBearer, len(captured.grants))
	}
	st.grants = append(st.grants, captured.grants[0])
	return plan, captured.grants[0]
}

func resolveBearerGrant(t *testing.T, h *harness, runID, grantID uuid.UUID) (int, string) {
	t.Helper()
	h.broker.minted = bedrockBearerGrant("jti-" + grantID.String())
	rr := do(t, h.srv, http.MethodGet, "/api/v1/internal/injection/"+grantID.String(), h.mintRunToken(t, runID), "")
	return rr.Code, rr.Body.String()
}

// TestBedrockBearerSink_SharedRunGetsTheOperatorsKeyNotTheMembersOwn: under
// `shared` dispatch reads the OPERATOR's bearer, so the sink must inject the
// operator's — even when the run's owner has stored a key of their own, which
// the owner-fallback read would have preferred, sending the run's Bedrock
// traffic to an AWS account the member controls.
func TestBedrockBearerSink_SharedRunGetsTheOperatorsKeyNotTheMembersOwn(t *testing.T) {
	st := &bearerGuardStore{run: types.AgentRun{ID: uuid.New(), Agent: "claude-code", CreatedBy: bedrockGuardMember},
		site: bearerRow(types.CredentialSourceShared)}
	h, sec := bearerGuardHarness(t, st)
	if err := sec.For(bedrockGuardMember).Put(context.Background(), bedrockAPIKeySecret, []byte(bedrockGuardMemberBearer)); err != nil {
		t.Fatal(err)
	}
	_, grant := dispatchBearerGrant(t, h, st, nil)

	code, body := resolveBearerGrant(t, h, st.run.ID, grant.ID)
	if code != http.StatusOK || !strings.Contains(body, bedrockGuardOperatorBearer) {
		t.Fatalf("status=%d body=%s, want 200 with the operator's key dispatch chose", code, body)
	}
	if strings.Contains(body, bedrockGuardMemberBearer) {
		t.Fatalf("the member's own key was injected on a shared run: %s", body)
	}
	ev := lastAuditEvent(t, h.audit.events, "secret.read")
	if !strings.Contains(string(ev.Data), `"owner":""`) {
		t.Errorf("secret.read names owner %s, want the operator namespace the key was read from", ev.Data)
	}
}

// TestBedrockBearerSink_PerUserRunGetsTheOwnersKey is the positive control: a
// per_user run's grant records the owner's namespace and resolves their key.
func TestBedrockBearerSink_PerUserRunGetsTheOwnersKey(t *testing.T) {
	st := &bearerGuardStore{run: types.AgentRun{ID: uuid.New(), Agent: "claude-code", CreatedBy: bedrockGuardMember},
		site: bearerRow(types.CredentialSourcePerUser)}
	h, sec := bearerGuardHarness(t, st)
	if err := sec.For(bedrockGuardMember).Put(context.Background(), bedrockAPIKeySecret, []byte(bedrockGuardMemberBearer)); err != nil {
		t.Fatal(err)
	}
	_, grant := dispatchBearerGrant(t, h, st, nil)

	code, body := resolveBearerGrant(t, h, st.run.ID, grant.ID)
	if code != http.StatusOK || !strings.Contains(body, bedrockGuardMemberBearer) {
		t.Fatalf("status=%d body=%s, want 200 with the owner's own key", code, body)
	}
}

// TestDispatch_BedrockBearerGrantIsTheOnlyOneNamingTheKey: an injection naming
// bedrock-api-key that dispatch did not author (a stored policy's, a recorded
// profile's) carries no record of this run's choice and would be refused at
// the sink, failing the proxy's startup — so dispatch drops it, audits the
// drop, and the run is served by its own grant alone.
func TestDispatch_BedrockBearerGrantIsTheOnlyOneNamingTheKey(t *testing.T) {
	st := &bearerGuardStore{run: types.AgentRun{ID: uuid.New(), Agent: "claude-code", CreatedBy: bedrockGuardMember},
		site: bearerRow(types.CredentialSourceShared)}
	h, _ := bearerGuardHarness(t, st)
	stale := runner.InjectionGrant{GrantID: uuid.New(), Rule: egress.InjectionRule{
		Host: "bedrock-runtime.us-east-1.amazonaws.com", Header: "Authorization",
		SecretName: bedrockAPIKeySecret, Format: "Bearer %s"}}
	plan, grant := dispatchBearerGrant(t, h, st, []runner.InjectionGrant{stale})

	var named []uuid.UUID
	for _, ig := range plan.injections {
		if ig.Rule.SecretName == bedrockAPIKeySecret {
			named = append(named, ig.GrantID)
		}
	}
	if len(named) != 1 || named[0] != grant.ID {
		t.Fatalf("injections naming %s = %v, want only dispatch's own grant %s", bedrockAPIKeySecret, named, grant.ID)
	}
	// The drop is audited, naming the dropped grant and why.
	ev := lastAuditEvent(t, h.audit.events, "run.injection.dropped")
	if ev.Target != stale.GrantID.String() || ev.Outcome != "denied" ||
		!strings.Contains(string(ev.Data), "bedrock_bearer_not_dispatch_authored") {
		t.Fatalf("drop audit = target %s outcome %s data %s, want the stale grant %s denied with its reason",
			ev.Target, ev.Outcome, ev.Data, stale.GrantID)
	}
}

// TestBedrockBearerSink_UnrecordedGrantOnAnotherAgentGetsNoOperatorKey: whether
// the operator's key may be served is a property of the grant dispatch
// authored, not of the run's agent row. A per_user bearer row on claude-code
// protects nothing if a grant naming the key on a codex-cli run (no row, so
// `shared`) falls through to the operator's key — here to a member-chosen host
// and header.
func TestBedrockBearerSink_UnrecordedGrantOnAnotherAgentGetsNoOperatorKey(t *testing.T) {
	runID := uuid.New()
	st := &bearerGuardStore{
		run: types.AgentRun{ID: runID, Agent: "codex-cli"},
		site: agentRoster(types.AgentProvider{ID: "claude-code", Mechanism: types.AgentMechanismBedrockBearer,
			CredentialSource: types.CredentialSourcePerUser}),
	}
	h, _ := bearerGuardHarness(t, st)
	token := h.mintRunToken(t, runID) // alice, owns no bedrock-api-key
	h.broker.minted = broker.Minted{Kind: types.GrantAPIKey, JTI: "j1",
		Injection: &egress.InjectionRule{Host: "api.openai.com", Header: "X-Member-Chosen",
			SecretName: bedrockAPIKeySecret, Format: "%s"}}
	rr := do(t, h.srv, http.MethodGet, "/api/v1/internal/injection/"+uuid.NewString(), token, "")
	t.Logf("status=%d body=%s", rr.Code, rr.Body.String())
	if strings.Contains(rr.Body.String(), bedrockGuardOperatorBearer) {
		t.Errorf("OPERATOR bearer injected on a per_user deployment for a member with no key of her own")
	}
}

// TestBedrockBearerSink_SharedUnrecordedGrantNeverServesTheMembersOwnKey: the
// same substitution as the shared-run test above, reached through a grant with
// no record — dispatch resolves the operator's bearer, and the sink must not
// answer with the member's own row.
func TestBedrockBearerSink_SharedUnrecordedGrantNeverServesTheMembersOwnKey(t *testing.T) {
	runID := uuid.New()
	st := &bearerGuardStore{run: types.AgentRun{ID: runID, Agent: "claude-code"},
		site: agentRoster(types.AgentProvider{ID: "claude-code", Mechanism: types.AgentMechanismBedrockBearer,
			CredentialSource: types.CredentialSourceShared})}
	h, sec := bearerGuardHarness(t, st)
	if err := sec.For("alice@example.com").Put(context.Background(), bedrockAPIKeySecret, []byte("alice-own-bearer-xyz")); err != nil {
		t.Fatal(err)
	}
	// dispatch-side read under shared
	got := h.srv.bedrockBearerFor(context.Background(), awsSSOScope{})
	t.Logf("dispatch-side bearer under shared = %q", got)
	token := h.mintRunToken(t, runID)
	h.broker.minted = bedrockBearerGrant("j2")
	rr := do(t, h.srv, http.MethodGet, "/api/v1/internal/injection/"+uuid.NewString(), token, "")
	t.Logf("status=%d body=%s", rr.Code, rr.Body.String())
	if string(got) == bedrockGuardOperatorBearer && strings.Contains(rr.Body.String(), "alice-own-bearer-xyz") {
		t.Errorf("dispatch resolved the operator bearer but the sink injected the member's own row")
	}
}

// TestBedrockBearerSink_MemberAuthoredGrantAfterDeleteGetsNoOperatorKey: a
// member who owned the name could author a grant naming it to any
// model-provider host under any header, then delete their key before the proxy
// resolved it — and the owner-fallback read would send the OPERATOR's key
// there. A grant with no dispatch record resolves nothing.
func TestBedrockBearerSink_MemberAuthoredGrantAfterDeleteGetsNoOperatorKey(t *testing.T) {
	runID := uuid.New()
	st := &bearerGuardStore{run: types.AgentRun{ID: runID, Agent: "claude-code", CreatedBy: bedrockGuardMember},
		site: bearerRow(types.CredentialSourceShared)}
	h, sec := bearerGuardHarness(t, st)
	own := sec.For(bedrockGuardMember)
	if err := own.Put(context.Background(), bedrockAPIKeySecret, []byte(bedrockGuardMemberBearer)); err != nil {
		t.Fatal(err)
	}
	if err := own.Delete(context.Background(), bedrockAPIKeySecret); err != nil {
		t.Fatal(err)
	}
	h.broker.minted = broker.Minted{Kind: types.GrantAPIKey, JTI: "j3",
		Injection: &egress.InjectionRule{Host: "api.openai.com", Header: "X-Member-Chosen",
			SecretName: bedrockAPIKeySecret, Format: "%s"}}
	rr := do(t, h.srv, http.MethodGet, "/api/v1/internal/injection/"+uuid.NewString(), h.mintRunToken(t, runID), "")
	if rr.Code == http.StatusOK || strings.Contains(rr.Body.String(), bedrockGuardOperatorBearer) {
		t.Fatalf("status=%d body=%s — the operator's key went to a member-chosen host and header", rr.Code, rr.Body.String())
	}
}

// TestBedrockBearerSink_RecordNotMatchingTheRosterIsRefused: the record names
// a decision, it grants nothing. A record that is not what the live roster
// names for this run's own subject — the roster moved mid-run, or a
// hand-written record names another principal or the operator — is refused,
// and no key is read.
func TestBedrockBearerSink_RecordNotMatchingTheRosterIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name          string
		site          types.SiteConfig
		owner, source string
	}{
		{"per_user record, roster now shared", bearerRow(types.CredentialSourceShared), bedrockGuardMember, "per_user"},
		{"another member's namespace", bearerRow(types.CredentialSourcePerUser), "bob@example.com", "per_user"},
		{"operator namespace under a per_user row", bearerRow(types.CredentialSourcePerUser), "", "shared"},
		{"per_user row now declares the SSO lane", agentRoster(types.AgentProvider{ID: "claude-code",
			Mechanism: types.AgentMechanismBedrockSSO, CredentialSource: types.CredentialSourcePerUser}),
			bedrockGuardMember, "per_user"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &bearerGuardStore{run: types.AgentRun{ID: uuid.New(), Agent: "claude-code"}, site: tc.site}
			h, sec := bearerGuardHarness(t, st)
			for _, who := range []string{bedrockGuardMember, "bob@example.com"} {
				if err := sec.For(who).Put(context.Background(), bedrockAPIKeySecret, []byte(who+"-bearer")); err != nil {
					t.Fatal(err)
				}
			}
			code, body := resolveBearerGrant(t, h, st.run.ID, st.recordBearerGrant(tc.owner, tc.source))
			if code != http.StatusForbidden || strings.Contains(body, bedrockGuardOperatorBearer) ||
				strings.Contains(body, "-bearer") {
				t.Fatalf("status=%d body=%s, want 403 with no key", code, body)
			}
			if ev := lastAuditEvent(t, h.audit.events, "secret.read"); ev.Outcome != "failure" ||
				!strings.Contains(string(ev.Data), "scope_changed") {
				t.Fatalf("audit = %s %s, want a scope_changed failure", ev.Outcome, ev.Data)
			}
			// #656: the wire body now carries the same reason.
			var eb errorBody
			if err := json.Unmarshal([]byte(body), &eb); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if eb.Reason != reasonScopeChanged {
				t.Errorf("wire reason = %q, want %q", eb.Reason, reasonScopeChanged)
			}
		})
	}
}

// The sink's three REFUSAL arms behind a valid dispatch record — run_unreadable,
// roster_unreadable and per_user_bearer_absent. Each seeds the OPERATOR's row
// with a distinguishable value and asserts three things together: a non-200
// status, that value absent from the response body, and no successful
// secret.read audit event — "refused", not merely "errored".

// errTransientStoreFailure is a generic (non-ErrNotFound) read failure: the
// shape a Postgres blip takes, as opposed to a genuinely missing row.
var errTransientStoreFailure = errors.New("store: transient failure")

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
	// #656: the same machine reason now reaches the WIRE body, not just the
	// audit row — the proxy has no audit access.
	var body errorBody
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Reason != wantReason {
		t.Errorf("wire reason = %q, want %q", body.Reason, wantReason)
	}
}

// resolveRecordedBearerGrant asks the sink for a grant recording (owner,
// source) on st's run, as the proxy would.
func resolveRecordedBearerGrant(t *testing.T, h *harness, st *bearerGuardStore, owner, source string) *httptest.ResponseRecorder {
	t.Helper()
	grantID := st.recordBearerGrant(owner, source)
	h.broker.minted = bedrockBearerGrant("jti-" + grantID.String())
	return do(t, h.srv, http.MethodGet, "/api/v1/internal/injection/"+grantID.String(), h.mintRunToken(t, st.run.ID), "")
}

// TestBedrockBearerSink_RefusesUnreadableRun pins the run_unreadable arm: the
// sink cannot check a record against a roster row without first knowing the
// run's agent, so a store that cannot read the run must fail CLOSED rather than
// let awsSSOScopeFor treat a zero-value run (empty Agent) as "no per_user row
// for this agent" and fall through to the operator's row.
//
// The run's FIRST read (by refuseTerminalRun, the /internal/* liveness
// middleware) succeeds — a run that middleware itself could not read never
// reaches this handler, so that is a different refusal, not this arm. Only the
// sink's OWN read (the second GetRun on this path) fails, the shape a
// transient store blip between the two takes.
func TestBedrockBearerSink_RefusesUnreadableRun(t *testing.T) {
	st := &bearerGuardStore{
		run:             types.AgentRun{ID: uuid.New(), Agent: "claude-code", State: types.RunRunning},
		failRunFromCall: 2,
		runErr:          errTransientStoreFailure,
	}
	h, _ := bearerGuardHarness(t, st)

	rr := resolveRecordedBearerGrant(t, h, st, "", "shared")
	assertBedrockBearerRefused(t, rr, h, http.StatusServiceUnavailable, credentialReauthRunUnreadableBody, "run_unreadable")
	if st.runCalls < 2 {
		t.Fatalf("GetRun called %d times, want >=2 (middleware + sink) — the sink's own read was never reached", st.runCalls)
	}
}

// TestBedrockBearerSink_RefusesUnreadableRoster pins the roster_unreadable arm:
// the run reads fine (agent = claude-code), but the roster does not, so the
// sink cannot tell "no per_user row" from "the row that says per_user could not
// be read" — both share the zero SiteConfig — and must refuse rather than
// treat the read failure as the former.
func TestBedrockBearerSink_RefusesUnreadableRoster(t *testing.T) {
	st := &bearerGuardStore{
		run:     types.AgentRun{ID: uuid.New(), Agent: "claude-code"},
		siteErr: errStoreNotFound,
	}
	h, _ := bearerGuardHarness(t, st)

	rr := resolveRecordedBearerGrant(t, h, st, "", "shared")
	assertBedrockBearerRefused(t, rr, h, http.StatusServiceUnavailable, credentialReauthStoreErrorBody, "roster_unreadable")
}

// TestBedrockBearerSink_RefusesAbsentPerUserBearer pins the
// per_user_bearer_absent arm: both reads succeed, the roster genuinely says
// per_user bearer for this agent, the grant records alice's own namespace, and
// alice (mintRunToken's subject) has no bedrock-api-key row of her own — she
// had one at dispatch and does not now, or never did. The owner-fallback read
// would serve the OPERATOR's row, billed to the org and attributed to nobody;
// the sink refuses that substitution.
func TestBedrockBearerSink_RefusesAbsentPerUserBearer(t *testing.T) {
	st := &bearerGuardStore{
		run:  types.AgentRun{ID: uuid.New(), Agent: "claude-code"},
		site: bearerRow(types.CredentialSourcePerUser),
	}
	h, _ := bearerGuardHarness(t, st)

	rr := resolveRecordedBearerGrant(t, h, st, bedrockGuardMember, "per_user")
	assertBedrockBearerRefused(t, rr, h, http.StatusFailedDependency, bedrockBearerNamespaceNotOwn, "per_user_bearer_absent")
}
