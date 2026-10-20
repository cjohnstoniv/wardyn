// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/testutil"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The Wardyn boundary's PG pins (#2945, #2946): every policy path is bounded in hard_limits, nothing
// changes in no_limits, the dry run reports exactly what hard_limits does, the admin token's
// break-glass run, the egress approval bound and the second human.

const boundaryMaxEnd = 3600

// testBoundary is the boundary the pins turn on: an allowlist the corpus profile and stored policy are
// wider than, a deny, a raised floor and a run-limit maximum.
func testBoundary(mode types.BoundaryMode) types.BoundaryWrite {
	allowed := []string{"api.anthropic.com", "pypi.org", "github.com"}
	denied := []string{"evil.example"}
	cc := types.CC3
	end := boundaryMaxEnd
	return types.BoundaryWrite{Mode: mode,
		Spec:   types.BoundarySpec{AllowedDomains: &allowed, DeniedDomains: &denied, MinConfinementClass: &cc},
		Limits: types.BoundaryLimits{MaxEndAheadSec: &end}}
}

func putBoundary(t *testing.T, e *govEnv, w types.BoundaryWrite) {
	t.Helper()
	if _, err := e.pg.PutBoundary(context.Background(), w, "seed"); err != nil {
		t.Fatalf("put boundary: %v", err)
	}
}

func boundaryJSON(w types.BoundaryWrite) string {
	b, _ := json.Marshal(w)
	return string(b)
}

// withinBoundary reports what about spec exceeds testBoundary, or "".
func withinBoundary(spec types.RunPolicySpec) string {
	w := testBoundary(types.BoundaryHardLimits)
	switch {
	case spec.AllowAllEgress:
		return "allow_all_egress is on"
	case spec.MinConfinementClass.Rank() < types.CC3.Rank():
		return "confinement floor " + string(spec.MinConfinementClass) + " is below CC3"
	case !slices.Contains(spec.DeniedDomains, "evil.example"):
		return "denied_domains lacks evil.example"
	}
	for _, d := range spec.AllowedDomains {
		if !ceilingDenies(*w.Spec.AllowedDomains, d) {
			return "allowed_domains carries " + d
		}
	}
	return ""
}

// TestBoundaryBindEveryPolicyPath: in hard_limits, the profile chain, the member pipeline, a
// stored policy an unassigned member selects, the deployment default, a super admin's runs and the
// admin token's run without a break-glass reason all resolve inside them.
func TestBoundaryBindEveryPolicyPath(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	c := seedBoundaryCorpus(t, e)
	putBoundary(t, e, testBoundary(types.BoundaryHardLimits))
	for _, wc := range boundaryCases() {
		t.Run(wc.name, func(t *testing.T) {
			got := resolveBoundaryCase(t, e.srv, c, wc)
			if !got.OK {
				t.Fatal("the boundary refused the run; it must narrow it")
			}
			if why := withinBoundary(got.Spec); why != "" {
				t.Errorf("run policy exceeds the boundary: %s (%+v)", why, got.Spec)
			}
			if why := withinBoundary(got.CeilingSpec); why != "" {
				t.Errorf("ceiling exceeds the boundary: %s", why)
			}
			if got.CeilingLimits.MaxEndAheadSec != boundaryMaxEnd {
				t.Errorf("ceiling max_end_ahead_sec = %d, want the boundary's %d", got.CeilingLimits.MaxEndAheadSec, boundaryMaxEnd)
			}
			if !slices.ContainsFunc(append(got.Warnings, got.CeilingWarnings...), func(s string) bool { return strings.HasPrefix(s, "the Wardyn boundary narrowed ") }) {
				t.Errorf("no boundary warning: run %q ceiling %q", got.Warnings, got.CeilingWarnings)
			}
		})
	}
}

// TestBoundaryProvenanceNamesTheBoundary: a value the boundary decided records layer boundary, whether the
// boundary narrowed the ceiling it came from or the run's own policy.
func TestBoundaryProvenanceNamesTheBoundary(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	c := seedBoundaryCorpus(t, e)
	putBoundary(t, e, testBoundary(types.BoundaryHardLimits))
	for _, wc := range boundaryCases() {
		if wc.name != "profile_chain_default" && wc.name != "stored_policy_unassigned" {
			continue
		}
		req := wc.req(c)
		_, _, _, src, ok := e.srv.resolveRunPolicy(wc.ctx, httptest.NewRecorder(), governReq(wc.ctx), &req, true)
		if !ok {
			t.Fatalf("%s: refused", wc.name)
		}
		for _, knob := range []string{"allowed_domains", "min_confinement_class"} {
			if got := src.knobs[knob]; got.Layer != policyref.LayerBoundary || got.Locked {
				t.Errorf("%s: provenance[%s] = %+v, want layer boundary, unlocked", wc.name, knob, got)
			}
		}
	}
}

// TestBoundaryNoLimitsIsByteIdentical: with a fresh install's seeded row, with the upgrade posture SetBoundaryNoLimits
// writes, and with a no_limits row that keeps a drafted spec, the corpus resolves to the golden
// recorded on a tree without the boundary.
func TestBoundaryNoLimitsIsByteIdentical(t *testing.T) {
	pool := throwawayPGPool(t)
	e := newGovEnvOn(t, pool)
	c := seedBoundaryCorpus(t, e)
	compareOrUpdateGolden(t, "testdata/boundary_no_limits_golden.json", resolveBoundaryCorpus(t, e.srv, c))
	testutil.SetBoundaryNoLimits(t, pool)
	compareOrUpdateGolden(t, "testdata/boundary_no_limits_golden.json", resolveBoundaryCorpus(t, e.srv, c))
	putBoundary(t, e, testBoundary(types.BoundaryNoLimits))
	compareOrUpdateGolden(t, "testdata/boundary_no_limits_golden.json", resolveBoundaryCorpus(t, e.srv, c))
}

// knobJSON is one top-level knob of spec as the dry run and provenance compare it.
func knobJSON(t *testing.T, spec types.RunPolicySpec, knob string) string {
	t.Helper()
	v := reflect.ValueOf(canonicalKnobs(redactSpecForRead(spec, true)))
	i := slices.Index(policyKnobFields, knob)
	if i < 0 {
		t.Fatalf("no knob %q", knob)
	}
	b, _ := json.Marshal(v.Field(i).Interface())
	return string(b)
}

// TestBoundaryDryRunEqualsTheOnSwitch: every change the dry run lists for the deployment, a profile
// and a stored policy is the value the resolve gives once the same boundary is in hard_limits, and a knob it does
// not list is unchanged.
func TestBoundaryDryRunEqualsTheOnSwitch(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	c := seedBoundaryCorpus(t, e)
	w := e.call(e.alice, http.MethodPost, "/api/v1/governance/dry-run", boundaryJSON(testBoundary(types.BoundaryHardLimits)))
	if w.Code != http.StatusOK {
		t.Fatalf("dry run = %d %s", w.Code, w.Body)
	}
	var report boundaryReport
	if err := json.Unmarshal(w.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Profiles) != 1 || report.Profiles[0].ID != c.profile.ID || len(report.StoredPolicies) != 1 ||
		report.StoredPolicies[0].ID != c.open.ID || len(report.Assignments) != 1 || report.Assignments[0].Subject != boundaryMemberSub {
		t.Fatalf("report rows = %s", w.Body)
	}
	cases := map[string]boundaryCase{}
	for _, wc := range boundaryCases() {
		cases[wc.name] = wc
	}
	off := map[string]boundaryResolved{}
	for _, name := range []string{"profile_chain_default", "deployment_default_unassigned", "stored_policy_unassigned"} {
		off[name] = resolveBoundaryCase(t, e.srv, c, cases[name])
	}
	putBoundary(t, e, testBoundary(types.BoundaryHardLimits))
	check := func(label string, changes []boundaryChange, before, after types.RunPolicySpec) {
		t.Helper()
		listed := map[string]bool{}
		for _, ch := range changes {
			if strings.HasPrefix(ch.Knob, "limits.") {
				continue
			}
			listed[ch.Knob] = true
			want, _ := json.Marshal(ch.After)
			if got := knobJSON(t, after, ch.Knob); got != string(want) {
				t.Errorf("%s %s: report says %s, hard_limits resolves %s", label, ch.Knob, want, got)
			}
		}
		for _, knob := range policyKnobFields {
			if knob != "" && !listed[knob] && knobJSON(t, before, knob) != knobJSON(t, after, knob) {
				t.Errorf("%s %s changed but the report does not list it", label, knob)
			}
		}
	}
	on := resolveBoundaryCase(t, e.srv, c, cases["profile_chain_default"])
	check("profile", report.Profiles[0].Changes, off["profile_chain_default"].CeilingSpec, on.CeilingSpec)
	on = resolveBoundaryCase(t, e.srv, c, cases["deployment_default_unassigned"])
	check("deployment", report.Deployment, off["deployment_default_unassigned"].CeilingSpec, on.CeilingSpec)
	on = resolveBoundaryCase(t, e.srv, c, cases["stored_policy_unassigned"])
	check("stored policy", report.StoredPolicies[0].Changes, off["stored_policy_unassigned"].Spec, on.Spec)
}

// TestBoundaryBreakGlass: in hard_limits, with a profile whose walls it would lift, the admin
// token's run needs a reason; with one it is tagged, audited, capped at the boundary's run-limit
// maximum and still bound by the boundary.
func TestBoundaryBreakGlass(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	e.seedProfile("team", "pypi.org")
	putBoundary(t, e, testBoundary(types.BoundaryHardLimits))
	body := `{"agent":"claude-code","task":"t","ends_after_sec":86400,"inline_policy":{"min_confinement_class":"CC2","allowed_domains":["api.anthropic.com"]}}`
	create := func(reason string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+adminToken)
		if reason != "" {
			r.Header.Set(breakGlassReasonHeader, reason)
		}
		w := httptest.NewRecorder()
		panicFails(t, e.srv.Handler()).ServeHTTP(w, r)
		return w
	}
	for _, bad := range []string{"", "   ", "line\nbreak"} {
		if w := create(bad); w.Code != http.StatusBadRequest || wireReason(t, w) != reasonBreakGlassReasonRequired {
			t.Errorf("reason %q: %d %s, want 400 %s", bad, w.Code, w.Body, reasonBreakGlassReasonRequired)
		}
	}
	w := create("prod outage INC-1")
	if w.Code != http.StatusCreated {
		t.Fatalf("break-glass create = %d %s", w.Code, w.Body)
	}
	var created createRunResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	run, err := e.pg.GetRun(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !run.BreakGlass || !created.BreakGlass {
		t.Errorf("run row break_glass = %v (wire %v), want true", run.BreakGlass, created.BreakGlass)
	}
	if run.EndsAt == nil || run.EndsAt.Sub(run.CreatedAt) > boundaryMaxEnd*time.Second {
		t.Errorf("ends_at = %v from %v, want capped at %ds", run.EndsAt, run.CreatedAt, boundaryMaxEnd)
	}
	// A later change of its end is held to the same maximum.
	far := time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)
	if w := e.admin(http.MethodPatch, "/api/v1/runs/"+created.ID.String(), `{"ends_at":"`+far+`"}`); w.Code != http.StatusOK {
		t.Errorf("patch end = %d %s", w.Code, w.Body)
	} else if run, _ := e.pg.GetRun(context.Background(), created.ID); run.EndsAt == nil || time.Until(*run.EndsAt) > boundaryMaxEnd*time.Second {
		t.Errorf("patched ends_at = %v, want at most %ds ahead", run.EndsAt, boundaryMaxEnd)
	}
	if rows := e.audits("run.break_glass.open"); len(rows) != 1 || !strings.Contains(string(rows[0].Data), "prod outage INC-1") {
		t.Errorf("run.break_glass.open rows = %+v", rows)
	}
	var datum map[string]any
	for _, ev := range e.audits("run.create") {
		_ = json.Unmarshal(ev.Data, &datum)
	}
	if datum["break_glass"] != true || datum["break_glass_reason"] != "prod outage INC-1" {
		t.Errorf("run.create break_glass = %v / %v", datum["break_glass"], datum["break_glass_reason"])
	}
	// Break-glass lifts a profile's walls, never the boundary: the run's own open policy resolves
	// inside it, and so does its ceiling.
	ctx := context.WithValue(context.Background(), breakGlassKey{}, "prod outage INC-1")
	open := types.RunPolicySpec{MinConfinementClass: types.CC1, AllowAllEgress: true}
	spec, _, _, _, ok := e.srv.resolveRunPolicy(ctx, httptest.NewRecorder(), governReq(ctx),
		&createRunRequest{Agent: "claude-code", Repo: "acme/widgets", InlinePolicy: &open}, true)
	if why := withinBoundary(spec); !ok || why != "" {
		t.Errorf("break-glass policy = %+v (ok %v): %s, want it inside the boundary", spec, ok, why)
	}
	if c, err := e.srv.effectiveCeiling(ctx); err != nil || withinBoundary(c.Spec) != "" {
		t.Errorf("break-glass ceiling = %+v (%v), want it inside the boundary", c.Spec, err)
	}
	// In no_limits the header changes nothing and no reason is asked for.
	putBoundary(t, e, testBoundary(types.BoundaryNoLimits))
	if w := create(""); w.Code != http.StatusCreated {
		t.Fatalf("no_limits, no reason: %d %s", w.Code, w.Body)
	}
}

// TestBoundaryBoundEgressApprovals: in hard_limits no decider admits a host the boundary does not
// allow, a host it allows is decided as before, and a break-glass run's approval is bound the same.
func TestBoundaryBoundEgressApprovals(t *testing.T) {
	pool := throwawayPGPool(t)
	testutil.SetBoundaryNoLimits(t, pool) // the walled run is created before the boundary is set
	e := newGovEnvOn(t, pool)
	e.seedProfile("team", "pypi.org") // so the admin token's reason makes a break-glass run
	mkRun := func(breakGlass bool) uuid.UUID {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", strings.NewReader(`{"agent":"claude-code","task":"t"}`))
		r.Header.Set("Authorization", "Bearer "+adminToken)
		if breakGlass {
			r.Header.Set(breakGlassReasonHeader, "incident")
		}
		w := httptest.NewRecorder()
		panicFails(t, e.srv.Handler()).ServeHTTP(w, r)
		var created createRunResponse
		if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &created) != nil {
			t.Fatalf("create run = %d %s", w.Code, w.Body)
		}
		return created.ID
	}
	walled := mkRun(false)
	putBoundary(t, e, testBoundary(types.BoundaryHardLimits))
	glass := mkRun(true)
	ask := func(run uuid.UUID, host string) uuid.UUID {
		ap, err := e.h.approvals.Request(context.Background(), types.ApprovalRequest{RunID: run, Kind: types.ApprovalEgressDomain,
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
	if w := approve(ask(walled, boundaryOutsideHost)); w.Code != http.StatusForbidden || wireReason(t, w) != reasonBoundaryHost {
		t.Errorf("outside host: %d %s, want 403 %s", w.Code, w.Body, reasonBoundaryHost)
	}
	if w := approve(ask(walled, "evil.example")); w.Code != http.StatusForbidden || wireReason(t, w) != reasonBoundaryHost {
		t.Errorf("denied host: %d %s, want 403 %s", w.Code, w.Body, reasonBoundaryHost)
	}
	if w := approve(ask(walled, "pypi.org")); w.Code != http.StatusOK {
		t.Errorf("allowed host: %d %s, want 200", w.Code, w.Body)
	}
	if run, _ := e.pg.GetRun(context.Background(), glass); !run.BreakGlass {
		t.Fatal("the reasoned admin-token run is not tagged break_glass")
	}
	if w := approve(ask(glass, boundaryOutsideHost)); w.Code != http.StatusForbidden || wireReason(t, w) != reasonBoundaryHost {
		t.Errorf("break-glass run, outside host: %d %s, want 403 %s", w.Code, w.Body, reasonBoundaryHost)
	}
}

// TestBreakGlassOnlyWhenItLifts: in hard_limits with no governance profile, an admin-token run lifts
// nothing, so it needs no reason, is not tagged and is bound by the boundary like any run; preflight
// agrees. Once one profile exists the same request is a break-glass run and needs a reason.
func TestBreakGlassOnlyWhenItLifts(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	putBoundary(t, e, testBoundary(types.BoundaryHardLimits))
	post := func(path string) *httptest.ResponseRecorder {
		return e.admin(http.MethodPost, path, `{"agent":"claude-code","task":"t","inline_policy":{"min_confinement_class":"CC1","allow_all_egress":true}}`)
	}
	w := post("/api/v1/runs")
	var created createRunResponse
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &created) != nil {
		t.Fatalf("no profile, no reason: %d %s, want 201", w.Code, w.Body)
	}
	run, err := e.pg.GetRun(context.Background(), created.ID)
	if err != nil || run.BreakGlass || len(e.audits("run.break_glass.open")) != 0 {
		t.Errorf("run break_glass = %v, open rows %d (%v), want an untagged run", run.BreakGlass, len(e.audits("run.break_glass.open")), err)
	}
	if run.ConfinementClass != types.CC3 {
		t.Errorf("confinement class = %q, want the boundary's floor CC3", run.ConfinementClass)
	}
	if w := post("/api/v1/runs/preflight"); w.Code != http.StatusOK {
		t.Errorf("preflight, no profile, no reason: %d %s, want 200", w.Code, w.Body)
	}
	e.seedProfile("team", "pypi.org")
	for _, path := range []string{"/api/v1/runs", "/api/v1/runs/preflight"} {
		if w := post(path); w.Code != http.StatusBadRequest || wireReason(t, w) != reasonBreakGlassReasonRequired {
			t.Errorf("%s with a profile, no reason: %d %s, want 400 %s", path, w.Code, w.Body, reasonBreakGlassReasonRequired)
		}
	}
}

// TestBoundaryBindsLocalMode: local mode's runs stand outside governance, yet in hard_limits the
// boundary binds them, and a local caller is never asked for a break-glass reason, even with a
// profile present.
func TestBoundaryBindsLocalMode(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t), func(c *Config) {
		c.LocalMode, c.LocalOperator, c.OIDC = true, "local:alice", nil
	})
	e.seedProfile("team", "pypi.org")
	putBoundary(t, e, testBoundary(types.BoundaryHardLimits))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runs", strings.NewReader(`{"agent":"claude-code","task":"t","inline_policy":{"min_confinement_class":"CC1","allow_all_egress":true}}`))
	req.Host, req.RemoteAddr = "127.0.0.1:8080", "127.0.0.1:54321"
	w := httptest.NewRecorder()
	panicFails(t, e.srv.Handler()).ServeHTTP(w, req)
	var created createRunResponse
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &created) != nil {
		t.Fatalf("local-mode run = %d %s, want 201 with no reason", w.Code, w.Body)
	}
	if run, err := e.pg.GetRun(context.Background(), created.ID); err != nil || run.BreakGlass || run.ConfinementClass != types.CC3 {
		t.Errorf("local-mode run = break_glass %v class %q (%v), want untagged at the boundary's floor CC3", run.BreakGlass, run.ConfinementClass, err)
	}
}

// TestBoundaryFourEyes: with the second-human switch on a security admin's write is held, the
// proposer cannot approve it, a second security admin does, and the audit names the mode.
func TestBoundaryFourEyes(t *testing.T) {
	e := newGovEnv(t)
	draft := boundaryJSON(testBoundary(types.BoundaryNoLimits))
	if w := e.call(e.user, http.MethodPut, "/api/v1/governance/boundary", draft); w.Code != http.StatusForbidden {
		t.Errorf("member write = %d, want 403", w.Code)
	}
	ch := e.pending(e.call(e.alice, http.MethodPut, "/api/v1/governance/boundary", draft))
	if ch.TargetKind != govKindBoundary {
		t.Errorf("target_kind = %q", ch.TargetKind)
	}
	if got, _ := e.pg.GetBoundary(context.Background()); got.Spec.AllowedDomains != nil {
		t.Fatalf("a held write applied: %+v", got)
	}
	if w := e.call(e.alice, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusForbidden || wireReason(t, w) != "second_human_required" {
		t.Errorf("self-approve = %d %s", w.Code, w.Body)
	}
	if w := e.call(e.bob, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusOK {
		t.Fatalf("second admin approve = %d %s", w.Code, w.Body)
	}
	got, err := e.pg.GetBoundary(context.Background())
	if err != nil || got.Mode != types.BoundaryNoLimits || got.Limits.MaxEndAheadSec == nil {
		t.Fatalf("boundary after approval = %+v (%v)", got, err)
	}
	rows := e.audits(boundaryWriteAction)
	if len(e.audits("governance.change.propose")) != 1 || len(rows) != 1 || !strings.Contains(string(rows[0].Data), `"mode":"no_limits"`) {
		t.Errorf("propose rows %d, write rows %+v, want 1 and 1 naming the mode", len(e.audits("governance.change.propose")), rows)
	}
	// A write that names a knob the boundary cannot hold is refused before anything is held.
	if w := e.call(e.alice, http.MethodPut, "/api/v1/governance/boundary", `{"mode":"no_limits","spec":{"allowed_methods":["GET"]},"limits":{}}`); w.Code != http.StatusBadRequest {
		t.Errorf("unknown knob = %d %s, want 400", w.Code, w.Body)
	}
}

// TestBoundaryWriteNeedsMode: a PUT that leaves the mode out is refused, so a spec-only edit cannot
// change the mode by omission (L2).
func TestBoundaryWriteNeedsMode(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	for _, body := range []string{`{"spec":{"denied_domains":["evil.example"]},"limits":{}}`, `{"mode":"on","spec":{},"limits":{}}`} {
		if w := e.call(e.alice, http.MethodPut, "/api/v1/governance/boundary", body); w.Code != http.StatusBadRequest || wireReason(t, w) != reasonBoundaryInvalid {
			t.Errorf("%s: %d %s, want 400 %s", body, w.Code, w.Body, reasonBoundaryInvalid)
		}
	}
}

// TestBoundaryHardLimitsWritable: a security admin's write puts the boundary in hard_limits, now
// that it bounds every brokered credential path a run is given (#3238).
func TestBoundaryHardLimitsWritable(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	w := e.call(e.alice, http.MethodPut, "/api/v1/governance/boundary", boundaryJSON(testBoundary(types.BoundaryHardLimits)))
	if w.Code != http.StatusOK {
		t.Fatalf("hard_limits write = %d %s, want 200", w.Code, w.Body)
	}
	if got, err := e.pg.GetBoundary(context.Background()); err != nil || !got.HardLimits() || got.Spec.AllowedDomains == nil {
		t.Errorf("boundary after the write = %+v (%v), want hard_limits with its allowlist", got, err)
	}
}
