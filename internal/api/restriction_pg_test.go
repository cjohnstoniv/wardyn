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
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The restriction PG pins (the RESTRICT-PIN lane): deny-only lists that follow people, applied at
// DESIGN-TREE §4.1 step 8 on the governed arm only, and re-read by every live path.

func strs(v ...string) *[]string { return &v }

func userMember(sub string) types.RestrictionMember {
	return types.RestrictionMember{SubjectType: types.CapabilitySubjectUser, Subject: sub}
}

// seedRestriction stores a restriction directly.
func seedRestriction(t *testing.T, e *govEnv, name string, spec types.RestrictionSpec, limits types.RestrictionLimits, members ...types.RestrictionMember) types.Restriction {
	t.Helper()
	r, err := e.pg.PutRestriction(context.Background(), types.Restriction{ID: uuid.New(), Name: name, Spec: spec, Limits: limits,
		Contact: &policyref.Contact{Owner: name + "-owner", Email: name + "@corp.example"}, Members: members}, "seed")
	if err != nil {
		t.Fatalf("seed restriction %q: %v", name, err)
	}
	return r
}

func restrictionJSON(t *testing.T, w types.RestrictionWrite) string {
	t.Helper()
	b, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func restrictionIDsOf(c governanceCeiling) []uuid.UUID { return c.RestrictionIDs }

// TestRestrictionDeniesInEveryContext: a member of two units covered by a restriction has its denied
// host in the ceiling under either run_in, and the run captures the restriction's id.
func TestRestrictionDeniesInEveryContext(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	a := e.seedProfile("rp-a", "pypi.org", "github.com")
	b := e.seedProfile("rp-b", "pypi.org", "github.com")
	treeMember(t, e, a, types.CapabilitySubjectUser, "sub-rp-ctx", 5, false)
	treeMember(t, e, b, types.CapabilitySubjectUser, "sub-rp-ctx", 1, false)
	r := seedRestriction(t, e, "rp-deny-pypi", types.RestrictionSpec{DeniedDomains: strs("pypi.org")}, types.RestrictionLimits{}, userMember("sub-rp-ctx"))
	for name, unit := range map[string]types.GovernanceUnit{"a": a, "b": b} {
		ctx := withContextChoice(treeCtx("sub-rp-ctx", nil, ""), contextChoice{RunIn: unit.ID})
		c := treeCeiling(t, e, ctx)
		if !slices.Contains(c.Spec.DeniedDomains, "pypi.org") {
			t.Errorf("run_in %s: denied %v, want pypi.org", name, c.Spec.DeniedDomains)
		}
		if unitIDOf(c) != unit.ID {
			t.Errorf("run_in %s resolved unit %s, want %s: a restriction never moves the context", name, unitIDOf(c), unit.ID)
		}
		if ids := restrictionIDsOf(c); len(ids) != 1 || ids[0] != r.ID {
			t.Errorf("run_in %s: restriction ids %v, want [%s]", name, ids, r.ID)
		}
		var run types.AgentRun
		e.srv.captureRunLimits(&run, c, nil)
		if len(run.RestrictionIDs) != 1 || run.RestrictionIDs[0] != r.ID {
			t.Errorf("run_in %s: the run captured %v, want [%s]", name, run.RestrictionIDs, r.ID)
		}
	}
	// A person it does not cover is untouched.
	other := treeCeiling(t, e, withContextChoice(treeCtx("sub-rp-other", nil, ""), contextChoice{}))
	if len(other.RestrictionIDs) != 0 || slices.Contains(other.Spec.DeniedDomains, "pypi.org") {
		t.Errorf("an uncovered person got %v / %v", other.RestrictionIDs, other.Spec.DeniedDomains)
	}
}

// TestRestrictionAllowlistRefused: a restriction cannot carry an allowlist, a credential grant list, an
// allow-all, or a true boolean; nothing is stored.
func TestRestrictionAllowlistRefused(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	for name, body := range map[string]string{
		"allowed_domains":   `{"name":"x","spec":{"allowed_domains":["a.example"]},"limits":{},"members":[]}`,
		"eligible_grants":   `{"name":"x","spec":{"eligible_grants":[{"kind":"ssh_key"}]},"limits":{},"members":[]}`,
		"allow_all_egress":  `{"name":"x","spec":{"allow_all_egress":true},"limits":{},"members":[]}`,
		"git_push_any":      `{"name":"x","spec":{"git_push_any_branch":true},"limits":{},"members":[]}`,
		"allow_no_end true": `{"name":"x","spec":{},"limits":{"allow_no_end":true},"members":[]}`,
	} {
		w := e.admin(http.MethodPost, "/api/v1/governance/restrictions", body)
		if w.Code != http.StatusUnprocessableEntity || wireReason(t, w) != reasonRestrictionInvalid {
			t.Errorf("%s: %d %s, want 422 %s", name, w.Code, w.Body, reasonRestrictionInvalid)
		}
	}
	list, err := e.pg.ListRestrictions(context.Background())
	if err != nil || len(list) != 0 {
		t.Errorf("stored restrictions after the refusals: %v %v, want none", list, err)
	}
}

// TestRestrictionDefaultMemberDoorsClose: a person in no unit but covered by a restriction runs in the
// Default unit with it, and the restriction's deny_interactive closes the run's door.
func TestRestrictionDefaultMemberDoorsClose(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	r := seedRestriction(t, e, "rp-no-shell", types.RestrictionSpec{}, types.RestrictionLimits{DenyInteractive: new(true)}, userMember("sub-rp-def"))
	c := treeCeiling(t, e, treeCtx("sub-rp-def", nil, ""))
	if unitIDOf(c) != store.DefaultUnitID || !c.Limits.DenyInteractive {
		t.Fatalf("ceiling unit %s deny_interactive %v, want Default and true", unitIDOf(c), c.Limits.DenyInteractive)
	}
	run := types.AgentRun{ID: uuid.New(), CreatedBy: "sub-rp-def"}
	e.srv.captureRunLimits(&run, c, nil)
	if run.UnitID == nil || *run.UnitID != store.DefaultUnitID || len(run.RestrictionIDs) != 1 || run.RestrictionIDs[0] != r.ID {
		t.Fatalf("captured unit %v restrictions %v", run.UnitID, run.RestrictionIDs)
	}
	p, err := e.srv.interactiveDeniedBy(context.Background(), run)
	if err != nil || p == nil {
		t.Errorf("the door for a Default-unit run under deny_interactive: %v %v, want it closed", p, err)
	}
	// Deleting the restriction opens the door: the doors read it live.
	if _, err := e.pg.DeleteRestriction(context.Background(), r.ID); err != nil {
		t.Fatal(err)
	}
	if p, err := e.srv.interactiveDeniedBy(context.Background(), run); err != nil || p != nil {
		t.Errorf("after the delete: %v %v, want the door open", p, err)
	}
}

// TestRestrictionSkipsUngovernedAndBreakGlass: an operator and the admin token are never narrowed by a
// restriction, and capture none; the boundary still binds them.
func TestRestrictionSkipsUngovernedAndBreakGlass(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	seedRestriction(t, e, "rp-all", types.RestrictionSpec{DeniedDomains: strs("pypi.org")}, types.RestrictionLimits{},
		types.RestrictionMember{SubjectType: types.CapabilitySubjectAll}, userMember("sub-rp-op"))
	op := operatorCtx("sub-rp-op", "rp-op@corp.example", oidc.RoleAdmin)
	for name, ctx := range map[string]context.Context{"operator": op, "admin token": context.Background()} {
		c := treeCeiling(t, e, ctx)
		if len(c.RestrictionIDs) != 0 || slices.Contains(c.Spec.DeniedDomains, "pypi.org") {
			t.Errorf("%s: restrictions %v denied %v, want none of the restriction", name, c.RestrictionIDs, c.Spec.DeniedDomains)
		}
		if !c.Operator {
			t.Errorf("%s: not an operator ceiling", name)
		}
	}
	putBoundary(t, e, testBoundary(types.BoundaryHardLimits))
	if c := treeCeiling(t, e, op); !slices.Contains(c.Spec.DeniedDomains, "evil.example") {
		t.Errorf("the boundary no longer binds the operator: %v", c.Spec.DeniedDomains)
	}
}

// TestRestrictionReReadAtQueuedStart: a restriction added while a run waits is applied and captured
// when it starts, and does not read as a changed profile.
func TestRestrictionReReadAtQueuedStart(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	start := func() (governanceCeiling, *queueEnd) {
		t.Helper()
		auth, _ := json.Marshal(queueAuthority{Sub: "sub-rp-q", Role: "user", UserType: types.UserTypeStandard, Groups: []string{}})
		raw, _ := json.Marshal(queuedLaunch{Request: json.RawMessage(`{}`)})
		run := types.AgentRun{ID: uuid.New(), Agent: "claude-code", CreatedBy: "sub-rp-q"}
		st, err := newQueuedStart(context.Background(), context.Background(), store.PromotedRun{Run: run, Entry: store.RunQueueEntry{Authority: auth, Launch: raw}})
		if err != nil {
			t.Fatal(err)
		}
		return e.srv.queueGovernance(st, createRunRequest{Agent: "claude-code"})
	}
	if c, end := start(); end != nil || len(c.RestrictionIDs) != 0 {
		t.Fatalf("before the restriction: %v %+v", c.RestrictionIDs, end)
	}
	r := seedRestriction(t, e, "rp-late", types.RestrictionSpec{DeniedDomains: strs("pypi.org")}, types.RestrictionLimits{}, userMember("sub-rp-q"))
	c, end := start()
	if end != nil {
		t.Fatalf("a restriction added while queued ended the start: %+v", end)
	}
	if len(c.RestrictionIDs) != 1 || c.RestrictionIDs[0] != r.ID {
		t.Errorf("captured at the start: %v, want [%s]", c.RestrictionIDs, r.ID)
	}
}

// TestRestrictionStaleGroupsFailClosed: with a snapshot that cannot answer groups, a group-keyed
// restriction refuses the resolve; with only user-keyed ones it resolves.
func TestRestrictionStaleGroupsFailClosed(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	seedRestriction(t, e, "rp-user", types.RestrictionSpec{DeniedDomains: strs("pypi.org")}, types.RestrictionLimits{}, userMember("sub-rp-stale"))
	stale := memberCtxFor("sub-rp-stale") // a cookie with no group snapshot
	if c, err := e.srv.effectiveCeiling(stale); err != nil || !slices.Contains(c.Spec.DeniedDomains, "pypi.org") {
		t.Fatalf("user-keyed restriction under a stale snapshot: %v %v, want it applied", c.Spec.DeniedDomains, err)
	}
	seedRestriction(t, e, "rp-group", types.RestrictionSpec{DeniedDomains: strs("github.com")}, types.RestrictionLimits{},
		types.RestrictionMember{SubjectType: types.CapabilitySubjectGroup, Subject: "rp-eng"})
	if _, err := e.srv.effectiveCeiling(stale); err == nil || err != errGroupsSnapshotStale {
		t.Errorf("group-keyed restriction under a stale snapshot: %v, want %v", err, errGroupsSnapshotStale)
	}
	if c, err := e.srv.effectiveCeiling(treeCtx("sub-rp-stale", []string{"rp-eng"}, "")); err != nil ||
		!slices.Contains(c.Spec.DeniedDomains, "github.com") || !slices.Contains(c.Spec.DeniedDomains, "pypi.org") {
		t.Errorf("with the groups known: %v %v, want both denies", c.Spec.DeniedDomains, err)
	}
}

// TestRestrictionSealAndMint: a host a restriction denies is sealed out of a running run's policy and
// withheld at a live mint, and a host added to the restriction later reaches the run.
func TestRestrictionSealAndMint(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	ctx := context.Background()
	r := seedRestriction(t, e, "rp-seal", types.RestrictionSpec{DeniedDomains: strs("pypi.org")}, types.RestrictionLimits{}, userMember("sub-rp-seal"))
	run := types.AgentRun{ID: uuid.New(), CreatedBy: "sub-rp-seal", RestrictionIDs: []uuid.UUID{r.ID}}
	policy := types.RunPolicySpec{AllowedDomains: []string{"pypi.org", "github.com"}, MinConfinementClass: types.CC2}
	prov := knobProvenance{}
	e.srv.sealRunPolicy(ctx, run, &policy, prov)
	if !slices.Contains(policy.DeniedDomains, "pypi.org") {
		t.Errorf("sealed policy denies %v, want pypi.org", policy.DeniedDomains)
	}
	if got := prov["denied_domains"]; got.Layer != policyref.LayerRestriction || got.Name != "rp-seal" {
		t.Errorf("the seal recorded %+v for denied_domains, want the restriction", got)
	}
	w, err := e.srv.liveWithholding(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	if !w.withheldAny([]string{"pypi.org"}) || w.withheldAny([]string{"github.com"}) {
		t.Errorf("live withholding: pypi.org %v github.com %v, want true false", w.withheldAny([]string{"pypi.org"}), w.withheldAny([]string{"github.com"}))
	}
	r.Spec.DeniedDomains = strs("pypi.org", "github.com")
	if _, err := e.pg.PutRestriction(ctx, r, "seed"); err != nil {
		t.Fatal(err)
	}
	if w, err = e.srv.liveWithholding(ctx, run); err != nil || !w.withheldAny([]string{"github.com"}) {
		t.Errorf("after the restriction gained github.com: %v %v, want it withheld at the mint", w, err)
	}
	// A run that captured none is not narrowed.
	if w, err := e.srv.liveWithholding(ctx, types.AgentRun{ID: uuid.New()}); err != nil || w.withheldAny([]string{"pypi.org"}) {
		t.Errorf("a run with no restriction: %v %v", w, err)
	}
}

// TestRestrictionApprovalRefused: a security admin cannot approve, through the route, a host a
// restriction on the run's owner denies.
func TestRestrictionApprovalRefused(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	r := seedRestriction(t, e, "rp-appr", types.RestrictionSpec{DeniedDomains: strs("pypi.org")}, types.RestrictionLimits{}, userMember("sub-rp-appr"))
	run, err := e.pg.CreateRun(context.Background(), types.AgentRun{ID: uuid.New(), CreatedBy: "sub-rp-appr", Agent: "claude-code",
		ConfinementClass: types.CC3, State: types.RunRunning, RunnerTarget: "docker", Task: "t", RestrictionIDs: []uuid.UUID{r.ID}})
	if err != nil {
		t.Fatal(err)
	}
	ask := func(host string) uuid.UUID {
		ap, err := e.h.approvals.Request(context.Background(), types.ApprovalRequest{RunID: run.ID, Kind: types.ApprovalEgressDomain,
			RequestedScope: json.RawMessage(`{"host":"` + host + `"}`)})
		if err != nil {
			t.Fatal(err)
		}
		return ap.ID
	}
	decider := e.otherServer(func(c *Config) { c.Approvals = e.h.approvals })
	approve := func(id uuid.UUID) *httptest.ResponseRecorder {
		return doSSO(t, decider, http.MethodPost, "/api/v1/approvals/"+id.String()+"/approve", e.alice, `{}`)
	}
	w := approve(ask("PyPI.org"))
	if w.Code != http.StatusForbidden || wireReason(t, w) != reasonRestrictionHost {
		t.Errorf("approving a denied host: %d %s, want 403 %s", w.Code, w.Body, reasonRestrictionHost)
	}
	if !strings.Contains(w.Body.String(), "rp-appr") || !strings.Contains(w.Body.String(), "rp-appr-owner") {
		t.Errorf("the refusal %s does not name the restriction and its contact", w.Body)
	}
	if w := approve(ask("github.com")); w.Code != http.StatusOK {
		t.Errorf("approving a host the restriction does not deny: %d %s, want 200", w.Code, w.Body)
	}
}

// TestRestrictionSweepReclamps: lowering a restriction's run-length maximum shortens a live run that
// captured it, in the Default unit included.
func TestRestrictionSweepReclamps(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	ctx := context.Background()
	hour := 3600
	r := seedRestriction(t, e, "rp-sweep", types.RestrictionSpec{}, types.RestrictionLimits{MaxEndAheadSec: new(24 * hour)}, userMember("sub-rp-sweep"))
	end := time.Now().UTC().Add(20 * time.Hour)
	def := store.DefaultUnitID
	run, err := e.pg.CreateRun(ctx, types.AgentRun{ID: uuid.New(), CreatedBy: "sub-rp-sweep", Agent: "claude-code",
		ConfinementClass: types.CC3, State: types.RunRunning, RunnerTarget: "docker", Task: "t", EndsAt: &end, UnitID: &def,
		RunLimits: types.RunLimits{MaxEndAheadSec: 24 * hour}, RestrictionIDs: []uuid.UUID{r.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.srv.sweepRunLimits(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.pg.GetRun(ctx, run.ID); got.EndsAt == nil || !got.EndsAt.Equal(*run.EndsAt) {
		t.Fatalf("an unchanged restriction moved the end to %v, want %v", got.EndsAt, run.EndsAt)
	}
	r.Limits.MaxEndAheadSec = new(2 * hour)
	if _, err := e.pg.PutRestriction(ctx, r, "seed"); err != nil {
		t.Fatal(err)
	}
	if err := e.srv.sweepRunLimits(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := e.pg.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if limit := time.Now().Add(2*time.Hour + time.Minute); got.EndsAt == nil || got.EndsAt.After(limit) || got.RunLimits.MaxEndAheadSec != 2*hour {
		t.Errorf("after the restriction tightened: ends %v max %d, want within 2h", got.EndsAt, got.RunLimits.MaxEndAheadSec)
	}
}
