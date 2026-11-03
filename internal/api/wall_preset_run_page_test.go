// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/ghscope"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// What the run page reads of a run's wall preset beyond the run read's id, name and version: a group preset's
// label, the knob that says which layer refuses every push, and the policy view's unstoppable-push flag.

// TestRunWallPresetGroupLabel: a group preset on the run read carries its group's label, read as the wall
// preset list reads it; an organisation preset and a group without a label carry none.
func TestRunWallPresetGroupLabel(t *testing.T) {
	e := newWallPresetEnv(t)
	ctx := context.Background()
	if _, err := e.ts.PutTemplateGroupAdmin(ctx, types.TemplateGroupAdmin{Group: "eng", Person: "gary", Label: "Engineering", GrantedBy: "admin"}); err != nil {
		t.Fatal(err)
	}
	read := func(p types.WallPreset) *client.RunWallPreset {
		run := types.AgentRun{ID: uuid.New(), CreatedBy: wallsSub, WallPreset: frozenPreset(p)}
		r := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+run.ID.String(), nil).WithContext(memberCtxFor("sub-admin"))
		return e.srv.runWallPresetViewLabelled(r, run)
	}
	// tightPreset gives a group preset the group id "eng".
	if v := read(tightPreset(types.WallPresetScopeGroup, "Eng tight")); v == nil || v.GroupID != "eng" || v.GroupLabel != "Engineering" {
		t.Errorf("a group preset's read = %+v, want group_id eng and group_label Engineering", v)
	}
	if v := read(tightPreset(types.WallPresetScopeOrg, "Org tight")); v == nil || v.GroupLabel != "" {
		t.Errorf("an org preset's read = %+v, want no group_label", v)
	}
	unlabelled := tightPreset(types.WallPresetScopeGroup, "Ops tight")
	unlabelled.GroupID = "ops"
	if v := read(unlabelled); v == nil || v.GroupLabel != "" {
		t.Errorf("a group with no label = %+v, want no group_label", v)
	}
}

// TestWallsSourceNamesDenyAll: the record of what a layer narrowed says, in added.push_rules, that it added
// deny_all, so the run page names the layer that refuses every push; deny_all already on before the layer, or
// push paths alone, add no such entry.
func TestWallsSourceNamesDenyAll(t *testing.T) {
	denyAll := &types.PushRulesSpec{DenyAll: true}
	paths := &types.PushRulesSpec{DenyPaths: []string{"infra/**"}}
	for _, tc := range []struct {
		name          string
		before, after types.RunPolicySpec
		want          []string
	}{
		{"the layer adds deny_all", types.RunPolicySpec{}, types.RunPolicySpec{PushRules: denyAll}, []string{"deny_all"}},
		{"deny_all was already on", types.RunPolicySpec{PushRules: denyAll}, types.RunPolicySpec{PushRules: &types.PushRulesSpec{DenyAll: true, DenyPaths: []string{"a/**"}}}, nil},
		{"push paths alone", types.RunPolicySpec{}, types.RunPolicySpec{PushRules: paths}, nil},
	} {
		rec := newWallsSource(tc.before, tc.after)
		if rec == nil {
			t.Errorf("%s: no record", tc.name)
			continue
		}
		if got := rec.Added["push_rules"]; !slices.Equal(got, tc.want) {
			t.Errorf("%s: added.push_rules = %v, want %v", tc.name, got, tc.want)
		}
		if !slices.Contains(rec.Narrowed, "push_rules") {
			t.Errorf("%s: narrowed = %v, want push_rules", tc.name, rec.Narrowed)
		}
	}
}

// TestRunPolicyViewPushesUnstoppable: the policy view says pushes_unstoppable exactly when the broker is off and the
// recorded capability list leaves out code_write, the decision behind unstoppablePushWarning.
func TestRunPolicyViewPushesUnstoppable(t *testing.T) {
	for _, tc := range []struct {
		name      string
		brokerOff bool
		spec      types.RunPolicySpec
		want      bool
	}{
		{"broker off, GitHub list without code_write", true, types.RunPolicySpec{GitHubCapabilities: []ghscope.Capability{ghscope.CapCodeRead}}, true},
		{"broker on, the same list", false, types.RunPolicySpec{GitHubCapabilities: []ghscope.Capability{ghscope.CapCodeRead}}, false},
		{"broker off, the list names code_write", true, types.RunPolicySpec{GitHubCapabilities: []ghscope.Capability{ghscope.CapCodeRead, ghscope.CapCodeWrite}}, false},
		{"broker off, no list", true, types.RunPolicySpec{}, false},
		// Every push refused outranks it: deny_all refuses a push to a known forge host even with the brokers off.
		{"broker off, the list lacks code_write but deny_all is on", true, types.RunPolicySpec{GitHubCapabilities: []ghscope.Capability{ghscope.CapCodeRead}, PushRules: &types.PushRulesSpec{DenyAll: true}}, false},
	} {
		srv, st := pvFixture(t, &capStore{})
		srv.cfg.DisableGitPATBroker = tc.brokerOff
		run := st.seedRun(pvOwner, types.RunRunning)
		st.rows(run.ID, pvCreate(nil, nil), pvResolve(tc.spec, false))
		_, resp := pvMember(t, pvOwner).get(t, srv, run.ID)
		if resp.PushesUnstoppable != tc.want {
			t.Errorf("%s: pushes_unstoppable = %v, want %v", tc.name, resp.PushesUnstoppable, tc.want)
		}
	}
}

// TestRunPolicyViewHidesPersonalPresetChanges: a person-scope wall preset's change rows (what it removed or added
// at start) are the owner's to see. The owner reads them under cause wall_preset; another reader, who is not even
// told the preset's name, reads none.
func TestRunPolicyViewHidesPersonalPresetChanges(t *testing.T) {
	for _, tc := range []struct {
		name  string
		scope types.WallPresetScope
		other bool
		want  bool
	}{
		{"the owner reads a personal preset's changes", types.WallPresetScopePerson, false, true},
		{"another reader reads none of a personal preset's changes", types.WallPresetScopePerson, true, false},
		{"another reader reads an org preset's changes", types.WallPresetScopeOrg, true, true},
	} {
		srv, st := pvFixture(t, &capStore{})
		run := st.seedRun(pvOwner, types.RunRunning)
		src := pvSource(policyKindInline, nil, "", false, pvSpec([]string{"a.example", "b.example"}, nil))
		src.WallPreset = &wallPresetSourceRecord{ID: "p1", Name: "Mine", Scope: tc.scope, Version: 1,
			WallsSourceRecord: &wallsSourceRecord{Narrowed: []string{"allowed_domains"}, Removed: map[string][]string{"allowed_domains": {"b.example"}}}}
		st.rows(run.ID, pvCreate(nil, src), pvResolve(pvSpec([]string{"a.example"}, nil), false))
		reader := pvMember(t, pvOwner)
		if tc.other {
			reader = pvSecurity(t)
		}
		w, resp := reader.get(t, srv, run.ID)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status %d", tc.name, w.Code)
		}
		if got := pvChange(resp.Changes, causeWallPreset, fieldAllowed) != nil; got != tc.want {
			t.Errorf("%s: a wall_preset change listed = %v, want %v (changes %+v)", tc.name, got, tc.want, resp.Changes)
		}
	}
}
