// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// An organization pool's member list holds the organization runners it serves, against a real Postgres.
// Guarded by WARDYN_TEST_PG: skipped cleanly when unset, must PASS when set.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

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
