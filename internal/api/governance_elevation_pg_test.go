// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// Elevation requests against Postgres (POST /governance/requests, kind elevation): who may ask, who decides,
// the membership with an end that an approval writes, the readers that ignore it once it has passed, the
// sweep, and a run's lifetime cap at the elevation's end. Guarded by WARDYN_TEST_PG.
//
// They run on the unit-admin tree of governance_unit_admin_pg_test.go (ua-a with ua-a1 and ua-a2 below it,
// ua-b beside it; adminA on ua-a, adminA1 on ua-a1). ua-a1 carries an elevation rule: unit admins and the
// el-deciders group decide, members of el-requesters may ask, for at most 4h.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	elevRequesters = "el-requesters"
	elevDeciders   = "el-deciders"
	elevReqSub     = "sub-el-req"
	elevDecider    = "sub-el-decider"
)

type elevEnv struct {
	*unitAdminEnv
	req, deciderGroup *http.Cookie
}

func newElevEnv(t *testing.T, mutate ...func(*Config)) *elevEnv {
	t.Helper()
	e := &elevEnv{unitAdminEnv: newUnitAdminEnv(t, false, mutate...)}
	e.req = uaSession(t, elevReqSub, []string{elevRequesters}, false)
	e.deciderGroup = uaSession(t, elevDecider, []string{elevDeciders}, false)
	e.setRules(e.a1, elevationRule(nil))
	return e
}

// elevationRule is ua-a1's rule, changed by f.
func elevationRule(f func(*types.DeciderRule)) types.DeciderRule {
	r := types.DeciderRule{Kinds: []string{types.DeciderKindElevation}, Who: []string{types.DeciderUnitAdmins, "group:" + elevDeciders},
		MaxElevation: "4h", EligibleRequesters: []string{"group:" + elevRequesters}}
	if f != nil {
		f(&r)
	}
	return r
}

// setRules stores u's rules as a write that passed validation would.
func (e *elevEnv) setRules(u types.GovernanceUnit, rules ...types.DeciderRule) {
	e.t.Helper()
	valid, err := types.ValidateDeciderRules(rules)
	if err != nil {
		e.t.Fatalf("rules for %s: %v", u.Name, err)
	}
	e.setRawRules(u, valid...)
}

// setRawRules stores u's rules as given, as a row written before this build may hold them.
func (e *elevEnv) setRawRules(u types.GovernanceUnit, rules ...types.DeciderRule) {
	e.t.Helper()
	if _, err := store.SetUnitDecidersQ(context.Background(), e.pool, u.ID, rules, "seed"); err != nil {
		e.t.Fatalf("store rules for %s: %v", u.Name, err)
	}
}

// requester is a fresh person in el-requesters.
func (e *elevEnv) requester() (*http.Cookie, string) {
	sub := e.fresh("sub-el-req")
	return uaSession(e.t, sub, []string{elevRequesters}, false), sub
}

func elevBody(unit uuid.UUID, dur, reason, ticket string) string {
	m := map[string]any{"kind": "elevation", "unit_id": unit, "duration": dur, "reason": reason}
	if ticket != "" {
		m["ticket"] = ticket
	}
	b, _ := json.Marshal(m)
	return string(b)
}

const requestsPath = "/api/v1/governance/requests"

func requestPath(id uuid.UUID, verb string) string {
	return requestsPath + "/" + id.String() + "/" + verb
}

type elevCreated struct {
	Pending types.GovernanceChange `json:"pending_change"`
	Request GovernanceRequestView  `json:"request"`
}

// ask posts an elevation request as c and requires the 202.
func (e *elevEnv) ask(c *http.Cookie, unit types.GovernanceUnit, dur, ticket string) elevCreated {
	e.t.Helper()
	w := e.call(c, http.MethodPost, requestsPath, elevBody(unit.ID, dur, "on call for the incident", ticket))
	if w.Code != http.StatusAccepted {
		e.t.Fatalf("request elevation in %s = %d %s, want 202", unit.Name, w.Code, w.Body)
	}
	if loc := w.Header().Get("Location"); loc != "" {
		e.t.Errorf("a request's 202 sets Location %q; a requester cannot read /governance/changes", loc)
	}
	var out elevCreated
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.Pending.ID == uuid.Nil || out.Request.ID != out.Pending.ID {
		e.t.Fatalf("202 body %s: %v", w.Body, err)
	}
	return out
}

type elevList struct {
	Requests []GovernanceRequestView `json:"requests"`
	Offered  []ElevationOffer        `json:"offered"`
}

func (e *elevEnv) list(c *http.Cookie) elevList {
	e.t.Helper()
	w := e.call(c, http.MethodGet, requestsPath, "")
	if w.Code != http.StatusOK {
		e.t.Fatalf("GET %s = %d %s", requestsPath, w.Code, w.Body)
	}
	var out elevList
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.Requests == nil || out.Offered == nil {
		e.t.Fatalf("list body %s: %v (both lists must encode as arrays)", w.Body, err)
	}
	return out
}

func offeredUnits(l elevList) []uuid.UUID {
	var out []uuid.UUID
	for _, o := range l.Offered {
		out = append(out, o.UnitID)
	}
	return out
}

// elevation is sub's membership row at u's key, nil when none.
func (e *elevEnv) elevation(u types.GovernanceUnit, sub string) *types.GovernanceMembership {
	e.t.Helper()
	m, err := store.GetGovernanceMembershipByKeyQ(context.Background(), e.pool, u.ID, "user", types.CanonicalUserSubject(sub), false)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		e.t.Fatal(err)
	}
	return &m
}

// seedElevation inserts sub's elevation in u ending at end, as an approval writes it.
func (e *elevEnv) seedElevation(u types.GovernanceUnit, sub string, end time.Time) types.GovernanceMembership {
	e.t.Helper()
	m := types.GovernanceMembership{ID: uuid.New(), SubjectType: types.CapabilitySubjectUser, Subject: types.CanonicalUserSubject(sub),
		UnitID: u.ID, CreatedAt: time.Now().UTC(), CreatedBy: "seed", ExpiresAt: &end}
	if err := store.InsertGovernanceMembershipQ(context.Background(), e.pool, m); err != nil {
		e.t.Fatal(err)
	}
	return m
}

func (e *elevEnv) wantReason(name string, w *httptest.ResponseRecorder, code int, reason string) {
	e.t.Helper()
	if w.Code != code || wireReason(e.t, w) != reason {
		e.t.Errorf("%s = %d %s, want %d %s", name, w.Code, w.Body, code, reason)
	}
}

// createAs calls the create handler with ctx as the caller, for identities a cookie cannot carry.
func (e *elevEnv) createAs(ctx context.Context, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	e.srv.handleCreateGovernanceRequest(w, httptest.NewRequest(http.MethodPost, requestsPath, strings.NewReader(body)).WithContext(ctx))
	return w
}

// listAs calls the list handler with ctx as the caller.
func (e *elevEnv) listAs(ctx context.Context) elevList {
	e.t.Helper()
	w := httptest.NewRecorder()
	e.srv.handleListGovernanceRequests(w, httptest.NewRequest(http.MethodGet, requestsPath, nil).WithContext(ctx))
	var out elevList
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		e.t.Fatalf("list as ctx = %d %s", w.Code, w.Body)
	}
	return out
}

// elevPersonCtx is a signed-in person with a group snapshot, as sign-in publishes one.
func elevPersonCtx(sub string, groups []string, truncated bool) context.Context {
	return withHumanIdentity(context.Background(), sub, sub+"@corp.example", oidc.RoleUser, types.UserTypeStandard, groups, truncated)
}

func (e *elevEnv) rowsFor(action, target string) []map[string]any {
	e.t.Helper()
	var out []map[string]any
	for _, ev := range e.audits(action) {
		if ev.Target == target {
			out = append(out, auditData(e.t, ev))
		}
	}
	return out
}

func TestElevationApproveCreatesExpiringMembership(t *testing.T) {
	e := newElevEnv(t)
	got := e.ask(e.req, e.a1, "2h", "")
	if got.Pending.TargetKind != types.GovernanceTargetElevation || got.Request.Kind != "elevation" || got.Request.Duration != "2h" ||
		!got.Request.Mine || got.Request.Decidable || got.Request.UnitID == nil || *got.Request.UnitID != e.a1.ID {
		t.Fatalf("created = %+v, want a pending elevation of 2h in ua-a1, mine and not decidable", got)
	}
	if e.elevation(e.a1, elevReqSub) != nil {
		t.Fatal("a pending request wrote a membership")
	}
	before := time.Now().UTC()
	if w := e.call(e.adminA1, http.MethodPost, requestPath(got.Pending.ID, "approve"), ""); w.Code != http.StatusOK {
		t.Fatalf("approve = %d %s", w.Code, w.Body)
	}
	m := e.elevation(e.a1, elevReqSub)
	if m == nil || m.ExpiresAt == nil || m.Legacy || m.Home || m.Priority != 0 || m.CreatedBy != uaSubAdminA1 {
		t.Fatalf("membership = %+v, want an expiring user row created by the approver", m)
	}
	if end := m.ExpiresAt.Sub(before); end < 2*time.Hour-time.Minute || end > 2*time.Hour+time.Minute {
		t.Errorf("expires_at = %v, want two hours from the decision", m.ExpiresAt)
	}
	l := e.list(e.req)
	if len(l.Requests) != 1 || l.Requests[0].State != types.GovernanceChangeApplied || l.Requests[0].EndsAt == nil || !l.Requests[0].EndsAt.Equal(*m.ExpiresAt) {
		t.Errorf("requester's list = %+v, want the applied request with ends_at the membership's end", l.Requests)
	}
	if slices.Contains(offeredUnits(l), e.a1.ID) {
		t.Errorf("offered = %v after the approval, want ua-a1 gone (already a member)", l.Offered)
	}
}

func TestElevationRequesterCannotApprove(t *testing.T) {
	e := newElevEnv(t)
	got := e.ask(e.req, e.a1, "1h", "")
	if w := e.call(e.req, http.MethodPost, requestPath(got.Pending.ID, "approve"), ""); !scopeRefused(t, w) {
		t.Errorf("the requester approving = %d %s, want 403 unit_scope_denied", w.Code, w.Body)
	}
	// A requester who is also a decider (in el-deciders) is still refused: the second-human rule.
	both, sub := uaSession(t, "sub-el-both", []string{elevRequesters, elevDeciders}, false), "sub-el-both"
	mine := e.ask(both, e.a1, "1h", "")
	if w := e.call(both, http.MethodPost, requestPath(mine.Pending.ID, "approve"), ""); w.Code != http.StatusForbidden || wireReason(t, w) != "second_human_required" {
		t.Errorf("a requester in the deciders group approving = %d %s, want 403 second_human_required", w.Code, w.Body)
	}
	if e.elevation(e.a1, sub) != nil || e.elevation(e.a1, elevReqSub) != nil {
		t.Error("a refused approval wrote a membership")
	}
}

func TestElevationExpiredRowIgnoredBeforeSweep(t *testing.T) {
	e := newElevEnv(t)
	e.seedElevation(e.a1, elevReqSub, time.Now().UTC().Add(-time.Minute))
	ms, err := store.ListMembershipsForQ(context.Background(), e.pool, []string{elevReqSub}, nil, "")
	if err != nil || len(ms) != 0 {
		t.Errorf("ListMembershipsForQ = %+v, %v; want the ended row skipped", ms, err)
	}
	w := e.admin(http.MethodGet, "/api/v1/governance", "")
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), elevReqSub) {
		t.Errorf("GET /governance = %d, lists the ended elevation: %v", w.Code, strings.Contains(w.Body.String(), elevReqSub))
	}
	ctx := withContextChoice(treeCtx(elevReqSub, []string{elevRequesters}, ""), contextChoice{RunIn: e.a1.ID})
	if _, err := e.srv.effectiveCeiling(ctx); !errors.Is(err, errUnitNotAContext) {
		t.Errorf("a run in ua-a1 after the end = %v, want unit_not_a_context", err)
	}
}

func TestElevationSweepDeletesAndAudits(t *testing.T) {
	e := newElevEnv(t)
	ended := e.seedElevation(e.a1, elevReqSub, time.Now().UTC().Add(-time.Minute))
	live := e.seedElevation(e.a2, elevReqSub, time.Now().UTC().Add(time.Hour))
	if err := e.srv.sweepExpiredElevations(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e.elevation(e.a1, elevReqSub) != nil || e.elevation(e.a2, elevReqSub) == nil {
		t.Errorf("after the sweep: ended row kept or live row deleted")
	}
	rows := e.audits("governance.elevation.expire")
	if len(rows) != 1 || rows[0].Target != ended.ID.String() || rows[0].ActorType != types.ActorSystem || rows[0].Actor != "wardynd" {
		t.Fatalf("expire rows = %+v, want one for %s by system/wardynd", rows, ended.ID)
	}
	d := auditData(t, rows[0])
	if d["unit_id"] != e.a1.ID.String() || d["subject"] != elevReqSub || d["subject_type"] != "user" || d["expired_at"] == nil {
		t.Errorf("expire row data = %v", d)
	}
	_ = live
}

func TestElevationRunCappedAtExpiry(t *testing.T) {
	e := newElevEnv(t)
	end := time.Now().UTC().Add(90 * time.Minute).Truncate(time.Microsecond)
	e.seedElevation(e.a1, elevReqSub, end)
	ctx := withContextChoice(treeCtx(elevReqSub, []string{elevRequesters}, ""), contextChoice{RunIn: e.a1.ID})
	c, err := e.srv.effectiveCeiling(ctx)
	if err != nil || unitIDOf(c) != e.a1.ID || c.ContextExpiresAt == nil || !c.ContextExpiresAt.Equal(end) {
		t.Fatalf("ceiling = unit %v, expires %v, %v; want ua-a1 ending at %v", unitIDOf(c), c.ContextExpiresAt, err, end)
	}
	now := time.Now().UTC()
	id := uuid.New()
	run := types.AgentRun{ID: id, CreatedAt: now, UpdatedAt: now, CreatedBy: elevReqSub, Agent: "claude-code", ConfinementClass: types.CC2,
		State: types.RunRunning, SPIFFEID: "spiffe://wardyn.local/agent-run/" + id.String(), RunnerTarget: "docker"}
	e.srv.captureRunLimits(&run, c, nil)
	if run.LifetimeCapAt == nil || !run.LifetimeCapAt.Equal(end) || run.LifetimeCapSource != types.LimitSourceElevation {
		t.Fatalf("captured cap = %v (%q), want %v from the elevation", run.LifetimeCapAt, run.LifetimeCapSource, end)
	}
	if run.EndsAt == nil || run.EndsAt.After(end) {
		t.Errorf("ends_at = %v, want clamped to the elevation's end", run.EndsAt)
	}
	// The insert proves the widened CHECK: the old constraint refuses 'elevation'.
	if _, err := e.pg.CreateRun(context.Background(), run); err != nil {
		t.Fatalf("store a run capped by an elevation: %v", err)
	}
	got, err := e.pg.GetRun(context.Background(), id)
	if err != nil || got.LifetimeCapSource != types.LimitSourceElevation || got.LifetimeCapAt == nil || !got.LifetimeCapAt.Equal(end) {
		t.Errorf("stored run = %v %q, %v", got.LifetimeCapAt, got.LifetimeCapSource, err)
	}
}

func TestElevationNotOfferedWithoutRule(t *testing.T) {
	e := newElevEnv(t)
	noRule := e.call(e.req, http.MethodPost, requestsPath, elevBody(e.b.ID, "1h", "need it", ""))
	e.wantReason("a unit with no rule", noRule, http.StatusBadRequest, reasonElevationNotOffered)
	def := e.call(e.req, http.MethodPost, requestsPath, elevBody(store.DefaultUnitID, "1h", "need it", ""))
	e.wantReason("Default", def, http.StatusBadRequest, reasonElevationNotOffered)
	unknown := e.call(e.req, http.MethodPost, requestsPath, elevBody(uuid.New(), "1h", "need it", ""))
	e.wantReason("an unknown unit", unknown, http.StatusBadRequest, reasonElevationNotOffered)
	outsider := uaSession(t, "sub-el-outsider", []string{"someone-else"}, false)
	notEligible := e.call(outsider, http.MethodPost, requestsPath, elevBody(e.a1.ID, "1h", "need it", ""))
	e.wantReason("not eligible", notEligible, http.StatusBadRequest, reasonElevationNotOffered)
	var a, b errorBody
	_ = json.Unmarshal(noRule.Body.Bytes(), &a)
	_ = json.Unmarshal(notEligible.Body.Bytes(), &b)
	if strings.Replace(a.Error, e.b.Name, "", 1) != strings.Replace(b.Error, e.a1.Name, "", 1) {
		t.Errorf("not eligible says %q, no rule says %q: want one sentence, so the answer is no oracle", b.Error, a.Error)
	}
	if l := e.list(outsider); len(l.Offered) != 0 {
		t.Errorf("offered to an outsider = %v, want none", l.Offered)
	}
}

func TestElevationGroupDeciderRecheckedInTx(t *testing.T) {
	e := newElevEnv(t)
	got := e.ask(e.req, e.a1, "1h", "")
	if l := e.list(e.deciderGroup); len(l.Requests) != 1 || !l.Requests[0].Decidable {
		t.Fatalf("group decider's list = %+v, want the request, decidable", l.Requests)
	}
	e.setRules(e.a1, elevationRule(func(r *types.DeciderRule) { r.Who = []string{types.DeciderUnitAdmins} }))
	// The decision's recheck reads the rule on its own transaction: the group, removed after the list, is refused.
	r := httptest.NewRequest(http.MethodPost, "/", nil).WithContext(elevPersonCtx(elevDecider, []string{elevDeciders}, false))
	kind := governanceChangeKinds[types.GovernanceTargetElevation].forChange(got.Pending)
	err := e.pg.DryRunGovernance(r.Context(), func(q store.Querier) error { return e.srv.recheckApprover(r.Context(), r, q, kind) })
	var refused *govRefusal
	if !errors.As(err, &refused) {
		t.Errorf("the in-transaction recheck of the removed group = %v, want a refusal", err)
	}
	// Through the route, the request is no longer theirs to see: the hidden 404 or, in a race, 403.
	if w := e.call(e.deciderGroup, http.MethodPost, requestPath(got.Pending.ID, "approve"), ""); !scopeRefused(t, w) && w.Code != http.StatusNotFound {
		t.Errorf("the removed group approving = %d %s, want 404 or 403 unit_scope_denied", w.Code, w.Body)
	}
	if e.elevation(e.a1, elevReqSub) != nil {
		t.Error("a refused approval wrote a membership")
	}
}

func TestElevationReasonAndTicketAudited(t *testing.T) {
	e := newElevEnv(t)
	got := e.ask(e.req, e.a1, "1h", "INC-1234")
	req := e.rowsFor("governance.elevation.request", got.Pending.ID.String())
	if len(req) != 1 || req[0]["reason"] != "on call for the incident" || req[0]["ticket"] != "INC-1234" || req[0]["change_id"] != got.Pending.ID.String() ||
		req[0]["duration"] != "1h" || req[0]["unit_id"] != e.a1.ID.String() || req[0]["expires_at"] == nil {
		t.Fatalf("request rows = %v", req)
	}
	if units, _ := req[0]["units"].([]any); len(units) != 1 || units[0] != e.a1.ID.String() {
		t.Errorf("request row units = %v, want [ua-a1]", req[0]["units"])
	}
	fields := audit.SealedFields("governance.elevation.request")
	if len(fields) != 1 || fields[0].Path != "reason" || fields[0].Subject != audit.SubjectActor {
		t.Errorf("sealed fields of the request row = %+v, want reason under the actor", fields)
	}
	if w := e.call(e.adminA1, http.MethodPost, requestPath(got.Pending.ID, "approve"), ""); w.Code != http.StatusOK {
		t.Fatalf("approve = %d %s", w.Code, w.Body)
	}
	m := e.elevation(e.a1, elevReqSub)
	app := e.rowsFor("governance.elevation.approve", m.ID.String())
	if len(app) != 1 || app[0]["ticket"] != "INC-1234" || app[0]["change_id"] != got.Pending.ID.String() || app[0]["subject"] != m.Subject ||
		app[0]["unit_scope"] != e.a1.ID.String() || app[0]["reason"] != nil {
		t.Errorf("approve rows = %v, want ticket, change_id, subject, unit_scope ua-a1 and no reason", app)
	}
	// The end, swept: the expire row carries the ticket and the change too.
	if _, err := e.pool.Exec(context.Background(), `UPDATE governance_memberships SET expires_at = now() - interval '1 second' WHERE id = $1`, m.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.srv.sweepExpiredElevations(context.Background()); err != nil {
		t.Fatal(err)
	}
	exp := e.rowsFor("governance.elevation.expire", m.ID.String())
	if len(exp) != 1 || exp[0]["ticket"] != "INC-1234" || exp[0]["change_id"] != got.Pending.ID.String() {
		t.Errorf("expire rows = %v, want the ticket and change_id", exp)
	}
	// A reject row carries the ticket and no reason.
	other, _ := e.requester()
	rej := e.ask(other, e.a1, "1h", "INC-9")
	if w := e.call(e.adminA1, http.MethodPost, requestPath(rej.Pending.ID, "reject"), `{"reason":"not now"}`); w.Code != http.StatusOK {
		t.Fatalf("reject = %d %s", w.Code, w.Body)
	}
	rows := e.rowsFor("governance.elevation.reject", rej.Pending.ID.String())
	if len(rows) != 1 || rows[0]["ticket"] != "INC-9" || rows[0]["reason"] != nil || rows[0]["withdrawn"] != nil {
		t.Errorf("reject rows = %v, want the ticket, no reason, not withdrawn", rows)
	}
}

func TestRequestFieldsErased(t *testing.T) {
	e := newElevEnv(t)
	got := e.ask(e.req, e.a1, "1h", "INC-77")
	if _, err := e.pg.EraseGovernanceChangePersonalFields(context.Background(), []string{elevReqSub}, []string{elevReqSub + "@corp.example"}); err != nil {
		t.Fatal(err)
	}
	ch, err := e.pg.GetGovernanceChange(context.Background(), got.Pending.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ch.Note != nil && (ch.Note.Reason != "" || ch.Note.Ticket != "") {
		t.Errorf("note after erasure = %+v, want cleared", ch.Note)
	}
	if ch.State == types.GovernanceChangePending {
		t.Error("an erased requester's request is still pending")
	}
}

func TestElevationOfferedListsInheritedRule(t *testing.T) {
	e := newElevEnv(t)
	e.setRules(e.a1)
	e.setRules(e.a, elevationRule(func(r *types.DeciderRule) { r.MaxElevation = "2h"; r.TicketPattern = "INC-[0-9]+" }))
	l := e.list(e.req)
	for _, o := range l.Offered {
		if o.MaxElevation != "2h" || !o.TicketRequired {
			t.Errorf("offer %+v, want the inherited 2h and a ticket required", o)
		}
	}
	got := offeredUnits(l)
	for _, u := range []types.GovernanceUnit{e.a, e.a1, e.a2} {
		if !slices.Contains(got, u.ID) {
			t.Errorf("offered = %v, want %s (its own or an inherited rule)", got, u.Name)
		}
	}
	if slices.Contains(got, e.b.ID) || slices.Contains(got, store.DefaultUnitID) {
		t.Errorf("offered = %v, want neither ua-b nor Default", got)
	}
	names := make([]string, len(l.Offered))
	for i, o := range l.Offered {
		names[i] = o.UnitName
	}
	if !slices.IsSorted(names) {
		t.Errorf("offered names %v, want sorted", names)
	}
}

func TestElevationAlreadyMemberRefused(t *testing.T) {
	e := newElevEnv(t)
	cases := []struct {
		name string
		seed func(sub string)
		c    func(sub string) *http.Cookie
	}{
		{"a user row", func(sub string) { treeMember(t, e.govEnv, e.a1, types.CapabilitySubjectUser, sub, 1, false) },
			func(sub string) *http.Cookie { return uaSession(t, sub, []string{elevRequesters}, false) }},
		{"a group row", func(string) { treeMember(t, e.govEnv, e.a1, types.CapabilitySubjectGroup, "el-team", 1, false) },
			func(sub string) *http.Cookie { return uaSession(t, sub, []string{elevRequesters, "el-team"}, false) }},
		{"a user_type row", func(string) {
			treeMember(t, e.govEnv, e.a1, types.CapabilitySubjectUserType, types.UserTypeStandard, 1, false)
		},
			func(sub string) *http.Cookie { return uaSession(t, sub, []string{elevRequesters}, false) }},
	}
	for _, tc := range cases {
		sub := e.fresh("sub-el-member")
		tc.seed(sub)
		c := tc.c(sub)
		e.wantReason(tc.name, e.call(c, http.MethodPost, requestsPath, elevBody(e.a1.ID, "1h", "need it", "")), http.StatusConflict, reasonElevationAlreadyMember)
		if slices.Contains(offeredUnits(e.list(c)), e.a1.ID) {
			t.Errorf("%s: ua-a1 is offered to a member", tc.name)
		}
		if _, err := e.pool.Exec(context.Background(), `DELETE FROM governance_memberships WHERE unit_id = $1`, e.a1.ID); err != nil {
			t.Fatal(err)
		}
	}
}

func TestElevationGroupDeciderAuditUnitScope(t *testing.T) {
	e := newElevEnv(t)
	got := e.ask(e.req, e.a1, "1h", "")
	if w := e.call(e.deciderGroup, http.MethodPost, requestPath(got.Pending.ID, "approve"), ""); w.Code != http.StatusOK {
		t.Fatalf("group approve = %d %s", w.Code, w.Body)
	}
	m := e.elevation(e.a1, elevReqSub)
	if rows := e.rowsFor("governance.elevation.approve", m.ID.String()); len(rows) != 1 || rows[0]["unit_scope"] != "group:"+elevDeciders {
		t.Errorf("approve rows = %v, want unit_scope group:%s", rows, elevDeciders)
	}
	other, _ := e.requester()
	rej := e.ask(other, e.a1, "1h", "")
	if w := e.call(e.deciderGroup, http.MethodPost, requestPath(rej.Pending.ID, "reject"), ""); w.Code != http.StatusOK {
		t.Fatalf("group reject = %d %s", w.Code, w.Body)
	}
	if rows := e.rowsFor("governance.elevation.reject", rej.Pending.ID.String()); len(rows) != 1 || rows[0]["unit_scope"] != "group:"+elevDeciders {
		t.Errorf("reject rows = %v, want unit_scope group:%s", rows, elevDeciders)
	}
}

func TestElevationAdminTokenBypassAudited(t *testing.T) {
	e := newElevEnv(t)
	got := e.ask(e.req, e.a1, "1h", "")
	if w := e.admin(http.MethodPost, requestPath(got.Pending.ID, "approve"), ""); w.Code != http.StatusOK {
		t.Fatalf("admin token approve = %d %s", w.Code, w.Body)
	}
	if e.elevation(e.a1, elevReqSub) == nil {
		t.Fatal("the admin token's approval wrote no membership")
	}
	found := false
	for _, ev := range e.audits("governance.change.bypass") {
		found = found || ev.Target == got.Pending.ID.String()
	}
	if !found {
		t.Error("no governance.change.bypass row for the admin token's approval")
	}
}

func TestElevationUpsertOverLiveElevationIsPermanent(t *testing.T) {
	e := newElevEnv(t)
	e.seedElevation(e.a1, elevReqSub, time.Now().UTC().Add(time.Hour))
	if w := e.call(e.alice, http.MethodPost, "/api/v1/governance/memberships", membershipBody(elevReqSub, e.a1.ID)); w.Code != http.StatusOK && w.Code != http.StatusCreated && w.Code != http.StatusAccepted {
		t.Fatalf("unit admin membership write = %d %s", w.Code, w.Body)
	} else if w.Code == http.StatusAccepted {
		ch := e.pending(w)
		if w := e.call(e.bob, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusOK {
			t.Fatalf("approve the held membership = %d %s", w.Code, w.Body)
		}
	}
	if m := e.elevation(e.a1, elevReqSub); m == nil || m.ExpiresAt != nil {
		t.Errorf("membership after the admin's write = %+v, want permanent", m)
	}
}

func TestElevationTooLongNamesUnitMax(t *testing.T) {
	e := newElevEnv(t)
	w := e.call(e.req, http.MethodPost, requestsPath, elevBody(e.a1.ID, "5h", "need it", ""))
	e.wantReason("5h under a 4h rule", w, http.StatusBadRequest, reasonElevationTooLong)
	if !strings.Contains(w.Body.String(), e.a1.Name) || !strings.Contains(w.Body.String(), "4h") {
		t.Errorf("refusal %s, want it to name %s and 4h", w.Body, e.a1.Name)
	}
}

func TestElevationDurationInvalid(t *testing.T) {
	e := newElevEnv(t)
	for _, d := range []string{"", "soon", "10m", "-1h", "1d"} {
		e.wantReason("duration "+d, e.call(e.req, http.MethodPost, requestsPath, elevBody(e.a1.ID, d, "need it", "")), http.StatusBadRequest, reasonElevationDurationInvalid)
	}
}

func TestElevationReasonRequired(t *testing.T) {
	e := newElevEnv(t)
	e.wantReason("no reason", e.call(e.req, http.MethodPost, requestsPath, elevBody(e.a1.ID, "1h", "  ", "")), http.StatusBadRequest, reasonRequestReasonRequired)
	e.wantReason("a long reason", e.call(e.req, http.MethodPost, requestsPath, elevBody(e.a1.ID, "1h", strings.Repeat("r", 501), "")), http.StatusBadRequest, reasonRequestReasonTooLong)
	e.wantReason("a two-line ticket", e.call(e.req, http.MethodPost, requestsPath, elevBody(e.a1.ID, "1h", "need it", "INC-1\nINC-2")), http.StatusBadRequest, reasonGovernanceChangeNoteInvalid)
	e.wantReason("an image on elevation", e.call(e.req, http.MethodPost, requestsPath, `{"kind":"elevation","unit_id":"`+e.a1.ID.String()+`","duration":"1h","reason":"x","image":"alpine"}`), http.StatusBadRequest, reasonInvalidRequestBody)
	e.wantReason("an unknown kind", e.call(e.req, http.MethodPost, requestsPath, `{"kind":"root","reason":"x"}`), http.StatusBadRequest, reasonGovernanceRequestKindInvalid)
	e.wantReason("an unknown field", e.call(e.req, http.MethodPost, requestsPath, `{"kind":"elevation","reason":"x","extra":1}`), http.StatusBadRequest, reasonInvalidRequestBody)
}

func TestGovernanceRequestNeedsPerson(t *testing.T) {
	e := newElevEnv(t)
	body := elevBody(e.a1.ID, "1h", "need it", "")
	e.wantReason("the admin token", e.admin(http.MethodPost, requestsPath, body), http.StatusForbidden, reasonGovernanceRequestNeedsPerson)
	service := withServiceIdentity(elevPersonCtx("sub-el-svc", []string{elevRequesters}, false), "deploy-bot", elevReqSub)
	e.wantReason("a service token", e.createAs(service, body), http.StatusForbidden, reasonGovernanceRequestNeedsPerson)
	device := context.WithValue(elevPersonCtx("sub-el-dev", []string{elevRequesters}, false), deviceCtxKey{}, types.Device{ID: uuid.New(), Name: "laptop"})
	e.wantReason("a device", e.createAs(device, body), http.StatusForbidden, reasonGovernanceRequestNeedsPerson)
	if l := e.listAs(service); len(l.Offered) != 0 {
		t.Errorf("offered to a service = %v, want none", l.Offered)
	}
	// A portal acting for a person never reaches the handler: the routes are off the delegation list.
	for key := range delegationAllowed {
		if strings.Contains(key, "/governance/requests") {
			t.Errorf("delegationAllowed holds %q; a portal must get delegation_scope", key)
		}
	}
}

func TestElevationUnitAdminDecidesAndGroupDecides(t *testing.T) {
	e := newElevEnv(t)
	first := e.ask(e.req, e.a1, "1h", "")
	if w := e.call(e.adminA, http.MethodPost, requestPath(first.Pending.ID, "approve"), ""); w.Code != http.StatusOK {
		t.Errorf("an admin above ua-a1 approving = %d %s", w.Code, w.Body)
	}
	c, _ := e.requester()
	second := e.ask(c, e.a1, "1h", "")
	if w := e.call(e.adminB, http.MethodPost, requestPath(second.Pending.ID, "approve"), ""); w.Code != http.StatusNotFound {
		t.Errorf("ua-b's admin approving = %d %s, want the hidden 404", w.Code, w.Body)
	}
	ch, err := e.pg.GetGovernanceChange(context.Background(), second.Pending.ID)
	if err != nil {
		t.Fatal(err)
	}
	approver := requestApprover(ch, types.DeciderKindElevation)
	as := func(ctx context.Context) bool {
		return approver.allowed(e.srv, httptest.NewRequest(http.MethodPost, "/", nil).WithContext(ctx))
	}
	token := withAPITokenID(elevPersonCtx(elevDecider, []string{elevDeciders}, false), uuid.New())
	cli := withCLISessionID(withAPITokenID(elevPersonCtx(elevDecider, []string{elevDeciders}, false), uuid.New()), uuid.New())
	if as(token) {
		t.Error("TTL off: a bare API token in the deciders group decides")
	}
	if !as(cli) {
		t.Error("TTL off: a CLI session in the deciders group does not decide")
	}
	e.srv.cfg.RoleStampTTL = time.Hour
	if !as(token) {
		t.Error("TTL on: a token in the deciders group does not decide")
	}
	e.srv.cfg.RoleStampTTL = 0
	if w := e.call(e.deciderGroup, http.MethodPost, requestPath(second.Pending.ID, "approve"), ""); w.Code != http.StatusOK {
		t.Errorf("a group decider approving = %d %s", w.Code, w.Body)
	}
	got := second.Request.Deciders
	if len(got) == 0 || got[0].Kind != "security" || !slices.ContainsFunc(got, func(a changeApprover) bool { return a.Kind == "group" && a.Name == elevDeciders }) ||
		!slices.ContainsFunc(got, func(a changeApprover) bool { return a.Kind == "unit_admin" && a.Name == uaSubAdminA1 }) {
		t.Errorf("deciders = %+v, want security, ua-a1's admins and the group", got)
	}
}

func TestElevationEligibleRequesters(t *testing.T) {
	e := newElevEnv(t)
	body := elevBody(e.a1.ID, "1h", "need it", "")
	for name, tc := range map[string]struct {
		ctx  context.Context
		code int
	}{
		"in the group":           {elevPersonCtx("sub-el-in", []string{elevRequesters}, false), http.StatusAccepted},
		"out of the group":       {elevPersonCtx("sub-el-out", []string{"other"}, false), http.StatusBadRequest},
		"a stale snapshot":       {elevPersonCtx("sub-el-stale", nil, false), http.StatusBadRequest},
		"a truncated snapshot":   {elevPersonCtx("sub-el-trunc", []string{elevRequesters}, true), http.StatusBadRequest},
		"a bare token, TTL off":  {withAPITokenID(elevPersonCtx("sub-el-token", []string{elevRequesters}, false), uuid.New()), http.StatusBadRequest},
		"a CLI session, TTL off": {withCLISessionID(withAPITokenID(elevPersonCtx("sub-el-cli", []string{elevRequesters}, false), uuid.New()), uuid.New()), http.StatusAccepted},
	} {
		w := e.createAs(tc.ctx, body)
		if w.Code != tc.code || (tc.code == http.StatusBadRequest && wireReason(t, w) != reasonElevationNotOffered) {
			t.Errorf("%s: %d %s, want %d", name, w.Code, w.Body, tc.code)
		}
		if offered := slices.Contains(offeredUnits(e.listAs(tc.ctx)), e.a1.ID); offered != (tc.code == http.StatusAccepted) {
			t.Errorf("%s: offered = %v, want it to match the create", name, offered)
		}
	}
	e.srv.cfg.RoleStampTTL = time.Hour
	if w := e.createAs(withAPITokenID(elevPersonCtx("sub-el-token-ttl", []string{elevRequesters}, false), uuid.New()), body); w.Code != http.StatusAccepted {
		t.Errorf("a bare token, TTL on: %d %s, want 202", w.Code, w.Body)
	}
}

func TestElevationRuleWithoutEligibleOffersNobody(t *testing.T) {
	e := newElevEnv(t)
	e.setRawRules(e.a1, types.DeciderRule{Kinds: []string{types.DeciderKindElevation}, Who: []string{types.DeciderUnitAdmins}, MaxElevation: "4h"})
	if slices.Contains(offeredUnits(e.list(e.req)), e.a1.ID) {
		t.Error("a rule with no eligible_requesters offers ua-a1")
	}
	e.wantReason("create under it", e.call(e.req, http.MethodPost, requestsPath, elevBody(e.a1.ID, "1h", "need it", "")), http.StatusBadRequest, reasonElevationNotOffered)
}

func TestElevationTicketPattern(t *testing.T) {
	e := newElevEnv(t)
	e.setRules(e.a1, elevationRule(func(r *types.DeciderRule) { r.TicketPattern = "INC-[0-9]{4}" }))
	e.wantReason("no ticket", e.call(e.req, http.MethodPost, requestsPath, elevBody(e.a1.ID, "1h", "need it", "")), http.StatusBadRequest, reasonElevationTicketRequired)
	bad := e.call(e.req, http.MethodPost, requestsPath, elevBody(e.a1.ID, "1h", "need it", "INC-12"))
	e.wantReason("a ticket that does not match", bad, http.StatusBadRequest, reasonElevationTicketInvalid)
	if !strings.Contains(bad.Body.String(), "INC-[0-9]{4}") {
		t.Errorf("refusal %s, want it to show the pattern", bad.Body)
	}
	e.wantReason("a partial match", e.call(e.req, http.MethodPost, requestsPath, elevBody(e.a1.ID, "1h", "need it", "xINC-1234")), http.StatusBadRequest, reasonElevationTicketInvalid)
	got := e.ask(e.req, e.a1, "1h", "INC-1234")
	if l := e.list(e.req); !slices.ContainsFunc(l.Offered, func(o ElevationOffer) bool { return o.TicketRequired }) && len(l.Offered) > 0 {
		t.Errorf("offered = %+v, want ticket_required", l.Offered)
	}
	// A note edit that breaks the pattern is refused. The note route is behind requireUnitScope, so the
	// requester is given a grant on an unrelated unit first.
	e.grant(e.b, types.CapabilitySubjectUser, elevReqSub)
	note := "/api/v1/governance/changes/" + got.Pending.ID.String() + "/note"
	e.wantReason("a note that drops the ticket", e.call(e.req, http.MethodPut, note, `{"reason":"still on call"}`), http.StatusBadRequest, reasonElevationTicketRequired)
	e.wantReason("a note that breaks the ticket", e.call(e.req, http.MethodPut, note, `{"reason":"still on call","ticket":"INC-1"}`), http.StatusBadRequest, reasonElevationTicketInvalid)
	e.wantReason("a note with no reason", e.call(e.req, http.MethodPut, note, `{"reason":"","ticket":"INC-1234"}`), http.StatusBadRequest, reasonGovernanceChangeNoteInvalid)
	if w := e.call(e.req, http.MethodPut, note, `{"reason":"still on call","ticket":"INC-5678"}`); w.Code != http.StatusOK {
		t.Errorf("a note that keeps the pattern = %d %s", w.Code, w.Body)
	}
}

func TestElevationApplyRechecksTicketPattern(t *testing.T) {
	e := newElevEnv(t)
	got := e.ask(e.req, e.a1, "1h", "free text")
	e.setRules(e.a, elevationRule(func(r *types.DeciderRule) { r.TicketPattern = "INC-[0-9]+" }))
	_ = e.call(e.adminA1, http.MethodPost, requestPath(got.Pending.ID, "approve"), "")
	if s := e.changeState(got.Pending.ID); s != types.GovernanceChangeStale {
		t.Errorf("state = %s, want stale: the ticket fails a pattern added after the request", s)
	}
	if e.elevation(e.a1, elevReqSub) != nil {
		t.Error("a stale approval wrote a membership")
	}
}

func TestElevationCooldown(t *testing.T) {
	e := newElevEnv(t)
	e.setRules(e.a1, elevationRule(func(r *types.DeciderRule) { r.RenewAfter = "24h" }))
	got := e.ask(e.req, e.a1, "1h", "")
	if w := e.call(e.adminA1, http.MethodPost, requestPath(got.Pending.ID, "approve"), ""); w.Code != http.StatusOK {
		t.Fatalf("approve = %d %s", w.Code, w.Body)
	}
	// The elevation ends; the next request inside renew_after is refused with when it may come.
	if _, err := e.pool.Exec(context.Background(), `DELETE FROM governance_memberships WHERE unit_id = $1`, e.a1.ID); err != nil {
		t.Fatal(err)
	}
	w := e.call(e.req, http.MethodPost, requestsPath, elevBody(e.a1.ID, "1h", "again", ""))
	e.wantReason("inside renew_after", w, http.StatusConflict, reasonElevationCooldown)
	var body struct {
		RetryAfter time.Time `json:"retry_after"`
	}
	if json.Unmarshal(w.Body.Bytes(), &body) != nil || body.RetryAfter.Before(time.Now().Add(24*time.Hour)) {
		t.Errorf("retry_after = %v, want about a day and an hour on", body.RetryAfter)
	}
	// An apply after a concurrent approval at the same key is stale.
	other, sub := e.requester()
	pend := e.ask(other, e.a1, "1h", "")
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO governance_changes (id, target_kind, op, target_key, payload, diff, base_hash, deployment_hash,
			proposed_by, proposed_by_email, expires_at, state, decided_at)
		SELECT $2, target_kind, op, target_key, payload, diff, base_hash, deployment_hash, proposed_by, proposed_by_email, expires_at, 'applied', now()
		FROM governance_changes WHERE id = $1`, pend.Pending.ID, uuid.New()); err != nil {
		t.Fatal(err)
	}
	_ = e.call(e.adminA1, http.MethodPost, requestPath(pend.Pending.ID, "approve"), "")
	if s := e.changeState(pend.Pending.ID); s != types.GovernanceChangeStale || e.elevation(e.a1, sub) != nil {
		t.Errorf("state = %s, want stale and no membership", s)
	}
}

func TestElevationChainMeet(t *testing.T) {
	e := newElevEnv(t)
	e.setRules(e.a, elevationRule(func(r *types.DeciderRule) {
		r.MaxElevation = "2h"
		r.EligibleRequesters = []string{"group:" + elevRequesters, "group:el-wide"}
	}))
	e.setRules(e.a1, elevationRule(nil))
	e.wantReason("3h under a parent's 2h", e.call(e.req, http.MethodPost, requestsPath, elevBody(e.a1.ID, "3h", "need it", "")), http.StatusBadRequest, reasonElevationTooLong)
	wide := uaSession(t, "sub-el-wide", []string{"el-wide"}, false)
	e.wantReason("a group only the parent names", e.call(wide, http.MethodPost, requestsPath, elevBody(e.a1.ID, "1h", "need it", "")), http.StatusBadRequest, reasonElevationNotOffered)
	if w := e.call(wide, http.MethodPost, requestsPath, elevBody(e.a2.ID, "1h", "need it", "")); w.Code != http.StatusAccepted {
		t.Errorf("the parent's group in ua-a2 (no rule of its own) = %d %s, want 202", w.Code, w.Body)
	}
	e.ask(e.req, e.a1, "2h", "")
}

func TestElevationPendingTwiceIs409(t *testing.T) {
	e := newElevEnv(t)
	e.ask(e.req, e.a1, "1h", "")
	e.wantReason("a second request", e.call(e.req, http.MethodPost, requestsPath, elevBody(e.a1.ID, "2h", "again", "")), http.StatusConflict, reasonGovernanceChangePending)
}

func TestElevationRequesterWithdrawWritesRejectRow(t *testing.T) {
	e := newElevEnv(t)
	got := e.ask(e.req, e.a1, "1h", "")
	if w := e.call(e.req, http.MethodPost, requestPath(got.Pending.ID, "reject"), ""); w.Code != http.StatusOK {
		t.Fatalf("withdraw = %d %s", w.Code, w.Body)
	}
	rows := e.rowsFor("governance.elevation.reject", got.Pending.ID.String())
	if len(rows) != 1 || rows[0]["withdrawn"] != true || rows[0]["unit_scope"] != nil || rows[0]["subject"] != elevReqSub || rows[0]["duration"] != "1h" {
		t.Errorf("reject rows = %v, want one withdrawn row with no unit_scope", rows)
	}
}

func TestElevationNeverDefaultContext(t *testing.T) {
	e := newElevEnv(t)
	end := time.Now().UTC().Add(time.Hour)
	e.seedElevation(e.a1, elevReqSub, end)
	ctx := treeCtx(elevReqSub, []string{elevRequesters}, "")
	if c, err := e.srv.effectiveCeiling(ctx); err != nil || unitIDOf(c) != uuid.Nil {
		t.Errorf("an elevation alone, no run_in = unit %v, %v; want Default", unitIDOf(c), err)
	}
	treeMember(t, e.govEnv, e.b, types.CapabilitySubjectUser, elevReqSub, 1, true)
	if c, err := e.srv.effectiveCeiling(ctx); err != nil || unitIDOf(c) != e.b.ID || c.ContextExpiresAt != nil {
		t.Errorf("no run_in = unit %v (expires %v), %v; want the home unit ua-b", unitIDOf(c), c.ContextExpiresAt, err)
	}
	if c, err := e.srv.effectiveCeiling(withContextChoice(ctx, contextChoice{RunIn: e.a1.ID})); err != nil || unitIDOf(c) != e.a1.ID || c.ContextExpiresAt == nil {
		t.Errorf("run_in ua-a1 = unit %v (expires %v), %v; want the elevation", unitIDOf(c), c.ContextExpiresAt, err)
	}
}

func TestElevationRunInPrefersPermanentRow(t *testing.T) {
	e := newElevEnv(t)
	e.seedElevation(e.a1, elevReqSub, time.Now().UTC().Add(time.Hour))
	treeMember(t, e.govEnv, e.a1, types.CapabilitySubjectGroup, "el-team", 1, false)
	ctx := withContextChoice(treeCtx(elevReqSub, []string{elevRequesters, "el-team"}, ""), contextChoice{RunIn: e.a1.ID})
	if c, err := e.srv.effectiveCeiling(ctx); err != nil || unitIDOf(c) != e.a1.ID || c.ContextExpiresAt != nil {
		t.Errorf("run_in ua-a1 = unit %v (expires %v), %v; want the permanent group row", unitIDOf(c), c.ContextExpiresAt, err)
	}
}

func TestElevationQueuedStartAfterExpiryEnds(t *testing.T) {
	e := newQueueEnv(t)
	unit := treeChild(t, e.govEnv, uaUnitBody("elev-queue", store.DefaultUnitID, map[string]any{}, nil))
	end := time.Now().UTC().Add(time.Hour)
	m := types.GovernanceMembership{ID: uuid.New(), SubjectType: types.CapabilitySubjectUser, Subject: types.CanonicalUserSubject(e.personP),
		UnitID: unit.ID, CreatedAt: time.Now().UTC(), CreatedBy: "seed", ExpiresAt: &end}
	if err := store.InsertGovernanceMembershipQ(context.Background(), e.pool, m); err != nil {
		t.Fatal(err)
	}
	pool := e.cappedPool("Busy", 1)
	waiting, occupant := e.waitingPerson(pool, `,"run_in":"`+unit.ID.String()+`"`)
	if waiting.LifetimeCapSource != types.LimitSourceElevation {
		t.Errorf("queued run's cap source = %q, want elevation", waiting.LifetimeCapSource)
	}
	e.exec(`UPDATE governance_memberships SET expires_at = now() - interval '1 second' WHERE id = $1`, m.ID)
	e.free(occupant)
	e.ended(waiting.ID, queueStartRecheck{reason: string(reasonUnitNotAContext)})
}

func TestElevationNoReviveAfterEnd(t *testing.T) {
	e := newElevEnv(t)
	m := e.seedElevation(e.a1, elevReqSub, time.Now().UTC().Add(time.Hour))
	ctx := treeCtx(elevReqSub, []string{elevRequesters}, "")
	c, err := e.srv.effectiveCeiling(withContextChoice(ctx, contextChoice{RunIn: e.a1.ID}))
	if err != nil || c.Unit == nil {
		t.Fatalf("ceiling in ua-a1: %v", err)
	}
	if _, err := e.pool.Exec(context.Background(), `UPDATE governance_memberships SET expires_at = now() - interval '1 second' WHERE id = $1`, m.ID); err != nil {
		t.Fatal(err)
	}
	run := types.AgentRun{ID: uuid.New(), CreatedBy: elevReqSub, UserType: types.UserTypeStandard}
	for name, as := range map[string]context.Context{"the owner": ctx, "someone else": context.Background()} {
		ref, err := e.srv.ownerMembershipRefusal(as, run, c.Unit)
		if err != nil || ref == nil || ref.status != http.StatusConflict || ref.reason != reasonUnitMembershipGone {
			t.Errorf("%s: refusal = %+v, %v; want 409 unit_membership_gone", name, ref, err)
		}
	}
}

func TestElevationApplyStaleWhenRuleRemoved(t *testing.T) {
	e := newElevEnv(t)
	got := e.ask(e.req, e.a1, "3h", "")
	e.setRules(e.a1, elevationRule(func(r *types.DeciderRule) { r.MaxElevation = "1h" }))
	_ = e.call(e.alice, http.MethodPost, requestPath(got.Pending.ID, "approve"), "")
	if s := e.changeState(got.Pending.ID); s != types.GovernanceChangeStale {
		t.Errorf("a 3h request under a rule cut to 1h = %s, want stale", s)
	}
	other, sub := e.requester()
	gone := e.ask(other, e.a1, "1h", "")
	e.setRules(e.a1)
	_ = e.call(e.alice, http.MethodPost, requestPath(gone.Pending.ID, "approve"), "")
	if s := e.changeState(gone.Pending.ID); s != types.GovernanceChangeStale || e.elevation(e.a1, sub) != nil {
		t.Errorf("a request whose rule was removed = %s, want stale and no membership", s)
	}
}

func TestElevationApplyReplacesExpiredUnsweptRow(t *testing.T) {
	e := newElevEnv(t)
	got := e.ask(e.req, e.a1, "1h", "")
	old := e.seedElevation(e.a1, elevReqSub, time.Now().UTC().Add(-time.Minute))
	if w := e.call(e.adminA1, http.MethodPost, requestPath(got.Pending.ID, "approve"), ""); w.Code != http.StatusOK {
		t.Fatalf("approve over an ended row = %d %s", w.Code, w.Body)
	}
	m := e.elevation(e.a1, elevReqSub)
	if m == nil || m.ID == old.ID || m.ExpiresAt == nil || !m.ExpiresAt.After(time.Now()) {
		t.Fatalf("membership = %+v, want a new live elevation", m)
	}
	if rows := e.rowsFor("governance.elevation.expire", old.ID.String()); len(rows) != 1 {
		t.Errorf("expire rows for the replaced row = %v, want one", rows)
	}
	app := e.rowsFor("governance.elevation.approve", m.ID.String())
	if len(app) != 1 || app[0]["replaced_expired"] != old.ID.String() {
		t.Errorf("approve rows = %v, want replaced_expired %s", app, old.ID)
	}
	// A live row at the key makes the apply stale.
	other, sub := e.requester()
	pend := e.ask(other, e.a1, "1h", "")
	e.seedElevation(e.a1, sub, time.Now().UTC().Add(time.Hour))
	_ = e.call(e.adminA1, http.MethodPost, requestPath(pend.Pending.ID, "approve"), "")
	if s := e.changeState(pend.Pending.ID); s != types.GovernanceChangeStale {
		t.Errorf("an apply over a live row = %s, want stale", s)
	}
}

func TestGovernanceRequestsHiddenFromOthers(t *testing.T) {
	e := newElevEnv(t)
	got := e.ask(e.req, e.a1, "1h", "")
	third := uaSession(t, "sub-el-third", []string{elevRequesters}, false)
	for _, verb := range []string{"approve", "reject"} {
		if w := e.call(third, http.MethodPost, requestPath(got.Pending.ID, verb), ""); w.Code != http.StatusNotFound || wireReason(t, w) != reasonGovernanceChangeNotFound {
			t.Errorf("a third member %s = %d %s, want 404", verb, w.Code, w.Body)
		}
	}
	unknown := e.call(third, http.MethodPost, requestPath(uuid.New(), "approve"), "")
	seen := e.call(third, http.MethodPost, requestPath(got.Pending.ID, "approve"), "")
	if unknown.Body.String() != seen.Body.String() {
		t.Errorf("an unknown id says %s, a hidden one %s: want one answer", unknown.Body, seen.Body)
	}
	// A held change of another kind is not a request.
	held := e.heldOn(e.a2)
	if w := e.call(e.alice, http.MethodPost, requestPath(held.ID, "approve"), ""); w.Code != http.StatusNotFound {
		t.Errorf("a membership change through the request route = %d %s, want 404", w.Code, w.Body)
	}
	if l := e.list(third); len(l.Requests) != 0 {
		t.Errorf("a third member's list = %+v, want empty", l.Requests)
	}
	if l := e.list(e.alice); len(l.Requests) != 1 || !l.Requests[0].Decidable || l.Requests[0].Mine {
		t.Errorf("a security admin's list = %+v, want the request, decidable", l.Requests)
	}
}

func TestGovernanceRequestsListKeepsOneDay(t *testing.T) {
	e := newElevEnv(t)
	got := e.ask(e.req, e.a1, "1h", "")
	if w := e.call(e.req, http.MethodPost, requestPath(got.Pending.ID, "reject"), ""); w.Code != http.StatusOK {
		t.Fatalf("withdraw = %d %s", w.Code, w.Body)
	}
	if l := e.list(e.req); len(l.Requests) != 1 {
		t.Errorf("a request withdrawn now is listed %d times, want once", len(l.Requests))
	}
	if _, err := e.pool.Exec(context.Background(), `UPDATE governance_changes SET decided_at = now() - interval '25 hours' WHERE id = $1`, got.Pending.ID); err != nil {
		t.Fatal(err)
	}
	if l := e.list(e.req); len(l.Requests) != 0 {
		t.Errorf("a request decided 25 hours ago is listed: %+v", l.Requests)
	}
}
