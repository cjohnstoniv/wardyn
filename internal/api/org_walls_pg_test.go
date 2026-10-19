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
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The org walls' PG pins (#2945, #2946): every policy path is bounded when they are on, nothing
// changes when they are off, the dry run reports exactly what the on-switch does, the admin token's
// break-glass run, the egress approval bound and the second human.

const wallsMaxEnd = 3600

// testWalls is the walls the pins turn on: an allowlist the corpus profile and stored policy are
// wider than, a deny, a raised floor and a run-limit maximum.
func testWalls(enabled bool) types.OrgWallsWrite {
	allowed := []string{"api.anthropic.com", "pypi.org", "github.com"}
	denied := []string{"evil.example"}
	cc := types.CC3
	end := wallsMaxEnd
	return types.OrgWallsWrite{Enabled: enabled,
		Spec:   types.OrgWallsSpec{AllowedDomains: &allowed, DeniedDomains: &denied, MinConfinementClass: &cc},
		Limits: types.OrgWallsLimits{MaxEndAheadSec: &end}}
}

func putWalls(t *testing.T, e *govEnv, w types.OrgWallsWrite) {
	t.Helper()
	if _, err := e.pg.PutOrgWalls(context.Background(), w, "seed"); err != nil {
		t.Fatalf("put walls: %v", err)
	}
}

func wallsJSON(w types.OrgWallsWrite) string {
	b, _ := json.Marshal(w)
	return string(b)
}

// withinWalls reports what about spec exceeds testWalls, or "".
func withinWalls(spec types.RunPolicySpec) string {
	w := testWalls(true)
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

// TestOrgWallsBindEveryPolicyPath: with the walls on, the profile chain, the member pipeline, a
// stored policy an unassigned member selects, the deployment default, a super admin's runs and the
// admin token's run without a break-glass reason all resolve inside them.
func TestOrgWallsBindEveryPolicyPath(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	c := seedWallsCorpus(t, e)
	putWalls(t, e, testWalls(true))
	for _, wc := range wallsCases() {
		t.Run(wc.name, func(t *testing.T) {
			got := resolveWallsCase(t, e.srv, c, wc)
			if !got.OK {
				t.Fatal("the walls refused the run; they must narrow it")
			}
			if why := withinWalls(got.Spec); why != "" {
				t.Errorf("run policy exceeds the walls: %s (%+v)", why, got.Spec)
			}
			if why := withinWalls(got.CeilingSpec); why != "" {
				t.Errorf("ceiling exceeds the walls: %s", why)
			}
			if got.CeilingLimits.MaxEndAheadSec != wallsMaxEnd {
				t.Errorf("ceiling max_end_ahead_sec = %d, want the walls' %d", got.CeilingLimits.MaxEndAheadSec, wallsMaxEnd)
			}
			if !slices.ContainsFunc(append(got.Warnings, got.CeilingWarnings...), func(s string) bool { return strings.HasPrefix(s, "the org walls narrowed ") }) {
				t.Errorf("no walls warning: run %q ceiling %q", got.Warnings, got.CeilingWarnings)
			}
		})
	}
}

// TestOrgWallsProvenanceNamesTheWalls: a value the walls decided records layer walls, whether the
// walls narrowed the ceiling it came from or the run's own policy.
func TestOrgWallsProvenanceNamesTheWalls(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	c := seedWallsCorpus(t, e)
	putWalls(t, e, testWalls(true))
	for _, wc := range wallsCases() {
		if wc.name != "profile_chain_default" && wc.name != "stored_policy_unassigned" {
			continue
		}
		req := wc.req(c)
		_, _, _, src, ok := e.srv.resolveRunPolicy(wc.ctx, httptest.NewRecorder(), governReq(wc.ctx), &req, true)
		if !ok {
			t.Fatalf("%s: refused", wc.name)
		}
		for _, knob := range []string{"allowed_domains", "min_confinement_class"} {
			if got := src.knobs[knob]; got.Layer != policyref.LayerWalls || got.Locked {
				t.Errorf("%s: provenance[%s] = %+v, want layer walls, unlocked", wc.name, knob, got)
			}
		}
	}
}

// TestOrgWallsOffIsByteIdentical: with no row, and with a row whose enabled is false, the corpus
// resolves to the golden recorded on a tree without the walls.
func TestOrgWallsOffIsByteIdentical(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	c := seedWallsCorpus(t, e)
	compareOrUpdateGolden(t, "testdata/org_walls_off_golden.json", resolveWallsCorpus(t, e.srv, c))
	putWalls(t, e, testWalls(false))
	compareOrUpdateGolden(t, "testdata/org_walls_off_golden.json", resolveWallsCorpus(t, e.srv, c))
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

// TestOrgWallsDryRunEqualsTheOnSwitch: every change the dry run lists for the deployment, a profile
// and a stored policy is the value the resolve gives once the same walls are on, and a knob it does
// not list is unchanged.
func TestOrgWallsDryRunEqualsTheOnSwitch(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	c := seedWallsCorpus(t, e)
	w := e.call(e.alice, http.MethodPost, "/api/v1/governance/walls/dry-run", wallsJSON(testWalls(true)))
	if w.Code != http.StatusOK {
		t.Fatalf("dry run = %d %s", w.Code, w.Body)
	}
	var report wallsReport
	if err := json.Unmarshal(w.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Profiles) != 1 || report.Profiles[0].ID != c.profile.ID || len(report.StoredPolicies) != 1 ||
		report.StoredPolicies[0].ID != c.open.ID || len(report.Assignments) != 1 || report.Assignments[0].Subject != wallsMemberSub {
		t.Fatalf("report rows = %s", w.Body)
	}
	cases := map[string]wallsCase{}
	for _, wc := range wallsCases() {
		cases[wc.name] = wc
	}
	off := map[string]wallsResolved{}
	for _, name := range []string{"profile_chain_default", "deployment_default_unassigned", "stored_policy_unassigned"} {
		off[name] = resolveWallsCase(t, e.srv, c, cases[name])
	}
	putWalls(t, e, testWalls(true))
	check := func(label string, changes []wallsChange, before, after types.RunPolicySpec) {
		t.Helper()
		listed := map[string]bool{}
		for _, ch := range changes {
			if strings.HasPrefix(ch.Knob, "limits.") {
				continue
			}
			listed[ch.Knob] = true
			want, _ := json.Marshal(ch.After)
			if got := knobJSON(t, after, ch.Knob); got != string(want) {
				t.Errorf("%s %s: report says %s, the walls on resolve %s", label, ch.Knob, want, got)
			}
		}
		for _, knob := range policyKnobFields {
			if knob != "" && !listed[knob] && knobJSON(t, before, knob) != knobJSON(t, after, knob) {
				t.Errorf("%s %s changed but the report does not list it", label, knob)
			}
		}
	}
	on := resolveWallsCase(t, e.srv, c, cases["profile_chain_default"])
	check("profile", report.Profiles[0].Changes, off["profile_chain_default"].CeilingSpec, on.CeilingSpec)
	on = resolveWallsCase(t, e.srv, c, cases["deployment_default_unassigned"])
	check("deployment", report.Deployment, off["deployment_default_unassigned"].CeilingSpec, on.CeilingSpec)
	on = resolveWallsCase(t, e.srv, c, cases["stored_policy_unassigned"])
	check("stored policy", report.StoredPolicies[0].Changes, off["stored_policy_unassigned"].Spec, on.Spec)
}

// TestOrgWallsBreakGlass: while the walls are on, the admin token's run needs a reason; with one it
// is tagged, audited, capped at the walls' run-limit maximum and not narrowed.
func TestOrgWallsBreakGlass(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	putWalls(t, e, testWalls(true))
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
	if run.EndsAt == nil || run.EndsAt.Sub(run.CreatedAt) > wallsMaxEnd*time.Second {
		t.Errorf("ends_at = %v from %v, want capped at %ds", run.EndsAt, run.CreatedAt, wallsMaxEnd)
	}
	// A later change of its end is held to the same maximum.
	far := time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)
	if w := e.admin(http.MethodPatch, "/api/v1/runs/"+created.ID.String(), `{"ends_at":"`+far+`"}`); w.Code != http.StatusOK {
		t.Errorf("patch end = %d %s", w.Code, w.Body)
	} else if run, _ := e.pg.GetRun(context.Background(), created.ID); run.EndsAt == nil || time.Until(*run.EndsAt) > wallsMaxEnd*time.Second {
		t.Errorf("patched ends_at = %v, want at most %ds ahead", run.EndsAt, wallsMaxEnd)
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
	// Only the walls are lifted: the run keeps its own policy, the floor included.
	ctx := context.WithValue(context.Background(), breakGlassKey{}, "prod outage INC-1")
	open := types.RunPolicySpec{MinConfinementClass: types.CC1, AllowAllEgress: true}
	spec, _, _, _, ok := e.srv.resolveRunPolicy(ctx, httptest.NewRecorder(), governReq(ctx),
		&createRunRequest{Agent: "claude-code", Repo: "acme/widgets", InlinePolicy: &open}, true)
	if !ok || !spec.AllowAllEgress {
		t.Errorf("break-glass policy = %+v (ok %v), want it unnarrowed", spec, ok)
	}
	// With the walls off the header changes nothing and no reason is asked for.
	putWalls(t, e, testWalls(false))
	if w := create(""); w.Code != http.StatusCreated {
		t.Fatalf("walls off, no reason: %d %s", w.Code, w.Body)
	}
}

// TestOrgWallsBoundEgressApprovals: while the walls are on no decider admits a host they do not
// allow, a host they allow is decided as before, and a break-glass run's approval is not bound.
func TestOrgWallsBoundEgressApprovals(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
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
	putWalls(t, e, testWalls(true))
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
	if w := approve(ask(walled, wallsOutsideHost)); w.Code != http.StatusForbidden || wireReason(t, w) != reasonOrgWallsHost {
		t.Errorf("outside host: %d %s, want 403 %s", w.Code, w.Body, reasonOrgWallsHost)
	}
	if w := approve(ask(walled, "evil.example")); w.Code != http.StatusForbidden || wireReason(t, w) != reasonOrgWallsHost {
		t.Errorf("denied host: %d %s, want 403 %s", w.Code, w.Body, reasonOrgWallsHost)
	}
	if w := approve(ask(walled, "pypi.org")); w.Code != http.StatusOK {
		t.Errorf("allowed host: %d %s, want 200", w.Code, w.Body)
	}
	if w := approve(ask(glass, wallsOutsideHost)); w.Code != http.StatusOK {
		t.Errorf("break-glass run: %d %s, want 200", w.Code, w.Body)
	}
}

// TestOrgWallsFourEyes: with the second-human switch on a security admin's write is held, the
// proposer cannot approve it, a second security admin does, and the audit names the switch flip.
func TestOrgWallsFourEyes(t *testing.T) {
	e := newGovEnv(t)
	if w := e.call(e.user, http.MethodPut, "/api/v1/governance/walls", wallsJSON(testWalls(true))); w.Code != http.StatusForbidden {
		t.Errorf("member write = %d, want 403", w.Code)
	}
	ch := e.pending(e.call(e.alice, http.MethodPut, "/api/v1/governance/walls", wallsJSON(testWalls(true))))
	if ch.TargetKind != govKindOrgWalls {
		t.Errorf("target_kind = %q", ch.TargetKind)
	}
	if got, _ := e.pg.GetOrgWalls(context.Background()); got.Enabled || got.Spec.AllowedDomains != nil {
		t.Fatalf("a held write applied: %+v", got)
	}
	if w := e.call(e.alice, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusForbidden || wireReason(t, w) != "second_human_required" {
		t.Errorf("self-approve = %d %s", w.Code, w.Body)
	}
	if w := e.call(e.bob, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusOK {
		t.Fatalf("second admin approve = %d %s", w.Code, w.Body)
	}
	got, err := e.pg.GetOrgWalls(context.Background())
	if err != nil || !got.Enabled || got.Limits.MaxEndAheadSec == nil {
		t.Fatalf("walls after approval = %+v (%v)", got, err)
	}
	if len(e.audits("governance.change.propose")) != 1 || len(e.audits(orgWallsEnableAction)) != 1 {
		t.Errorf("propose rows %d, enable rows %d, want 1 and 1", len(e.audits("governance.change.propose")), len(e.audits(orgWallsEnableAction)))
	}
	// A write that names a knob the walls cannot hold is refused before anything is held.
	if w := e.call(e.alice, http.MethodPut, "/api/v1/governance/walls", `{"enabled":true,"spec":{"allowed_methods":["GET"]},"limits":{}}`); w.Code != http.StatusBadRequest {
		t.Errorf("unknown knob = %d %s, want 400", w.Code, w.Body)
	}
}
