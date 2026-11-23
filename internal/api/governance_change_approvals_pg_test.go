// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// Approvals from more than one person on a held governance change: the minimum, the record each approver
// is judged on, what a recorded approval leaves pending, the last approval's re-check, and the take-back.
// Guarded by WARDYN_TEST_PG (throwawayPGPool): skipped cleanly when unset, must PASS when set. The test
// server's authenticator has no role map, so every signed-in record derives the admin role and the request
// tier (security admin) is the lower of the two.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func approvalPath(id uuid.UUID) string {
	return "/api/v1/governance/changes/" + id.String() + "/approval"
}

// minApprovals is the configuration mutation that sets WARDYN_GOVERNANCE_MIN_APPROVALS.
func minApprovals(n int) func(*Config) { return func(c *Config) { c.GovernanceMinApprovals = n } }

// signIn records the latest sign-in of a person, as the identity gate does at sign-in.
func (e *govEnv) signIn(sub, email string) {
	e.t.Helper()
	if err := e.pg.PutPrincipalSignIn(context.Background(), sub, email, []string{}, nil, false); err != nil {
		e.t.Fatalf("record sign-in of %s: %v", sub, err)
	}
}

// held is an update of profile p proposed by alice, held for approval.
func (e *govEnv) held() types.GovernanceChange {
	e.t.Helper()
	name := "approvals-" + uuid.NewString()[:8]
	p := e.seedProfile(name, "pypi.org")
	return e.pending(e.call(e.alice, http.MethodPut, "/api/v1/governance/units/"+p.ID.String(), profileBody(name, "pypi.org", "github.com")))
}

// read is GET /governance/changes/{id} as bob, decoded to a generic map.
func (e *govEnv) read(id uuid.UUID) map[string]any {
	e.t.Helper()
	w := e.call(e.bob, http.MethodGet, "/api/v1/governance/changes/"+id.String(), "")
	if w.Code != http.StatusOK {
		e.t.Fatalf("read change %s = %d %s", id, w.Code, w.Body)
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		e.t.Fatalf("decode change: %v", err)
	}
	return out
}

// approvalsOf is the approvals block of a decoded change, nil when it has none.
func approvalsOf(m map[string]any) map[string]any {
	a, _ := m["approvals"].(map[string]any)
	return a
}

// TestPG_Approvals_UnsetMinimumIsToday: with no minimum, an approval applies on the one decider's word with no
// sign-in record, no approval row is written, the reject reason stays optional, and neither GET nor the list
// carries an approvals block.
func TestPG_Approvals_UnsetMinimumIsToday(t *testing.T) {
	e := newGovEnv(t)
	ch := e.held()
	if a := approvalsOf(e.read(ch.ID)); a != nil {
		t.Errorf("GET carries an approvals block with the minimum unset: %v", a)
	}
	if w := e.call(e.bob, http.MethodGet, "/api/v1/governance/changes", ""); w.Code != http.StatusOK || strings.Contains(w.Body.String(), `"approvals"`) {
		t.Errorf("the list = %d %s, want no approvals key", w.Code, w.Body)
	}
	if w := e.call(e.bob, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusOK {
		t.Fatalf("approve = %d %s, want 200", w.Code, w.Body)
	}
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM governance_change_approvals`).Scan(&n); err != nil || n != 0 {
		t.Errorf("approval rows = %d (%v), want none", n, err)
	}
	other := e.held()
	if w := e.call(e.bob, http.MethodPost, rejectPath(other.ID), ""); w.Code != http.StatusOK {
		t.Errorf("a reject with no reason = %d %s, want 200 with the minimum unset", w.Code, w.Body)
	}
}

// TestPG_Approvals_TwoPeopleApply: with a minimum of two, the first approval is recorded and the change stays
// pending, the same person cannot count twice, the proposer cannot approve, and the second person's approval
// applies it with the keys the approve row gains.
func TestPG_Approvals_TwoPeopleApply(t *testing.T) {
	e := newGovEnv(t, minApprovals(2))
	ch := e.held()
	if w := e.call(e.bob, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusForbidden {
		t.Fatalf("approve with no sign-in record = %d %s, want 403", w.Code, w.Body)
	}
	e.signIn(govSubBob, "bob@corp.example")
	e.signIn(govSubCarol, "carol@corp.example")

	w := e.call(e.bob, http.MethodPost, approvePath(ch.ID), `{"reason":"checked the diff"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("first approval = %d %s, want 200", w.Code, w.Body)
	}
	if got := e.changeState(ch.ID); got != types.GovernanceChangePending {
		t.Fatalf("state after one of two = %q, want pending", got)
	}
	a := approvalsOf(e.read(ch.ID))
	if a == nil || a["have"] != float64(1) || a["required"] != float64(2) {
		t.Fatalf("approvals block = %v, want have 1 of 2", a)
	}
	if n := len(e.audits("governance.change.approval")); n != 1 {
		t.Errorf("%d governance.change.approval rows, want 1", n)
	}
	if n := len(e.audits("governance.change.approve")); n != 0 {
		t.Errorf("%d governance.change.approve rows after a recorded approval, want 0", n)
	}
	if w := e.call(e.bob, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusConflict || wireReason(t, w) != reasonGovernanceChangeAlreadyApproved {
		t.Errorf("a repeat approval = %d %s, want 409 %s", w.Code, w.Body, reasonGovernanceChangeAlreadyApproved)
	}
	if w := e.call(e.alice, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusForbidden {
		t.Errorf("the proposer approving = %d %s, want 403", w.Code, w.Body)
	}

	if w := e.call(e.carol, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusOK {
		t.Fatalf("second approval = %d %s, want 200", w.Code, w.Body)
	}
	if got := e.changeState(ch.ID); got != types.GovernanceChangeApplied {
		t.Fatalf("state after two of two = %q, want applied", got)
	}
	rows := e.audits("governance.change.approve")
	if len(rows) != 1 {
		t.Fatalf("%d governance.change.approve rows, want 1", len(rows))
	}
	d := auditData(t, rows[0])
	if d["have"] != float64(2) || d["required"] != float64(2) {
		t.Errorf("approve data = %v, want have 2 and required 2", d)
	}
}

// TestPG_Approvals_TakeBack: a person takes their approval back, once; the second take-back is a 404.
func TestPG_Approvals_TakeBack(t *testing.T) {
	e := newGovEnv(t, minApprovals(2))
	ch := e.held()
	e.signIn(govSubBob, "bob@corp.example")
	if w := e.call(e.bob, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusOK {
		t.Fatalf("approve = %d %s, want 200", w.Code, w.Body)
	}
	if w := e.call(e.bob, http.MethodDelete, approvalPath(ch.ID), ""); w.Code != http.StatusOK {
		t.Fatalf("take back = %d %s, want 200", w.Code, w.Body)
	}
	if a := approvalsOf(e.read(ch.ID)); a == nil || a["have"] != float64(0) {
		t.Errorf("approvals after the take-back = %v, want have 0", a)
	}
	if w := e.call(e.bob, http.MethodDelete, approvalPath(ch.ID), ""); w.Code != http.StatusNotFound || wireReason(t, w) != reasonGovernanceChangeApprovalNotFound {
		t.Errorf("a second take-back = %d %s, want 404 %s", w.Code, w.Body, reasonGovernanceChangeApprovalNotFound)
	}
	if n := len(e.audits("governance.change.retract")); n != 1 {
		t.Errorf("%d governance.change.retract rows, want 1", n)
	}
}

// TestPG_Approvals_RejectNeedsAReason: under the rules a reject by someone other than the proposer needs a
// reason; the proposer withdraws without one.
func TestPG_Approvals_RejectNeedsAReason(t *testing.T) {
	e := newGovEnv(t, minApprovals(2))
	ch := e.held()
	if w := e.call(e.bob, http.MethodPost, rejectPath(ch.ID), ""); w.Code != http.StatusBadRequest || wireReason(t, w) != reasonGovernanceChangeReasonRequired {
		t.Errorf("a reject with no reason = %d %s, want 400 %s", w.Code, w.Body, reasonGovernanceChangeReasonRequired)
	}
	if w := e.call(e.alice, http.MethodPost, rejectPath(ch.ID), ""); w.Code != http.StatusOK {
		t.Errorf("the proposer withdrawing = %d %s, want 200 with no reason", w.Code, w.Body)
	}
}

// TestPG_Approvals_AnEarlierApprovalThatCannotBeConfirmedIsDropped: the last approval re-checks the earlier one
// on the person's latest record; with no record left it drops as cant_confirm, so the change stays pending.
func TestPG_Approvals_AnEarlierApprovalThatCannotBeConfirmedIsDropped(t *testing.T) {
	e := newGovEnv(t, minApprovals(2))
	ch := e.held()
	e.signIn(govSubBob, "bob@corp.example")
	e.signIn(govSubCarol, "carol@corp.example")
	if w := e.call(e.bob, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusOK {
		t.Fatalf("approve = %d %s, want 200", w.Code, w.Body)
	}
	if _, err := e.pool.Exec(context.Background(), `DELETE FROM principal_groups WHERE principal = $1`, govSubBob); err != nil {
		t.Fatalf("remove the record: %v", err)
	}
	if w := e.call(e.carol, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusOK {
		t.Fatalf("second approval = %d %s, want 200", w.Code, w.Body)
	}
	if got := e.changeState(ch.ID); got != types.GovernanceChangePending {
		t.Fatalf("state = %q, want pending: the earlier approval no longer counts", got)
	}
	var why string
	if err := e.pool.QueryRow(context.Background(), `SELECT dropped FROM governance_change_approvals WHERE approver = $1`, govSubBob).Scan(&why); err != nil || why != droppedCannotConfirmed {
		t.Errorf("bob's approval dropped = %q (%v), want %s", why, err, droppedCannotConfirmed)
	}
}

// ── the tier rules, the pool and the re-check (hosted) ───────────────────────────────────────────────────────────

// Groups the role mappings below turn into roles. With a role map in force a record derives only what its
// groups or roles claim name, so a person's tier is set by the record they last signed in with.
const (
	apGroupUsers = "grp-users"
	apGroupSec   = "grp-sec"
	apGroupRoot  = "grp-root"
)

// apEnv is the unit-admin tree with the approval rules on and a role map, so a record can derive a unit
// admin's user role, a security admin's, or a super admin's.
type apEnv struct{ *unitAdminEnv }

func newApEnv(t *testing.T, minimum int, mutate ...func(*Config)) *apEnv {
	t.Helper()
	e := &apEnv{newUnitAdminEnv(t, false, append([]func(*Config){minApprovals(minimum)}, mutate...)...)}
	for value, role := range map[string]string{apGroupUsers: oidc.RoleUser, apGroupSec: oidc.RoleSecurityAdmin, apGroupRoot: oidc.RoleAdmin} {
		if _, err := e.pg.UpsertRoleMapping(context.Background(), types.RoleMapping{Value: value, Role: role, CreatedBy: "seed"}); err != nil {
			t.Fatalf("seed role mapping %q: %v", value, err)
		}
	}
	return e
}

// record is a person's latest sign-in record.
func (e *apEnv) record(sub, email string, groups, roles []string, truncated bool) {
	e.t.Helper()
	if groups == nil {
		groups = []string{}
	}
	if err := e.pg.PutPrincipalSignIn(context.Background(), sub, email, groups, roles, truncated); err != nil {
		e.t.Fatalf("record sign-in of %s: %v", sub, err)
	}
}

// requireCounts makes a kind's own count: required approvals, and whether one must be a security admin.
func (e *apEnv) requireCounts(kind string, required int, security bool) {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(),
		`INSERT INTO governance_approval_counts (target_kind, required, require_security) VALUES ($1,$2,$3)
		 ON CONFLICT (target_kind) DO UPDATE SET required = $2, require_security = $3`, kind, required, security); err != nil {
		e.t.Fatalf("seed the counts of %s: %v", kind, err)
	}
}

// approve is POST /approve by cookie c, with a status check.
func (e *apEnv) approve(c *http.Cookie, id uuid.UUID, want int) {
	e.t.Helper()
	if w := e.call(c, http.MethodPost, approvePath(id), ""); w.Code != want {
		e.t.Fatalf("approve %s = %d %s, want %d", id, w.Code, w.Body, want)
	}
}

// droppedOf is why an approver's row stopped counting, "" while it counts.
func (e *apEnv) droppedOf(id uuid.UUID, approver string) string {
	e.t.Helper()
	var why string
	if err := e.pool.QueryRow(context.Background(),
		`SELECT dropped FROM governance_change_approvals WHERE change_id = $1 AND approver = $2`, id, approver).Scan(&why); err != nil {
		e.t.Fatalf("read the approval of %s: %v", approver, err)
	}
	return why
}

// seedPeople records adminA, peerA and the security admins as the people their sessions say they are.
func (e *apEnv) seedPeople() {
	e.record(uaSubAdminA, uaSubAdminA+"@corp.example", []string{apGroupUsers}, nil, false)
	e.record(uaSubPeerA, uaSubPeerA+"@corp.example", []string{apGroupUsers}, nil, false)
	e.record(govSubAlice, "alice@corp.example", []string{apGroupSec}, nil, false)
	e.record(govSubBob, "bob@corp.example", []string{apGroupSec}, nil, false)
	e.record(govSubCarol, "carol@corp.example", []string{apGroupSec}, nil, false)
}

// TestPG_Approvals_RequireSecurity: the `require_security` table. The rule is judged on the last approval only;
// a unit admin and then a unit admin is refused 409 and writes nothing; a unit admin and then a security admin
// applies; a security admin who was since mapped to the user role but keeps a unit-admin grant counts as a unit
// admin; a caller whose record now says user is refused 403 whatever their older session says; a record
// that says security admin cannot raise a unit-admin request or a CLI token above the unit-admin tier.
func TestPG_Approvals_RequireSecurity(t *testing.T) {
	e := newApEnv(t, 1)
	e.requireCounts(govKindMembership, 2, true)
	e.seedPeople()
	rows := func(id uuid.UUID) int {
		var n int
		if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM governance_change_approvals WHERE change_id = $1`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// A unit admin, then a unit admin: the final approval is refused and nothing is written.
	ch := e.heldOn(e.a2)
	e.approve(e.adminA, ch.ID, http.StatusOK)
	if w := e.call(e.peerA, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusConflict || wireReason(t, w) != reasonGovernanceChangeSecurityApprover {
		t.Fatalf("unit admin then unit admin = %d %s, want 409 %s", w.Code, w.Body, reasonGovernanceChangeSecurityApprover)
	}
	if got, n := e.changeState(ch.ID), rows(ch.ID); got != types.GovernanceChangePending || n != 1 {
		t.Errorf("after the refusal: state %q with %d rows, want pending with the first approval only", got, n)
	}

	// A unit admin, then a security admin: applies.
	ch = e.heldOn(e.a2)
	e.approve(e.adminA, ch.ID, http.StatusOK)
	e.approve(e.alice, ch.ID, http.StatusOK)
	if got := e.changeState(ch.ID); got != types.GovernanceChangeApplied {
		t.Errorf("unit admin then security admin: state %q, want applied", got)
	}

	// A security admin approved, is since mapped to the user role, and keeps a unit-admin grant. Their row
	// counts as unit admin, so the rule refuses.
	e.grant(e.a, types.CapabilitySubjectUser, govSubBob)
	ch = e.heldOn(e.a2)
	e.approve(e.bob, ch.ID, http.StatusOK)
	e.record(govSubBob, "bob@corp.example", []string{apGroupUsers}, nil, false)
	if w := e.call(e.adminA, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusConflict || wireReason(t, w) != reasonGovernanceChangeSecurityApprover {
		t.Fatalf("final after the security admin was remapped = %d %s, want 409 %s", w.Code, w.Body, reasonGovernanceChangeSecurityApprover)
	}
	e.record(govSubBob, "bob@corp.example", []string{apGroupSec}, nil, false)

	// An older session as a security admin, a record that now says user and no grant: refused 403, no row,
	// and the page says not_eligible.
	e.record(govSubCarol, "carol@corp.example", []string{apGroupUsers}, nil, false)
	ch = e.heldOn(e.a2)
	if w := e.call(e.carol, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusForbidden {
		t.Fatalf("an older session whose record now says user = %d %s, want 403", w.Code, w.Body)
	}
	if n := rows(ch.ID); n != 0 {
		t.Errorf("a refused approval left %d rows", n)
	}
	var view map[string]any
	if err := json.Unmarshal(e.call(e.carol, http.MethodGet, "/api/v1/governance/changes/"+ch.ID.String(), "").Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if v, _ := approvalsOf(view)["viewer"].(map[string]any); v == nil || v["state"] != "not_eligible" {
		t.Errorf("viewer = %v, want not_eligible", v)
	}

	// The tier is capped by the record: a security-admin request whose record now says user but who holds a
	// unit-admin grant counts as a unit admin, so as the final seat the rule refuses.
	e.grant(e.a, types.CapabilitySubjectUser, govSubCarol)
	e.approve(e.adminA, ch.ID, http.StatusOK)
	if w := e.call(e.carol, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusConflict || wireReason(t, w) != reasonGovernanceChangeSecurityApprover {
		t.Fatalf("a security-admin request with a record that says unit admin = %d %s, want 409 %s", w.Code, w.Body, reasonGovernanceChangeSecurityApprover)
	}
}

// TestPG_Approvals_CLITokenTierIsCapped: a CLI access token publishes the user role, so a person whose record
// says security admin and who is admitted through a unit-admin grant approves at the unit-admin tier, and the
// security rule refuses their final approval. An admin-token caller still takes today's bypass.
func TestPG_Approvals_CLITokenTierIsCapped(t *testing.T) {
	e := newApEnv(t, 1)
	e.requireCounts(govKindMembership, 2, true)
	e.seedPeople()
	e.grant(e.a, types.CapabilitySubjectUser, govSubAlice)
	raw := apiTokenPrefix + strings.ReplaceAll(uuid.NewString(), "-", "")
	notTruncated := false
	if _, err := e.pg.CreateAPIToken(context.Background(), types.APIToken{
		ID: uuid.New(), Principal: govSubAlice, Email: "alice@corp.example", Role: oidc.RoleUser, UserType: types.UserTypeStandard,
		Kind: types.APITokenKindCLIAccess, Name: "cli", GroupsTruncated: &notTruncated,
	}, raw); err != nil {
		t.Fatalf("mint the CLI token: %v", err)
	}
	ch := e.heldOn(e.a2)
	e.approve(e.adminA, ch.ID, http.StatusOK)
	w := do(t, e.srv, http.MethodPost, approvePath(ch.ID), raw, "")
	if w.Code != http.StatusConflict || wireReason(t, w) != reasonGovernanceChangeSecurityApprover {
		t.Fatalf("a CLI token whose record says security admin = %d %s, want 409 %s", w.Code, w.Body, reasonGovernanceChangeSecurityApprover)
	}
	// The admin token takes the bypass whatever the rules say, writes no approval row and counts for nothing.
	if w := e.admin(http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusOK {
		t.Fatalf("the admin token = %d %s, want 200", w.Code, w.Body)
	}
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM governance_change_approvals WHERE approver = ''`).Scan(&n); err != nil || n != 0 {
		t.Errorf("rows for the admin token = %d (%v), want none", n, err)
	}
}

// TestPG_Approvals_TokenAndSessionDrops: an approval made with a personal API token still counts when the token
// expires and drops as token_revoked when it is revoked; a revocation of the approver's sessions after the
// approval drops it as session_revoked at the final approval.
func TestPG_Approvals_TokenAndSessionDrops(t *testing.T) {
	revocations := &memRevocations{cut: map[string]time.Time{}}
	e := newGovEnv(t, minApprovals(2), func(c *Config) { c.SessionRevocations = revocations })
	for _, sub := range []string{govSubBob, govSubCarol} {
		if err := e.pg.PutPrincipalSignIn(context.Background(), sub, strings.TrimPrefix(sub, "sub-")+"@corp.example", []string{}, nil, false); err != nil {
			t.Fatal(err)
		}
	}
	dropped := func(id uuid.UUID) string {
		var why string
		if err := e.pool.QueryRow(context.Background(), `SELECT dropped FROM governance_change_approvals WHERE change_id = $1 AND approver = $2`, id, govSubBob).Scan(&why); err != nil {
			t.Fatal(err)
		}
		return why
	}

	// Expired: still counts, so the second approval applies.
	tok, raw := e.mintToken(govSubBob, "bob@corp.example", oidc.RoleSecurityAdmin)
	ch := e.held()
	if w := do(t, e.srv, http.MethodPost, approvePath(ch.ID), raw, ""); w.Code != http.StatusOK {
		t.Fatalf("approve with the token = %d %s, want 200", w.Code, w.Body)
	}
	if _, err := e.pool.Exec(context.Background(), `UPDATE api_tokens SET expires_at = now() - interval '1 minute' WHERE id = $1`, tok.ID); err != nil {
		t.Fatal(err)
	}
	e.approveBy(e.carol, ch.ID)
	if got := e.changeState(ch.ID); got != types.GovernanceChangeApplied {
		t.Errorf("after the token expired: state %q, want applied (an expired token still counts)", got)
	}

	// Revoked: drops as token_revoked, and the change stays pending.
	tok, raw = e.mintToken(govSubBob, "bob@corp.example", oidc.RoleSecurityAdmin)
	ch = e.held()
	if w := do(t, e.srv, http.MethodPost, approvePath(ch.ID), raw, ""); w.Code != http.StatusOK {
		t.Fatalf("approve with the token = %d %s, want 200", w.Code, w.Body)
	}
	if _, err := e.pg.RevokeAPIToken(context.Background(), tok.ID, "", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	e.approveBy(e.carol, ch.ID)
	if got, why := e.changeState(ch.ID), dropped(ch.ID); got != types.GovernanceChangePending || why != droppedTokenRevoked {
		t.Errorf("after the token was revoked: state %q dropped %q, want pending and %s", got, why, droppedTokenRevoked)
	}

	// Sessions revoked after the approval: session_revoked.
	ch = e.held()
	e.approveBy(e.bob, ch.ID)
	time.Sleep(20 * time.Millisecond) // the cut must fall after the approval
	if err := revocations.RevokeSub(context.Background(), govSubBob); err != nil {
		t.Fatal(err)
	}
	e.approveBy(e.carol, ch.ID)
	if got, why := e.changeState(ch.ID), dropped(ch.ID); got != types.GovernanceChangePending || why != droppedSessionRevoked {
		t.Errorf("after the sessions were revoked: state %q dropped %q, want pending and %s", got, why, droppedSessionRevoked)
	}
}

// approveBy is POST /approve by cookie c, expecting 200.
func (e *govEnv) approveBy(c *http.Cookie, id uuid.UUID) {
	e.t.Helper()
	if w := e.call(c, http.MethodPost, approvePath(id), ""); w.Code != http.StatusOK {
		e.t.Fatalf("approve %s = %d %s, want 200", id, w.Code, w.Body)
	}
}

// TestPG_Approvals_RolesClaimRecord: a person who is a super admin by the roles claim alone counts when the roles
// are recorded; counts when roles is NULL and the claim is in the recorded groups (a record written before the
// approvals migration); and reads cant_confirm, never role_removed, when the record is truncated so the role fell
// out of the groups and roles is NULL.
func TestPG_Approvals_RolesClaimRecord(t *testing.T) {
	e := newApEnv(t, 2)
	e.seedPeople()
	e.record(govSubBob, "bob@corp.example", nil, []string{apGroupRoot}, false)

	ch := e.heldOn(e.a2)
	e.approve(e.bob, ch.ID, http.StatusOK)
	e.approve(e.alice, ch.ID, http.StatusOK)
	if got := e.changeState(ch.ID); got != types.GovernanceChangeApplied {
		t.Fatalf("roles recorded: state %q, want applied", got)
	}

	// A record from before the migration: roles NULL, the claim already in the groups.
	e.record(govSubBob, "bob@corp.example", nil, []string{apGroupRoot}, false)
	ch = e.heldOn(e.a2)
	e.approve(e.bob, ch.ID, http.StatusOK)
	if _, err := e.pool.Exec(context.Background(), `UPDATE principal_groups SET roles = NULL, groups = $2 WHERE principal = $1`, govSubBob, `["`+apGroupRoot+`"]`); err != nil {
		t.Fatal(err)
	}
	e.approve(e.alice, ch.ID, http.StatusOK)
	if got := e.changeState(ch.ID); got != types.GovernanceChangeApplied {
		t.Fatalf("roles NULL with the claim in the groups: state %q, want applied", got)
	}

	// Truncated, the claim fell out of the groups and roles is NULL: cant_confirm.
	e.record(govSubBob, "bob@corp.example", nil, []string{apGroupRoot}, false)
	ch = e.heldOn(e.a2)
	e.approve(e.bob, ch.ID, http.StatusOK)
	if _, err := e.pool.Exec(context.Background(), `UPDATE principal_groups SET roles = NULL, groups = '[]', truncated = true WHERE principal = $1`, govSubBob); err != nil {
		t.Fatal(err)
	}
	e.approve(e.alice, ch.ID, http.StatusOK)
	if got, why := e.changeState(ch.ID), e.droppedOf(ch.ID, govSubBob); got != types.GovernanceChangePending || why != droppedCannotConfirmed {
		t.Errorf("a truncated record with roles NULL: state %q dropped %q, want pending and %s", got, why, droppedCannotConfirmed)
	}
}

// poolOn runs fn on the pool inputs of one rolled-back read transaction of srv.
func poolOn(t *testing.T, srv *Server, fn func(in *poolInputs)) {
	t.Helper()
	err := srv.cfg.Store.DryRunGovernance(context.Background(), func(q store.Querier) error {
		in, err := srv.loadPoolInputs(context.Background(), q)
		if err != nil {
			return err
		}
		fn(in)
		return nil
	})
	if err != nil {
		t.Fatalf("load the pool inputs: %v", err)
	}
}

// principalsOf is the principals of a pool's people, sorted.
func principalsOf(people []poolPerson) []string {
	out := make([]string, len(people))
	for i, p := range people {
		out[i] = p.Principal
	}
	slices.Sort(out)
	return out
}

// TestPG_Approvals_PoolForKind: a unit kind and a unit counts a group-subject unit admin who signed in with the
// group, skips a person with no record, and counts a truncated record into unconfirmed_people; a security kind
// counts the security admins alone.
func TestPG_Approvals_PoolForKind(t *testing.T) {
	e := newApEnv(t, 2)
	e.seedPeople()
	e.record(uaSubGroupA, uaSubGroupA+"@corp.example", []string{apGroupUsers, uaGroupA}, nil, false)
	e.record("sub-ap-partial", "partial@corp.example", []string{apGroupUsers}, nil, true)
	// adminA1 holds a grant but never signed in: not eligible now, and not counted anywhere.
	poolOn(t, e.srv, func(in *poolInputs) {
		got, err := e.srv.poolForKind(in, govKindMembership, e.a2.ID)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{govSubAlice, govSubBob, govSubCarol, uaSubAdminA, uaSubGroupA, uaSubPeerA}
		if names := principalsOf(got.Eligible); !slices.Equal(names, want) {
			t.Errorf("unit pool = %v, want %v", names, want)
		}
		if len(got.Uncountable) != 1 || got.Uncountable[0].Why != uncountableUnconfirmed || got.Uncountable[0].Count != 1 {
			t.Errorf("uncountable = %+v, want one unconfirmed person", got.Uncountable)
		}
		sec, err := e.srv.poolForKind(in, govKindEnforcement, uuid.Nil)
		if err != nil {
			t.Fatal(err)
		}
		if names := principalsOf(sec.Eligible); !slices.Equal(names, []string{govSubAlice, govSubBob, govSubCarol}) {
			t.Errorf("security pool = %v, want the security admins", names)
		}
	})
}

// TestPG_Approvals_PoolIsTheSameForAnyCaller: the pool is computed for the people, not the caller: a GET by an API
// token yields the same eligible people as a GET by a session.
func TestPG_Approvals_PoolIsTheSameForAnyCaller(t *testing.T) {
	e := newApEnv(t, 2)
	e.seedPeople()
	ch := e.heldOn(e.a2)
	eligible := func(w *httptest.ResponseRecorder) any {
		t.Helper()
		var m map[string]any
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &m) != nil {
			t.Fatalf("GET = %d %s", w.Code, w.Body)
		}
		return approvalsOf(m)["eligible"]
	}
	_, raw := e.mintToken(govSubBob, "bob@corp.example", oidc.RoleSecurityAdmin)
	bySession := eligible(e.call(e.bob, http.MethodGet, "/api/v1/governance/changes/"+ch.ID.String(), ""))
	byToken := eligible(do(t, e.srv, http.MethodGet, "/api/v1/governance/changes/"+ch.ID.String(), raw, ""))
	if !reflect.DeepEqual(bySession, byToken) || bySession == nil {
		t.Errorf("eligible by session %v, by token %v, want the same non-empty pool", bySession, byToken)
	}
}

// unitScopeCounter is a store that counts UnitScope reads.
type unitScopeCounter struct {
	store.PG
	n *atomic.Int64
}

func (c unitScopeCounter) UnitScope(ctx context.Context, unit uuid.UUID, users, groups []string, strict bool) (bool, error) {
	c.n.Add(1)
	return c.PG.UnitScope(ctx, unit, users, groups, strict)
}

// grantReads counts the reads of the unit-admin grants.
type grantReads struct {
	store.Querier
	n *atomic.Int64
}

func (g grantReads) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if strings.Contains(sql, "FROM governance_unit_admins") {
		g.n.Add(1)
	}
	return g.Querier.Query(ctx, sql, args...)
}

// TestPG_Approvals_ScopeReadsCountHoldersNotCandidates: 50 candidates who are not security admins, two named by a
// grant (one by user subject, one by group subject), one unit, kind governance_membership. UnitScope is read
// exactly twice and the grants exactly once, and the two holders are the eligible people.
func TestPG_Approvals_ScopeReadsCountHoldersNotCandidates(t *testing.T) {
	var scopeReads, grantListReads atomic.Int64
	e := newApEnv(t, 2, func(c *Config) { c.Store = unitScopeCounter{PG: c.Store.(store.PG), n: &scopeReads} })
	for i := range 50 {
		sub := "sub-ap-cand-" + strings.Repeat("x", 1) + string(rune('a'+i%26)) + string(rune('a'+i/26))
		e.record(sub, sub+"@corp.example", []string{apGroupUsers}, nil, false)
	}
	holderUser := "sub-ap-cand-aa"
	e.grant(e.a, types.CapabilitySubjectUser, holderUser)
	holderGroup := "sub-ap-cand-ba"
	e.record(holderGroup, holderGroup+"@corp.example", []string{apGroupUsers, "grp-ap-holders"}, nil, false)
	e.grant(e.a, types.CapabilitySubjectGroup, "grp-ap-holders")

	err := e.srv.cfg.Store.DryRunGovernance(context.Background(), func(q store.Querier) error {
		in, err := e.srv.loadPoolInputs(context.Background(), grantReads{Querier: q, n: &grantListReads})
		if err != nil {
			return err
		}
		scopeReads.Store(0)
		got, err := e.srv.poolForKind(in, govKindMembership, e.a2.ID)
		if err != nil {
			return err
		}
		if names := principalsOf(got.Eligible); !slices.Equal(names, []string{holderGroup, holderUser}) {
			t.Errorf("eligible = %v, want the two holders", names)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := scopeReads.Load(); n != 2 {
		t.Errorf("UnitScope reads = %d, want 2 (one for each holder)", n)
	}
	if n := grantListReads.Load(); n != 1 {
		t.Errorf("unit-admin grant reads = %d, want 1", n)
	}
}

// TestPG_Approvals_TooManyPeople: a candidate read is bounded at 2000 records; a longer one says so.
func TestPG_Approvals_TooManyPeople(t *testing.T) {
	e := newApEnv(t, 2)
	if _, err := e.pool.Exec(context.Background(),
		`INSERT INTO principal_groups (principal, email, groups, truncated)
		 SELECT 'sub-many-' || g, 'many' || g || '@corp.example', '["`+apGroupUsers+`"]'::jsonb, false FROM generate_series(1, 2001) g`); err != nil {
		t.Fatal(err)
	}
	poolOn(t, e.srv, func(in *poolInputs) {
		if len(in.records) != store.SignInRecordCap || !in.tooMany {
			t.Fatalf("records = %d, tooMany = %v, want %d and true", len(in.records), in.tooMany, store.SignInRecordCap)
		}
		got, err := e.srv.poolForKind(in, govKindMembership, e.a2.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.ContainsFunc(got.Uncountable, func(s poolSource) bool { return s.Why == uncountableTooMany }) {
			t.Errorf("uncountable = %+v, want too_many_people", got.Uncountable)
		}
	})
}

// TestPG_Approvals_OneDistinctHuman: two principals that share one email, differing in case, are one person. A
// second approval by the alias of a counting approver is 409; the pool counts the two once, at the higher tier;
// an unconfirmed candidate at that email adds nothing; the change pool leaves out the proposer's alias and every
// counted approver's alias, by the email stored on the approval and even when the counted record is outside the
// candidate read; a candidate with no email is never merged.
func TestPG_Approvals_OneDistinctHuman(t *testing.T) {
	e := newApEnv(t, 2)
	e.seedPeople()
	for _, sub := range []string{"sub-ap-1", "sub-ap-2", "sub-ap-3", "sub-ap-4", "sub-ap-5", "sub-ap-old"} {
		e.grant(e.a, types.CapabilitySubjectUser, sub)
	}
	e.record("sub-ap-1", "Ap@corp.example", []string{apGroupUsers}, nil, false)
	e.record("sub-ap-2", "ap@corp.example", []string{apGroupSec}, nil, false)
	e.record("sub-ap-3", "AP@corp.example", []string{apGroupUsers}, nil, true) // unconfirmed: no grant is read for it
	e.record("sub-ap-4", "", []string{apGroupUsers}, nil, false)
	e.record("sub-ap-5", "", []string{apGroupUsers}, nil, false)
	e.record("sub-ap-old", "old@corp.example", []string{apGroupUsers}, nil, false)

	poolOn(t, e.srv, func(in *poolInputs) {
		got, err := e.srv.poolForKind(in, govKindMembership, e.a2.ID)
		if err != nil {
			t.Fatal(err)
		}
		var ap []poolPerson
		for _, p := range got.Eligible {
			if strings.EqualFold(p.Email, "ap@corp.example") || p.Principal == "sub-ap-1" || p.Principal == "sub-ap-2" {
				ap = append(ap, p)
			}
		}
		if len(ap) != 1 || ap[0].Principal != "sub-ap-2" || ap[0].Tier != tierSecurityAdmin {
			t.Errorf("the two principals at one email = %+v, want one person: sub-ap-2 at the security tier", ap)
		}
		if !slices.Contains(principalsOf(got.Eligible), "sub-ap-4") || !slices.Contains(principalsOf(got.Eligible), "sub-ap-5") {
			t.Errorf("candidates with no email were merged: %v", principalsOf(got.Eligible))
		}
		for _, s := range got.Uncountable {
			if s.Why == uncountableUnconfirmed {
				t.Errorf("an unconfirmed candidate at an eligible person's email was counted: %+v", s)
			}
		}

		ch := e.heldOn(e.a2)
		counted := []countedPerson{{Principal: "sub-ap-gone", Email: "old@corp.example"}, {Principal: "sub-ap-1", Email: "ap@corp.example"}}
		pool, err := e.srv.poolForChange(in, ch, counted)
		if err != nil {
			t.Fatal(err)
		}
		for _, banned := range []string{"sub-ap-1", "sub-ap-2", "sub-ap-3", "sub-ap-old"} {
			if slices.Contains(principalsOf(pool.Eligible), banned) {
				t.Errorf("%s is an alias of a counted approver and is listed as eligible", banned)
			}
		}
		if slices.Contains(principalsOf(pool.Eligible), ch.ProposedBy) {
			t.Errorf("the proposer %s is listed as eligible", ch.ProposedBy)
		}
	})

	// Acceptance: the alias of a counting approver is a 409.
	ch := e.heldOn(e.a2)
	e.approve(e.adminA, ch.ID, http.StatusOK)
	if _, err := e.pool.Exec(context.Background(), `UPDATE governance_change_approvals SET approver_email = 'ap@corp.example' WHERE change_id = $1`, ch.ID); err != nil {
		t.Fatal(err)
	}
	ap := e.sessionOf("sub-ap-2", "AP@corp.example")
	if w := e.call(ap, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusConflict || wireReason(t, w) != reasonGovernanceChangeAlreadyApproved {
		t.Errorf("the alias of a counting approver = %d %s, want 409 %s", w.Code, w.Body, reasonGovernanceChangeAlreadyApproved)
	}
}

// sessionOf is a session of a person with the unit-admin role.
func (e *apEnv) sessionOf(sub, email string) *http.Cookie {
	return ssoSession(e.t, sub, email, oidc.RoleUser)
}

// TestPG_Approvals_ValueTable: one case for each row of the value table of a kind with more than one decider.
// The caller is an eligible approver (alice), not the proposer, not the admin token, and holds no counting row;
// the earlier row, when there is one, is bob's. minimum is the deployment's now, snapshot what the change was
// held needing. Rows for a request or a run node record, which always have one decider, are TestMultiAndEffective.
func TestPG_Approvals_ValueTable(t *testing.T) {
	cases := []struct {
		name       string
		minimum    int
		snapshot   int
		earlier    bool
		dropEarly  bool // the earlier approver's record is gone at the decision
		inactive   bool // the proposer is deactivated at the decision
		wantState  string
		wantRows   int // approval rows that still count afterwards
		wantRefuse int // the status of the proposer-inactive refusal tried first, 0 when it is not reached
		rejectNeed bool
	}{
		{"0/1/0 applies as today", 0, 1, false, false, false, types.GovernanceChangeApplied, 1, 0, false},
		{"0/1/1 the earlier row is neither read nor re-checked", 0, 1, true, true, false, types.GovernanceChangeApplied, 1, 0, false},
		{"0/3/0 recorded 1 of 3", 0, 3, false, false, true, types.GovernanceChangePending, 1, 0, true},
		{"0/3/1 recorded 2 of 3", 0, 3, true, false, false, types.GovernanceChangePending, 2, 0, true},
		{"1/1/0 final applies", 1, 1, false, false, false, types.GovernanceChangeApplied, 1, 0, true},
		{"1/1/0 final, proposer inactive", 1, 1, false, false, true, types.GovernanceChangePending, 0, http.StatusConflict, true},
		{"1/1/1 final, the earlier row is re-checked and its drop recorded, applies", 1, 1, true, true, false, types.GovernanceChangeApplied, 1, 0, true},
		{"1/3/0 recorded 1 of 3", 1, 3, false, false, false, types.GovernanceChangePending, 1, 0, true},
		{"1/3/1 recorded 2 of 3", 1, 3, true, false, false, types.GovernanceChangePending, 2, 0, true},
		{"2/1/0 recorded 1 of 2", 2, 1, false, false, false, types.GovernanceChangePending, 1, 0, true},
		{"2/1/0 recorded 1 of 2, proposer inactive is not reached", 2, 1, false, false, true, types.GovernanceChangePending, 1, 0, true},
		{"2/1/1 final, the earlier row still counts, applies", 2, 1, true, false, false, types.GovernanceChangeApplied, 2, 0, true},
		{"2/1/1 final, the earlier row dropped, recorded 1 of 2", 2, 1, true, true, false, types.GovernanceChangePending, 1, 0, true},
		{"2/1/1 final, proposer inactive", 2, 1, true, false, true, types.GovernanceChangePending, 1, http.StatusConflict, true},
		{"2/3/0 recorded 1 of 3", 2, 3, false, false, false, types.GovernanceChangePending, 1, 0, true},
		{"2/3/1 recorded 2 of 3", 2, 3, true, false, false, types.GovernanceChangePending, 2, 0, true},
	}
	e := newApEnv(t, 2)
	e.seedPeople()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			srv := e.otherServer(func(c *Config) { c.GovernanceMinApprovals = tc.minimum })
			ch := e.heldOn(e.a2)
			if _, err := e.pool.Exec(ctx, `UPDATE governance_changes SET required_approvals = $2 WHERE id = $1`, ch.ID, tc.snapshot); err != nil {
				t.Fatal(err)
			}
			if tc.earlier {
				if _, err := e.pool.Exec(ctx, `INSERT INTO governance_change_approvals (change_id, approver, approver_email, tier)
					VALUES ($1, $2, 'bob@corp.example', 'security_admin')`, ch.ID, govSubBob); err != nil {
					t.Fatal(err)
				}
				if tc.dropEarly {
					if _, err := e.pool.Exec(ctx, `DELETE FROM principal_groups WHERE principal = $1`, govSubBob); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { e.record(govSubBob, "bob@corp.example", []string{apGroupSec}, nil, false) })
				}
			}
			if tc.rejectNeed {
				if w := doSSO(t, srv, http.MethodPost, rejectPath(ch.ID), e.carol, ""); w.Code != http.StatusBadRequest {
					t.Errorf("a reject with no reason = %d %s, want 400", w.Code, w.Body)
				}
			}
			if tc.inactive {
				if _, err := e.pool.Exec(ctx, `INSERT INTO people (principal, created_by, deactivated_at) VALUES ($1, 'seed', now())
					ON CONFLICT (principal) DO UPDATE SET deactivated_at = now()`, ch.ProposedBy); err != nil {
					t.Fatal(err)
				}
			}
			if tc.wantRefuse != 0 {
				w := doSSO(t, srv, http.MethodPost, approvePath(ch.ID), e.alice, "")
				if w.Code != tc.wantRefuse || wireReason(t, w) != reasonGovernanceChangeProposerInactive {
					t.Fatalf("the proposer-inactive refusal = %d %s, want %d %s", w.Code, w.Body, tc.wantRefuse, reasonGovernanceChangeProposerInactive)
				}
				if got := e.changeState(ch.ID); got != types.GovernanceChangePending {
					t.Fatalf("after the refusal: state %q, want pending with nothing written", got)
				}
				return
			}
			if w := doSSO(t, srv, http.MethodPost, approvePath(ch.ID), e.alice, ""); w.Code != http.StatusOK {
				t.Fatalf("approve = %d %s, want 200", w.Code, w.Body)
			}
			if got := e.changeState(ch.ID); got != tc.wantState {
				t.Errorf("state = %q, want %q", got, tc.wantState)
			}
			var counting int
			if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM governance_change_approvals WHERE change_id = $1 AND dropped = ''`, ch.ID).Scan(&counting); err != nil {
				t.Fatal(err)
			}
			wantRows := tc.wantRows
			if tc.minimum == 0 && tc.snapshot == 1 {
				// The unset minimum is today's path: no row is read or written by this approval.
				wantRows = map[bool]int{true: 1, false: 0}[tc.earlier]
			}
			if counting != wantRows {
				t.Errorf("rows that still count = %d, want %d", counting, wantRows)
			}
		})
	}
}

// The four kinds a held change can have that did not exist when the approval rules were
// first written. Each is held under operatorApprover, so each is decided by super admins; with a minimum of 2 the
// first approval records and writes nothing outside its own row (the apply it replays runs under a savepoint
// that is rolled back; every lock an apply takes is a transaction-level advisory lock, which a ROLLBACK TO
// SAVEPOINT does not release, so the target stays locked until the decision's transaction ends), and the second
// applies.
//
// recordsFor records the super admins as the people their sessions say they are.
func recordsFor(t *testing.T, st store.PG, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if err := st.PutPrincipalSignIn(context.Background(), sub, strings.TrimPrefix(sub, "sub-")+"@corp.example", []string{}, nil, false); err != nil {
			t.Fatal(err)
		}
	}
}

// stateVia is a change's state read through the API.
func stateVia(t *testing.T, srv *Server, c *http.Cookie, id uuid.UUID) string {
	t.Helper()
	w := doSSO(t, srv, http.MethodGet, "/api/v1/governance/changes/"+id.String(), c, "")
	var got types.GovernanceChange
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil {
		t.Fatalf("GET change = %d %s", w.Code, w.Body)
	}
	return got.State
}

// TestPG_Approvals_AssistantSettingsRecordsThenApplies: the assistant settings kind at minimum 2: the first
// approval leaves the stored block as it was, the second writes it.
func TestPG_Approvals_AssistantSettingsRecordsThenApplies(t *testing.T) {
	direct := assistantTestProvider("direct", "uid-direct", types.ModelProviderAnthropicAPIKey, "m")
	e := newAssistantHoldEnv(t, assistantHoldStored(), direct)
	e.srv.cfg.GovernanceMinApprovals = 2
	third := ssoSession(t, "sub-super3", "super3@corp.example", oidc.RoleAdmin)
	recordsFor(t, e.pg, "sub-super2", "sub-super3")
	ch := e.pending(e.put(e.super, assistantReviewOnBody))
	if w := e.call(e.super2, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusOK {
		t.Fatalf("first approval = %d %s, want 200", w.Code, w.Body)
	}
	if got := e.stored().Assistant; got == nil || got.Review {
		t.Fatalf("the first approval wrote the block: %+v", got)
	}
	if got := stateVia(t, e.srv, e.super2, ch.ID); got != types.GovernanceChangePending {
		t.Fatalf("state after the first approval = %q, want pending", got)
	}
	if w := e.call(third, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusOK {
		t.Fatalf("second approval = %d %s, want 200", w.Code, w.Body)
	}
	if got := e.stored().Assistant; got == nil || !got.Review {
		t.Fatalf("the second approval did not write the block: %+v", got)
	}
}

// TestPG_Approvals_PortalKindsRecordThenApply: a portal registration (delegate), an admission change
// (delegate_admission) and a binding's retirement (portal_issuer_retire) at minimum 2.
func TestPG_Approvals_PortalKindsRecordThenApply(t *testing.T) {
	t.Run("delegate", func(t *testing.T) {
		t.Setenv(envGovernanceSecondHuman, "true")
		e := newRegPG(t)
		e.h.srv.cfg.GovernanceMinApprovals = 2
		third := accessSession(t, "root3", "root3@corp.example", oidc.RoleAdmin, []string{})
		recordsFor(t, e.st, "root2", "root3")
		change, _, w := e.proposeHeld(t, map[string]any{"issuer": "https://approvals-two.example.com"})
		if change == "" {
			t.Fatalf("proposal = %d %s", w.Code, w.Body.String())
		}
		id := uuid.MustParse(change)
		if w := e.decideHeld(t, change, "approve"); w.Code != http.StatusOK {
			t.Fatalf("first approval = %d %s, want 200", w.Code, w.Body.String())
		}
		if got := stateVia(t, e.h.srv, e.admin2, id); got != types.GovernanceChangePending {
			t.Fatalf("state after the first approval = %q, want pending", got)
		}
		if w := doSSO(t, e.h.srv, http.MethodPost, approvePath(id), third, ""); w.Code != http.StatusOK {
			t.Fatalf("second approval = %d %s, want 200", w.Code, w.Body.String())
		}
		if got := stateVia(t, e.h.srv, e.admin2, id); got != types.GovernanceChangeApplied {
			t.Fatalf("state after the second approval = %q, want applied", got)
		}
	})
	t.Run("delegate_admission", func(t *testing.T) {
		e := newGovEnv(t)
		e.srv.cfg.GovernanceMinApprovals = 2
		d := seedIssuerPortal(t, e.pg, nil, withClaim("tenant", "env-a"))
		cur := types.PortalAdmission{PortalManaged: true, RequiredClaim: d.RequiredClaim}
		path := "/api/v1/admin/delegates/" + d.ID.String() + "/admission"
		second := ssoSession(t, "sub-super-2", "super2@corp.example", oidc.RoleAdmin)
		third := ssoSession(t, "sub-super-3", "super3@corp.example", oidc.RoleAdmin)
		recordsFor(t, e.pg, "sub-super-2", "sub-super-3")
		ch := e.pending(e.call(e.super, http.MethodPut, path, admissionBody(types.PortalAdmission{PortalManaged: true}, cur)))
		if w := e.call(second, http.MethodPost, approvePath(ch.ID), `{}`); w.Code != http.StatusOK {
			t.Fatalf("first approval = %d %s, want 200", w.Code, w.Body)
		}
		if got, _ := e.pg.GetDelegateByID(context.Background(), d.ID); got.RequiredClaim == nil || got.AdmissionRev != 1 {
			t.Fatalf("the first approval wrote the portal row: claim %v rev %d", got.RequiredClaim, got.AdmissionRev)
		}
		if w := e.call(third, http.MethodPost, approvePath(ch.ID), `{}`); w.Code != http.StatusOK {
			t.Fatalf("second approval = %d %s, want 200", w.Code, w.Body)
		}
		if got, _ := e.pg.GetDelegateByID(context.Background(), d.ID); got.RequiredClaim != nil || got.AdmissionRev != 2 {
			t.Fatalf("the second approval left claim %v rev %d, want none at rev 2", got.RequiredClaim, got.AdmissionRev)
		}
	})
	t.Run("portal_issuer_retire", func(t *testing.T) {
		e := newGovEnv(t)
		e.srv.cfg.GovernanceMinApprovals = 2
		b := bindForRetire(t, e.pg)
		second := ssoSession(t, "sub-super-2", "super2@corp.example", oidc.RoleAdmin)
		third := ssoSession(t, "sub-super-3", "super3@corp.example", oidc.RoleAdmin)
		recordsFor(t, e.pg, "sub-super-2", "sub-super-3")
		ch := e.pending(e.call(e.super, http.MethodDelete, "/api/v1/admin/portal-issuers/"+b.IssuerKey, ""))
		if w := e.call(second, http.MethodPost, approvePath(ch.ID), `{}`); w.Code != http.StatusOK {
			t.Fatalf("first approval = %d %s, want 200", w.Code, w.Body)
		}
		if bound, err := store.IssuerKeyBoundQ(context.Background(), e.pg.Pool, b.IssuerKey); err != nil || !bound {
			t.Fatalf("after the first approval bound = %v (%v), want the binding in place", bound, err)
		}
		if w := e.call(third, http.MethodPost, approvePath(ch.ID), `{}`); w.Code != http.StatusOK {
			t.Fatalf("second approval = %d %s, want 200", w.Code, w.Body)
		}
		if bound, err := store.IssuerKeyBoundQ(context.Background(), e.pg.Pool, b.IssuerKey); err != nil || bound {
			t.Fatalf("after the second approval bound = %v (%v), want the binding retired", bound, err)
		}
	})
}

// ── what an earlier approval is dropped for, and what it is not ──────────────────────────────────────────────────

// deactivate marks a person inactive in the people directory.
func (e *apEnv) deactivate(sub string) {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO people (principal, created_by, deactivated_at) VALUES ($1, 'seed', now())
		ON CONFLICT (principal) DO UPDATE SET deactivated_at = now()`, sub); err != nil {
		e.t.Fatalf("deactivate %s: %v", sub, err)
	}
}

// newUnitAdmin is a new person who is a unit admin of unit a: a session, a sign-in record in the user group and
// the grant, which is returned so a test can take it away.
func (e *apEnv) newUnitAdmin(sub string) (*http.Cookie, types.GovernanceUnitAdmin) {
	e.t.Helper()
	email := sub + "@corp.example"
	e.record(sub, email, []string{apGroupUsers}, nil, false)
	return e.sessionOf(sub, email), e.grant(e.a, types.CapabilitySubjectUser, sub)
}

// eligibleOf is the principals the approvals block of a decoded change lists as able to approve.
func eligibleOf(m map[string]any) []string {
	var out []string
	list, _ := approvalsOf(m)["eligible"].([]any)
	for _, p := range list {
		if row, ok := p.(map[string]any); ok {
			who, _ := row["principal"].(string)
			out = append(out, who)
		}
	}
	return out
}

// TestPG_Approvals_DeactivatedAndGrantRemovedApproversAreDropped: an approver deactivated between the approvals
// is dropped as deactivated, and one whose unit-admin grant was taken away is dropped as grant_removed; either
// way the last approval is recorded, the change stays pending, and the page counts one.
func TestPG_Approvals_DeactivatedAndGrantRemovedApproversAreDropped(t *testing.T) {
	e := newApEnv(t, 2)
	e.seedPeople()

	first, _ := e.newUnitAdmin("sub-ap-leaver")
	ch := e.heldOn(e.a2)
	e.approve(first, ch.ID, http.StatusOK)
	e.deactivate("sub-ap-leaver")
	e.approve(e.peerA, ch.ID, http.StatusOK)
	if got, why := e.changeState(ch.ID), e.droppedOf(ch.ID, "sub-ap-leaver"); got != types.GovernanceChangePending || why != droppedDeactivated {
		t.Errorf("an approver deactivated in between: state %q dropped %q, want pending and %s", got, why, droppedDeactivated)
	}
	if have := approvalsOf(e.read(ch.ID))["have"]; have != float64(1) {
		t.Errorf("have = %v, want 1: only the second approval counts", have)
	}

	second, grant := e.newUnitAdmin("sub-ap-moved")
	ch = e.heldOn(e.a2)
	e.approve(second, ch.ID, http.StatusOK)
	if _, err := store.RevokeUnitAdminQ(context.Background(), e.pool, e.a.ID, grant.ID); err != nil {
		t.Fatalf("take the grant away: %v", err)
	}
	e.approve(e.peerA, ch.ID, http.StatusOK)
	if got, why := e.changeState(ch.ID), e.droppedOf(ch.ID, "sub-ap-moved"); got != types.GovernanceChangePending || why != droppedGrantRemoved {
		t.Errorf("an approver whose grant was removed: state %q dropped %q, want pending and %s", got, why, droppedGrantRemoved)
	}
	if have := approvalsOf(e.read(ch.ID))["have"]; have != float64(1) {
		t.Errorf("have = %v, want 1: only the second approval counts", have)
	}
}

// TestPG_Approvals_StaleAtANonFinalApproval: a write to the target after the proposal makes the change stale at
// the first approval of two, not only at the last; the stale answer is the same 409, the state is stale, and
// no approval row is written.
func TestPG_Approvals_StaleAtANonFinalApproval(t *testing.T) {
	e := newGovEnv(t, minApprovals(2))
	e.signIn(govSubBob, "bob@corp.example")
	p := e.seedProfile("stale-first", "pypi.org")
	ch := e.pending(e.call(e.alice, http.MethodPut, "/api/v1/governance/units/"+p.ID.String(), profileBody("stale-first", "pypi.org", "github.com")))
	if w := e.admin(http.MethodPut, "/api/v1/governance/units/"+p.ID.String(), profileBody("stale-first", "github.com")); w.Code != http.StatusOK {
		t.Fatalf("the admin-token write = %d %s, want 200", w.Code, w.Body)
	}
	w := e.call(e.bob, http.MethodPost, approvePath(ch.ID), "")
	if w.Code != http.StatusConflict || wireReason(t, w) != reasonGovernanceChangeStale {
		t.Fatalf("the first of two approvals of a stale change = %d %s, want 409 %s", w.Code, w.Body, reasonGovernanceChangeStale)
	}
	if got := e.changeState(ch.ID); got != types.GovernanceChangeStale {
		t.Errorf("state = %q, want stale", got)
	}
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM governance_change_approvals WHERE change_id = $1`, ch.ID).Scan(&n); err != nil || n != 0 {
		t.Errorf("approval rows = %d (%v), want none", n, err)
	}
}

// TestPG_Approvals_RunNodeRecordNeedsOneApproval: a run node record has one decider whatever the deployment's
// minimum is. With a minimum of 3 it is held needing one approval and applies on it, with today's two-person
// record, and no approval row is read or written.
func TestPG_Approvals_RunNodeRecordNeedsOneApproval(t *testing.T) {
	e := newContainmentEnv(t, false)
	e.cfg.GovernanceMinApprovals = 3
	e.srv = New(e.cfg)
	reuse := runner.NodeSample{Name: "n1", UID: "uid-n1-new", MachineID: "m7", SystemUUID: "s7"}
	e.mustPage(&runnerwire.ContainmentPending{Kind: runner.ContainmentPendingDelete, Since: time.Now().Add(-time.Hour), NameReused: true, Tuple: e.tuple(reuse)}, nil)
	tuple := e.containment(e.alice).Pending.Tuple
	ch := e.pending(e.recordW(e.alice, types.NodeRecordPoweredOff, tuple))
	var required int
	if err := e.pool.QueryRow(context.Background(), `SELECT required_approvals FROM governance_changes WHERE id = $1`, ch.ID).Scan(&required); err != nil || required != 1 {
		t.Fatalf("required approvals = %d (%v), want 1", required, err)
	}
	e.approve(ch.ID)
	if got := e.changeState(ch.ID); got != types.GovernanceChangeApplied {
		t.Fatalf("state after one approval = %q, want applied", got)
	}
	actions := e.recordActions()
	if len(actions) != 1 || len(actions[0].NodeRecord.Approvers) != 2 || actions[0].NodeRecord.Approvers[0] == actions[0].NodeRecord.Approvers[1] {
		t.Fatalf("PENDING after the approval = %+v, want one record by two people", actions)
	}
	var rows int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM governance_change_approvals WHERE change_id = $1`, ch.ID).Scan(&rows); err != nil || rows != 0 {
		t.Errorf("approval rows = %d (%v), want none", rows, err)
	}
}

// TestPG_Approvals_SignOutStillCountsAndARevocationDoesNot: with the revocation store the server really uses,
// an approver who signs out afterwards (a cut of their browser sessions) still counts at the last approval,
// and one whose sessions are revoked afterwards is dropped as session_revoked.
func TestPG_Approvals_SignOutStillCountsAndARevocationDoesNot(t *testing.T) {
	e := newApEnv(t, 2)
	e.seedPeople()
	rev := &pgTestRevocations{pool: e.pool, st: e.pg}
	srv := e.otherServer(func(c *Config) { c.SessionRevocations = rev })
	decide := func(c *http.Cookie, id uuid.UUID) {
		t.Helper()
		if w := doSSO(t, srv, http.MethodPost, approvePath(id), c, ""); w.Code != http.StatusOK {
			t.Fatalf("approve %s = %d %s, want 200", id, w.Code, w.Body)
		}
	}

	ch := e.heldOn(e.a2)
	decide(e.adminA, ch.ID)
	if err := rev.CutSessions(context.Background(), uaSubAdminA); err != nil {
		t.Fatal(err)
	}
	decide(e.peerA, ch.ID)
	if got := e.changeState(ch.ID); got != types.GovernanceChangeApplied {
		t.Errorf("after the first approver signed out: state %q, want applied (a sign-out leaves the approval counting)", got)
	}

	ch = e.heldOn(e.a2)
	decide(e.alice, ch.ID)
	if err := rev.RevokeSub(context.Background(), govSubAlice); err != nil {
		t.Fatal(err)
	}
	decide(e.bob, ch.ID)
	if got, why := e.changeState(ch.ID), e.droppedOf(ch.ID, govSubAlice); got != types.GovernanceChangePending || why != droppedSessionRevoked {
		t.Errorf("after the first approver's sessions were revoked: state %q dropped %q, want pending and %s", got, why, droppedSessionRevoked)
	}
}

// TestPG_Approvals_ACLITokenApprovalSurvivesARefreshAndASignOut: an approval made with a CLI access token still
// counts after the session refreshes (the old token is revoked at the first refresh and deleted at a later
// one) and after the person signs out of the CLI (the token is revoked); it is dropped as session_revoked when
// the person's identity is revoked.
func TestPG_Approvals_ACLITokenApprovalSurvivesARefreshAndASignOut(t *testing.T) {
	e := newApEnv(t, 2)
	e.seedPeople()
	rev := &pgTestRevocations{pool: e.pool, st: e.pg}
	srv := e.otherServer(func(c *Config) { c.SessionRevocations = rev })
	const sub = "sub-ap-cli"
	_, _ = e.newUnitAdmin(sub)
	mint := func() (uuid.UUID, string) {
		t.Helper()
		raw := apiTokenPrefix + strings.ReplaceAll(uuid.NewString(), "-", "")
		notTruncated := false
		tok, err := e.pg.CreateAPIToken(context.Background(), types.APIToken{
			ID: uuid.New(), Principal: sub, Email: sub + "@corp.example", Role: oidc.RoleUser, UserType: types.UserTypeStandard,
			Kind: types.APITokenKindCLIAccess, Name: "cli", GroupsTruncated: &notTruncated,
		}, raw)
		if err != nil {
			t.Fatalf("mint the CLI token: %v", err)
		}
		return tok.ID, raw
	}
	// approvedWith is a held change that sub approved with a new CLI token, then what happens to that token,
	// then the second approval by peerA.
	approvedWith := func(after func(token uuid.UUID)) uuid.UUID {
		t.Helper()
		token, raw := mint()
		ch := e.heldOn(e.a2)
		if w := do(t, srv, http.MethodPost, approvePath(ch.ID), raw, ""); w.Code != http.StatusOK {
			t.Fatalf("approve with the CLI token = %d %s, want 200", w.Code, w.Body)
		}
		var kind string
		if err := e.pool.QueryRow(context.Background(), `SELECT api_token_kind FROM governance_change_approvals WHERE change_id = $1 AND approver = $2`, ch.ID, sub).Scan(&kind); err != nil || kind != types.APITokenKindCLIAccess {
			t.Fatalf("the approval's token kind = %q (%v), want %s", kind, err, types.APITokenKindCLIAccess)
		}
		after(token)
		if w := doSSO(t, srv, http.MethodPost, approvePath(ch.ID), e.peerA, ""); w.Code != http.StatusOK {
			t.Fatalf("second approval = %d %s, want 200", w.Code, w.Body)
		}
		return ch.ID
	}
	exec := func(sql string, id uuid.UUID) {
		t.Helper()
		if _, err := e.pool.Exec(context.Background(), sql, id); err != nil {
			t.Fatal(err)
		}
	}

	for name, after := range map[string]func(uuid.UUID){
		"a refresh at generation 1 revokes the old token": func(id uuid.UUID) {
			exec(`UPDATE api_tokens SET revoked_at = clock_timestamp() WHERE id = $1`, id)
		},
		"a refresh past generation 1 deletes the old token": func(id uuid.UUID) { exec(`DELETE FROM api_tokens WHERE id = $1`, id) },
		"a sign-out of the CLI stamps revoked_at": func(id uuid.UUID) {
			exec(`UPDATE api_tokens SET revoked_at = clock_timestamp() WHERE id = $1`, id)
		},
	} {
		id := approvedWith(after)
		if got := e.changeState(id); got != types.GovernanceChangeApplied {
			t.Errorf("%s: state %q, want applied (the approval still counts)", name, got)
		}
	}

	id := approvedWith(func(uuid.UUID) {
		if err := rev.RevokeSub(context.Background(), sub); err != nil {
			t.Fatal(err)
		}
	})
	if got, why := e.changeState(id), e.droppedOf(id, sub); got != types.GovernanceChangePending || why != droppedSessionRevoked {
		t.Errorf("after the person's identity was revoked: state %q dropped %q, want pending and %s", got, why, droppedSessionRevoked)
	}
}

// TestPG_Approvals_EmptyPrincipalIsRefusedWithNoRow: a caller with an empty principal, who is not the admin token,
// is refused as an approver is and writes no row, and no predicate is asked about them: an empty principal
// reads as the admin token.
func TestPG_Approvals_EmptyPrincipalIsRefusedWithNoRow(t *testing.T) {
	e := newApEnv(t, 2)
	e.seedPeople()
	ch := e.heldOn(e.a2)
	kind := governanceChangeKinds[ch.TargetKind].forChange(ch)
	ctx := context.Background()
	r := httptest.NewRequest(http.MethodPost, approvePath(ch.ID), nil)
	_, err := e.pg.DecideGovernanceChange(ctx, ch.ID, store.GovernanceDecision{To: types.GovernanceChangeApplied}, func(q store.Querier, locked types.GovernanceChange) error {
		return e.srv.decideMulti(ctx, r, q, kind, locked, &approvalRun{caller: approvalCaller{email: "nobody@corp.example"}}, &govApplied{})
	})
	var refusal *govRefusal
	if !errors.As(err, &refusal) || refusal.why != "approver tier" {
		t.Fatalf("an approval with an empty principal = %v, want the approver refusal", err)
	}
	var n int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM governance_change_approvals WHERE change_id = $1`, ch.ID).Scan(&n); err != nil || n != 0 {
		t.Errorf("approval rows = %d (%v), want none", n, err)
	}
	if got := e.changeState(ch.ID); got != types.GovernanceChangePending {
		t.Errorf("state = %q, want pending", got)
	}
}

// TestPG_Approvals_UnsetAuditRowsAreToday: with the minimum unset the propose and approve rows carry exactly the
// keys they always did, none of the keys the approval rules add, and no approval or retract row is written.
func TestPG_Approvals_UnsetAuditRowsAreToday(t *testing.T) {
	e := newGovEnv(t)
	ch := e.held()
	e.approveBy(e.bob, ch.ID)
	keysOf := func(action string, ignore ...string) []string {
		t.Helper()
		rows := e.audits(action)
		if len(rows) == 0 {
			t.Fatalf("no %s row", action)
		}
		var out []string
		for k := range auditData(t, rows[len(rows)-1]) {
			if !slices.Contains(ignore, k) {
				out = append(out, k)
			}
		}
		slices.Sort(out)
		return out
	}
	if got, want := keysOf("governance.change.propose", "units"), []string{"changed", "expires_at", "op", "target_key", "target_kind"}; !slices.Equal(got, want) {
		t.Errorf("propose row keys = %v, want %v", got, want)
	}
	if got, want := keysOf("governance.change.approve"), []string{"proposed_by", "target_key", "target_kind"}; !slices.Equal(got, want) {
		t.Errorf("approve row keys = %v, want %v", got, want)
	}
	for _, action := range []string{"governance.change.approval", "governance.change.retract"} {
		if rows := e.audits(action); len(rows) != 0 {
			t.Errorf("%d %s rows with the minimum unset, want none", len(rows), action)
		}
	}
}

// TestPG_Approvals_AnApproversOldEmailStaysWithTheApproval: an approver whose email changed after approving is
// still known by the email stored on the approval: another principal who now has that old email is left out of
// the pool and refused as a duplicate.
func TestPG_Approvals_AnApproversOldEmailStaysWithTheApproval(t *testing.T) {
	e := newApEnv(t, 2)
	e.seedPeople()
	ch := e.heldOn(e.a2)
	e.approve(e.adminA, ch.ID, http.StatusOK)
	oldEmail := uaSubAdminA + "@corp.example"
	e.record(uaSubAdminA, "new@corp.example", []string{apGroupUsers}, nil, false)

	const heir = "sub-ap-heir"
	e.grant(e.a, types.CapabilitySubjectUser, heir)
	e.record(heir, oldEmail, []string{apGroupUsers}, nil, false)
	if listed := eligibleOf(e.read(ch.ID)); slices.Contains(listed, heir) || slices.Contains(listed, uaSubAdminA) {
		t.Errorf("eligible = %v, want neither the counted approver nor the principal at their stored email", listed)
	}
	w := e.call(e.sessionOf(heir, oldEmail), http.MethodPost, approvePath(ch.ID), "")
	if w.Code != http.StatusConflict || wireReason(t, w) != reasonGovernanceChangeAlreadyApproved {
		t.Errorf("a principal at the approver's stored email = %d %s, want 409 %s", w.Code, w.Body, reasonGovernanceChangeAlreadyApproved)
	}
	if got := e.changeState(ch.ID); got != types.GovernanceChangePending {
		t.Errorf("state = %q, want pending", got)
	}
}

// TestPG_Approvals_TwoApproversWhoNowShareAnEmailBothCount: two approvals recorded under different emails keep
// counting when both people later sign in with one email, so have does not change; a third principal at that
// email is not a duplicate of either, because it matches no stored email, and its approval applies the change.
func TestPG_Approvals_TwoApproversWhoNowShareAnEmailBothCount(t *testing.T) {
	e := newApEnv(t, 3)
	e.seedPeople()
	ch := e.heldOn(e.a2)
	e.approve(e.adminA, ch.ID, http.StatusOK)
	e.approve(e.peerA, ch.ID, http.StatusOK)
	const shared = "shared@corp.example"
	e.record(uaSubAdminA, shared, []string{apGroupUsers}, nil, false)
	e.record(uaSubPeerA, shared, []string{apGroupUsers}, nil, false)
	if have := approvalsOf(e.read(ch.ID))["have"]; have != float64(2) {
		t.Fatalf("have = %v after both approvers now share one email, want 2", have)
	}

	const third = "sub-ap-third"
	e.grant(e.a, types.CapabilitySubjectUser, third)
	e.record(third, shared, []string{apGroupUsers}, nil, false)
	e.approve(e.sessionOf(third, shared), ch.ID, http.StatusOK)
	if got := e.changeState(ch.ID); got != types.GovernanceChangeApplied {
		t.Errorf("state = %q, want applied: the third principal matches no stored email", got)
	}
}

// TestPG_Approvals_ScimRemovalAfterAnApproval: a SCIM group removal clears the recorded roles claim, so a record
// the removal touched reads like one written before the approvals migration. A removal with a blank external id
// truncates the record: the role claim is gone and the approval reads cant_confirm, never role_removed. A removal
// with an external id leaves the record whole, and the role derives from the narrowed groups alone. A record
// truncated at sign-in, whose role lived only in roles, loses it at any removal, a named group included.
func TestPG_Approvals_ScimRemovalAfterAnApproval(t *testing.T) {
	e := newApEnv(t, 2)
	e.seedPeople()
	ctx := context.Background()
	now := time.Now()
	remove := func(sub, externalID string) {
		t.Helper()
		var identity uuid.UUID
		if err := e.pool.QueryRow(ctx, `INSERT INTO principal_identities (principal, issuer, email_lower) VALUES ($1, 'https://idp.example', $2) RETURNING id`,
			sub, strings.TrimPrefix(sub, "sub-")+"@corp.example").Scan(&identity); err != nil {
			t.Fatal(err)
		}
		g, err := e.pg.CreateScimGroup(ctx, externalID, "group "+sub, now)
		if err != nil {
			t.Fatal(err)
		}
		if err := e.pg.AddScimGroupMembers(ctx, g.ID, []uuid.UUID{identity}, now); err != nil {
			t.Fatal(err)
		}
		if err := e.pg.StartGroupRemoval(ctx, g.ID, identity); err != nil {
			t.Fatal(err)
		}
	}
	// held is a change the person approves, then the removal runs, then alice makes the second approval.
	held := func(approver *http.Cookie, removal func()) uuid.UUID {
		t.Helper()
		ch := e.heldOn(e.a2)
		e.approve(approver, ch.ID, http.StatusOK)
		removal()
		e.approve(e.alice, ch.ID, http.StatusOK)
		return ch.ID
	}

	// A blank external id: the record is truncated and the roles are gone.
	e.record(govSubBob, "bob@corp.example", nil, []string{apGroupRoot}, false)
	id := held(e.bob, func() { remove(govSubBob, " ") })
	if got, why := e.changeState(id), e.droppedOf(id, govSubBob); got != types.GovernanceChangePending || why != droppedCannotConfirmed {
		t.Errorf("a removal with a blank external id: state %q dropped %q, want pending and %s", got, why, droppedCannotConfirmed)
	}

	// A named group: the record stays whole and the role still derives from the groups.
	e.record(govSubCarol, "carol@corp.example", []string{"eng-two", apGroupRoot}, []string{apGroupRoot}, false)
	id = held(e.carol, func() { remove(govSubCarol, "eng-two") })
	if got, why := e.changeState(id), e.droppedOf(id, govSubCarol); got != types.GovernanceChangeApplied || why != "" {
		t.Errorf("a removal with a named group: state %q dropped %q, want applied and counted", got, why)
	}

	// Truncated at sign-in, the role only in roles: any removal takes it.
	const late = "sub-ap-scim-late"
	e.record(late, late+"@corp.example", nil, []string{apGroupRoot}, true)
	id = held(ssoSession(t, late, late+"@corp.example", oidc.RoleSecurityAdmin), func() { remove(late, "eng-three") })
	if got, why := e.changeState(id), e.droppedOf(id, late); got != types.GovernanceChangePending || why != droppedCannotConfirmed {
		t.Errorf("a record truncated at sign-in, after a named removal: state %q dropped %q, want pending and %s", got, why, droppedCannotConfirmed)
	}
}

// TestPG_Approvals_TheViewerCannotBeLast: when the change needs a security admin and the next approval would be
// the last, a unit admin sees cannot_be_last, a security admin sees can_approve, and the first approver sees
// that their approval counts.
func TestPG_Approvals_TheViewerCannotBeLast(t *testing.T) {
	e := newApEnv(t, 2)
	e.requireCounts(govKindMembership, 2, true)
	e.seedPeople()
	ch := e.heldOn(e.a2)
	e.approve(e.adminA, ch.ID, http.StatusOK)
	viewer := func(c *http.Cookie) map[string]any {
		t.Helper()
		var m map[string]any
		w := e.call(c, http.MethodGet, "/api/v1/governance/changes/"+ch.ID.String(), "")
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &m) != nil {
			t.Fatalf("GET = %d %s", w.Code, w.Body)
		}
		v, _ := approvalsOf(m)["viewer"].(map[string]any)
		return v
	}
	if v := viewer(e.peerA); v["state"] != viewerCannotBeLast || v["last"] != true {
		t.Errorf("a unit admin as the last seat = %v, want %s and last", v, viewerCannotBeLast)
	}
	if v := viewer(e.alice); v["state"] != viewerCanApprove || v["last"] != true {
		t.Errorf("a security admin as the last seat = %v, want %s and last", v, viewerCanApprove)
	}
	if v := viewer(e.adminA); v["state"] != viewerApproved {
		t.Errorf("the first approver = %v, want %s", v, viewerApproved)
	}
}

// TestPG_Approvals_TakeBackNeverUsesAnUnvouchedEmail: a take-back deletes the caller's own approval. A caller
// whose email claim sign-in does not vouch for, and who shares an approver's stored email, is told they hold no
// approval and nothing is deleted; the approver's own take-back deletes it and the retract row names whose
// approval went.
func TestPG_Approvals_TakeBackNeverUsesAnUnvouchedEmail(t *testing.T) {
	e := newApEnv(t, 2)
	e.seedPeople()
	ch := e.heldOn(e.a2)
	e.approve(e.adminA, ch.ID, http.StatusOK)
	counting := func() int {
		t.Helper()
		var n int
		if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM governance_change_approvals WHERE change_id = $1 AND dropped = ''`, ch.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	const twin = "sub-ap-twin"
	adminEmail := uaSubAdminA + "@corp.example"
	e.grant(e.a, types.CapabilitySubjectUser, twin)
	e.record(twin, adminEmail, []string{apGroupUsers}, nil, false)
	w := e.call(e.sessionOf(twin, adminEmail), http.MethodDelete, approvalPath(ch.ID), "")
	if w.Code != http.StatusNotFound || wireReason(t, w) != reasonGovernanceChangeApprovalNotFound {
		t.Fatalf("a take-back by an unvouched email = %d %s, want 404 %s", w.Code, w.Body, reasonGovernanceChangeApprovalNotFound)
	}
	if n := counting(); n != 1 {
		t.Fatalf("approvals that count = %d after the unvouched take-back, want the approver's own still there", n)
	}

	if w := e.call(e.adminA, http.MethodDelete, approvalPath(ch.ID), ""); w.Code != http.StatusOK {
		t.Fatalf("the approver's own take-back = %d %s, want 200", w.Code, w.Body)
	}
	if n := counting(); n != 0 {
		t.Errorf("approvals that count = %d after the take-back, want none", n)
	}
	rows := e.audits("governance.change.retract")
	if len(rows) != 1 {
		t.Fatalf("%d retract rows, want one", len(rows))
	}
	if d := auditData(t, rows[0]); d["approver"] != uaSubAdminA || d["have"] != float64(0) {
		t.Errorf("retract data = %v, want the deleted approver %s and have 0", d, uaSubAdminA)
	}
}

// TestPG_Approvals_AnInactiveFinalApproverIsRefused: an approver the people directory counts as inactive is
// refused as an approver is, on whatever session they still hold, and writes no row; the page says not_eligible.
func TestPG_Approvals_AnInactiveFinalApproverIsRefused(t *testing.T) {
	e := newApEnv(t, 2)
	e.seedPeople()
	ch := e.heldOn(e.a2)
	e.approve(e.adminA, ch.ID, http.StatusOK)
	e.deactivate(uaSubPeerA)
	if w := e.call(e.peerA, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusForbidden {
		t.Fatalf("an inactive final approver = %d %s, want 403", w.Code, w.Body)
	}
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM governance_change_approvals WHERE change_id = $1 AND approver = $2`, ch.ID, uaSubPeerA).Scan(&n); err != nil || n != 0 {
		t.Errorf("rows for the inactive approver = %d (%v), want none", n, err)
	}
	if got := e.changeState(ch.ID); got != types.GovernanceChangePending {
		t.Errorf("state = %q, want pending", got)
	}
	var view map[string]any
	if err := json.Unmarshal(e.call(e.peerA, http.MethodGet, "/api/v1/governance/changes/"+ch.ID.String(), "").Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if v, _ := approvalsOf(view)["viewer"].(map[string]any); v == nil || v["state"] != viewerNotEligible {
		t.Errorf("viewer = %v, want %s", v, viewerNotEligible)
	}
}

// TestPG_Approvals_EntriesNameAPrincipalNotAnEmail: the entries of the approvals block name each approval's
// principal and carry no directory email, whatever email the approval was given with.
func TestPG_Approvals_EntriesNameAPrincipalNotAnEmail(t *testing.T) {
	e := newApEnv(t, 3)
	e.seedPeople()
	ch := e.heldOn(e.a2)
	e.approve(e.adminA, ch.ID, http.StatusOK)
	e.approve(e.peerA, ch.ID, http.StatusOK)
	entries, _ := approvalsOf(e.read(ch.ID))["entries"].([]any)
	if len(entries) != 2 {
		t.Fatalf("entries = %v, want the two approvals", entries)
	}
	for _, raw := range entries {
		entry, _ := raw.(map[string]any)
		blob, _ := json.Marshal(entry)
		if strings.Contains(string(blob), "@") || entry["email"] != nil {
			t.Errorf("entry %s carries an email", blob)
		}
		if who, _ := entry["approver"].(string); who != uaSubAdminA && who != uaSubPeerA {
			t.Errorf("entry names %q, want a principal", who)
		}
	}
}
