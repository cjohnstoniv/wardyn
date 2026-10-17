// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// An organization pool's member list holds the organization runners it serves, against a real Postgres.
// Guarded by WARDYN_TEST_PG: skipped cleanly when unset, must PASS when set.

import (
	"encoding/json"
	"net/http"
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
		if r := got[0].Runner; r.OwnerKind != types.RunnerOwnerOrg || r.TrustClass != types.RunnerTrustOrgCluster || r.Owner != "" || r.TrustSetBy != "" {
			t.Errorf("%s sees owner_kind %q trust %q owner %q trust_set_by %q, want org / org_cluster and no administrators' fields", name, r.OwnerKind, r.TrustClass, r.Owner, r.TrustSetBy)
		}
	}
	if w := e.admin(http.MethodDelete, orgRunnerPath(org.ID, cluster), ""); w.Code != http.StatusNoContent {
		t.Fatalf("remove = %d %s", w.Code, w.Body)
	}
	if got := members(nil); len(got) != 0 {
		t.Errorf("after the removal the pool lists %+v", got)
	}
}
