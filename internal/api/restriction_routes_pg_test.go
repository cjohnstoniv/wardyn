// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestRestrictionRevive: an owner's revive picks up a restriction added after the pause, user-keyed
// and group-keyed; a super admin's revive and an admin bulk restart keep the captured ids and pick up
// one that names the owner's sub, and the ids the revive holds the run to are what it persists.
func TestRestrictionRevive(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	ctx := context.Background()
	captured := seedRestriction(t, e, "rv-captured", types.RestrictionSpec{DeniedDomains: strs("a.example")}, types.RestrictionLimits{}, userMember("sub-rv-r"))
	run, err := e.pg.CreateRun(ctx, types.AgentRun{ID: uuid.New(), CreatedBy: "sub-rv-r", UserType: types.UserTypeStandard, Agent: "claude-code",
		ConfinementClass: types.CC3, State: types.RunRunning, RunnerTarget: "docker", Task: "t", RestrictionIDs: []uuid.UUID{captured.ID}})
	if err != nil {
		t.Fatal(err)
	}
	ids := func(c ownerCeiling) []uuid.UUID { return sortedRestrictionIDs(c.restrictionIDs) }
	late := seedRestriction(t, e, "rv-late-user", types.RestrictionSpec{DeniedDomains: strs("b.example")}, types.RestrictionLimits{}, userMember("sub-rv-r"))
	lateGroup := seedRestriction(t, e, "rv-late-group", types.RestrictionSpec{DeniedDomains: strs("c.example")}, types.RestrictionLimits{},
		types.RestrictionMember{SubjectType: types.CapabilitySubjectGroup, Subject: "rv-eng"})
	want := func(rs ...types.Restriction) []uuid.UUID {
		var out []uuid.UUID
		for _, r := range rs {
			out = append(out, r.ID)
		}
		return sortedRestrictionIDs(out)
	}
	owner := treeCtx("sub-rv-r", []string{"rv-eng"}, "")
	c, rerr := e.srv.reviveCeiling(owner, run, types.ActorHuman, "sub-rv-r")
	if rerr != nil {
		t.Fatalf("owner revive: %+v", rerr)
	}
	if got := ids(c); !slices.Equal(got, want(captured, late, lateGroup)) {
		t.Errorf("owner revive holds %v, want the captured, the late user-keyed and the late group-keyed", got)
	}
	for _, d := range []string{"a.example", "b.example", "c.example"} {
		if !slices.Contains(c.deny, d) {
			t.Errorf("owner revive denies %v, want %s", c.deny, d)
		}
	}
	for name, ctx := range map[string]context.Context{
		"super-admin revive": operatorCtx("sub-rv-super", "rv-super@corp.example", oidc.RoleAdmin),
		"admin bulk restart": context.Background(),
	} {
		c, rerr := e.srv.reviveCeiling(ctx, run, types.ActorHuman, "someone")
		if rerr != nil {
			t.Fatalf("%s: %+v", name, rerr)
		}
		if got := ids(c); !slices.Equal(got, want(captured, late)) {
			t.Errorf("%s holds %v, want the captured and the user-keyed late one (groups are unknowable here)", name, got)
		}
	}
	// The ids a revive holds the run to are written, so the live paths read them next.
	run.RestrictionIDs = c.restrictionIDs
	if rerr := e.srv.saveRevivedRestrictions(ctx, run, []uuid.UUID{captured.ID}); rerr != nil {
		t.Fatalf("persist: %+v", rerr)
	}
	got, err := e.pg.GetRun(ctx, run.ID)
	if err != nil || !slices.Equal(sortedRestrictionIDs(got.RestrictionIDs), want(captured, late, lateGroup)) {
		t.Errorf("persisted ids %v %v, want the owner's revive set", got.RestrictionIDs, err)
	}
	// A restriction deleted since is gone from the revive.
	if _, err := e.pg.DeleteRestriction(ctx, captured.ID); err != nil {
		t.Fatal(err)
	}
	if c, rerr := e.srv.reviveCeiling(owner, run, types.ActorHuman, "sub-rv-r"); rerr != nil || slices.Contains(c.restrictionIDs, captured.ID) {
		t.Errorf("after the delete: %v %+v", c.restrictionIDs, rerr)
	}
}

// TestRestrictionWriteFourEyes: with the switch on, adding a deny or a member is direct and removing
// one, or deleting, is held; with it off every write is direct.
func TestRestrictionWriteFourEyes(t *testing.T) {
	e := newGovEnv(t)
	base := types.RestrictionWrite{Name: "fe-r", Spec: types.RestrictionSpec{DeniedDomains: strs("a.example")},
		Members: []types.RestrictionMember{userMember("sub-fe-1")}}
	w := e.call(e.alice, http.MethodPost, "/api/v1/governance/restrictions", restrictionJSON(t, base))
	if w.Code != http.StatusCreated {
		t.Fatalf("create under the switch: %d %s, want 201 (a create only narrows)", w.Code, w.Body)
	}
	var created types.Restriction
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	put := func(body types.RestrictionWrite) *httptest.ResponseRecorder {
		return e.call(e.alice, http.MethodPut, "/api/v1/governance/restrictions/"+created.ID.String(), restrictionJSON(t, body))
	}
	stricter := base
	stricter.Spec = types.RestrictionSpec{DeniedDomains: strs("a.example", "b.example")}
	stricter.Members = []types.RestrictionMember{userMember("sub-fe-1"), userMember("sub-fe-2")}
	if w := put(stricter); w.Code != http.StatusOK {
		t.Fatalf("adding a deny and a member: %d %s, want 200 direct", w.Code, w.Body)
	}
	looser := stricter
	looser.Spec = types.RestrictionSpec{DeniedDomains: strs("a.example")}
	w = put(looser)
	ch := e.pending(w)
	if ch.TargetKind != govKindRestriction || ch.TargetKey != created.ID.String() {
		t.Errorf("held change %s/%s, want %s/%s", ch.TargetKind, ch.TargetKey, govKindRestriction, created.ID)
	}
	fewer := stricter
	fewer.Members = []types.RestrictionMember{userMember("sub-fe-1")}
	if w := put(fewer); w.Code != http.StatusConflict && w.Code != http.StatusAccepted {
		t.Errorf("removing a member: %d %s, want held (202, or 409 beside the pending change)", w.Code, w.Body)
	}
	e.approve(ch.ID)
	got, err := e.pg.GetRestriction(context.Background(), created.ID)
	if err != nil || len(*got.Spec.DeniedDomains) != 1 {
		t.Errorf("after the approval: %+v %v, want the looser deny list", got.Spec, err)
	}
	del := e.call(e.alice, http.MethodDelete, "/api/v1/governance/restrictions/"+created.ID.String(), "")
	dch := e.pending(del)
	if _, err := e.pg.GetRestriction(context.Background(), created.ID); err != nil {
		t.Errorf("a held delete removed the restriction: %v", err)
	}
	e.approve(dch.ID)
	if _, err := e.pg.GetRestriction(context.Background(), created.ID); err != store.ErrNotFound {
		t.Errorf("after the approved delete: %v, want not found", err)
	}
	if len(e.audits("governance.restriction.delete")) != 1 || len(e.audits("governance.restriction.write")) < 2 {
		t.Errorf("audit rows: write %d delete %d", len(e.audits("governance.restriction.write")), len(e.audits("governance.restriction.delete")))
	}

	t.Setenv(envGovernanceSecondHuman, "")
	off := newGovEnvOn(t, throwawayPGPool(t))
	rec := off.call(off.alice, http.MethodPost, "/api/v1/governance/restrictions", restrictionJSON(t, base))
	var r2 types.Restriction
	_ = json.Unmarshal(rec.Body.Bytes(), &r2)
	if rec.Code != http.StatusCreated {
		t.Fatalf("switch off create: %d %s", rec.Code, rec.Body)
	}
	if rec := off.call(off.alice, http.MethodDelete, "/api/v1/governance/restrictions/"+r2.ID.String(), ""); rec.Code != http.StatusNoContent {
		t.Errorf("switch off delete: %d %s, want 204", rec.Code, rec.Body)
	}
}

// TestRestrictionNotAContext: a restriction is never one of a person's contexts, and cannot be run_in.
func TestRestrictionNotAContext(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	r := seedRestriction(t, e, "nc-r", types.RestrictionSpec{DeniedDomains: strs("a.example")}, types.RestrictionLimits{}, userMember("sub-nc"))
	ctx := treeCtx("sub-nc", nil, "")
	ctxs, err := e.srv.callerContexts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range ctxs {
		if c.UnitID == r.ID || strings.Contains(c.UnitName, "nc-r") {
			t.Errorf("contexts list the restriction: %+v", c)
		}
	}
	if _, err := e.srv.effectiveCeiling(withContextChoice(ctx, contextChoice{RunIn: r.ID})); err == nil {
		t.Error("run_in a restriction id resolved; want errUnitNotAContext")
	}
	w := e.admin(http.MethodPost, "/api/v1/governance/check", `{"user_subjects":["sub-nc"]}`)
	var got governanceCheckResponse
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil {
		t.Fatalf("check = %d %s", w.Code, w.Body)
	}
	for _, c := range got.Contexts {
		if c.UnitID == r.ID {
			t.Errorf("check lists the restriction as a context: %+v", c)
		}
	}
	if got.Effective == nil || !slices.Contains(got.Effective.Ceiling.DeniedDomains, "a.example") {
		t.Errorf("check's effective walls %+v, want the restriction's deny", got.Effective)
	}
}

// TestRestrictionProvenance: a knob a restriction alone sets is recorded with layer restriction and the
// restriction's name, and the run policy read gives its contact.
func TestRestrictionProvenance(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	r := seedRestriction(t, e, "pv-r", types.RestrictionSpec{DeniedDomains: strs("a.example")}, types.RestrictionLimits{}, userMember("sub-pv"))
	c := treeCeiling(t, e, treeCtx("sub-pv", nil, ""))
	knobs := startProvenance(c.provenance())
	c.restrictionRunStep(knobs)
	want := policyref.Provenance{Layer: policyref.LayerRestriction, Name: "pv-r"}
	if knobs["denied_domains"] != want {
		t.Errorf("denied_domains recorded %+v, want %+v", knobs["denied_domains"], want)
	}
	if knobs["allowed_domains"].Layer == policyref.LayerRestriction {
		t.Errorf("allowed_domains recorded as the restriction's, which never set it")
	}
	got := knobs.withContacts(nil, nil, e.srv.restrictionContacts(context.Background(), []uuid.UUID{r.ID}))["denied_domains"]
	if got.Contact == nil || got.Contact.Owner != "pv-r-owner" {
		t.Errorf("contact %+v, want the restriction's", got.Contact)
	}
	ref := e.srv.restrictionAttribution(context.Background(), types.AgentRun{RestrictionIDs: []uuid.UUID{r.ID}})
	if ref == nil || ref.Name != "pv-r" || ref.Source != policyref.SourceUnit {
		t.Errorf("attribution %+v, want the restriction", ref)
	}
}

// TestBreakGlassRestriction: in hard_limits a restriction alone makes an admin-token run a break-glass
// run that needs a reason; deleting it asks for none.
func TestBreakGlassRestriction(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	putBoundary(t, e, testBoundary(types.BoundaryHardLimits))
	post := func() int {
		w := e.admin(http.MethodPost, "/api/v1/runs/preflight", `{"agent":"claude-code","task":"t","inline_policy":{"min_confinement_class":"CC1","allow_all_egress":true}}`)
		if w.Code == http.StatusBadRequest && wireReason(t, w) != reasonBreakGlassReasonRequired {
			t.Fatalf("preflight: %d %s", w.Code, w.Body)
		}
		return w.Code
	}
	if code := post(); code != http.StatusOK {
		t.Fatalf("no restriction, no own-walls unit: %d, want 200", code)
	}
	r := seedRestriction(t, e, "bg-r", types.RestrictionSpec{DeniedDomains: strs("a.example")}, types.RestrictionLimits{}, userMember("sub-bg"))
	if code := post(); code != http.StatusBadRequest {
		t.Errorf("with a restriction and no reason: %d, want 400 %s", code, reasonBreakGlassReasonRequired)
	}
	if _, err := e.pg.DeleteRestriction(context.Background(), r.ID); err != nil {
		t.Fatal(err)
	}
	if code := post(); code != http.StatusOK {
		t.Errorf("after the delete: %d, want 200", code)
	}
}

// TestBreakGlassRestrictionSeededBoundary: the fresh-install seed bounds nothing, so a restriction
// does not make an admin-token run a break-glass run there, as it does not in no_limits.
func TestBreakGlassRestrictionSeededBoundary(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	requireSeeded(t, e)
	seedRestriction(t, e, "bg-seed", types.RestrictionSpec{DeniedDomains: strs("a.example")}, types.RestrictionLimits{}, userMember("sub-seed"))
	w := e.admin(http.MethodPost, "/api/v1/runs", `{"agent":"claude-code","task":"t","inline_policy":{"min_confinement_class":"CC1","allow_all_egress":true}}`)
	var created createRunResponse
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &created) != nil {
		t.Fatalf("admin-token run, seeded boundary, a restriction, no reason: %d %s, want 201", w.Code, w.Body)
	}
	run, err := e.pg.GetRun(context.Background(), created.ID)
	if err != nil || run.BreakGlass || len(e.audits("run.break_glass.open")) != 0 {
		t.Errorf("run break_glass = %v, open rows %d (%v), want an untagged run", run.BreakGlass, len(e.audits("run.break_glass.open")), err)
	}
}

// TestRestrictionWriteRefusesAuthoredGrantFields: a restriction write that names owner_only_refs on a
// withheld grant is refused as a policy write naming it is, with the same sentence. Status and
// reason are each route's own: 400 policy_request_invalid, 422 restriction_invalid.
func TestRestrictionWriteRefusesAuthoredGrantFields(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	pol := e.admin(http.MethodPost, "/api/v1/policies", `{"name":"p-refs","spec":{"min_confinement_class":"CC2","allowed_domains":["github.com"],`+
		`"eligible_grants":[{"kind":"ssh_key","owner_only_refs":["k"],"scope":{"host":"github.com","key_secret_ref":"k","known_hosts_secret_ref":"kh"}}]}}`)
	res := e.admin(http.MethodPost, "/api/v1/governance/restrictions", restrictionJSON(t, types.RestrictionWrite{Name: "r-refs",
		Spec:    types.RestrictionSpec{WithheldGrants: []types.GrantSpec{{Kind: types.GrantSSHKey, OwnerOnlyRefs: []string{"k"}}}},
		Members: []types.RestrictionMember{userMember("sub-refs")}}))
	if pol.Code != http.StatusBadRequest || wireReason(t, pol) != reasonPolicyRequestInvalid {
		t.Fatalf("policy naming owner_only_refs: %d %s, want 400 %s", pol.Code, pol.Body, reasonPolicyRequestInvalid)
	}
	if res.Code != http.StatusUnprocessableEntity || wireReason(t, res) != reasonRestrictionInvalid {
		t.Fatalf("restriction naming owner_only_refs: %d %s, want 422 %s", res.Code, res.Body, reasonRestrictionInvalid)
	}
	const sentence = "owner_only_refs is set by Wardyn at launch, never authored; mark the grant owner_only instead"
	for name, w := range map[string]*httptest.ResponseRecorder{"policy": pol, "restriction": res} {
		if !strings.Contains(w.Body.String(), sentence) {
			t.Errorf("%s refusal %s lacks %q", name, w.Body, sentence)
		}
	}
}

func sortedRestrictionIDs(ids []uuid.UUID) []uuid.UUID {
	return slices.SortedFunc(slices.Values(ids), func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
}

// TestRestrictionPolicyReadNoUnit: a member a restriction alone binds reads the restriction and no
// unit, no unit path and no contexts (the Default unit it rides on is not one of their units).
func TestRestrictionPolicyReadNoUnit(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	seedRestriction(t, e, "pr-all", types.RestrictionSpec{DeniedDomains: strs("a.example")}, types.RestrictionLimits{},
		types.RestrictionMember{SubjectType: types.CapabilitySubjectAll})
	w := e.call(e.user, http.MethodGet, "/api/v1/policies/default", "")
	var got map[string]json.RawMessage
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil {
		t.Fatalf("policies/default = %d %s", w.Code, w.Body)
	}
	if _, ok := got["restrictions"]; !ok {
		t.Errorf("the read omits the restriction: %s", w.Body)
	}
	for _, k := range []string{"unit_name", "unit_path", "contexts"} {
		if v, ok := got[k]; ok && string(v) != "null" && string(v) != `""` && string(v) != "[]" {
			t.Errorf("%s = %s, want absent for a caller only a restriction binds", k, v)
		}
	}
}
