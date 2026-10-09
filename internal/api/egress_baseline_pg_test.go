// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The egress baseline's four-eyes path against a real Postgres: a held replacement of the exact hosts
// and the internal-host marks applies only on a distinct approver's decision, inside the decision
// transaction, without disturbing the rest of the site-config document, and goes stale if either list
// moved after the proposal. And the sidecar's config never carries the mark.
//
// Guarded by WARDYN_TEST_PG (throwawayPGPool): skipped cleanly when unset, must PASS when set.

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestPG_GovernanceChanges_EgressBaselineIsHeldThenApplied(t *testing.T) {
	e := newGovEnv(t)
	ctx := context.Background()
	if _, err := e.pg.PutSiteConfig(ctx, types.SiteConfig{
		UpstreamProxyURL: "http://proxy.corp.example:3128", ScmHosts: []string{"git.corp.example"},
		InternalHosts: []types.InternalHost{{HostSuffix: "corp.example", CIDRs: []string{"10.0.0.0/8"}}, {HostSuffix: "svc.example"}},
	}); err != nil {
		t.Fatal(err)
	}
	state := func() egressBaselineBody {
		sc, err := e.pg.GetSiteConfig(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return egressBaselineState(sc)
	}

	const body = `{"baseline_hosts":["llm.corp.example"],"internal_host_suffixes":["corp.example"]}`
	held := e.pending(e.call(e.alice, http.MethodPut, egressBaselinePath, body))
	if held.TargetKind != govKindEgressBaseline || held.Op != "replace" {
		t.Errorf("held = %s %s", held.TargetKind, held.Op)
	}
	if st := state(); len(st.BaselineHosts) != 0 || len(st.InternalHostSuffixes) != 0 {
		t.Fatalf("a held replacement changed the stored declaration: %+v", st)
	}
	// A suffix naming no stored entry is the direct 400, not a held change.
	if w := e.call(e.carol, http.MethodPut, egressBaselinePath, `{"internal_host_suffixes":["nowhere.example"]}`); w.Code != http.StatusBadRequest {
		t.Errorf("undeclared suffix = %d %s, want 400", w.Code, w.Body)
	}
	// The proposer cannot approve their own change; a distinct security admin can.
	if w := e.call(e.alice, http.MethodPost, "/api/v1/governance/changes/"+held.ID.String()+"/approve", ""); w.Code != http.StatusForbidden {
		t.Errorf("self-approval = %d, want 403", w.Code)
	}
	e.approve(held.ID)
	if st := state(); !slices.Equal(st.BaselineHosts, []string{"llm.corp.example"}) || !slices.Equal(st.InternalHostSuffixes, []string{"corp.example"}) {
		t.Fatalf("stored declaration = %+v after approval", st)
	}
	sc, _ := e.pg.GetSiteConfig(ctx)
	if sc.UpstreamProxyURL != "http://proxy.corp.example:3128" || !slices.Equal(sc.ScmHosts, []string{"git.corp.example"}) ||
		len(sc.InternalHosts) != 2 || !slices.Equal(sc.InternalHosts[0].CIDRs, []string{"10.0.0.0/8"}) || sc.InternalHosts[1].Baseline {
		t.Errorf("the rest of the site config changed: %+v", sc)
	}
	rows := e.audits(egressBaselineWriteName)
	if len(rows) != 1 || rows[0].Actor != govSubBob {
		t.Fatalf("audit rows = %+v, want one by the approver", rows)
	}
	if d := auditData(t, rows[0]); !jsonListIs(d["after"], "llm.corp.example") || !jsonListIs(d["suffixes_added"], "corp.example") ||
		d["proposed_by"] != govSubAlice || d["change_id"] == nil {
		t.Errorf("audit data = %v", d)
	}

	// A change proposed against one declaration goes stale when EITHER list moves first.
	for _, move := range []struct {
		name string
		do   func(sc *types.SiteConfig)
	}{
		{"the exact hosts", func(sc *types.SiteConfig) {
			sc.Egress = &types.SiteEgress{BaselineHosts: []string{"elsewhere.corp.example"}}
		}},
		{"the marks", func(sc *types.SiteConfig) { sc.InternalHosts[0].Baseline, sc.InternalHosts[1].Baseline = false, true }},
	} {
		stale := e.pending(e.call(e.alice, http.MethodPut, egressBaselinePath, `{"baseline_hosts":["b.corp.example"],"internal_host_suffixes":["svc.example"]}`))
		cur, _ := e.pg.GetSiteConfig(ctx)
		before := state()
		move.do(&cur)
		if _, err := e.pg.PutSiteConfig(ctx, cur); err != nil {
			t.Fatal(err)
		}
		moved := state()
		if w := e.call(e.bob, http.MethodPost, "/api/v1/governance/changes/"+stale.ID.String()+"/approve", ""); w.Code == http.StatusOK {
			t.Fatalf("approving a change gone stale on %s = %d, want a refusal", move.name, w.Code)
		}
		if got := state(); !slices.Equal(got.BaselineHosts, moved.BaselineHosts) || !slices.Equal(got.InternalHostSuffixes, moved.InternalHostSuffixes) {
			t.Errorf("stale on %s: declaration %+v, a stale approval changed it (was %+v, moved to %+v)", move.name, got, before, moved)
		}
		e.call(e.bob, http.MethodPost, "/api/v1/governance/changes/"+stale.ID.String()+"/reject", "{}")
	}
}

// Dispatch hands the sidecar the lift and never the mark: the mark is a grading fact, and a sidecar
// image older than wardynd refuses a key it does not know.
func TestPG_DispatchNeverSendsTheBaselineMarkToTheSidecar(t *testing.T) {
	fr := &fakeRunner{}
	srv, _ := pgHarnessWithRunner(t, fr)
	if _, err := srv.cfg.Store.PutSiteConfig(context.Background(), types.SiteConfig{
		InternalHosts: []types.InternalHost{{HostSuffix: "corp.example", CIDRs: []string{"10.0.0.0/8"}, Baseline: true}},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = srv.cfg.Store.PutSiteConfig(context.Background(), types.SiteConfig{}) })

	spec := dispatchAndCaptureSpec(t, srv, fr)
	raw, err := runner.BuildProxyConfig(spec.RunID, spec.ProxyConfig, 3128)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		InternalHosts []map[string]json.RawMessage `json:"internal_hosts"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.InternalHosts) != 1 || string(wire.InternalHosts[0]["host_suffix"]) != `"corp.example"` {
		t.Fatalf("rendered internal_hosts = %v, want the declared lift", wire.InternalHosts)
	}
	if _, ok := wire.InternalHosts[0]["baseline"]; ok {
		t.Errorf("the rendered sidecar config carries the baseline mark: %s", raw)
	}
}
