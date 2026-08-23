// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Coverage for the decision-SCOPE rules on POST /approvals/{id}/{approve,deny}.
//
// There is no approvals_test.go in this package — the decide path's existing
// coverage is spread across rbac_test.go, authz_test.go, approvals_list_test.go
// and approved_egress_test.go — so this file is the deliberate home for the
// scope rules rather than a duplicate of any of those.
//
// Three properties here are security invariants, not conveniences:
//   - the member kind/ownership gate runs BEFORE any scope rule, so a scope rule
//     can never become an existence oracle for another user's approval;
//   - `always` is operator-only, because it writes durable workspace config that
//     every other route guards behind operatorOnly;
//   - `always` actually REACHES that config, in both directions, and rule 7's two
//     reject sets stay the asymmetric pair they were designed as. Everything
//     `always` promises lives outside the response body, so a status-code-only
//     suite can be entirely green over a feature that persists nothing.

// decideBody marshals a decision request body the way a real client would.
func decideBody(t *testing.T, scope types.ApprovalScope, expires *time.Time) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"reason":              "test",
		"decision_scope":      scope,
		"decision_expires_at": expires,
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	return string(b)
}

// scopeFixture wires the same harness the authz suite uses, with one PENDING
// egress_domain approval on a run the member owns.
type scopeFixture struct {
	srv      *Server
	store    *authzStore
	approval *authzApprovals
	runID    uuid.UUID
	memberID string
}

func newScopeFixture(t *testing.T) *scopeFixture {
	t.Helper()
	ast := newAuthzStore()
	aap := newAuthzApprovals(ast)
	h := newHarness(t)
	cfg := baseTestConfig(h, ast)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Approvals = aap
	// A WILDCARD-only model-provider ceiling, which is what applyLLMCredMount's
	// own error text tells operators to write. It is also the shape that decides
	// whether rule 7's deny guard fires at all: modelProviderEgress returns
	// ceiling entries verbatim, so "api.anthropic.com" is NOT a member of
	// {"*.anthropic.com"} — a guard written as set membership would silently pass
	// the deny that bricks the workspace. See the asymmetry test below.
	cfg.DefaultPolicy = types.RunPolicySpec{AllowedDomains: []string{"*.anthropic.com"}}
	srv := New(cfg)

	const memberSub = "sub-member-scope"
	runID := uuid.New()
	ast.mu.Lock()
	ast.runs[runID] = types.AgentRun{ID: runID, CreatedBy: memberSub, State: types.RunRunning}
	ast.mu.Unlock()

	return &scopeFixture{srv: srv, store: ast, approval: aap, runID: runID, memberID: memberSub}
}

func (f *scopeFixture) seedEgress(t *testing.T, host string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	f.approval.mu.Lock()
	f.approval.byID[id] = types.ApprovalRequest{
		ID: id, RunID: f.runID, Kind: types.ApprovalEgressDomain,
		RequestedScope: json.RawMessage(`{"host":` + strconv.Quote(host) + `}`),
		State:          types.ApprovalPending, RequestedAt: time.Now().UTC(),
	}
	f.approval.mu.Unlock()
	return id
}

// seedWorkspace gives the fixture's run something for `always` to persist to,
// via WorkspaceIDs — the read-only denormalization runs.go writes at create —
// and never via WorkspaceID, which is the TRUSTED scan/verify/record linkage a
// user run must not claim. Rule 5's tie-break prefers WorkspaceIDs[0], so this
// is the ordinary-run path an operator actually clicks through.
func (f *scopeFixture) seedWorkspace(t *testing.T, approved, denied []string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	f.store.mu.Lock()
	defer f.store.mu.Unlock()
	f.store.workspaces[id] = types.Workspace{
		ID: id, Name: "scope-ws", ApprovedEgress: approved, DeniedEgress: denied,
	}
	run := f.store.runs[f.runID]
	run.WorkspaceIDs = []uuid.UUID{id}
	f.store.runs[f.runID] = run
	return id
}

// egressLists reads the durable half back. `always` promises nothing the
// response body can show, so every assertion about it has to come from here.
func (f *scopeFixture) egressLists(t *testing.T, wsID uuid.UUID) (approved, denied []string) {
	t.Helper()
	ws, err := f.store.GetWorkspace(t.Context(), wsID)
	if err != nil {
		t.Fatalf("read workspace back: %v", err)
	}
	return ws.ApprovedEgress, ws.DeniedEgress
}

// TestDecideScope_BodyValidation covers rules 0-3: the shapes the server must
// refuse before the decision becomes durable, because PENDING->decided is
// one-way and a 4xx afterwards would be meaningless.
func TestDecideScope_BodyValidation(t *testing.T) {
	f := newScopeFixture(t)
	admin := ssoSession(t, "sub-admin-scope", "admin@corp.example", oidc.RoleAdmin)
	future := time.Now().UTC().Add(time.Hour)
	past := time.Now().UTC().Add(-time.Hour)
	tooFar := time.Now().UTC().Add(60 * 24 * time.Hour)

	cases := []struct {
		name string
		body string
		want int
	}{
		// Rule 0: an EMPTY body must still work — three shipped shell clients
		// POST approve with no -d at all and assert 2xx, so io.EOF is not an error.
		{"empty body still decides", "", http.StatusOK},
		{"garbage body is rejected", `{"reason":`, http.StatusBadRequest},
		// Rule 1
		{"unknown scope is rejected", decideBody(t, "wat", nil), http.StatusBadRequest},
		// Rule 2
		{"until without an expiry", decideBody(t, types.ScopeUntil, nil), http.StatusBadRequest},
		{"until in the past", decideBody(t, types.ScopeUntil, &past), http.StatusBadRequest},
		{"until beyond the cap", decideBody(t, types.ScopeUntil, &tooFar), http.StatusBadRequest},
		{"until with a future expiry is accepted", decideBody(t, types.ScopeUntil, &future), http.StatusOK},
		// Rule 3: never silently ignore an expiry the scope cannot use.
		{"expiry without until", decideBody(t, types.ScopeOnce, &future), http.StatusBadRequest},
		// The two scopes that need no extra data.
		{"once is accepted", decideBody(t, types.ScopeOnce, nil), http.StatusOK},
		{"run is accepted", decideBody(t, types.ScopeRun, nil), http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := f.seedEgress(t, "registry.npmjs.org")
			w := doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+"/approve", admin, tc.body)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d; body=%s", w.Code, tc.want, w.Body.String())
			}
		})
	}
}

// TestDecideScope_MemberGateRunsBeforeScopeRules is the oracle guard. Rule 4
// (a non-egress kind rejects a scope) must never be reachable by a member,
// because a 400 there would distinguish "a credential approval exists on someone
// else's run" from "no such approval" — exactly the distinction the surrounding
// 404s are built to erase.
func TestDecideScope_MemberGateRunsBeforeScopeRules(t *testing.T) {
	f := newScopeFixture(t)
	member := ssoSession(t, f.memberID, "member@corp.example", oidc.RoleMember)

	// A credential approval on a run this member does NOT own.
	foreignRun := uuid.New()
	f.store.mu.Lock()
	f.store.runs[foreignRun] = types.AgentRun{ID: foreignRun, CreatedBy: "someone-else", State: types.RunRunning}
	f.store.mu.Unlock()

	credID := uuid.New()
	f.approval.mu.Lock()
	f.approval.byID[credID] = types.ApprovalRequest{
		ID: credID, RunID: foreignRun, Kind: types.ApprovalCredential,
		State: types.ApprovalPending, RequestedAt: time.Now().UTC(),
	}
	f.approval.mu.Unlock()

	w := doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+credID.String()+"/approve",
		member, decideBody(t, types.ScopeOnce, nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("member sending a scope at a foreign credential approval: status = %d, want 404 "+
			"(a 400 from the scope rule would confirm the approval exists); body=%s", w.Code, w.Body.String())
	}
}

// TestDecideScope_AlwaysIsOperatorOnly covers rule 6. The approve/deny routes sit
// on the MEMBER group, so without this check a member self-grants a permanent
// workspace allowlist entry through the approval queue — a back door around the
// operatorOnly gate on PUT /workspaces/{id}/approved-egress.
func TestDecideScope_AlwaysIsOperatorOnly(t *testing.T) {
	f := newScopeFixture(t)
	member := ssoSession(t, f.memberID, "member@corp.example", oidc.RoleMember)

	id := f.seedEgress(t, "registry.npmjs.org")
	w := doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+"/approve",
		member, decideBody(t, types.ScopeAlways, nil))
	if w.Code != http.StatusForbidden {
		t.Fatalf("member picking always: status = %d, want 403; body=%s", w.Code, w.Body.String())
	}

	// The approval must remain PENDING — a rejected rule may never leave a
	// half-applied decision behind, because the transition is one-way.
	f.approval.mu.Lock()
	got := f.approval.byID[id].State
	f.approval.mu.Unlock()
	if got != types.ApprovalPending {
		t.Fatalf("approval state after a rejected always = %q, want PENDING", got)
	}
}

// TestDecideScope_AlwaysNeedsAWorkspace covers rule 5. run.WorkspaceIDs is empty
// for a run that references no onboarded workspace, and `always` has nowhere to
// persist to — so it must be refused rather than silently downgraded.
func TestDecideScope_AlwaysNeedsAWorkspace(t *testing.T) {
	f := newScopeFixture(t)
	admin := ssoSession(t, "sub-admin-ws", "admin@corp.example", oidc.RoleAdmin)

	id := f.seedEgress(t, "registry.npmjs.org")
	w := doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+"/approve",
		admin, decideBody(t, types.ScopeAlways, nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("always on a workspace-free run: status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	f.approval.mu.Lock()
	got := f.approval.byID[id].State
	f.approval.mu.Unlock()
	if got != types.ApprovalPending {
		t.Fatalf("approval state after a rejected always = %q, want PENDING", got)
	}
}

// TestDecideScope_NonEgressKindRejectsScope covers rule 4 on the path where it
// IS reachable — an operator, who is past the member gate. A credential mints
// exactly once by construction and a tool_call is clamped, so a scope there is
// meaningless and must be refused rather than persisted as a no-op.
func TestDecideScope_NonEgressKindRejectsScope(t *testing.T) {
	f := newScopeFixture(t)
	admin := ssoSession(t, "sub-admin-kind", "admin@corp.example", oidc.RoleAdmin)

	for _, kind := range []types.ApprovalKind{types.ApprovalCredential, types.ApprovalToolCall} {
		t.Run(string(kind), func(t *testing.T) {
			id := uuid.New()
			f.approval.mu.Lock()
			f.approval.byID[id] = types.ApprovalRequest{
				ID: id, RunID: f.runID, Kind: kind,
				State: types.ApprovalPending, RequestedAt: time.Now().UTC(),
			}
			f.approval.mu.Unlock()

			w := doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+"/approve",
				admin, decideBody(t, types.ScopeOnce, nil))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("scope on a %s approval: status = %d, want 400; body=%s", kind, w.Code, w.Body.String())
			}

			// ...but the SAME approval must still decide fine with no scope, so the
			// new rule cannot have broken the binary path these kinds still use.
			w = doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+"/approve", admin, "")
			if w.Code != http.StatusOK {
				t.Fatalf("scopeless decide on a %s approval: status = %d, want 200; body=%s", kind, w.Code, w.Body.String())
			}
		})
	}
}

// TestDecideScope_DefaultsToRun pins the compatibility promise: a caller that
// sends no scope at all gets today's behavior. Every pre-existing client — the
// SDK, the console, and the three shell suites — is in this case.
func TestDecideScope_DefaultsToRun(t *testing.T) {
	f := newScopeFixture(t)
	admin := ssoSession(t, "sub-admin-default", "admin@corp.example", oidc.RoleAdmin)

	id := f.seedEgress(t, "registry.npmjs.org")
	w := doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+"/approve", admin, `{"reason":"no scope"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("scopeless approve: status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got types.ApprovalRequest
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.State != types.ApprovalApproved {
		t.Fatalf("state = %q, want APPROVED", got.State)
	}
	// The wire value stays EMPTY rather than a materialized "run": a decision
	// nobody expressed must not be reported as one. Normalize() is what supplies
	// run at every runtime read.
	if got.DecisionScope != "" {
		t.Fatalf("decision_scope = %q for a scopeless decide, want empty", got.DecisionScope)
	}
}

// TestDecideScope_AlwaysPersistsToTheWorkspace is `always`'s durable half — the
// only part of the feature that outlives the run, and the part a 200 cannot
// speak for. Two invariants, one per direction:
//
//   - DENY·always must persist. The write-back sits OUTSIDE decide()'s
//     `if approve` block deliberately; tucking it inside — beside
//     learnVerifyEgress, where it visually belongs — compiles, keeps every other
//     assertion in this file green, and turns an operator's permanent deny into a
//     no-op behind a 200 and a green console.
//   - The host MOVES between the two lists rather than merely being appended to
//     one. Deny beats allow everywhere the proxy evaluates policy, so a host left
//     on both lists makes one of the two directions silently do nothing — an
//     approve·always that never un-denies is exactly as broken as one that never
//     writes.
//
// Asserted through the STORE, not the response body, for a third reason: the
// write-back is guarded on result.DecisionScope, i.e. on what the store echoed
// back rather than on what the handler sent. A store that stops persisting
// decision_scope therefore stops firing the write-back too, and only a durable
// read catches both failures at once.
func TestDecideScope_AlwaysPersistsToTheWorkspace(t *testing.T) {
	const host = "registry.npmjs.org"
	// A second host on each list that must survive untouched: the write-back
	// edits one entry, it does not replace the operator's list.
	const bystander = "pypi.org"

	cases := []struct {
		name                     string
		verb                     string
		seedApproved, seedDenied []string
		wantApproved, wantDenied []string
	}{
		{
			name: "deny always persists and clears the stale allow", verb: "deny",
			seedApproved: []string{host, bystander},
			wantApproved: []string{bystander},
			wantDenied:   []string{host},
		},
		{
			name: "approve always persists and clears the stale deny", verb: "approve",
			seedDenied:   []string{host, bystander},
			wantApproved: []string{host},
			wantDenied:   []string{bystander},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newScopeFixture(t)
			admin := ssoSession(t, "sub-admin-persist", "admin@corp.example", oidc.RoleAdmin)
			wsID := f.seedWorkspace(t, tc.seedApproved, tc.seedDenied)

			id := f.seedEgress(t, host)
			w := doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+"/"+tc.verb,
				admin, decideBody(t, types.ScopeAlways, nil))
			if w.Code != http.StatusOK {
				t.Fatalf("%s always: status = %d, want 200; body=%s", tc.verb, w.Code, w.Body.String())
			}

			approved, denied := f.egressLists(t, wsID)
			if !slices.Equal(approved, tc.wantApproved) {
				t.Errorf("%s always: approved_egress = %v, want %v (the write-back never reached the workspace, "+
					"or added without removing from the other list)", tc.verb, approved, tc.wantApproved)
			}
			if !slices.Equal(denied, tc.wantDenied) {
				t.Errorf("%s always: denied_egress = %v, want %v (the write-back never reached the workspace, "+
					"or added without removing from the other list)", tc.verb, denied, tc.wantDenied)
			}
		})
	}
}

// TestDecideScope_AlwaysRejectSetsAreNotSymmetric pins rule 7's two sets as the
// deliberately asymmetric pair they are. Swapping them — or collapsing them into
// one shared set, the obvious "cleanup" — leaves every other test in this file
// green, so this is the only thing standing between the code and that edit.
//
// The two rationales do not transfer:
//
//   - approveAlwaysRejects refuses hosts a real run's proxy will NEVER consult,
//     so promoting one writes dead weight the operator believes is a grant. A
//     git-broker host is dead weight as an allow — but as a DENY it is genuinely
//     consulted (the dispatcher reads and extends policy.DeniedDomains), so the
//     "never consulted" argument is allow-shaped only and a deny must go through.
//   - denyAlwaysReject refuses the opposite hazard: deny beats everything, and
//     Policy.AllowedExactHost — the gate for proxy-side credential injection —
//     returns false on a denied host, so one deny·always on a model-provider host
//     leaves every future run of the workspace holding a credential it can never
//     use. The same host as an ALLOW is merely redundant, and refusing it would
//     be wrong.
//
// The model-provider rows run against the fixture's WILDCARD-only ceiling on
// purpose. That is the shape the product's own error text instructs operators to
// write, and it is precisely where a guard implemented as membership in
// modelProviderEgress's output stops firing while a guard implemented as the
// isModelProviderHost predicate keeps working — a guard whose firing depends on
// deployment config is worse than one uniformly absent.
func TestDecideScope_AlwaysRejectSetsAreNotSymmetric(t *testing.T) {
	cases := []struct {
		name string
		host string
		verb string
		want int
	}{
		// A git-broker host: dead weight as a permanent allow...
		{"approve always on a broker-routed host", "github.com", "approve", http.StatusBadRequest},
		// ...but a real, consulted policy entry as a permanent deny.
		{"deny always on a broker-routed host", "github.com", "deny", http.StatusOK},
		// A model-provider host under a wildcard ceiling: redundant as an allow...
		{"approve always on a model-provider host", "api.anthropic.com", "approve", http.StatusOK},
		// ...and workspace-bricking as a deny.
		{"deny always on a model-provider host", "api.anthropic.com", "deny", http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newScopeFixture(t)
			admin := ssoSession(t, "sub-admin-reject", "admin@corp.example", oidc.RoleAdmin)
			wsID := f.seedWorkspace(t, nil, nil)

			id := f.seedEgress(t, tc.host)
			w := doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+"/"+tc.verb,
				admin, decideBody(t, types.ScopeAlways, nil))
			if w.Code != tc.want {
				t.Fatalf("%s always on %s: status = %d, want %d; body=%s", tc.verb, tc.host, w.Code, tc.want, w.Body.String())
			}

			approved, denied := f.egressLists(t, wsID)
			if tc.want != http.StatusOK {
				// Rule 7 runs BEFORE Decide(), so a rejection leaves nothing
				// half-applied: the row is still decidable and the workspace is
				// untouched. A guard that fired after the transition would be
				// refusing a decision the operator can no longer take back.
				f.approval.mu.Lock()
				state := f.approval.byID[id].State
				f.approval.mu.Unlock()
				if state != types.ApprovalPending {
					t.Errorf("approval state after a rejected always = %q, want PENDING", state)
				}
				if len(approved)+len(denied) != 0 {
					t.Errorf("rejected always still wrote the workspace: approved=%v denied=%v", approved, denied)
				}
				return
			}
			// The accepted direction is only meaningfully accepted if it landed:
			// a rule 7 that rejected nothing at all would pass the rows above.
			want := &denied
			if tc.verb == "approve" {
				want = &approved
			}
			if !slices.Contains(*want, tc.host) {
				t.Errorf("%s always on %s returned 200 but did not persist: approved=%v denied=%v",
					tc.verb, tc.host, approved, denied)
			}
		})
	}
}

// TestReconcileWorkspaceEgressDecisions is the D28 heal: the post-Decide
// write-back is not atomic with Decide, so a PG blip there leaves an approval
// durably decided `always` while its workspace never got the allow/deny row —
// the operator's permanent decision dropped behind a 200. Reconcile re-applies
// every decided always-egress decision to its workspace, recreating the dropped
// row (and idempotently no-op'ing already-persisted ones).
//
// The pre-fix state is modelled directly: a decided always approval whose
// workspace egress lists are empty (the dropped write-back). Before the fix
// nothing recreated it; after, reconcile does — proven by the row appearing.
func TestReconcileWorkspaceEgressDecisions(t *testing.T) {
	f := newScopeFixture(t)
	// A workspace linked to the fixture's run, with EMPTY egress lists — the
	// state left behind when the write-back was dropped.
	wsID := f.seedWorkspace(t, nil, nil)

	seedDecided := func(host string, state types.ApprovalState) {
		id := uuid.New()
		f.approval.mu.Lock()
		f.approval.byID[id] = types.ApprovalRequest{
			ID: id, RunID: f.runID, Kind: types.ApprovalEgressDomain,
			RequestedScope: json.RawMessage(`{"host":"` + host + `"}`),
			State:          state, DecisionScope: types.ScopeAlways,
		}
		f.approval.mu.Unlock()
	}
	seedDecided("registry.npmjs.org", types.ApprovalApproved) // -> approved_egress
	seedDecided("evil.example.com", types.ApprovalDenied)     // -> denied_egress
	// A non-always decision must NOT be persisted by the reconcile.
	f.approval.mu.Lock()
	idRun := uuid.New()
	f.approval.byID[idRun] = types.ApprovalRequest{
		ID: idRun, RunID: f.runID, Kind: types.ApprovalEgressDomain,
		RequestedScope: json.RawMessage(`{"host":"ephemeral.example.com"}`),
		State:          types.ApprovalApproved, DecisionScope: types.ScopeRun,
	}
	f.approval.mu.Unlock()

	n, err := f.srv.ReconcileWorkspaceEgressDecisions(t.Context())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if n != 2 {
		t.Fatalf("reconciled %d decisions, want 2 (the two always ones)", n)
	}
	approved, denied := f.egressLists(t, wsID)
	if !slices.Contains(approved, "registry.npmjs.org") {
		t.Errorf("approve·always host not healed onto approved_egress: %v", approved)
	}
	if !slices.Contains(denied, "evil.example.com") {
		t.Errorf("deny·always host not healed onto denied_egress: %v", denied)
	}
	if slices.Contains(approved, "ephemeral.example.com") {
		t.Errorf("a run-scoped decision must not be persisted: %v", approved)
	}

	// Idempotent: a second reconcile re-applies the same rows without duplicating.
	if _, err := f.srv.ReconcileWorkspaceEgressDecisions(t.Context()); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	approved, _ = f.egressLists(t, wsID)
	if got := slices.Contains(approved, "registry.npmjs.org"); !got || len(approved) != 1 {
		t.Errorf("reconcile not idempotent: approved=%v", approved)
	}
}
