// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
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

// ─── the escape harness ───────────────────────────────────────────────────────

// govEscapeStore is the store surface ONE member create-and-dispatch drives:
// capStore's grants + governance answers, plus the run/policy/token rows the
// create path and dispatch read. Assembled from the two doubles this package
// already has rather than a third shape — the point of the test is the policy
// pipeline, not the store.
type govEscapeStore struct {
	*capStore
	mu         sync.Mutex
	policies   map[uuid.UUID]types.RunPolicy
	runs       map[uuid.UUID]types.AgentRun
	states     map[uuid.UUID]types.RunState
	token      *types.APIToken
	tokenRaw   string
	workspaces []types.Workspace
}

func newGovEscapeStore(cs *capStore) *govEscapeStore {
	return &govEscapeStore{
		capStore: cs,
		policies: map[uuid.UUID]types.RunPolicy{},
		runs:     map[uuid.UUID]types.AgentRun{},
		states:   map[uuid.UUID]types.RunState{},
	}
}

func (s *govEscapeStore) ListRuns(context.Context) ([]types.AgentRun, error) { return nil, nil }
func (s *govEscapeStore) ListWorkspaces(context.Context) ([]types.Workspace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.workspaces, nil
}
func (s *govEscapeStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return types.SiteConfig{}, nil
}
func (s *govEscapeStore) SetRunImage(context.Context, uuid.UUID, string) error   { return nil }
func (s *govEscapeStore) SetSandboxRef(context.Context, uuid.UUID, string) error { return nil }
func (s *govEscapeStore) SetRunAgentExecID(context.Context, uuid.UUID, string) error {
	return nil
}
func (s *govEscapeStore) SetRunFailureHint(context.Context, uuid.UUID, string) error { return nil }
func (s *govEscapeStore) CreateGrant(_ context.Context, g types.CredentialGrant) (types.CredentialGrant, error) {
	return g, nil
}

func (s *govEscapeStore) CreateRun(_ context.Context, run types.AgentRun) (types.AgentRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs[run.ID] = run
	s.states[run.ID] = run.State
	return run, nil
}

func (s *govEscapeStore) GetRun(_ context.Context, id uuid.UUID) (types.AgentRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[id]
	if !ok {
		return types.AgentRun{}, store.ErrNotFound
	}
	r.State = s.states[id]
	return r, nil
}

func (s *govEscapeStore) UpdateRunStateIf(_ context.Context, id uuid.UUID, from, to types.RunState) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.states[id] != from {
		return false, nil
	}
	s.states[id] = to
	return true, nil
}

func (s *govEscapeStore) GetPolicy(_ context.Context, id uuid.UUID) (types.RunPolicy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.policies[id]
	if !ok {
		return types.RunPolicy{}, store.ErrNotFound
	}
	return p, nil
}

func (s *govEscapeStore) GetAPITokenByRaw(_ context.Context, raw string) (types.APIToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token == nil || raw != s.tokenRaw {
		return types.APIToken{}, store.ErrNotFound
	}
	return *s.token, nil
}

func (s *govEscapeStore) TouchAPIToken(context.Context, uuid.UUID, time.Time) error { return nil }

// govEscapeFixture wires a Server that really creates AND dispatches a run, so
// the assertions below can read the run.policy.effective envelope — dispatch's
// post-widening authorization snapshot, and the only durable record of what a
// run was actually allowed to do (agent_runs carries no spec, run_policies.spec
// is overwritten in place, and an inline/default policy has no row at all).
func govEscapeFixture(t *testing.T, cs *capStore) (*Server, *govEscapeStore, *recRecorder) {
	t.Helper()
	h := newHarness(t)
	st := newGovEscapeStore(cs)
	audit := &recRecorder{}
	cfg := baseTestConfig(h, st)
	cfg.Audit = audit
	cfg.Broker = h.broker
	cfg.Runner = &fakeRunner{}
	// A secret store has to exist or validateInlineSecretRefs 422s any spec
	// carrying an api_key grant BEFORE the pipeline under test is reached —
	// which would make the grant rows pass for the wrong reason.
	cfg.Secrets = &memSecrets{m: map[string][]byte{govCorpSecret: []byte("v")}}
	cfg.OIDC = &oidc.Authenticator{}
	cfg.DefaultPolicy = govDeployment()
	return New(cfg), st, audit
}

// govSession is ssoSession with a group snapshot and its PF-26 truncation bit —
// the two fields the ceiling resolver reads and the shared helper does not
// carry.
func govSession(t *testing.T, sub string, groups []string, truncated bool) *http.Cookie {
	t.Helper()
	payload, err := json.Marshal(oidc.Session{
		V: oidc.SessionCodecVersion, Sub: sub, Email: sub + "@corp.example",
		Role: oidc.RoleMember, Expiry: time.Now().UTC().Add(time.Hour),
		Groups: groups, GroupsTruncated: truncated,
	})
	if err != nil {
		t.Fatalf("marshal session: %v", err)
	}
	mac := hmac.New(sha256.New, nil)
	mac.Write(payload)
	return &http.Cookie{
		Name:  "wardyn_session",
		Value: base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)),
	}
}

// govCreateAndDispatch POSTs a run as the given member and returns the decoded
// run.policy.effective envelope. It fails the test unless the create really
// reached dispatch — an escape row that 500s early would otherwise "pass" by
// never producing an envelope to contradict it.
func govCreateAndDispatch(t *testing.T, srv *Server, st *govEscapeStore, audit *recRecorder, cookie *http.Cookie, body string) types.RunPolicySpec {
	t.Helper()
	w := doSSO(t, srv, http.MethodPost, "/api/v1/runs", cookie, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201: %s", w.Code, w.Body.String())
	}
	st.mu.Lock()
	var runID uuid.UUID
	for id := range st.runs {
		runID = id
	}
	st.mu.Unlock()
	ev := findAudit(audit.events, runID, "run.policy.effective", "success")
	if ev == nil {
		t.Fatalf("dispatch recorded no run.policy.effective envelope for %s", runID)
	}
	var spec types.RunPolicySpec
	if err := json.Unmarshal(ev.Data, &spec); err != nil {
		t.Fatalf("envelope is not a RunPolicySpec: %v (%s)", err, ev.Data)
	}
	return spec
}

// govCorpSecret is the operator-stored secret every grant row tries to reach,
// and govWorkspaceRepo the onboarded repo row 10 references.
const (
	govCorpSecret    = "corp-api-key"
	govWorkspaceRepo = "https://github.com/octocat/Hello-World.git"
)

// ─── the escape table ─────────────────────────────────────────────────────────

// TestGovernanceProfileNonEscape is the escape table's create-time half (rows
// 1-10) plus row 16, asserted on the decoded run.policy.effective envelope —
// the DEFINED post-widening truth, not the spec the handler happened to hold at
// some intermediate step. Asserting anywhere earlier would prove nothing: the
// artifact-redirect phase inside dispatch adds hosts and injections AFTER
// create, which is the whole reason the envelope is the oracle.
//
// The fixture is one deployment ceiling (wide) and one assigned profile
// (narrow), so every row asks the same question in a different door: can a
// member reach something the DEPLOYMENT allows but their own PROFILE does not?
//
// Rows 11-15 (artifact-redirect, brokered git/PAT lanes, barrier
// self-approval, raw-IP, credential injection) belong to the dispatch
// re-assertion phase, which is a later slice; they are not silently missing.
func TestGovernanceProfileNonEscape(t *testing.T) {
	profile := govProfile("walled")
	profile.Ceiling.ToolRules = []types.ToolRule{{Tool: "Bash", Effect: types.ToolDeny}}
	assigned := func() *capStore {
		return &capStore{govProfile: profile, govTier: types.CapabilitySubjectGroup, govHasGroupTier: true}
	}
	member := func(t *testing.T) *http.Cookie { return govSession(t, "sub-walled", []string{"eng"}, false) }

	// Row 1 — INLINE WIDENING. The member names a host the deployment allows and
	// their profile does not. Counterfactual: clamp against s.cfg.DefaultPolicy
	// (the pre-0.7 line) and wide.example survives into the envelope.
	t.Run("row 1: inline widening to a deployment-allowed host", func(t *testing.T) {
		srv, st, audit := govEscapeFixture(t, assigned())
		got := govCreateAndDispatch(t, srv, st, audit, member(t),
			`{"agent":"claude-code","task":"t","inline_policy":{"min_confinement_class":"CC2","allowed_domains":["api.anthropic.com","wide.example"]}}`)
		if slices.Contains(got.AllowedDomains, "wide.example") {
			t.Errorf("allowed_domains = %v — a host only the DEPLOYMENT ceiling allows reached the run", got.AllowedDomains)
		}
	})

	// Row 2 — DENY REMOVAL. Omitting the profile's denied_domains must not
	// unwall them: composer.Clamp unions the ceiling's denies in, and the
	// envelope is where that has to be visible.
	t.Run("row 2: deny removal by omission", func(t *testing.T) {
		srv, st, audit := govEscapeFixture(t, assigned())
		got := govCreateAndDispatch(t, srv, st, audit, member(t),
			`{"agent":"claude-code","task":"t","inline_policy":{"min_confinement_class":"CC2","allowed_domains":["api.anthropic.com"]}}`)
		if !slices.Contains(got.DeniedDomains, "corp.internal") {
			t.Errorf("denied_domains = %v — the profile's wall vanished because the member simply did not mention it", got.DeniedDomains)
		}
	})

	// Row 3 — ALLOW-ALL. The single widest escape there is: one boolean that
	// makes every non-denied public host reachable.
	t.Run("row 3: allow_all_egress", func(t *testing.T) {
		srv, st, audit := govEscapeFixture(t, assigned())
		// The DEPLOYMENT is allow-all here, so only the PROFILE can drop it —
		// otherwise this row would pass against the old DefaultPolicy clamp too
		// and prove nothing about routing.
		srv.cfg.DefaultPolicy.AllowAllEgress = true
		got := govCreateAndDispatch(t, srv, st, audit, member(t),
			`{"agent":"claude-code","task":"t","inline_policy":{"min_confinement_class":"CC2","allow_all_egress":true,"allowed_domains":["api.anthropic.com"]}}`)
		if got.AllowAllEgress {
			t.Error("allow_all_egress survived: the member widened past their profile with one boolean")
		}
	})

	// Row 4 — FIRST-USE POSTURE. wait_for_review holds the connection open until
	// a human decides; always_deny never asks. A member must not be able to
	// downgrade the profile's posture to the deployment's looser one.
	t.Run("row 4: first_use_approval downgrade", func(t *testing.T) {
		srv, st, audit := govEscapeFixture(t, assigned())
		got := govCreateAndDispatch(t, srv, st, audit, member(t),
			`{"agent":"claude-code","task":"t","inline_policy":{"min_confinement_class":"CC2","first_use_approval":"wait_for_review","allowed_domains":["api.anthropic.com"]}}`)
		if got.FirstUseApproval != types.FirstUseAlwaysDeny {
			t.Errorf("first_use_approval = %q, want the profile's %q", got.FirstUseApproval, types.FirstUseAlwaysDeny)
		}
	})

	// Row 5 — CONFINEMENT FLOOR. The deployment floors at CC1, the profile at
	// CC2; a member asking for CC1 must get CC2.
	t.Run("row 5: confinement floor", func(t *testing.T) {
		srv, st, audit := govEscapeFixture(t, assigned())
		got := govCreateAndDispatch(t, srv, st, audit, member(t),
			`{"agent":"claude-code","task":"t","inline_policy":{"min_confinement_class":"CC1","allowed_domains":["api.anthropic.com"]}}`)
		if got.MinConfinementClass != types.CC2 {
			t.Errorf("min_confinement_class = %q, want the profile's CC2", got.MinConfinementClass)
		}
		st.mu.Lock()
		defer st.mu.Unlock()
		for _, run := range st.runs {
			if run.ConfinementClass.Rank() < types.CC2.Rank() {
				t.Errorf("the RUN launched at %q, below its own policy floor", run.ConfinementClass)
			}
		}
	})

	// Row 6 — TOOL RULES. The profile denies Bash; the member proposes allowing
	// it. tool_rules is proxy-enforced outside the sandbox, so a widened rule is
	// a real capability gain, not a preference.
	t.Run("row 6: tool_rules widening", func(t *testing.T) {
		srv, st, audit := govEscapeFixture(t, assigned())
		got := govCreateAndDispatch(t, srv, st, audit, member(t),
			`{"agent":"claude-code","task":"t","inline_policy":{"min_confinement_class":"CC2","allowed_domains":["api.anthropic.com"],"tool_rules":[{"tool":"Bash","effect":"allow"}]}}`)
		// DENY specifically, not merely "not allow": clamping against a
		// rule-less ceiling raises an unnamed tool to HOLD, so asserting only
		// "not allow" would pass against the deployment ceiling too and say
		// nothing about which ceiling was used.
		var bash types.ToolEffect
		for _, r := range got.ToolRules {
			if r.Tool == "Bash" {
				bash = r.Effect
			}
		}
		if bash != types.ToolDeny {
			t.Errorf("tool_rules Bash = %q, want the profile's %q (rules = %+v)", bash, types.ToolDeny, got.ToolRules)
		}
	})

	// Row 7 — GRANT PAIRING, in two legs, because two different gates drop a
	// grant and only one of them is the ceiling-routing under test.
	//
	//	7a: a KIND the profile does not carry at all — composer.Clamp's job.
	//	7b: a kind the profile DOES carry, paired with a DIFFERENT operator
	//	    secret. Clamp keeps same-kind grants, so this one reaches
	//	    filterMemberGrants — the seam that had to be re-pointed from
	//	    Config.DefaultPolicy to the caller's own ceiling. The deployment
	//	    eligible-lists the pairing, so nothing but the profile can drop it.
	t.Run("row 7a: a grant KIND the profile does not carry", func(t *testing.T) {
		srv, st, audit := govEscapeFixture(t, assigned())
		srv.cfg.DefaultPolicy.EligibleGrants = []types.GrantSpec{{
			Kind: types.GrantAPIKey, Scope: apiKeyScope(t, "api.anthropic.com", govCorpSecret), TTLSeconds: 300,
		}}
		got := govCreateAndDispatch(t, srv, st, audit, member(t),
			`{"agent":"claude-code","task":"t","inline_policy":{"min_confinement_class":"CC2","allowed_domains":["api.anthropic.com"],`+
				`"eligible_grants":[{"kind":"api_key","scope":{"host":"api.anthropic.com","header":"Authorization","secret_name":"`+govCorpSecret+`"}}]}}`)
		if len(got.EligibleGrants) != 0 {
			t.Errorf("eligible_grants = %+v — a grant kind the profile never lists reached the run", got.EligibleGrants)
		}
	})

	t.Run("row 7b: a same-kind grant paired with a secret the profile never blessed", func(t *testing.T) {
		blessed := govProfile("walled-with-a-grant")
		blessed.Ceiling.EligibleGrants = []types.GrantSpec{{
			Kind: types.GrantAPIKey, Scope: apiKeyScope(t, "api.anthropic.com", "profile-blessed-key"), TTLSeconds: 60,
		}}
		srv, st, audit := govEscapeFixture(t, &capStore{
			govProfile: blessed, govTier: types.CapabilitySubjectGroup, govHasGroupTier: true,
		})
		// The deployment blesses BOTH pairings, so composer.Clamp keeps the kind
		// and the ONLY gate left is filterMemberGrants reading the right list.
		srv.cfg.DefaultPolicy.EligibleGrants = []types.GrantSpec{
			{Kind: types.GrantAPIKey, Scope: apiKeyScope(t, "api.anthropic.com", "profile-blessed-key"), TTLSeconds: 300},
			{Kind: types.GrantAPIKey, Scope: apiKeyScope(t, "api.anthropic.com", govCorpSecret), TTLSeconds: 300},
		}
		got := govCreateAndDispatch(t, srv, st, audit, member(t),
			`{"agent":"claude-code","task":"t","inline_policy":{"min_confinement_class":"CC2","allowed_domains":["api.anthropic.com"],`+
				`"eligible_grants":[{"kind":"api_key","scope":{"host":"api.anthropic.com","header":"Authorization","secret_name":"`+govCorpSecret+`"}}]}}`)
		for _, g := range got.EligibleGrants {
			if strings.Contains(string(g.Scope), govCorpSecret) {
				t.Errorf("eligible_grants = %+v — a member paired an operator secret their profile never made eligible", got.EligibleGrants)
			}
		}
	})

	// Row 8 — STORED-POLICY SELECTION (PF-1, the central escape). policy_id is
	// ungated by design, so a member could select an admin-authored row far
	// wider than their own ceiling and get every byte of it — entirely past the
	// clamp their inline_policy would have hit.
	t.Run("row 8: selecting a wide stored policy", func(t *testing.T) {
		srv, st, audit := govEscapeFixture(t, assigned())
		wide := types.RunPolicy{ID: uuid.New(), Name: "wide", Spec: types.RunPolicySpec{
			AllowedDomains:      []string{"api.anthropic.com", "wide.example"},
			MinConfinementClass: types.CC1,
			FirstUseApproval:    types.FirstUseWaitForReview,
		}}
		st.policies[wide.ID] = wide
		got := govCreateAndDispatch(t, srv, st, audit, member(t),
			`{"agent":"claude-code","task":"t","policy_id":"`+wide.ID.String()+`"}`)
		if slices.Contains(got.AllowedDomains, "wide.example") {
			t.Errorf("allowed_domains = %v — a stored row let the member out past their own ceiling", got.AllowedDomains)
		}
		if got.MinConfinementClass != types.CC2 {
			t.Errorf("min_confinement_class = %q, want the profile's CC2", got.MinConfinementClass)
		}
	})

	// Row 9 — NO POLICY AT ALL. The most-travelled create path: if the
	// no-policy default stayed on Config.DefaultPolicy the feature would be
	// optional in practice, since a member need only omit the policy to get the
	// deployment ceiling.
	t.Run("row 9: no policy authored", func(t *testing.T) {
		srv, st, audit := govEscapeFixture(t, assigned())
		got := govCreateAndDispatch(t, srv, st, audit, member(t), `{"agent":"claude-code","task":"t"}`)
		if slices.Contains(got.AllowedDomains, "wide.example") {
			t.Errorf("allowed_domains = %v — omitting the policy handed the member the DEPLOYMENT ceiling", got.AllowedDomains)
		}
		if !slices.Contains(got.DeniedDomains, "corp.internal") {
			t.Errorf("denied_domains = %v — the profile's walls are absent from a no-policy run", got.DeniedDomains)
		}
	})

	// Row 10 — WORKSPACE REPO REFERENCE. An inline workspace_repos entry is a
	// second door into the workspace lane, whose admin-authored egress and
	// operator_set grants get folded into the run. The member's own ceiling
	// still has to bound the egress the run ends up with.
	t.Run("row 10: workspace repo reference does not widen egress", func(t *testing.T) {
		srv, st, audit := govEscapeFixture(t, assigned())
		// The repo must be ONBOARDED, or validateWorkspaceSources 422s it as
		// un-onboarded and this row proves nothing about the ceiling.
		st.workspaces = []types.Workspace{{
			ID: uuid.New(), Name: "hello",
			Sources: []types.WorkspaceSource{{
				Type: types.WorkspaceSourceTypeRepo, Source: govWorkspaceRepo,
			}},
		}}
		got := govCreateAndDispatch(t, srv, st, audit, member(t),
			`{"agent":"claude-code","task":"t","inline_policy":{"min_confinement_class":"CC2","allowed_domains":["api.anthropic.com","wide.example"],`+
				`"workspace_repos":[{"repo":"`+govWorkspaceRepo+`"}]}}`)
		if slices.Contains(got.AllowedDomains, "wide.example") {
			t.Errorf("allowed_domains = %v — the workspace lane re-widened past the profile", got.AllowedDomains)
		}
	})

	// ─── row 16, BOTH legs ────────────────────────────────────────────────────
	//
	// The group tier can EVAPORATE. sessionGroups truncates the snapshot at the
	// cookie byte cap, so the group whose assignment walls a member can simply
	// be missing — and without the truncation bit the resolver answers "no group
	// matched", hands out the deployment ceiling, and nothing anywhere says so.
	// Both credentials that carry a snapshot have to close it.

	t.Run("row 16a: a TRUNCATED session snapshot is refused, not silently widened", func(t *testing.T) {
		// Counterfactual: read only capabilitySubjects' nil-vs-empty `stale` and
		// this create returns 201 under the DEPLOYMENT ceiling — the exact
		// silent widening, with a normal-looking run and a normal-looking audit.
		srv, _, _ := govEscapeFixture(t, assigned())
		w := doSSO(t, srv, http.MethodPost, "/api/v1/runs",
			govSession(t, "sub-many-groups", []string{"a-team"}, true),
			`{"agent":"claude-code","task":"t"}`)
		if w.Code != http.StatusForbidden {
			t.Fatalf("create = %d, want 403: %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "groups_snapshot_stale") {
			t.Errorf("403 body does not name the condition: %s", w.Body.String())
		}
	})

	t.Run("row 16b: a NULL-marker (pre-0.7) API token is refused", func(t *testing.T) {
		// The token lane replays a FROZEN snapshot into the same
		// capabilitySubjects the resolver reads (apitokens.go), so it re-imports
		// the evaporation unless the marker rides along. A pre-0.7 row has NULL
		// there — completeness genuinely unknown — and unknown must read as
		// truncated. Counterfactual: treat NULL as false and this token gets a
		// 201 under the deployment ceiling, which is the cookie bug reopened
		// through the credential that ships first.
		srv, st, _ := govEscapeFixture(t, assigned())
		st.tokenRaw = apiTokenPrefix + "deadbeef"
		st.token = &types.APIToken{
			ID: uuid.New(), Principal: "sub-legacy-token", Email: "legacy@corp.example",
			Role: oidc.RoleMember, Groups: []string{"a-team"},
			GroupsTruncated: nil, // the NULL marker: minted before 0.7
			Name:            "legacy",
		}
		r := httptest.NewRequest(http.MethodPost, "/api/v1/runs",
			strings.NewReader(`{"agent":"claude-code","task":"t"}`))
		r.Header.Set("Authorization", "Bearer "+st.tokenRaw)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Fatalf("create with a pre-0.7 token = %d, want 403: %s", w.Code, w.Body.String())
		}

		// And a token minted AFTER the bump, stamped complete, works normally —
		// otherwise "fail closed" would just mean "the token lane is broken".
		complete := false
		st.token.GroupsTruncated = &complete
		r = httptest.NewRequest(http.MethodPost, "/api/v1/runs",
			strings.NewReader(`{"agent":"claude-code","task":"t"}`))
		r.Header.Set("Authorization", "Bearer "+st.tokenRaw)
		w = httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusCreated {
			t.Fatalf("create with a 0.7-stamped token = %d, want 201: %s", w.Code, w.Body.String())
		}
	})
}

// ─── the stored-policy clamp, red then green ──────────────────────────────────

// TestStoredPolicyClampCounterfactual is PF-1's before/after in one test: the
// SAME member selects the SAME wide stored policy, and the only thing that
// changes between the two halves is whether a governance profile applies to
// them.
//
// BEFORE (unassigned): unclamped, byte-for-byte today. That is deliberate and
// is the named residual — stored policies are routinely wider than a minimal
// DefaultPolicy, so clamping unconditionally would shred deployments that have
// never heard of this feature. The deployment-wide opt-in is an `all`-subject
// assignment.
//
// AFTER (assigned): the FULL member pipeline. Not Clamp alone — clampGrants
// passes same-kind grant pairings through VERBATIM, so a Clamp-only stored
// branch would hand the member every operator-secret pairing the row carried,
// and "one rule whether body or row id" would be false at exactly the
// grant-bearing rows.
func TestStoredPolicyClampCounterfactual(t *testing.T) {
	const corpHost = "api.corp.example"
	wide := types.RunPolicy{ID: uuid.New(), Name: "wide", Spec: types.RunPolicySpec{
		AllowedDomains:      []string{"api.anthropic.com", "wide.example", corpHost},
		MinConfinementClass: types.CC1,
		FirstUseApproval:    types.FirstUseWaitForReview,
		EligibleGrants: []types.GrantSpec{{
			Kind: types.GrantAPIKey, Scope: apiKeyScope(t, corpHost, govCorpSecret), TTLSeconds: 300,
		}},
	}}
	body := `{"agent":"claude-code","task":"t","policy_id":"` + wide.ID.String() + `"}`

	t.Run("BEFORE: no assignment — the stored row is served unclamped", func(t *testing.T) {
		srv, st, audit := govEscapeFixture(t, &capStore{}) // resolves ErrNotFound
		// The deployment ceiling must ALSO carry the pairing, or the residual
		// would be masked by an unrelated drop rather than proven.
		srv.cfg.DefaultPolicy.EligibleGrants = wide.Spec.EligibleGrants
		st.policies[wide.ID] = wide
		got := govCreateAndDispatch(t, srv, st, audit, govSession(t, "sub-unassigned", []string{"eng"}, false), body)
		if !slices.Contains(got.AllowedDomains, "wide.example") {
			t.Errorf("allowed_domains = %v — an UNASSIGNED member's stored-policy run changed shape; that is not byte-for-byte today", got.AllowedDomains)
		}
		if got.MinConfinementClass != types.CC1 {
			t.Errorf("min_confinement_class = %q, want the stored row's CC1 for an unassigned member", got.MinConfinementClass)
		}
	})

	t.Run("AFTER: assigned — clamp + grant filter + capability narrowing", func(t *testing.T) {
		profile := govProfile("walled")
		// The profile carries the api_key KIND but a DIFFERENT pairing, so
		// composer.Clamp keeps the stored row's grant and the drop has to come
		// from filterMemberGrants — which is the point: clampGrants passes
		// same-kind pairings through verbatim, so Clamp alone would hand this
		// member the operator secret the stored row named.
		profile.Ceiling.EligibleGrants = []types.GrantSpec{{
			Kind: types.GrantAPIKey, Scope: apiKeyScope(t, corpHost, "profile-blessed-key"), TTLSeconds: 60,
		}}
		cs := &capStore{govProfile: profile, govTier: types.CapabilitySubjectUser}
		srv, st, audit := govEscapeFixture(t, cs)
		srv.cfg.DefaultPolicy.EligibleGrants = append(
			append([]types.GrantSpec(nil), wide.Spec.EligibleGrants...),
			types.GrantSpec{Kind: types.GrantAPIKey, Scope: apiKeyScope(t, corpHost, "profile-blessed-key"), TTLSeconds: 300})
		st.policies[wide.ID] = wide
		got := govCreateAndDispatch(t, srv, st, audit, govSession(t, "sub-walled", []string{"eng"}, false), body)

		if slices.Contains(got.AllowedDomains, "wide.example") {
			t.Errorf("allowed_domains = %v — the stored row was served past the member's ceiling", got.AllowedDomains)
		}
		if got.MinConfinementClass != types.CC2 {
			t.Errorf("min_confinement_class = %q, want the profile's CC2", got.MinConfinementClass)
		}
		// The grant-bearing half, which is the reason Clamp alone is not enough:
		// the profile carries no eligible grants, so filterMemberGrants must
		// drop the pairing the stored row carried verbatim.
		for _, g := range got.EligibleGrants {
			if strings.Contains(string(g.Scope), govCorpSecret) {
				t.Errorf("eligible_grants = %+v — a stored row's operator-secret pairing survived into a walled member's run", got.EligibleGrants)
			}
		}
	})
}
