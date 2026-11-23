// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The approval counts over Postgres: what GET reads and reports, the PUT's refusals and who it holds, and the
// approval of a held edit. Guarded by WARDYN_TEST_PG (throwawayPGPool): skipped cleanly when unset, must PASS
// when set. People are counted from their sign-in records, with the role mappings newApEnv seeds.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// counts is GET /governance/approval-counts as c, decoded.
func (e *apEnv) counts(c *http.Cookie) approvalCountsView {
	e.t.Helper()
	return decodeCounts(e.t, e.call(c, http.MethodGet, approvalCountsPath, ""))
}

func decodeCounts(t *testing.T, w *httptest.ResponseRecorder) approvalCountsView {
	t.Helper()
	var v approvalCountsView
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &v) != nil {
		t.Fatalf("approval counts = %d %s, want 200 with the counts", w.Code, w.Body)
	}
	return v
}

// kindRow is one kind's row of the page.
func kindRow(t *testing.T, v approvalCountsView, kind string) approvalCountsKind {
	t.Helper()
	for _, k := range v.Kinds {
		if k.Kind == kind {
			return k
		}
	}
	t.Fatalf("kind %s is not listed", kind)
	return approvalCountsKind{}
}

// people records n people with the role group, named prefix-1 to prefix-n, each with their own email.
func (e *apEnv) people(prefix, group string, n int) {
	e.t.Helper()
	for i := 1; i <= n; i++ {
		sub := prefix + "-" + string(rune('0'+i))
		e.record(sub, sub+"@corp.example", []string{group}, nil, false)
	}
}

// countRows is how many rows the counts hold.
func (e *apEnv) countRows() int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM governance_approval_counts`).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// TestPG_ApprovalCounts_UnsetMinimumOnAFreshDeployment: with no minimum and no stored rows, the minimum reads 0
// and every count 1; the kinds are the derived set, assistant_settings among them as a super admin's.
func TestPG_ApprovalCounts_UnsetMinimumOnAFreshDeployment(t *testing.T) {
	e := newApEnv(t, 0)
	v := e.counts(e.alice)
	if v.Minimum != 0 || v.Highest != 1 || v.Default != (approvalCountsRule{Required: 1}) {
		t.Errorf("minimum %d highest %d default %+v, want 0, 1 and {1 false}", v.Minimum, v.Highest, v.Default)
	}
	var got []string
	for _, k := range v.Kinds {
		got = append(got, k.Kind)
		if k.Own || k.Required != 1 {
			t.Errorf("%s = %+v, want required 1 from the default", k.Kind, k)
		}
	}
	if !slices.Equal(got, approvalCountKinds()) {
		t.Errorf("kinds = %v, want %v", got, approvalCountKinds())
	}
	if a := kindRow(t, v, govKindAssistantSettings).Approvers; a != approversSuperAdmin {
		t.Errorf("assistant_settings approvers = %q, want super_admin", a)
	}
}

// countsReadCounter counts what one GET reads: the sign-in records, the unit-admin grants and the unit scopes.
type countsReadCounter struct {
	store.PG
	records, grants, scopes *atomic.Int64
}

func (c countsReadCounter) UnitScope(ctx context.Context, unit uuid.UUID, users, groups []string, strict bool) (bool, error) {
	c.scopes.Add(1)
	return c.PG.UnitScope(ctx, unit, users, groups, strict)
}

func (c countsReadCounter) DryRunGovernance(ctx context.Context, fn func(q store.Querier) error) error {
	return c.PG.DryRunGovernance(ctx, func(q store.Querier) error { return fn(countsQuerier{Querier: q, c: c}) })
}

type countsQuerier struct {
	store.Querier
	c countsReadCounter
}

func (q countsQuerier) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	switch {
	case strings.Contains(sql, "FROM principal_groups"):
		q.c.records.Add(1)
	case strings.Contains(sql, "FROM governance_unit_admins"):
		q.c.grants.Add(1)
	}
	return q.Querier.Query(ctx, sql, args...)
}

func newCounter() countsReadCounter {
	return countsReadCounter{records: &atomic.Int64{}, grants: &atomic.Int64{}, scopes: &atomic.Int64{}}
}

// TestPG_ApprovalCounts_OneGETReadsRecordsAndGrantsOnce: whatever the number of kinds and units, one GET reads
// the sign-in records once and the unit-admin grants once.
func TestPG_ApprovalCounts_OneGETReadsRecordsAndGrantsOnce(t *testing.T) {
	c := newCounter()
	e := newApEnv(t, 2, func(cfg *Config) { c.PG = cfg.Store.(store.PG); cfg.Store = c })
	e.seedPeople()
	c.records.Store(0)
	c.grants.Store(0)
	e.counts(e.alice)
	if n := c.records.Load(); n != 1 {
		t.Errorf("sign-in record reads = %d, want 1", n)
	}
	if n := c.grants.Load(); n != 1 {
		t.Errorf("unit-admin grant reads = %d, want 1", n)
	}
}

// TestPG_ApprovalCounts_ScopeReadsCountGrantHolders: 40 candidates who are not security admins, 3 of them grant
// holders, and 2 units: one GET reads the unit scope at most 2 × 2 × 3 = 12 times, and never with no grant.
func TestPG_ApprovalCounts_ScopeReadsCountGrantHolders(t *testing.T) {
	c := newCounter()
	g := newGovEnv(t, minApprovals(2), func(cfg *Config) { c.PG = cfg.Store.(store.PG); cfg.Store = c })
	e := &apEnv{&unitAdminEnv{govEnv: g}}
	for value, role := range map[string]string{apGroupUsers: oidc.RoleUser, apGroupSec: oidc.RoleSecurityAdmin, apGroupRoot: oidc.RoleAdmin} {
		if _, err := g.pg.UpsertRoleMapping(context.Background(), types.RoleMapping{Value: value, Role: role, CreatedBy: "seed"}); err != nil {
			t.Fatal(err)
		}
	}
	unit := treeChild(t, g, uaUnitBody("counts-a", store.DefaultUnitID, uaDomains("pypi.org"), nil))
	units, err := g.pg.ListGovernanceUnits(context.Background())
	if err != nil || len(units) != 2 {
		t.Fatalf("units = %d (%v), want 2", len(units), err)
	}
	for i := range 40 {
		sub := "sub-counts-" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		e.record(sub, sub+"@corp.example", []string{apGroupUsers}, nil, false)
	}
	c.scopes.Store(0)
	e.counts(g.alice)
	if n := c.scopes.Load(); n != 0 {
		t.Errorf("unit scope reads with no grant = %d, want 0", n)
	}
	for _, sub := range []string{"sub-counts-aa", "sub-counts-ba", "sub-counts-ca"} {
		if _, err := store.GrantUnitAdminQ(context.Background(), g.pool, types.GovernanceUnitAdmin{UnitID: unit.ID, SubjectType: types.CapabilitySubjectUser, Subject: sub, GrantedBy: "seed"}); err != nil {
			t.Fatal(err)
		}
	}
	c.scopes.Store(0)
	e.counts(g.alice)
	if n := c.scopes.Load(); n > 12 || n == 0 {
		t.Errorf("unit scope reads = %d, want 1 to 12 (two for each unit and holder)", n)
	}
}

// TestPG_ApprovalCounts_OwnRowAndDefault: a kind with its own row reads it; every other kind follows the default
// row; highest is the largest of them and the minimum.
func TestPG_ApprovalCounts_OwnRowAndDefault(t *testing.T) {
	e := newApEnv(t, 2)
	e.requireCounts(govKindRoleMapping, 3, true)
	e.requireCounts(store.GovernanceApprovalDefaultKind, 2, false)
	v := e.counts(e.alice)
	if v.Highest != 3 || v.Default != (approvalCountsRule{Required: 2}) {
		t.Errorf("highest %d default %+v, want 3 and {2 false}", v.Highest, v.Default)
	}
	if k := kindRow(t, v, govKindRoleMapping); !k.Own || k.Required != 3 || !k.RequireSecurity {
		t.Errorf("role_mapping = %+v, want its own row 3 with the security rule", k)
	}
	if k := kindRow(t, v, govKindEgressBaseline); k.Own || k.Required != 2 || k.RequireSecurity {
		t.Errorf("egress_baseline = %+v, want the default", k)
	}
}

// TestPG_ApprovalCounts_UnitKindNamesItsSmallestPool: a unit kind reports the unit with the fewest people who can
// approve it, by name.
func TestPG_ApprovalCounts_UnitKindNamesItsSmallestPool(t *testing.T) {
	e := newApEnv(t, 2)
	e.seedPeople()
	want := approvalCountsPool{Count: -1}
	poolOn(t, e.srv, func(in *poolInputs) {
		units, err := e.pg.ListGovernanceUnits(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, u := range units {
			res, err := e.srv.poolForKind(in, govKindMembership, u.ID)
			if err != nil {
				t.Fatal(err)
			}
			if want.Count < 0 || len(res.Eligible) < want.Count {
				want = approvalCountsPool{Count: len(res.Eligible), UnitName: u.Name}
			}
		}
	})
	got := kindRow(t, e.counts(e.alice), govKindMembership).Pool
	if got.Count != want.Count || got.UnitName != want.UnitName || got.UnitName == "" {
		t.Errorf("membership pool = %+v, want %d people in %q", got, want.Count, want.UnitName)
	}
}

// TestPG_ApprovalCounts_StatusAtEachBoundary: three security admins against a count of 2, 3 and 4; and a
// truncated record makes a short count cant_confirm, but leaves an ample one ok.
func TestPG_ApprovalCounts_StatusAtEachBoundary(t *testing.T) {
	e := newApEnv(t, 2)
	e.seedPeople()
	for required, want := range map[int]string{2: poolStatusOK, 3: poolStatusExact, 4: poolStatusShort} {
		e.requireCounts(govKindEgressBaseline, required, false)
		if p := kindRow(t, e.counts(e.alice), govKindEgressBaseline).Pool; p.Status != want || p.Count != 3 {
			t.Errorf("required %d: pool %+v, want %s with 3", required, p, want)
		}
	}
	e.record("sub-counts-partial", "partial@corp.example", []string{apGroupUsers}, nil, true)
	e.requireCounts(govKindEgressBaseline, 5, false)
	if p := kindRow(t, e.counts(e.alice), govKindEgressBaseline).Pool; p.Status != poolStatusCantConfirm || p.Why != uncountableUnconfirmed {
		t.Errorf("short with a truncated record: %+v, want cant_confirm unconfirmed_people", p)
	}
	e.requireCounts(govKindEgressBaseline, 2, false)
	if p := kindRow(t, e.counts(e.alice), govKindEgressBaseline).Pool; p.Status != poolStatusOK {
		t.Errorf("ample with a truncated record: %+v, want ok", p)
	}
}

// TestPG_ApprovalCounts_DistinctHumans: two security-tier principals with one email in different case are one
// person, so a count of 2 is short, not exact.
func TestPG_ApprovalCounts_DistinctHumans(t *testing.T) {
	e := newApEnv(t, 2)
	e.record("sub-counts-x1", "X@corp.example", []string{apGroupSec}, nil, false)
	e.record("sub-counts-x2", "x@corp.example", []string{apGroupSec}, nil, false)
	e.requireCounts(govKindEgressBaseline, 2, false)
	if p := kindRow(t, e.counts(e.alice), govKindEgressBaseline).Pool; p.Status != poolStatusShort || p.Count != 1 {
		t.Errorf("pool = %+v, want short with 1 person", p)
	}
}

// superEnv is four security admins who are not super admins and two super admins, all signed in.
func superEnv(t *testing.T) *apEnv {
	e := newApEnv(t, 2)
	e.people("sub-counts-sec", apGroupSec, 4)
	e.people("sub-counts-root", apGroupRoot, 2)
	return e
}

// TestPG_ApprovalCounts_RunnerChangesCountSuperAdmins: a use policy that names a runner is a super admin's to
// decide, so the page counts the two super admins, not the six security-tier people, and names their tier.
func TestPG_ApprovalCounts_RunnerChangesCountSuperAdmins(t *testing.T) {
	e := superEnv(t)
	e.requireCounts(govKindRunnerPoolUsePolicy, 3, false)
	k := kindRow(t, e.counts(e.alice), govKindRunnerPoolUsePolicy)
	if k.Pool.Status != poolStatusShort || k.Pool.Count != 2 || k.Approvers != approversSuperAdmin {
		t.Errorf("runner_pool_use_policy = %+v, want short with 2 and super_admin", k)
	}
	// A super admin with no email on their sign-in record is still one more person.
	e.record("sub-counts-root-noemail", "", []string{apGroupRoot}, nil, false)
	if k := kindRow(t, e.counts(e.alice), govKindRunnerPoolUsePolicy); k.Pool.Count != 3 || k.Approvers != approversSuperAdmin {
		t.Errorf("with an email-less super admin: %+v, want 3 super admins", k)
	}
}

// TestPG_ApprovalCounts_PutRefusesARaisedPoolThatIsShort: setting the use policy's count to 3 with two super
// admins is a 422 for a person and for the admin token, though six security-tier people would pass the table
// tier; at 2 it is saved.
func TestPG_ApprovalCounts_PutRefusesARaisedPoolThatIsShort(t *testing.T) {
	e := superEnv(t)
	body := func(n int) string {
		return `{"default":{"required":2},"kinds":[{"kind":"runner_pool_use_policy","required":` + string(rune('0'+n)) + `}]}`
	}
	for who, w := range map[string]*httptest.ResponseRecorder{
		"a security admin": e.call(e.alice, http.MethodPut, approvalCountsPath, body(3)),
		"the admin token":  e.admin(http.MethodPut, approvalCountsPath, body(3)),
	} {
		if w.Code != http.StatusUnprocessableEntity || wireReason(t, w) != reasonGovernanceApprovalCountsUnreachable ||
			!strings.Contains(w.Body.String(), "Runner pool use policy needs 3 and only 2 people can approve its changes that need a super admin.") {
			t.Errorf("%s: %d %s, want 422 naming the raised case", who, w.Code, w.Body)
		}
	}
	if n := e.countRows(); n != 0 {
		t.Errorf("%d rows written by refused saves", n)
	}
	if w := e.admin(http.MethodPut, approvalCountsPath, body(2)); w.Code != http.StatusOK {
		t.Errorf("at 2: %d %s, want 200", w.Code, w.Body)
	}
}

// TestPG_ApprovalCounts_PutRefusesAUnitKindOnlySuperAdminsCouldReach: a unit change that sets a host path is a
// super admin's, so a count of 3 with two super admins is refused, whatever the unit admins.
func TestPG_ApprovalCounts_PutRefusesAUnitKindOnlySuperAdminsCouldReach(t *testing.T) {
	e := newApEnv(t, 2)
	e.people("sub-counts-root", apGroupRoot, 2)
	for i := 1; i <= 5; i++ {
		sub := "sub-counts-ua-" + string(rune('0'+i))
		e.record(sub, sub+"@corp.example", []string{apGroupUsers}, nil, false)
		e.grant(e.a, types.CapabilitySubjectUser, sub)
		e.grant(e.b, types.CapabilitySubjectUser, sub)
	}
	w := e.admin(http.MethodPut, approvalCountsPath, `{"default":{"required":2},"kinds":[{"kind":"governance_unit","required":3}]}`)
	if w.Code != http.StatusUnprocessableEntity || wireReason(t, w) != reasonGovernanceApprovalCountsUnreachable ||
		!strings.Contains(w.Body.String(), "Governance unit needs 3 and only 2 people can approve its changes that need a super admin.") {
		t.Errorf("governance_unit at 3 = %d %s, want 422 naming the raised case", w.Code, w.Body)
	}
}

// TestPG_ApprovalCounts_PutRefusesAnUnreachableCount: a count above the people who can approve its kind is a
// 422, also for the admin token, and nothing is written.
func TestPG_ApprovalCounts_PutRefusesAnUnreachableCount(t *testing.T) {
	e := newApEnv(t, 2)
	e.seedPeople()
	e.people("sub-counts-sec", apGroupSec, 2) // five security admins, so edits to the counts stay possible
	const body = `{"default":{"required":2},"kinds":[{"kind":"role_mapping","required":2}]}`
	for who, w := range map[string]*httptest.ResponseRecorder{
		"a security admin": e.call(e.alice, http.MethodPut, approvalCountsPath, body),
		"the admin token":  e.admin(http.MethodPut, approvalCountsPath, body),
	} {
		if w.Code != http.StatusUnprocessableEntity || wireReason(t, w) != reasonGovernanceApprovalCountsUnreachable {
			t.Errorf("%s: %d %s, want 422 (no super admin has signed in)", who, w.Code, w.Body)
		}
	}
	if n := e.countRows(); n != 0 {
		t.Errorf("%d rows written", n)
	}
}

// TestPG_ApprovalCounts_EditsToTheCountsStayPossible: a new highest count of 3 with three security admins is a
// 422 (two besides the proposer), also for the admin token; with a fourth it is saved. Two principals at one
// email are one person, so four principals that are three people are refused as well.
func TestPG_ApprovalCounts_EditsToTheCountsStayPossible(t *testing.T) {
	const body = `{"default":{"required":2},"kinds":[{"kind":"egress_baseline","required":3}]}`
	e := newApEnv(t, 2)
	e.seedPeople()
	for _, w := range []*httptest.ResponseRecorder{
		e.call(e.alice, http.MethodPut, approvalCountsPath, body),
		e.admin(http.MethodPut, approvalCountsPath, body),
	} {
		if w.Code != http.StatusUnprocessableEntity ||
			!strings.Contains(w.Body.String(), "Edits to these counts would need 3 approvals and only 2 security admins besides the proposer could give them.") {
			t.Errorf("three security admins: %d %s, want 422", w.Code, w.Body)
		}
	}
	e.record("sub-counts-alias", "Carol@Corp.Example", []string{apGroupSec}, nil, false) // carol again
	if w := e.admin(http.MethodPut, approvalCountsPath, body); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("four principals, three people: %d %s, want 422", w.Code, w.Body)
	}
	e.record("sub-counts-dave", "dave@corp.example", []string{apGroupSec}, nil, false)
	if w := e.admin(http.MethodPut, approvalCountsPath, body); w.Code != http.StatusOK {
		t.Errorf("four people: %d %s, want 200", w.Code, w.Body)
	}
}

// TestPG_ApprovalCounts_APersonsEditIsHeld: with the switch off, a security admin's edit is held (202) needing
// the highest count in force at proposal; with the switch on it is held too.
func TestPG_ApprovalCounts_APersonsEditIsHeld(t *testing.T) {
	e := newApEnv(t, 2)
	e.seedPeople()
	e.requireCounts(govKindRoleMapping, 3, false)
	const body = `{"default":{"required":2},"kinds":[]}`
	ch := e.pending(e.call(e.alice, http.MethodPut, approvalCountsPath, body))
	if ch.TargetKind != govKindApprovalCounts || ch.RequiredApprovals != 3 || ch.RequireSecurity {
		t.Errorf("held %s needing %d (security %v), want governance_approval_counts needing 3", ch.TargetKind, ch.RequiredApprovals, ch.RequireSecurity)
	}
	if w := e.call(e.carol, http.MethodPost, rejectPath(ch.ID), `{"reason":"not now"}`); w.Code != http.StatusOK {
		t.Fatalf("reject = %d %s", w.Code, w.Body)
	}
	t.Setenv(envGovernanceSecondHuman, "true")
	e.pending(e.call(e.alice, http.MethodPut, approvalCountsPath, body))
	if n := e.countRows(); n != 1 {
		t.Errorf("%d rows, want only the seeded one", n)
	}
}

// TestPG_ApprovalCounts_AdminTokenWritesDirectly: the admin token's save is a 200 with the page, an audit row
// and the bypass row.
func TestPG_ApprovalCounts_AdminTokenWritesDirectly(t *testing.T) {
	e := newApEnv(t, 2)
	e.seedPeople()
	e.people("sub-counts-sec", apGroupSec, 1)
	w := e.admin(http.MethodPut, approvalCountsPath, `{"default":{"required":2},"kinds":[{"kind":"egress_baseline","required":3,"require_security":true}]}`)
	v := decodeCounts(t, w)
	if k := kindRow(t, v, govKindEgressBaseline); !k.Own || k.Required != 3 || !k.RequireSecurity || v.Highest != 3 {
		t.Errorf("egress_baseline = %+v, highest %d, want its own 3 with the security rule", k, v.Highest)
	}
	if n := len(e.audits(approvalCountsWriteAction)); n != 1 {
		t.Errorf("%d %s rows, want 1", n, approvalCountsWriteAction)
	}
	if n := len(e.audits("governance.change.bypass")); n != 1 {
		t.Errorf("%d bypass rows, want 1", n)
	}
}

// TestPG_ApprovalCounts_ApprovalWritesTheRows: an approval of a held edit writes the rows; an edit whose rows
// changed in between is stale.
func TestPG_ApprovalCounts_ApprovalWritesTheRows(t *testing.T) {
	e := newApEnv(t, 1)
	e.seedPeople()
	ch := e.pending(e.call(e.alice, http.MethodPut, approvalCountsPath, `{"default":{"required":2},"kinds":[{"kind":"egress_baseline","required":2}]}`))
	e.approve(e.bob, ch.ID, http.StatusOK)
	if s := e.changeState(ch.ID); s != types.GovernanceChangeApplied || e.countRows() != 2 {
		t.Errorf("state %s with %d rows, want applied with 2", s, e.countRows())
	}
	stale := e.pending(e.call(e.alice, http.MethodPut, approvalCountsPath, `{"default":{"required":1},"kinds":[]}`))
	if w := e.admin(http.MethodPut, approvalCountsPath, `{"default":{"required":2},"kinds":[]}`); w.Code != http.StatusOK {
		t.Fatalf("direct write = %d %s", w.Code, w.Body)
	}
	if w := e.call(e.carol, http.MethodPost, approvePath(stale.ID), ""); w.Code != http.StatusConflict || wireReason(t, w) != reasonGovernanceChangeStale {
		t.Errorf("approve a stale edit = %d %s, want 409 %s", w.Code, w.Body, reasonGovernanceChangeStale)
	}
}

// TestPG_ApprovalCounts_ApplyRechecksTheMinimum: a held edit proposed with counts of 2, then the minimum raised to
// 3 before its last approval: the apply refuses it 400 governance_approval_counts_invalid, nothing is written and
// the edit stays pending.
func TestPG_ApprovalCounts_ApplyRechecksTheMinimum(t *testing.T) {
	e := newApEnv(t, 2)
	e.seedPeople()
	e.record("sub-counts-dave", "dave@corp.example", []string{apGroupSec}, nil, false)
	ch := e.pending(e.call(e.alice, http.MethodPut, approvalCountsPath, `{"default":{"required":2},"kinds":[{"kind":"egress_baseline","required":2}]}`))
	e.srv.cfg.GovernanceMinApprovals = 3
	e.approve(e.bob, ch.ID, http.StatusOK)
	e.approve(e.carol, ch.ID, http.StatusOK)
	dave := ssoSession(t, "sub-counts-dave", "dave@corp.example", oidc.RoleSecurityAdmin)
	if w := e.call(dave, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusBadRequest || wireReason(t, w) != reasonGovernanceApprovalCountsInvalid {
		t.Errorf("last approval = %d %s, want 400 %s", w.Code, w.Body, reasonGovernanceApprovalCountsInvalid)
	}
	if s := e.changeState(ch.ID); s != types.GovernanceChangePending || e.countRows() != 0 {
		t.Errorf("state %s with %d rows, want pending with none", s, e.countRows())
	}
}

// TestPG_ApprovalCounts_ApprovalDoesNotCountPeopleAgain: a held edit whose pool shrank below its count after the
// proposal (a super admin deactivated) is applied by its last approval: the approvers are deciding.
func TestPG_ApprovalCounts_ApprovalDoesNotCountPeopleAgain(t *testing.T) {
	e := newApEnv(t, 2)
	e.seedPeople()
	e.people("sub-counts-root", apGroupRoot, 2)
	ch := e.pending(e.call(e.alice, http.MethodPut, approvalCountsPath, `{"default":{"required":2},"kinds":[{"kind":"role_mapping","required":2}]}`))
	e.deactivate("sub-counts-root-2")
	e.approve(e.bob, ch.ID, http.StatusOK)
	e.approve(e.carol, ch.ID, http.StatusOK)
	if s := e.changeState(ch.ID); s != types.GovernanceChangeApplied || e.countRows() != 2 {
		t.Errorf("state %s with %d rows, want applied with 2", s, e.countRows())
	}
}
