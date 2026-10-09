// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The egress baseline's four-eyes path against a real Postgres: a held replacement applies only on a
// distinct approver's decision, inside the decision transaction, without disturbing the rest of the
// site-config document, and goes stale if the set moved after the proposal.
//
// Guarded by WARDYN_TEST_PG (throwawayPGPool): skipped cleanly when unset, must PASS when set.

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestPG_GovernanceChanges_EgressBaselineIsHeldThenApplied(t *testing.T) {
	e := newGovEnv(t)
	ctx := context.Background()
	if _, err := e.pg.PutSiteConfig(ctx, types.SiteConfig{
		UpstreamProxyURL: "http://proxy.corp.example:3128", ScmHosts: []string{"git.corp.example"},
	}); err != nil {
		t.Fatal(err)
	}
	hosts := func() []string {
		sc, err := e.pg.GetSiteConfig(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return egressBaselineHosts(sc)
	}

	held := e.pending(e.call(e.alice, http.MethodPut, egressBaselinePath, `{"baseline_hosts":["llm.corp.example"]}`))
	if held.TargetKind != govKindEgressBaseline || held.Op != "replace" {
		t.Errorf("held = %s %s", held.TargetKind, held.Op)
	}
	if len(hosts()) != 0 {
		t.Fatal("a held replacement changed the stored baseline")
	}
	// The proposer cannot approve their own change; a distinct security admin can.
	if w := e.call(e.alice, http.MethodPost, "/api/v1/governance/changes/"+held.ID.String()+"/approve", ""); w.Code != http.StatusForbidden {
		t.Errorf("self-approval = %d, want 403", w.Code)
	}
	e.approve(held.ID)
	if got := hosts(); !slices.Equal(got, []string{"llm.corp.example"}) {
		t.Fatalf("stored baseline = %v after approval", got)
	}
	sc, _ := e.pg.GetSiteConfig(ctx)
	if sc.UpstreamProxyURL != "http://proxy.corp.example:3128" || !slices.Equal(sc.ScmHosts, []string{"git.corp.example"}) {
		t.Errorf("the rest of the site config changed: %+v", sc)
	}
	rows := e.audits(egressBaselineWriteName)
	if len(rows) != 1 || rows[0].Actor != govSubBob {
		t.Fatalf("audit rows = %+v, want one by the approver", rows)
	}
	if d := auditData(t, rows[0]); !jsonListIs(d["after"], "llm.corp.example") || !jsonListIs(d["added"], "llm.corp.example") ||
		d["proposed_by"] != govSubAlice || d["change_id"] == nil {
		t.Errorf("audit data = %v", d)
	}

	// A change proposed against one set goes stale when the set moves first.
	stale := e.pending(e.call(e.alice, http.MethodPut, egressBaselinePath, `{"baseline_hosts":["llm.corp.example","b.corp.example"]}`))
	if _, err := e.pg.PutSiteConfig(ctx, func() types.SiteConfig {
		sc, _ := e.pg.GetSiteConfig(ctx)
		sc.Egress = &types.SiteEgress{BaselineHosts: []string{"elsewhere.corp.example"}}
		return sc
	}()); err != nil {
		t.Fatal(err)
	}
	if w := e.call(e.bob, http.MethodPost, "/api/v1/governance/changes/"+stale.ID.String()+"/approve", ""); w.Code == http.StatusOK {
		t.Fatalf("approving a stale change = %d, want a refusal", w.Code)
	}
	if got := hosts(); !slices.Equal(got, []string{"elsewhere.corp.example"}) {
		t.Errorf("stored baseline = %v, a stale approval changed it", got)
	}
}
