// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The control-plane half of the Azure DevOps capability hold. The Postgres
// halves — the once-spend UPDATE and the pending-uniqueness index — are pinned
// in internal/store (store_approval_once_pg_test.go); here the store is a
// double that models them.

// adoCapStore is the resolve fixture's store plus the two transactional seams
// the arm type-asserts for, modelled over the approvals double.
type adoCapStore struct {
	*adoTestStore
	approvals *fakeApprovals
}

func (s *adoCapStore) SpendApprovalOnce(_ context.Context, id uuid.UUID, jti string) (bool, error) {
	s.approvals.mu.Lock()
	defer s.approvals.mu.Unlock()
	ap, ok := s.approvals.byID[id]
	if !ok || ap.MintedJTI != "" || ap.State != types.ApprovalApproved || ap.DecisionScope != types.ScopeOnce ||
		ap.Kind != types.ApprovalToolCall || ap.GrantID == nil {
		return false, nil
	}
	ap.MintedJTI = jti
	s.approvals.byID[id] = ap
	return true, nil
}

func (s *adoCapStore) ResolveReauthApproval(_ context.Context, id uuid.UUID, d types.ApprovalDecision, _ types.AuditEvent) (types.ApprovalRequest, error) {
	s.approvals.mu.Lock()
	defer s.approvals.mu.Unlock()
	ap := s.approvals.byID[id]
	if ap.State != types.ApprovalPending || ap.Kind != types.ApprovalCredentialReauth {
		return types.ApprovalRequest{}, errStoreNotFound
	}
	ap.State, ap.DecidedBy = d.State, d.DecidedBy
	s.approvals.byID[id] = ap
	return ap, nil
}

type adoCapFixture struct {
	*adoResolveFixture
	approvals *fakeApprovals
	jti       atomic.Int64
}

// newADOCapFixture is a dispatched run on the read+code_write profile, under a
// ceiling of read+code_write+pr — so `pr` is the escalation a person may grant
// and `repo_admin` the one nobody may.
func newADOCapFixture(t *testing.T) *adoCapFixture {
	t.Helper()
	rf := newADOResolveFixture(t)
	fa := newFakeApprovals()
	rf.srv.cfg.Approvals = fa
	rf.srv.cfg.Store = &adoCapStore{adoTestStore: rf.st, approvals: fa}
	return &adoCapFixture{adoResolveFixture: rf, approvals: fa}
}

// ask is one capability resolve as the proxy makes it.
func (f *adoCapFixture) ask(t *testing.T, c adoscope.Capability, mode types.FirstUseMode, approval uuid.UUID, path string) *httptest.ResponseRecorder {
	t.Helper()
	q := url.Values{"capability": {string(c)}, "first_use": {string(mode)}, "method": {"POST"}, "path": {path}, "repo": {testRepoOf(path)}}
	if approval != uuid.Nil {
		q.Set("approval", approval.String())
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/internal/injection/"+f.grantID.String()+"?"+q.Encode(), nil)
	f.srv.resolveADOInjection(w, r,
		&identity.Claims{RunID: f.runID, Sub: f.subject, SPIFFEID: "spiffe://wardyn.local/run"},
		broker.Minted{JTI: "jti-" + strconv.FormatInt(f.jti.Add(1), 10),
			Injection: &egress.InjectionRule{Host: "dev.azure.com", SecretName: types.ADOEntraAccessTokenSecret}},
		f.grantID)
	return w
}

// pendingID decodes a 423 and returns its approval id.
func pendingID(t *testing.T, w *httptest.ResponseRecorder, state string) uuid.UUID {
	t.Helper()
	if w.Code != http.StatusLocked {
		t.Fatalf("status %d body %s, want 423", w.Code, w.Body.String())
	}
	var body reauthPendingResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.State != state {
		t.Fatalf("423 body %s, want state %s", w.Body.String(), state)
	}
	return body.ApprovalID
}

func (f *adoCapFixture) decide(t *testing.T, id uuid.UUID, state types.ApprovalState, scope types.ApprovalScope) {
	t.Helper()
	if _, err := f.approvals.Decide(context.Background(), id, types.ActorHuman, types.ApprovalDecision{State: state, Scope: scope}); err != nil {
		t.Fatal(err)
	}
}

func (f *adoCapFixture) row(id uuid.UUID) types.ApprovalRequest {
	ap, _ := f.approvals.Get(context.Background(), id)
	return ap
}

func granted(t *testing.T, w *httptest.ResponseRecorder) []string {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s, want 200", w.Code, w.Body.String())
	}
	var resp types.ResolvedInjection
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	return resp.Capabilities
}

const prPath = "/contoso/proj/_apis/git/repositories/app/pullrequests"

// The row is kind tool_call, raised by the control plane with the grant in
// grant_id, and its scope is CANONICAL: two asks differing only in the raw
// path are one pending request.
func TestADOCapability_RaisesOneCanonicalToolCallRow(t *testing.T) {
	f := newADOCapFixture(t)
	a := pendingID(t, f.ask(t, adoscope.CapPR, types.FirstUseWaitForReview, uuid.Nil, prPath), adoCapabilityPendingState)
	b := pendingID(t, f.ask(t, adoscope.CapPR, types.FirstUseWaitForReview, uuid.Nil, prPath+"?x=1&y=/other"), adoCapabilityPendingState)
	if a != b || len(f.approvals.requested) != 1 {
		t.Fatalf("ids %s/%s, %d rows: two asks differing only in path must dedupe to one", a, b, len(f.approvals.requested))
	}
	ap := f.row(a)
	if ap.Kind != types.ApprovalToolCall || ap.GrantID == nil || *ap.GrantID != f.grantID {
		t.Fatalf("row = %+v, want kind tool_call carrying the grant", ap)
	}
	var sc map[string]any
	_ = json.Unmarshal(ap.RequestedScope, &sc)
	for _, k := range []string{"lane", "provider_id", "org", "grant_id", "capability", "repo", "ref_class", "tool", "cmd"} {
		if _, ok := sc[k]; !ok {
			t.Errorf("scope %s lacks %q", ap.RequestedScope, k)
		}
	}
	if len(sc) != 9 || strings.Contains(string(ap.RequestedScope), "pullrequests") || sc["repo"] != "app" || sc["lane"] != "azure_devops" {
		t.Errorf("scope %s is not canonical (raw path leaked, or extra keys)", ap.RequestedScope)
	}
	if rows := f.audit.find("credential.capability.request"); len(rows) != 1 {
		t.Errorf("credential.capability.request rows = %d, want 1 (the dedup is silent)", len(rows))
	}
}

// Approved ONCE: one re-resolve goes through and spends it; the next names the
// same approval and raises a NEW request instead.
func TestADOCapability_OnceIsSpentByOneResolve(t *testing.T) {
	f := newADOCapFixture(t)
	a := pendingID(t, f.ask(t, adoscope.CapPR, types.FirstUseWaitForReview, uuid.Nil, prPath), adoCapabilityPendingState)
	if got := pendingID(t, f.ask(t, adoscope.CapPR, types.FirstUseWaitForReview, a, prPath), adoCapabilityPendingState); got != a {
		t.Fatalf("a re-resolve on a pending request answered %s, want the same %s", got, a)
	}
	f.decide(t, a, types.ApprovalApproved, types.ScopeOnce)

	caps := granted(t, f.ask(t, adoscope.CapPR, types.FirstUseWaitForReview, a, prPath))
	if strings.Contains(strings.Join(caps, ","), "pr") {
		t.Errorf("a once approval came back in the standing set %v; it would widen the run", caps)
	}
	if f.row(a).MintedJTI == "" {
		t.Fatal("the once approval was not spent")
	}
	b := pendingID(t, f.ask(t, adoscope.CapPR, types.FirstUseWaitForReview, a, prPath), adoCapabilityPendingState)
	if b == a {
		t.Fatal("a spent once approval let a second request through its own id")
	}
}

// A retry after the hold ran out finds the approval it was waiting for (now
// approved once), rather than raising a second one the person must answer.
func TestADOCapability_RetryAfterTheHoldFindsItsApproval(t *testing.T) {
	f := newADOCapFixture(t)
	a := pendingID(t, f.ask(t, adoscope.CapPR, types.FirstUseWaitForReview, uuid.Nil, prPath), adoCapabilityPendingState)
	f.decide(t, a, types.ApprovalApproved, types.ScopeOnce)
	if got := pendingID(t, f.ask(t, adoscope.CapPR, types.FirstUseWaitForReview, uuid.Nil, prPath), adoCapabilityPendingState); got != a {
		t.Fatalf("the retry was pointed at %s, want the approved %s", got, a)
	}
	granted(t, f.ask(t, adoscope.CapPR, types.FirstUseWaitForReview, a, prPath))
	if len(f.approvals.requested) != 1 {
		t.Errorf("rows = %d, want the one", len(f.approvals.requested))
	}
}

// Exactly-once under concurrency: many re-resolves naming one approved once
// row, one 200.
func TestADOCapability_OnceUnderConcurrency(t *testing.T) {
	f := newADOCapFixture(t)
	a := pendingID(t, f.ask(t, adoscope.CapPR, types.FirstUseWaitForReview, uuid.Nil, prPath), adoCapabilityPendingState)
	f.decide(t, a, types.ApprovalApproved, types.ScopeOnce)
	// Warm the token cache so every resolve races on the spend, not the redeem.
	granted(t, f.ask(t, adoscope.CapCodeWrite, types.FirstUseWaitForReview, uuid.Nil, prPath))

	const n = 16
	codes := make([]int, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			codes[i] = f.ask(t, adoscope.CapPR, types.FirstUseWaitForReview, a, prPath).Code
		}(i)
	}
	close(start)
	wg.Wait()
	ok := 0
	for _, c := range codes {
		if c == http.StatusOK {
			ok++
		}
	}
	if ok != 1 {
		t.Fatalf("codes = %v: %d resolves spent one once approval, want exactly 1", codes, ok)
	}
}

// Approved FOR THIS RUN: the capability joins the standing set, and a later
// first ask is granted without any approval named.
func TestADOCapability_ForThisRunWidensTheRun(t *testing.T) {
	f := newADOCapFixture(t)
	a := pendingID(t, f.ask(t, adoscope.CapPR, types.FirstUseWaitForReview, uuid.Nil, prPath), adoCapabilityPendingState)
	f.decide(t, a, types.ApprovalApproved, types.ScopeRun)
	if caps := granted(t, f.ask(t, adoscope.CapPR, types.FirstUseWaitForReview, a, prPath)); !strings.Contains(strings.Join(caps, ","), "pr") {
		t.Fatalf("standing set %v lacks the run-approved capability", caps)
	}
	granted(t, f.ask(t, adoscope.CapPR, types.FirstUseWaitForReview, uuid.Nil, "/contoso/other"))
	if len(f.approvals.requested) != 1 {
		t.Errorf("rows = %d, want the one approval only", len(f.approvals.requested))
	}
	// An administrator narrowing the ceiling takes it back.
	f.st.site.WorkspaceProviders.Git[0].Entra.CapabilityCeiling = []adoscope.Capability{adoscope.CapRead, adoscope.CapCodeWrite}
	if w := f.ask(t, adoscope.CapPR, types.FirstUseWaitForReview, uuid.Nil, prPath); w.Code != http.StatusForbidden {
		t.Errorf("after narrowing: status %d, want 403", w.Code)
	}
}

// Denied: the re-resolve is a 403 the proxy relays.
func TestADOCapability_DeniedIsRefused(t *testing.T) {
	f := newADOCapFixture(t)
	a := pendingID(t, f.ask(t, adoscope.CapPR, types.FirstUseWaitForReview, uuid.Nil, prPath), adoCapabilityPendingState)
	f.decide(t, a, types.ApprovalDenied, "")
	w := f.ask(t, adoscope.CapPR, types.FirstUseWaitForReview, a, prPath)
	if w.Code != http.StatusForbidden || f.failureReasonOf(t)["reason"] != "capability_denied" {
		t.Fatalf("status %d body %s, want 403 capability_denied", w.Code, w.Body.String())
	}
}

// Above the ceiling, always_deny: refused outright, nothing raised. And
// deny_with_review raises and refuses THIS attempt, naming the request.
func TestADOCapability_RefusalsRaiseNothingExceptUnderReview(t *testing.T) {
	for _, tc := range []struct {
		name   string
		c      adoscope.Capability
		mode   types.FirstUseMode
		reason string
		raised int
	}{
		{"above the ceiling", adoscope.CapRepoAdmin, types.FirstUseWaitForReview, "capability_above_ceiling", 0},
		{"always_deny", adoscope.CapPR, types.FirstUseAlwaysDeny, "capability_always_deny", 0},
		{"no mode is always_deny", adoscope.CapPR, "", "capability_always_deny", 0},
		{"not grantable", adoscope.CapDeniedTokens, types.FirstUseWaitForReview, "capability_not_grantable", 0},
		{"deny_with_review", adoscope.CapPR, types.FirstUseDenyWithReview, "capability_review", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newADOCapFixture(t)
			w := f.ask(t, tc.c, tc.mode, uuid.Nil, prPath)
			if w.Code != http.StatusForbidden || f.failureReasonOf(t)["reason"] != tc.reason {
				t.Fatalf("status %d body %s, want 403 %s", w.Code, w.Body.String(), tc.reason)
			}
			if len(f.approvals.requested) != tc.raised {
				t.Errorf("rows raised = %d, want %d", len(f.approvals.requested), tc.raised)
			}
			if tc.raised == 1 && !strings.Contains(w.Body.String(), f.approvals.requested[0].ID.String()) {
				t.Errorf("deny_with_review body %s does not name the request", w.Body.String())
			}
		})
	}
}

// The server-side per-run cap: counted over this lane's rows in any state,
// independent of the proxy's own.
func TestADOCapability_ServerCapRefusesTheNext(t *testing.T) {
	f := newADOCapFixture(t)
	for i := 0; i < maxADOCapabilityHoldsPerRun; i++ {
		id := pendingID(t, f.ask(t, adoscope.CapPR, types.FirstUseWaitForReview, uuid.Nil, "/contoso/_apis/git/repositories/r"+strconv.Itoa(i)+"/pullrequests"), adoCapabilityPendingState)
		f.decide(t, id, types.ApprovalDenied, "")
	}
	w := f.ask(t, adoscope.CapPR, types.FirstUseWaitForReview, uuid.Nil, prPath)
	if w.Code != http.StatusForbidden || f.failureReasonOf(t)["reason"] != "capability_holds_exhausted" {
		t.Fatalf("status %d body %s, want 403 capability_holds_exhausted", w.Code, w.Body.String())
	}
	if len(f.approvals.requested) != maxADOCapabilityHoldsPerRun {
		t.Errorf("rows = %d, want the cap", len(f.approvals.requested))
	}
}

// CONSENT: an approved capability the person has not consented to (Entra
// refuses the whole redemption, AADSTS65001) is a consent request the hold
// chains onto; a fresh sign-in resolves it and the SAME approval then spends.
func TestADOCapability_ConsentChainsAndTheSignInResolvesIt(t *testing.T) {
	f := newADOCapFixture(t)
	a := pendingID(t, f.ask(t, adoscope.CapPR, types.FirstUseWaitForReview, uuid.Nil, prPath), adoCapabilityPendingState)
	f.decide(t, a, types.ApprovalApproved, types.ScopeOnce)

	f.fake.SetConsentRequired(true)
	b := pendingID(t, f.ask(t, adoscope.CapPR, types.FirstUseWaitForReview, a, prPath), reauthPendingState)
	consent := f.row(b)
	if consent.Kind != types.ApprovalCredentialReauth || !strings.Contains(string(consent.RequestedScope), "vso.code_write") {
		t.Fatalf("consent row = %+v, want a credential_reauth naming the needed scope", consent)
	}
	if f.row(a).MintedJTI != "" {
		t.Fatal("the once approval was spent on a resolve that granted nothing")
	}
	// Not yet signed in again: reading it changes nothing.
	if got := f.srv.reconcileADOReauthOnRead(context.Background(), consent); got.State != types.ApprovalPending {
		t.Fatalf("resolved before any sign-in: %s", got.State)
	}

	f.fake.SetConsentRequired(false)
	later := time.Now().Add(time.Minute)
	f.srv.cfg.Now = func() time.Time { return later }
	if w := f.capture(t, f.subject); w.Code != http.StatusFound {
		t.Fatalf("re-sign-in: %d %s", w.Code, w.Body.String())
	}
	if got := f.srv.reconcileADOReauthOnRead(context.Background(), f.row(b)); got.State != types.ApprovalApproved {
		t.Fatalf("after the sign-in the consent request is %s, want APPROVED", got.State)
	}
	granted(t, f.ask(t, adoscope.CapPR, types.FirstUseWaitForReview, a, prPath))
	if f.row(a).MintedJTI == "" {
		t.Error("the once approval was not spent by the resolve that went through")
	}
}

// failureReasonOf is the reason on the LAST secret.read failure row.
func (f *adoCapFixture) failureReasonOf(t *testing.T) map[string]any {
	t.Helper()
	rows := f.audit.find("secret.read")
	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i].Outcome == "failure" {
			var d map[string]any
			_ = json.Unmarshal(rows[i].Data, &d)
			return d
		}
	}
	return map[string]any{}
}

// ─── the decide matrix ──────────────────────────────────────────────────────

// seedADO puts an escalation row on the fixture's run. sandboxShaped drops the
// grant id, which is what the sandbox's own route produces whatever its scope
// says.
func seedADO(t *testing.T, f *scopeFixture, c adoscope.Capability, sandboxShaped bool) uuid.UUID {
	t.Helper()
	grant := uuid.New()
	scope, _ := json.Marshal(adoCapabilityScope{Lane: adoApprovalLane, ProviderID: "ado-row-1", Org: "contoso",
		GrantID: grant, Capability: string(c), Tool: "Azure DevOps", Cmd: "x"})
	id := uuid.New()
	ap := types.ApprovalRequest{ID: id, RunID: f.runID, Kind: types.ApprovalToolCall, RequestedScope: scope,
		State: types.ApprovalPending, RequestedAt: time.Now().UTC()}
	if !sandboxShaped {
		ap.GrantID = &grant
	}
	f.approval.mu.Lock()
	f.approval.byID[id] = ap
	f.approval.mu.Unlock()
	return id
}

func newADODecideFixture(t *testing.T) *scopeFixture {
	t.Helper()
	f := newScopeFixture(t)
	row := adoEntraTestRow()
	f.store.mu.Lock()
	f.store.siteCfg = adoSite(row)
	f.store.mu.Unlock()
	return f
}

// Owner and administrator decide; an unrelated member gets the same 404 as a
// row that does not exist — and a sandbox-raised tool_call that SMUGGLES the
// lane is still not the owner's to decide.
func TestADOCapability_DecideMatrix(t *testing.T) {
	owner := func(t *testing.T, f *scopeFixture) *http.Cookie {
		return ssoSession(t, f.memberID, "owner@corp.example", oidc.RoleMember)
	}
	stranger := func(t *testing.T, _ *scopeFixture) *http.Cookie {
		return ssoSession(t, "sub-stranger", "stranger@corp.example", oidc.RoleMember)
	}
	admin := func(t *testing.T, _ *scopeFixture) *http.Cookie {
		return ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)
	}
	for _, tc := range []struct {
		name      string
		who       func(*testing.T, *scopeFixture) *http.Cookie
		c         adoscope.Capability
		sandbox   bool
		scope     types.ApprovalScope
		want      int
		wantScope types.ApprovalScope
	}{
		{"owner once", owner, adoscope.CapPR, false, types.ScopeOnce, http.StatusOK, types.ScopeOnce},
		{"owner bodyless is once", owner, adoscope.CapPR, false, "", http.StatusOK, types.ScopeOnce},
		{"admin for the run", admin, adoscope.CapPR, false, types.ScopeRun, http.StatusOK, types.ScopeRun},
		{"unrelated member", stranger, adoscope.CapPR, false, types.ScopeOnce, http.StatusNotFound, ""},
		{"owner, sandbox row smuggling the lane", owner, adoscope.CapPR, true, types.ScopeOnce, http.StatusNotFound, ""},
		{"until refused", owner, adoscope.CapPR, false, types.ScopeUntil, http.StatusBadRequest, ""},
		{"always refused", admin, adoscope.CapPR, false, types.ScopeAlways, http.StatusBadRequest, ""},
		{"above the ceiling", admin, adoscope.CapRepoAdmin, false, types.ScopeOnce, http.StatusForbidden, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newADODecideFixture(t)
			id := seedADO(t, f, tc.c, tc.sandbox)
			body := ""
			if tc.scope != "" {
				var exp *time.Time
				if tc.scope == types.ScopeUntil {
					e := time.Now().Add(time.Hour)
					exp = &e
				}
				body = decideBody(t, tc.scope, exp)
			}
			w := doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+"/approve", tc.who(t, f), body)
			if w.Code != tc.want {
				t.Fatalf("status %d body %s, want %d", w.Code, w.Body.String(), tc.want)
			}
			f.approval.mu.Lock()
			got := f.approval.byID[id]
			f.approval.mu.Unlock()
			if tc.want == http.StatusOK && (got.State != types.ApprovalApproved || got.DecisionScope != tc.wantScope) {
				t.Errorf("row = %s/%s, want APPROVED/%s", got.State, got.DecisionScope, tc.wantScope)
			}
			if tc.want != http.StatusOK && got.State != types.ApprovalPending {
				t.Errorf("a refused decision moved the row to %s", got.State)
			}
		})
	}
}

// The sandbox route refuses a `lane` key in any letter case and raises nothing.
func TestInternalApprovalRequest_RefusesALaneKey(t *testing.T) {
	for _, key := range []string{"lane", "LANE", "Lane"} {
		h := newHarness(t)
		tok := h.mintRunToken(t, uuid.New())
		w := do(t, h.srv, http.MethodPost, "/api/v1/internal/approvals", tok,
			`{"kind":"tool_call","requested_scope":{"`+key+`":"azure_devops","tool":"az","cmd":"x"}}`)
		if w.Code != http.StatusBadRequest || len(h.approvals.requested) != 0 {
			t.Errorf("%s: status %d, %d rows; want 400 and nothing raised", key, w.Code, len(h.approvals.requested))
		}
		if ev := lastAuditEvent(t, h.audit.events, "auth.fail"); !strings.Contains(string(ev.Data), "reserved_scope_key") {
			t.Errorf("%s: auth.fail row %s does not name the reason", key, ev.Data)
		}
	}
}

// F4: an approval is spent only by the request it was raised for. A `once`
// approved for pr, named on an ask for work_write, is refused and stays
// unspent.
func TestADOCapability_ANamedApprovalMustMatchTheCapability(t *testing.T) {
	f := newADOCapFixture(t)
	row := &f.st.site.WorkspaceProviders.Git[0]
	row.Entra.CapabilityCeiling = append(row.Entra.CapabilityCeiling, adoscope.CapWorkWrite)
	a := pendingID(t, f.ask(t, adoscope.CapPR, types.FirstUseWaitForReview, uuid.Nil, prPath), adoCapabilityPendingState)
	f.decide(t, a, types.ApprovalApproved, types.ScopeOnce)

	w := f.ask(t, adoscope.CapWorkWrite, types.FirstUseWaitForReview, a, "/contoso/proj/_apis/wit/workitems/1")
	if w.Code != http.StatusForbidden || f.failureReasonOf(t)["reason"] != "approval_mismatch" {
		t.Fatalf("status %d body %s, want 403 approval_mismatch", w.Code, w.Body.String())
	}
	if f.row(a).MintedJTI != "" {
		t.Fatal("the pr approval was spent by a work_write request")
	}
}

// F5: a deny sticks for the run — the same canonical request is refused naming
// the decision, and nothing new is raised.
func TestADOCapability_ADenySticksForTheRun(t *testing.T) {
	f := newADOCapFixture(t)
	a := pendingID(t, f.ask(t, adoscope.CapPR, types.FirstUseWaitForReview, uuid.Nil, prPath), adoCapabilityPendingState)
	f.decide(t, a, types.ApprovalDenied, "")
	for i := 0; i < 3; i++ {
		w := f.ask(t, adoscope.CapPR, types.FirstUseWaitForReview, uuid.Nil, prPath+"?try="+strconv.Itoa(i))
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), a.String()) ||
			f.failureReasonOf(t)["reason"] != "capability_denied" {
			t.Fatalf("attempt %d: status %d body %s, want 403 naming %s", i, w.Code, w.Body.String(), a)
		}
	}
	if len(f.approvals.requested) != 1 {
		t.Errorf("rows = %d, want the denied one only", len(f.approvals.requested))
	}
	// Another repository is another question.
	pendingID(t, f.ask(t, adoscope.CapPR, types.FirstUseWaitForReview, uuid.Nil, "/contoso/_apis/git/repositories/other/pullrequests"), adoCapabilityPendingState)
}

// F8: a consent Entra refuses is not re-asked on every request: inside the
// negative-cache window, two refused requests cost one redemption.
func TestADOCapability_ConsentRefusalIsCachedBriefly(t *testing.T) {
	f := newADOCapFixture(t)
	var redeems atomic.Int32
	target, _ := url.Parse(f.fake.URL())
	rp := httputil.NewSingleHostReverseProxy(target)
	counter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/token") {
			redeems.Add(1)
		}
		rp.ServeHTTP(w, r)
	}))
	t.Cleanup(counter.Close)
	f.cfg.AuthorityOverride = counter.URL

	a := pendingID(t, f.ask(t, adoscope.CapPR, types.FirstUseWaitForReview, uuid.Nil, prPath), adoCapabilityPendingState)
	f.decide(t, a, types.ApprovalApproved, types.ScopeOnce)
	f.fake.SetConsentRequired(true)
	b := pendingID(t, f.ask(t, adoscope.CapPR, types.FirstUseWaitForReview, a, prPath), reauthPendingState)
	if got := pendingID(t, f.ask(t, adoscope.CapPR, types.FirstUseWaitForReview, a, prPath), reauthPendingState); got != b {
		t.Fatalf("second refusal named %s, want the same consent request %s", got, b)
	}
	if n := redeems.Load(); n != 1 {
		t.Fatalf("redemptions = %d, want 1 inside the window", n)
	}
	// Past the window the authority is asked again.
	later := adoTestNow.Add(adoConsentRefusalTTL + time.Second)
	f.srv.cfg.Now = func() time.Time { return later }
	pendingID(t, f.ask(t, adoscope.CapPR, types.FirstUseWaitForReview, a, prPath), reauthPendingState)
	if n := redeems.Load(); n != 2 {
		t.Errorf("redemptions after the window = %d, want 2", n)
	}
}

// testRepoOf is the repository the proxy reports for path (proxy.adoRepoOf).
func testRepoOf(path string) string {
	_, rest, ok := strings.Cut(path, "/_apis/git/repositories/")
	if !ok {
		return ""
	}
	repo, _, _ := strings.Cut(rest, "/")
	return repo
}
