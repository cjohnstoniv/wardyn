// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// An organization pool's member list holds the organization runners it serves, against a real Postgres.
// Guarded by WARDYN_TEST_PG: skipped cleanly when unset, must PASS when set.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

func TestPG_OrgPoolMembersListTheOrgRunners(t *testing.T) {
	e := newPlacementEnv(t)
	org := e.createPool("Cluster", types.RunnerPoolOrganization)
	cluster := e.orgRunner()
	listPath := "/api/v1/runner-pools/" + org.ID.String() + "/members"
	members := func(c *http.Cookie) []client.RunnerPoolMemberView {
		t.Helper()
		w := e.as(c, http.MethodGet, listPath, "")
		if w.Code != http.StatusOK {
			t.Fatalf("members = %d %s", w.Code, w.Body)
		}
		var out []client.RunnerPoolMemberView
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got := members(nil); len(got) != 0 {
		t.Fatalf("an empty pool lists %+v", got)
	}
	if w := e.admin(http.MethodPut, orgRunnerPath(org.ID, cluster), ""); w.Code != http.StatusCreated {
		t.Fatalf("add the org runner = %d %s", w.Code, w.Body)
	}
	for name, c := range map[string]*http.Cookie{"admin": nil, "member": e.person} {
		got := members(c)
		if len(got) != 1 || got[0].Kind != types.RunnerKindSelfHosted || got[0].ID != cluster.String() || got[0].Name != "cluster" || got[0].Runner == nil {
			t.Fatalf("%s lists %+v, want the org runner as a self-hosted member", name, got)
		}
		r := got[0].Runner
		if r.OwnerKind != types.RunnerOwnerOrg || r.TrustClass != types.RunnerTrustOrgCluster {
			t.Errorf("%s sees owner_kind %q trust %q, want org / org_cluster", name, r.OwnerKind, r.TrustClass)
		}
		if (r.TrustSetBy != "") != (name == "admin") {
			t.Errorf("%s sees trust_set_by %q: the trust writer is the security operator's to read", name, r.TrustSetBy)
		}
	}
	if w := e.admin(http.MethodDelete, orgRunnerPath(org.ID, cluster), ""); w.Code != http.StatusNoContent {
		t.Fatalf("remove = %d %s", w.Code, w.Body)
	}
	if got := members(nil); len(got) != 0 {
		t.Errorf("after the removal the pool lists %+v", got)
	}
}

// The people behind an organization runner (who minted it, trusted it and revoked it, and why) are for a
// security operator, as the administrators' inventory shows them; any other reader of the pool sees the
// runner without them, in the body as sent.
func TestPG_OrgPoolMembersKeepThePeopleBehindARunnerFromMembers(t *testing.T) {
	e := newPlacementEnv(t)
	org := e.createPool("Cluster", types.RunnerPoolOrganization)
	cluster := e.orgRunner()
	if w := e.admin(http.MethodPut, orgRunnerPath(org.ID, cluster), ""); w.Code != http.StatusCreated {
		t.Fatalf("add the org runner = %d %s", w.Code, w.Body)
	}
	const reason = types.RunnerRevokeNotMine
	if _, err := e.pool.Exec(context.Background(), `UPDATE runners SET minted_by = $2, state = 'revoked', revoked_at = now(), revoked_by = $3, revoke_reason = $4 WHERE id = $1`,
		cluster, "minting-admin-subject", "revoking-admin-subject", reason); err != nil {
		t.Fatal(err)
	}
	listPath := "/api/v1/runner-pools/" + org.ID.String() + "/members"

	member := e.call(e.person, http.MethodGet, listPath, "")
	if member.Code != http.StatusOK {
		t.Fatalf("member read = %d %s", member.Code, member.Body)
	}
	for _, leaked := range []string{"minting-admin-subject", "revoking-admin-subject", "sec-test", reason, `"owner"`, `"minted_by`, `"revoked_by`, `"revoke_reason"`, `"trust_set_`} {
		if strings.Contains(member.Body.String(), leaked) {
			t.Errorf("a pool member's read carries %s: %s", leaked, member.Body)
		}
	}
	var seen []client.RunnerPoolMemberView
	if err := json.Unmarshal(member.Body.Bytes(), &seen); err != nil || len(seen) != 1 || seen[0].Runner == nil || seen[0].Runner.State != types.RunnerRevoked {
		t.Fatalf("member read = %v %+v, want the revoked org runner still listed", err, seen)
	}

	inv := e.admin(http.MethodGet, "/api/v1/runners?state=all", "")
	var all []types.RunnerView
	if inv.Code != http.StatusOK || json.Unmarshal(inv.Body.Bytes(), &all) != nil {
		t.Fatalf("inventory = %d %s", inv.Code, inv.Body)
	}
	var want types.RunnerView
	for _, r := range all {
		if r.ID == cluster {
			want = r
		}
	}
	if want.MintedBy != "minting-admin-subject" || want.RevokedBy != "revoking-admin-subject" || want.RevokeReason != reason || want.TrustSetBy == "" {
		t.Fatalf("the inventory shows %+v, want the minter, revoker, reason and trust writer", want)
	}
	var got []client.RunnerPoolMemberView
	op := e.admin(http.MethodGet, listPath, "")
	if op.Code != http.StatusOK || json.Unmarshal(op.Body.Bytes(), &got) != nil || len(got) != 1 || got[0].Runner == nil {
		t.Fatalf("operator read = %d %s", op.Code, op.Body)
	}
	g := got[0].Runner
	if g.Owner != want.Owner || g.MintedBy != want.MintedBy || g.MintedByEmail != "" || g.RevokedBy != want.RevokedBy || g.RevokedByEmail != "" ||
		g.RevokeReason != want.RevokeReason || g.TrustSetBy != want.TrustSetBy || g.TrustSetAt == nil {
		t.Errorf("the operator's member read shows %+v, want what the inventory shows %+v and no email lookup", g, want)
	}
}

const namedUsePolicy = `{"subjects":[{"subject_type":"user","subject":"sub-named"}]}`

// An organization runner's removal from a pool that carries a use policy is held like a remote-hosted
// runner's: otherwise one administrator removes the team's runner, so that no policy pool holds it any
// more, and adds it to an open pool.
func TestPG_OrgRunnerDetachFromAPolicyPoolIsHeld(t *testing.T) {
	e := newPlacementEnvOn(t, true)
	a, open := e.createPool("A", types.RunnerPoolOrganization), e.createPool("Open", types.RunnerPoolOrganization)
	cluster, spare := e.orgRunner(), e.orgRunner()
	// With no policy on the pool the removal is direct.
	if w := e.call(e.super, http.MethodPut, orgRunnerPath(open.ID, spare), ""); w.Code != http.StatusCreated {
		t.Fatalf("add = %d %s", w.Code, w.Body)
	}
	if w := e.call(e.super, http.MethodDelete, orgRunnerPath(open.ID, spare), ""); w.Code != http.StatusNoContent || e.members(open.ID) != 0 {
		t.Fatalf("removal from a pool with no policy = %d %s, want direct 204", w.Code, w.Body)
	}
	if w := e.call(e.super, http.MethodPut, orgRunnerPath(a.ID, cluster), ""); w.Code != http.StatusCreated {
		t.Fatalf("add = %d %s", w.Code, w.Body)
	}
	e.approve(e.pending(e.call(e.alice, http.MethodPut, usePolicyPath(a.ID), namedUsePolicy)).ID)

	// Removing a runner the pool does not hold is not a removal, so it is not held.
	if w := e.call(e.super, http.MethodDelete, orgRunnerPath(a.ID, spare), ""); w.Code != http.StatusNoContent {
		t.Fatalf("removal of a non-member = %d %s, want 204", w.Code, w.Body)
	}
	direct := len(e.audits(auditPoolRunnerRemove))
	held := e.pending(e.call(e.super, http.MethodDelete, orgRunnerPath(a.ID, cluster), ""))
	if held.TargetKind != govKindRunnerPoolUsePolicy || held.TargetKey != a.ID.String() || held.Op != opExecutorAttach {
		t.Errorf("held = %+v", held)
	}
	if n := e.members(a.ID); n != 1 {
		t.Fatalf("a held removal took the member out (%d left)", n)
	}
	// A security admin can neither see nor decide it; a second super admin approves.
	e.approveExecutor(held.ID)
	if n := e.members(a.ID); n != 0 {
		t.Fatalf("the approved removal left %d members", n)
	}
	rows := e.audits(auditPoolRunnerRemove)
	if len(rows) != direct+1 || rows[direct].Actor != poolSubSuperTwo || !strings.Contains(string(rows[direct].Data), cluster.String()) ||
		!strings.Contains(string(rows[direct].Data), held.ID.String()) {
		t.Errorf("removal audit rows = %+v, want one more, by the approver, naming the runner and the change", rows)
	}
	// The admin token removes directly and leaves its break-glass row.
	if w := e.call(e.super, http.MethodPut, orgRunnerPath(a.ID, cluster), ""); w.Code != http.StatusCreated {
		t.Fatalf("re-add = %d %s", w.Code, w.Body)
	}
	before := len(e.audits("governance.change.bypass"))
	if w := e.admin(http.MethodDelete, orgRunnerPath(a.ID, cluster), ""); w.Code != http.StatusNoContent || e.members(a.ID) != 0 {
		t.Fatalf("admin token removal = %d %s", w.Code, w.Body)
	}
	if n := len(e.audits("governance.change.bypass")); n != before+1 {
		t.Errorf("%d bypass rows, want %d", n, before+1)
	}
}

// With the second-human hold off, the removal is direct even from a pool that carries a use policy.
func TestPG_OrgRunnerDetachFromAPolicyPoolIsDirectWhenHoldIsOff(t *testing.T) {
	e := newPlacementEnv(t)
	a := e.createPool("A", types.RunnerPoolOrganization)
	cluster := e.orgRunner()
	if w := e.admin(http.MethodPut, orgRunnerPath(a.ID, cluster), ""); w.Code != http.StatusCreated {
		t.Fatalf("add = %d %s", w.Code, w.Body)
	}
	if w := e.call(e.alice, http.MethodPut, usePolicyPath(a.ID), namedUsePolicy); w.Code != http.StatusOK {
		t.Fatalf("policy = %d %s", w.Code, w.Body)
	}
	if w := e.admin(http.MethodDelete, orgRunnerPath(a.ID, cluster), ""); w.Code != http.StatusNoContent || e.members(a.ID) != 0 {
		t.Fatalf("removal = %d %s, want direct 204", w.Code, w.Body)
	}
	if rows := e.audits(auditPoolRunnerRemove); len(rows) != 1 {
		t.Errorf("%d removal audit rows, want 1", len(rows))
	}
}

// A held removal binds the pool's organization runners: it goes stale when they moved after it was
// proposed, and no removal row is written for it.
func TestPG_OrgRunnerHeldDetachGoesStaleWhenTheMembersMove(t *testing.T) {
	e := newPlacementEnvOn(t, true)
	a := e.createPool("A", types.RunnerPoolOrganization)
	cluster, other := e.orgRunner(), e.orgRunner()
	if w := e.call(e.super, http.MethodPut, orgRunnerPath(a.ID, cluster), ""); w.Code != http.StatusCreated {
		t.Fatalf("add = %d %s", w.Code, w.Body)
	}
	e.approve(e.pending(e.call(e.alice, http.MethodPut, usePolicyPath(a.ID), namedUsePolicy)).ID)
	held := e.pending(e.call(e.super, http.MethodDelete, orgRunnerPath(a.ID, cluster), ""))
	if w := e.call(e.super, http.MethodPut, orgRunnerPath(a.ID, other), ""); w.Code != http.StatusCreated {
		t.Fatalf("add = %d %s", w.Code, w.Body)
	}
	w := e.call(e.superTwo, http.MethodPost, approvePath(held.ID), `{"reason":"ok"}`)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "governance_change_stale") {
		t.Fatalf("approving after a member change = %d %s, want 409 governance_change_stale", w.Code, w.Body)
	}
	if n := e.members(a.ID); n != 2 || len(e.audits(auditPoolRunnerRemove)) != 0 {
		t.Errorf("a stale approval left %d members and wrote removal rows", n)
	}
}

// A use policy proposed on a pool with no organization runners keeps the hash it was proposed on: the
// organization runners are in the state only when there are some.
func TestRunnerPoolUsePolicyStateHashesOrgRunnersOnlyWhenPresent(t *testing.T) {
	subjects := []types.RunnerPoolSubject{{SubjectType: types.CapabilitySubjectUser, Subject: "sub-named"}}
	old := map[string]any{"policy_present": true, "subjects": subjects, "remote_runner_ids": []string{"fake"}}
	if got := computeETag(runnerPoolUsePolicyState(true, subjects, []string{"fake"}, nil)); got != computeETag(old) {
		t.Errorf("state without organization runners = %s, want the hash a pending change was proposed on, %s", got, computeETag(old))
	}
	if got := computeETag(runnerPoolUsePolicyState(true, subjects, []string{"fake"}, []string{uuid.NewString()})); got == computeETag(old) {
		t.Error("an organization runner in the pool does not change the state hash")
	}
}

// The store takes the pool's use-policy target lock before deciding, and a removal decided as direct
// is refused (ErrRunnerPoolGoverned) when a policy has been approved on the pool in between.
func TestPG_OrgRunnerRemovalWaitsOnTheTargetLockAndRefusesAGovernedPool(t *testing.T) {
	e := newPlacementEnv(t)
	a := e.createPool("A", types.RunnerPoolOrganization)
	cluster := e.orgRunner()
	ctx := context.Background()
	if w := e.admin(http.MethodPut, orgRunnerPath(a.ID, cluster), ""); w.Code != http.StatusCreated {
		t.Fatalf("add = %d %s", w.Code, w.Body)
	}
	if _, err := e.pg.PutRunnerPoolUsePolicy(ctx, a.ID, nil, []types.RunnerPoolSubject{{SubjectType: types.CapabilitySubjectUser, Subject: "sub-named"}}, "test"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.pg.RemoveOrgRunnerFromPool(ctx, a.ID, cluster, true); !errors.Is(err, store.ErrRunnerPoolGoverned) {
		t.Fatalf("a removal decided as direct on a governed pool = %v, want ErrRunnerPoolGoverned", err)
	}
	if n := e.members(a.ID); n != 1 {
		t.Fatalf("a refused removal left %d members", n)
	}
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := store.LockGovernanceTarget(ctx, tx, types.GovernanceTargetRunnerPoolUsePolicy, a.ID.String()); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := e.pg.RemoveOrgRunnerFromPool(ctx, a.ID, cluster, false)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("the removal returned (%v) while the policy target lock was held", err)
	case <-time.After(500 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("the removal after the lock was released = %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the removal did not finish after the lock was released")
	}
	if n := e.members(a.ID); n != 0 {
		t.Errorf("%d members after the removal", n)
	}
}
