// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// GET /approvals?decidable_by=me: the PENDING approvals the caller may decide now, on the fixture of
// deciders_resolve_test.go. The list is the resolver's verdict (mayDecide) over every candidate, so these pin
// who sees what, and that nobody learns of a hold they may not decide.

// ListPendingApprovalsOnUnits is the unit-scoped read of a store that serves decidable_by=me: every PENDING
// approval on a run launched in one of units.
func (a *authzApprovals) ListPendingApprovalsOnUnits(ctx context.Context, units []uuid.UUID) ([]types.ApprovalRequest, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := []types.ApprovalRequest{}
	for _, ap := range a.byID {
		if ap.State != types.ApprovalPending {
			continue
		}
		run, err := a.store.GetRun(ctx, ap.RunID)
		if err != nil || run.UnitID == nil || !slices.Contains(units, *run.UnitID) {
			continue
		}
		out = append(out, ap)
	}
	return out, nil
}

// dcAllKinds is every hold a fixture seeds, plus a plain tool_call that no rule names.
var dcAllKinds = append(slices.Clone(dcRuleKinds), "plain")

// seedAll seeds one PENDING hold of every kind and returns them by kind.
func (f *dcFix) seedAll() map[string]types.ApprovalRequest {
	f.t.Helper()
	out := map[string]types.ApprovalRequest{}
	for _, k := range dcAllKinds {
		out[k] = f.hold(k)
	}
	return out
}

// dcInboxRules is one rule per kind, on the root unit, naming a different decider mix each.
func dcInboxRules(f *dcFix) {
	f.st.setRules(f.root.ID,
		dcRule(types.DeciderKindEgressDomain, types.DeciderUnitAdmins),
		dcRule(types.DeciderKindMCPCall, types.DeciderGroupPrefix+dcGroup),
		dcRule(types.DeciderKindCredential, types.DeciderUnitAdmins, types.DeciderGroupPrefix+dcGroup),
		dcRule(types.DeciderKindPushContent),
		dcRule(types.DeciderKindADOEscalation, types.DeciderOwner),
	)
}

// inbox is GET /approvals<query> as c: the status, the rows and the X-Wardyn-Truncated header.
func (f *dcFix) inbox(c *http.Cookie, query string) (*httptest.ResponseRecorder, []map[string]any) {
	f.t.Helper()
	w := doSSO(f.t, f.srv, http.MethodGet, "/api/v1/approvals"+query, c, "")
	var rows []map[string]any
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
			f.t.Fatalf("list body %s: %v", w.Body, err)
		}
	}
	return w, rows
}

// inboxIDs is the sorted ids of the rows decidable_by=me answers c.
func (f *dcFix) inboxIDs(c *http.Cookie) []string {
	f.t.Helper()
	w, rows := f.inbox(c, "?decidable_by=me")
	if w.Code != http.StatusOK {
		f.t.Fatalf("decidable_by=me: %d %s", w.Code, w.Body)
	}
	return rowIDs(rows)
}

func rowIDs(rows []map[string]any) []string {
	ids := []string{}
	for _, r := range rows {
		ids = append(ids, r["id"].(string))
	}
	sort.Strings(ids)
	return ids
}

// holdIDsOf is the sorted ids of the holds of kinds.
func holdIDsOf(all map[string]types.ApprovalRequest, kinds ...string) []string {
	ids := []string{}
	for _, k := range kinds {
		ids = append(ids, all[k].ID.String())
	}
	sort.Strings(ids)
	return ids
}

func wantIDs(t *testing.T, who string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s: decidable_by=me = %v, want %v", who, got, want)
	}
}

// TestDecidableByMeOnlyDecidable: each caller is told of exactly the holds they decide now. A unit admin, a
// group decider, the run's owner (where a rule names owner) and a security admin each see theirs; a member
// with no entry sees nothing, however many holds the unit has.
func TestDecidableByMeOnlyDecidable(t *testing.T) {
	f := newDCFix(t)
	dcInboxRules(f)
	all := f.seedAll()
	wantIDs(t, "unit admin", f.inboxIDs(dcMember(t, dcAdmin)), holdIDsOf(all, types.DeciderKindEgressDomain, types.DeciderKindCredential))
	wantIDs(t, "group decider", f.inboxIDs(dcMember(t, dcLead, dcGroup)), holdIDsOf(all, types.DeciderKindMCPCall, types.DeciderKindCredential))
	wantIDs(t, "owner a rule names", f.inboxIDs(dcMember(t, dcOwner)), holdIDsOf(all, types.DeciderKindADOEscalation))
	wantIDs(t, "security admin", f.inboxIDs(dcSecurity(t, dcSec)), holdIDsOf(all, dcAllKinds...))
	wantIDs(t, "member with no entry", f.inboxIDs(dcMember(t, dcOther)), []string{})
	wantIDs(t, "group member of another group", f.inboxIDs(dcMember(t, dcOther, "other-group")), []string{})
}

// TestDecidableByMeWithoutRulesIsToday: with no deciders rule anywhere the list is what decide answers
// without them: the owner's egress, Azure DevOps and MCP holds, never their credential or push; a unit admin
// nothing; security every hold.
func TestDecidableByMeWithoutRulesIsToday(t *testing.T) {
	f := newDCFix(t)
	all := f.seedAll()
	wantIDs(t, "owner", f.inboxIDs(dcMember(t, dcOwner)),
		holdIDsOf(all, types.DeciderKindEgressDomain, types.DeciderKindADOEscalation, types.DeciderKindMCPCall))
	wantIDs(t, "unit admin", f.inboxIDs(dcMember(t, dcAdmin)), []string{})
	wantIDs(t, "security admin", f.inboxIDs(dcSecurity(t, dcSec)), holdIDsOf(all, dcAllKinds...))
}

// TestDecidableByMeNeverListsTheAuthorsOwnRun: a run's author in a decider group, or a unit admin, never
// sees their own run's hold as decidable under a rule that does not name owner (the resolver's author rule),
// and their credential hold is not theirs without a rule either. Another group member sees it.
func TestDecidableByMeNeverListsTheAuthorsOwnRun(t *testing.T) {
	f := newDCFix(t, createdBy(dcLead))
	f.st.setRules(f.root.ID,
		dcRule(types.DeciderKindEgressDomain, types.DeciderGroupPrefix+dcGroup),
		dcRule(types.DeciderKindMCPCall, types.DeciderUnitAdmins))
	egress, mcp := f.hold(types.DeciderKindEgressDomain), f.hold(types.DeciderKindMCPCall)
	f.hold(types.DeciderKindCredential)
	wantIDs(t, "author in the group", f.inboxIDs(dcMember(t, dcLead, dcGroup)), []string{})
	wantIDs(t, "another group member", f.inboxIDs(dcMember(t, dcLead2, dcGroup)), []string{egress.ID.String()})
	wantIDs(t, "unit admin", f.inboxIDs(dcMember(t, dcAdmin)), []string{mcp.ID.String()})

	g := newDCFix(t, createdBy(dcAdmin))
	g.st.setRules(g.root.ID, dcRule(types.DeciderKindMCPCall, types.DeciderUnitAdmins))
	own := g.hold(types.DeciderKindMCPCall)
	wantIDs(t, "author who is a unit admin", g.inboxIDs(dcMember(t, dcAdmin)), []string{})
	wantIDs(t, "another unit admin", g.inboxIDs(dcMember(t, dcAdmin2)), []string{own.ID.String()})
}

// TestApprovalsDecidableByMe_MatchesMayDecide: for every caller and every hold, the list is exactly the holds
// mayDecide says yes to over the same set, whatever the run's author and the rules are.
func TestApprovalsDecidableByMe_MatchesMayDecide(t *testing.T) {
	type caller struct {
		name   string
		cookie func(*testing.T) *http.Cookie
		ctx    context.Context
	}
	callers := []caller{
		{"owner", func(t *testing.T) *http.Cookie { return dcMember(t, dcOwner) }, dcCtx(dcOwner, oidc.RoleUser, nil, false)},
		{"root unit admin", func(t *testing.T) *http.Cookie { return dcMember(t, dcAdmin) }, dcCtx(dcAdmin, oidc.RoleUser, nil, false)},
		{"team unit admin", func(t *testing.T) *http.Cookie { return dcMember(t, dcAdmin2) }, dcCtx(dcAdmin2, oidc.RoleUser, nil, false)},
		{"group", func(t *testing.T) *http.Cookie { return dcMember(t, dcLead, dcGroup) }, dcCtx(dcLead, oidc.RoleUser, []string{dcGroup}, false)},
		{"second group member", func(t *testing.T) *http.Cookie { return dcMember(t, dcLead2, dcGroup) }, dcCtx(dcLead2, oidc.RoleUser, []string{dcGroup}, false)},
		{"stale group", func(t *testing.T) *http.Cookie { return uaSession(t, dcLead, []string{dcGroup}, true) }, dcCtx(dcLead, oidc.RoleUser, []string{dcGroup}, true)},
		{"nobody", func(t *testing.T) *http.Cookie { return dcMember(t, dcOther) }, dcCtx(dcOther, oidc.RoleUser, nil, false)},
		{"security", func(t *testing.T) *http.Cookie { return dcSecurity(t, dcSec) }, dcCtx(dcSec, oidc.RoleSecurityAdmin, nil, false)},
	}
	for _, tc := range []struct {
		name  string
		run   func(*dcFix)
		rules func(*dcFix)
	}{
		{"no rules", createdBy(dcOwner), func(*dcFix) {}},
		{"mixed rules", createdBy(dcOwner), dcInboxRules},
		{"author in the group", createdBy(dcLead), dcInboxRules},
		{"owner left out", createdBy(dcOwner), func(f *dcFix) {
			f.st.setRules(f.team.ID, dcRule(types.DeciderKindEgressDomain, types.DeciderGroupPrefix+dcGroup),
				dcRule(types.DeciderKindMCPCall), dcRule(types.DeciderKindADOEscalation, types.DeciderUnitAdmins))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDCFix(t, tc.run)
			tc.rules(f)
			all := f.seedAll()
			for _, c := range callers {
				want := []string{}
				for _, ap := range all {
					if f.mayDecideAs(c.ctx, ap) {
						want = append(want, ap.ID.String())
					}
				}
				sort.Strings(want)
				wantIDs(t, c.name, f.inboxIDs(c.cookie(t)), want)
			}
		})
	}
}

// TestDecidableByMeRowsNameTheRequesterToADecider: a row served to someone other than the run's creator
// carries requested_by (the run's creator) and the deciders; the creator's own row carries no requested_by.
func TestDecidableByMeRowsNameTheRequesterToADecider(t *testing.T) {
	f := newDCFix(t)
	dcInboxRules(f)
	all := f.seedAll()
	_, rows := f.inbox(dcMember(t, dcAdmin), "?decidable_by=me")
	if len(rows) != 2 {
		t.Fatalf("unit admin rows = %d, want 2", len(rows))
	}
	for _, r := range rows {
		if r["requested_by"] != dcOwner {
			t.Errorf("row %v requested_by = %v, want %s", r["id"], r["requested_by"], dcOwner)
		}
		if r["id"] == all[types.DeciderKindEgressDomain].ID.String() {
			if d, _ := json.Marshal(r["deciders"]); string(d) != `["unit_admins","security"]` {
				t.Errorf("egress deciders = %s", d)
			}
		}
	}
	_, own := f.inbox(dcMember(t, dcOwner), "?decidable_by=me")
	if len(own) != 1 {
		t.Fatalf("owner rows = %d, want 1", len(own))
	}
	if _, has := own[0]["requested_by"]; has {
		t.Errorf("the creator's own row carries requested_by %v", own[0]["requested_by"])
	}
}

// TestDecidersNonDecider404Identical: a hold the caller may not decide and a hold that does not exist answer
// alike: the same empty list for decidable_by=me&run_id=, and the same 404 for the push path list. A decider
// reads the path list of the hold they decide.
func TestDecidersNonDecider404Identical(t *testing.T) {
	f := newDCFix(t)
	f.st.setRules(f.root.ID, dcRule(types.DeciderKindPushContent, types.DeciderUnitAdmins))
	ap := f.hold(types.DeciderKindPushContent)
	other := dcMember(t, dcOther)

	hidden, rows := f.inbox(other, "?decidable_by=me&run_id="+f.run.ID.String())
	missing, _ := f.inbox(other, "?decidable_by=me&run_id="+uuid.NewString())
	if hidden.Code != http.StatusOK || len(rows) != 0 || hidden.Body.String() != missing.Body.String() || missing.Code != http.StatusOK {
		t.Errorf("hidden run = %d %s, missing run = %d %s; want the same empty 200", hidden.Code, hidden.Body, missing.Code, missing.Body)
	}

	paths := func(c *http.Cookie, id uuid.UUID) *httptest.ResponseRecorder {
		return doSSO(t, f.srv, http.MethodGet, "/api/v1/approvals/"+id.String()+"/paths", c, "")
	}
	hiddenPaths, missingPaths := paths(other, ap.ID), paths(other, uuid.New())
	if hiddenPaths.Code != http.StatusNotFound || hiddenPaths.Body.String() != missingPaths.Body.String() {
		t.Errorf("hidden path list = %d %s, missing = %d %s; want the same 404", hiddenPaths.Code, hiddenPaths.Body, missingPaths.Code, missingPaths.Body)
	}
	if w := paths(dcMember(t, dcAdmin), ap.ID); w.Code == http.StatusNotFound {
		t.Errorf("the unit admin who decides the push hold got %d %s on its path list", w.Code, w.Body)
	}
}

// TestDecidableByMeRefusesWhatItDoesNotList: only decidable_by=me exists, and it lists PENDING rows only.
func TestDecidableByMeRefusesWhatItDoesNotList(t *testing.T) {
	f := newDCFix(t)
	for query, reason := range map[string]string{
		"?decidable_by=you":               "invalid_decidable_by",
		"?decidable_by=":                  "invalid_decidable_by",
		"?decidable_by=me&state=APPROVED": "invalid_approval_state",
		"?decidable_by=me&state=nonsense": "invalid_approval_state",
	} {
		if w, _ := f.inbox(dcSecurity(t, dcSec), query); w.Code != http.StatusBadRequest || wireReason(t, w) != reason {
			t.Errorf("%s: %d %s, want 400 %s", query, w.Code, w.Body, reason)
		}
	}
	if w, _ := f.inbox(dcSecurity(t, dcSec), "?decidable_by=me&state=PENDING"); w.Code != http.StatusOK {
		t.Errorf("state=PENDING: %d %s", w.Code, w.Body)
	}
}

// TestDecidableByMeTruncatesAt500: the list holds at most 500 rows, newest first, and says so with
// X-Wardyn-Truncated when more were decidable; exactly 500 is not truncated.
func TestDecidableByMeTruncatesAt500(t *testing.T) {
	f := newDCFix(t)
	base := time.Now().UTC().Add(-time.Hour)
	var oldest uuid.UUID
	for i := 0; i < 501; i++ {
		ap := f.hold(types.DeciderKindEgressDomain)
		ap.RequestedAt = base.Add(time.Duration(i) * time.Second)
		f.aps.mu.Lock()
		f.aps.byID[ap.ID] = ap
		f.aps.mu.Unlock()
		if i == 0 {
			oldest = ap.ID
		}
	}
	sec := dcSecurity(t, dcSec)
	w, rows := f.inbox(sec, "?decidable_by=me")
	if w.Code != http.StatusOK || len(rows) != 500 || w.Header().Get("X-Wardyn-Truncated") != "true" {
		t.Fatalf("501 decidable: %d, %d rows, truncated=%q; want 200, 500 rows, true", w.Code, len(rows), w.Header().Get("X-Wardyn-Truncated"))
	}
	if slices.Contains(rowIDs(rows), oldest.String()) {
		t.Error("the oldest hold is in the list; the newest 500 should be")
	}
	f.aps.mu.Lock()
	delete(f.aps.byID, oldest)
	f.aps.mu.Unlock()
	w, rows = f.inbox(sec, "?decidable_by=me")
	if len(rows) != 500 || w.Header().Get("X-Wardyn-Truncated") != "" {
		t.Errorf("500 decidable: %d rows, truncated=%q; want 500 rows, not truncated", len(rows), w.Header().Get("X-Wardyn-Truncated"))
	}
}

// TestDelegatedTokenCannotDecideOrListDecidable: a portal's delegated token reaches neither the decision nor
// the decidable list, even for a person who decides: 403 delegation_scope.
func TestDelegatedTokenCannotDecideOrListDecidable(t *testing.T) {
	srv, ast, aap, _ := newAuthzMatrixServer(t)
	const person = "sub-portal-person"
	tok, _ := seedDelegation(t, ast.fakeDelegateStore, person)
	runID := uuid.New()
	ast.mu.Lock()
	ast.runs[runID] = types.AgentRun{ID: runID, CreatedBy: person, State: types.RunRunning, Agent: "claude-code"}
	ast.mu.Unlock()
	id := aap.seed(runID)
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/approvals?decidable_by=me", ""},
		{http.MethodPost, "/api/v1/approvals/" + id.String() + "/approve", `{}`},
	} {
		w := do(t, srv, c.method, c.path, tok, c.body)
		if w.Code != http.StatusForbidden || wireReason(t, w) != "delegation_scope" {
			t.Errorf("%s %s: %d %s, want 403 delegation_scope", c.method, c.path, w.Code, w.Body)
		}
	}
}

// TestDecidableByMeSkipsRequestsAnsweredByTakingOver: a human_takeover request is answered by taking over or
// declining, never decided (approve and deny answer 409), so it is not in the list even for a security admin.
func TestDecidableByMeSkipsRequestsAnsweredByTakingOver(t *testing.T) {
	f := newDCFix(t)
	ap := types.ApprovalRequest{ID: uuid.New(), RunID: f.run.ID, Kind: types.ApprovalHumanTakeover, State: types.ApprovalPending,
		RequestedScope: json.RawMessage(`{}`), RequestedAt: time.Now().UTC()}
	f.aps.mu.Lock()
	f.aps.byID[ap.ID] = ap
	f.aps.mu.Unlock()
	wantIDs(t, "security admin", f.inboxIDs(dcSecurity(t, dcSec)), []string{})
}
