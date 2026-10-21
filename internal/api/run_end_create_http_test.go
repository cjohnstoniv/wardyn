// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// endBody is a run-mode request (Background or Interactive) with extra fields
// spliced in, for the create door.
func endBody(experience, extra string) string {
	core := `"title":"t","experience":"background","workload":{"kind":"agent_task","agent":"claude-code","task":"t"},` +
		`"tools":[{"id":"claude-code","kind":"harness"}],"no_repositories_or_drives":true`
	if experience == "interactive" {
		core = `"title":"t","experience":"interactive","tools":[{"id":"claude-code","kind":"harness"}],"no_repositories_or_drives":true`
	}
	if extra != "" {
		core += "," + extra
	}
	return "{" + core + "}"
}

type endCreate struct {
	code   int
	reason string
	run    types.AgentRun
	resp   struct {
		Warnings []string `json:"warnings"`
	}
	audit []types.AuditEvent
}

// createWithEnd posts a create as a member under profile and returns what came
// back, the stored run and the audit rows.
func createWithEnd(t *testing.T, profile *types.GovernanceUnit, maxAge time.Duration, body string) endCreate {
	t.Helper()
	srv, st := runLimitsFixture(t, assignedStore(profile))
	srv.cfg.AgentImages = composeAgentImages
	srv.cfg.RunMaxAge = maxAge
	w := doSSO(t, srv, http.MethodPost, "/api/v1/runs", govSession(t, govMemberSub, []string{"eng"}, false), body)
	out := endCreate{code: w.Code, reason: errorReason(w)}
	out.audit = srv.cfg.Audit.(*recRecorder).snapshot()
	if w.Code == http.StatusCreated {
		out.run = decodedCreatedRun(t, st, w)
		if err := json.Unmarshal(w.Body.Bytes(), &out.resp); err != nil {
			t.Fatal(err)
		}
	} else {
		st.mu.Lock()
		defer st.mu.Unlock()
		if len(st.runs) != 0 {
			t.Errorf("a refused create wrote %d run rows", len(st.runs))
		}
	}
	return out
}

func (c endCreate) auditRow(action string) (types.AuditEvent, bool) {
	for _, ev := range c.audit {
		if ev.Action == action {
			return ev, true
		}
	}
	return types.AuditEvent{}, false
}

// TestCreateEnd_StoresTheEndAndTheLifetimeCap: a create under a profile with a week's
// lease and a deployment age of two weeks cuts a month-long ask to the week, stores
// ends_at and lifetime_cap_at counted from launch, says so in the 201 and records
// what was asked, applied, capped and which limit bound it on run.create.
func TestCreateEnd_StoresTheEndAndTheLifetimeCap(t *testing.T) {
	profile := limitsProfile("weekly", types.GovernanceLimits{RunLimits: types.RunLimits{MaxEndAheadSec: 7 * secDay}})
	got := createWithEnd(t, profile, 14*24*time.Hour, endBody("background", fmt.Sprintf(`"ends_after_sec":%d`, 30*secDay)))
	if got.code != http.StatusCreated {
		t.Fatalf("create = %d %s", got.code, got.reason)
	}
	run := got.run
	if want := run.CreatedAt.Add(7 * 24 * time.Hour); run.EndsAt == nil || !run.EndsAt.Equal(want) {
		t.Errorf("ends_at = %v, want created_at + the week %v", run.EndsAt, want)
	}
	if want := run.CreatedAt.Add(14 * 24 * time.Hour); run.LifetimeCapAt == nil || !run.LifetimeCapAt.Equal(want) || run.LifetimeCapSource != types.LimitSourceDeployment {
		t.Errorf("lifetime_cap_at = %v (%s), want created_at + 14 days from the deployment", run.LifetimeCapAt, run.LifetimeCapSource)
	}
	if !slices.ContainsFunc(got.resp.Warnings, func(w string) bool { return strings.Contains(w, "your admin") }) {
		t.Errorf("warnings %q do not name the limit that cut the end", got.resp.Warnings)
	}
	ev, ok := got.auditRow("run.create")
	if !ok {
		t.Fatal("no run.create row")
	}
	end, _ := leaseAuditData(t, ev)["end"].(map[string]any)
	if end["asked_sec"] != float64(30*secDay) || end["no_end"] != false || end["capped"] != true || end["source"] != "your admin" || end["applied"] == nil {
		t.Errorf("run.create end = %v; want asked, not No end, capped by your admin, with the applied time", end)
	}
}

// TestCreateEnd_AnUntouchedRunTakesTheDefault: no field, no override. An Interactive
// run is 24 hours; a Background run under a profile that names nothing has no end.
func TestCreateEnd_AnUntouchedRunTakesTheDefault(t *testing.T) {
	bare := limitsProfile("bare", types.GovernanceLimits{RunLimits: types.RunLimits{AllowNoEnd: true, UserChangesLimits: true}})
	ia := createWithEnd(t, bare, 0, endBody("interactive", ""))
	if ia.code != http.StatusCreated || ia.run.EndsAt == nil || !ia.run.EndsAt.Equal(ia.run.CreatedAt.Add(24*time.Hour)) || ia.run.LifetimeCapAt != nil {
		t.Errorf("interactive = %d, ends %v, cap %v; want 24 hours and no cap", ia.code, ia.run.EndsAt, ia.run.LifetimeCapAt)
	}
	bg := createWithEnd(t, bare, 0, endBody("background", ""))
	if bg.code != http.StatusCreated || bg.run.EndsAt != nil {
		t.Errorf("background = %d, ends %v; want No end", bg.code, bg.run.EndsAt)
	}
	ev, _ := bg.auditRow("run.create")
	if end, _ := leaseAuditData(t, ev)["end"].(map[string]any); end["applied"] != nil || end["capped"] != false {
		t.Errorf("run.create end = %v; want applied null, not capped", end)
	}
}

// TestCreateEnd_ARefusalWritesNoRowAndNoAudit: No end where governance does not allow
// it, and each shape error, answer before any identity, row or audit row exists.
func TestCreateEnd_ARefusalWritesNoRowAndNoAudit(t *testing.T) {
	week := limitsProfile("week", types.GovernanceLimits{RunLimits: types.RunLimits{MaxEndAheadSec: 7 * secDay}})
	gated := limitsProfile("gated", types.GovernanceLimits{RunLimits: types.RunLimits{MaxEndAheadSec: 7 * secDay, AllowNoEnd: true}})
	for _, tc := range []struct {
		name    string
		profile *types.GovernanceUnit
		extra   string
		code    int
		reason  string
	}{
		{"No end without allow_no_end", week, `"no_end":true`, http.StatusForbidden, reasonRunEndNoEndNotAllowed},
		{"No end without the gate", gated, `"no_end":true`, http.StatusForbidden, reasonRunLimitsGateDenied},
		{"both fields", week, `"no_end":true,"ends_after_sec":60`, http.StatusBadRequest, reasonRunEndConflictingFields},
		{"zero seconds", week, `"ends_after_sec":0`, http.StatusBadRequest, reasonRunEndAfterTooSmall},
		{"negative seconds", week, `"ends_after_sec":-5`, http.StatusBadRequest, reasonRunEndAfterTooSmall},
		{"past the ceiling", week, `"ends_after_sec":2147483648`, http.StatusBadRequest, reasonRunEndAfterTooLarge},
	} {
		got := createWithEnd(t, tc.profile, 0, endBody("background", tc.extra))
		if got.code != tc.code || got.reason != tc.reason {
			t.Errorf("%s = %d %q, want %d %q", tc.name, got.code, got.reason, tc.code, tc.reason)
		}
		if ev, ok := got.auditRow("run.create"); ok {
			t.Errorf("%s wrote a run.create row: %s", tc.name, ev.Data)
		}
		if n := len(got.audit); n != 0 {
			t.Errorf("%s wrote %d audit rows, want none: %+v", tc.name, n, got.audit)
		}
	}
}

// TestCreateEnd_NoEndWhereEveryLimitAllowsIt: the explicit No end lands on a Background
// and an Interactive run alike when governance offers it and nothing else bounds it.
func TestCreateEnd_NoEndWhereEveryLimitAllowsIt(t *testing.T) {
	open := limitsProfile("open", types.GovernanceLimits{RunLimits: types.RunLimits{AllowNoEnd: true, UserChangesLimits: true}})
	for _, experience := range []string{"background", "interactive"} {
		got := createWithEnd(t, open, 0, endBody(experience, `"no_end":true`))
		if got.code != http.StatusCreated || got.run.EndsAt != nil {
			t.Errorf("%s no_end = %d, ends %v; want 201 and No end", experience, got.code, got.run.EndsAt)
		}
		ev, _ := got.auditRow("run.create")
		if end, _ := leaseAuditData(t, ev)["end"].(map[string]any); end["no_end"] != true {
			t.Errorf("%s run.create end = %v; want no_end recorded", experience, end)
		}
	}
	// ...and not where the deployment's age bounds every run.
	if got := createWithEnd(t, open, 14*24*time.Hour, endBody("interactive", `"no_end":true`)); got.code != http.StatusForbidden || got.reason != reasonRunEndNoEndNotAllowed {
		t.Errorf("no_end under the deployment's age = %d %q, want 403 %s", got.code, got.reason, reasonRunEndNoEndNotAllowed)
	}
}

// TestPatchEnd_LatestEndIsTheEarlierBoundAndNamesItsSource: an extension is bounded by
// the earlier of now plus the captured governance lease and the run's lifetime cap,
// and the response says whose limit that is.
func TestPatchEnd_LatestEndIsTheEarlierBoundAndNamesItsSource(t *testing.T) {
	for _, tc := range []struct {
		name       string
		lease      int
		capIn      time.Duration
		capSource  types.LimitSource
		wantIn     time.Duration
		wantSource types.LimitSource
	}{
		{"the cap is nearer", 30 * secDay, 3 * 24 * time.Hour, types.LimitSourcePool, 3 * 24 * time.Hour, types.LimitSourcePool},
		{"the deployment's cap is nearer", 30 * secDay, 2 * 24 * time.Hour, types.LimitSourceDeployment, 2 * 24 * time.Hour, types.LimitSourceDeployment},
		{"the lease is nearer", 2 * secDay, 10 * 24 * time.Hour, types.LimitSourcePool, 2 * 24 * time.Hour, types.LimitSourceGovernance},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newEndWaitFixture(t, types.RunLimits{MaxEndAheadSec: tc.lease})
			capAt := f.now.Add(tc.capIn)
			f.st.run.LifetimeCapAt, f.st.run.LifetimeCapSource = &capAt, tc.capSource
			code, out := f.patch(t, ownerSession(t), endsAtBody(f.now.Add(100*24*time.Hour)))
			want := f.now.Add(tc.wantIn)
			if code != http.StatusOK || out.EndsAt == nil || !out.EndsAt.Equal(want) || out.LatestEnd == nil || !out.LatestEnd.Equal(want) ||
				out.LatestEndSource != tc.wantSource || !slices.Equal(out.Capped, []string{"ends_at"}) {
				t.Fatalf("patch = %d %+v; want 200, capped at %v by %s", code, out, want, tc.wantSource)
			}
			row := f.rows(t, "run.end.set")
			if len(row) != 1 || leaseAuditData(t, row[0])["source"] != string(tc.wantSource) {
				t.Errorf("run.end.set = %+v; want the binding source %s", row, tc.wantSource)
			}
		})
	}
}

// TestPatchEnd_SuperAdminBoundByTheCapturedCap (O10-A): a super admin skips governance
// only. The pool's and the deployment's captured cap still bounds their extension and
// No end, where the exemption used to bound them by the deployment alone.
func TestPatchEnd_SuperAdminBoundByTheCapturedCap(t *testing.T) {
	f := newEndWaitFixture(t, types.RunLimits{MaxEndAheadSec: 2 * secDay})
	capAt := f.now.Add(10 * 24 * time.Hour)
	f.st.run.LifetimeCapAt, f.st.run.LifetimeCapSource = &capAt, types.LimitSourcePool
	admin := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)

	code, out := f.patch(t, admin, endsAtBody(f.now.Add(365*24*time.Hour)))
	if code != http.StatusOK || out.EndsAt == nil || !out.EndsAt.Equal(capAt) || out.LatestEndSource != types.LimitSourcePool {
		t.Errorf("past the cap = %d %+v; want 200 at the cap %v, the pool's (governance's two days skipped)", code, out, capAt)
	}
	if code, _ := f.patch(t, admin, `{"ends_at":null}`); code != http.StatusForbidden {
		t.Errorf("No end on a run the pool bounds = %d, want 403", code)
	}
	rows := f.rows(t, "run.end.set")
	if len(rows) != 2 || leaseAuditData(t, rows[0])["limits_exempt"] != true {
		t.Errorf("run.end.set rows = %+v; want the change marked limits_exempt and the refusal", rows)
	}
}

// TestPatchEnd_PastTheMaxLifetimeIsRefused (N3): once the run's lifetime cap has
// passed, the lease sweep is ending it, and a PATCH of its end or wait is refused 409
// run_end_wait_max_lifetime (not "already finished": the run is still RUNNING) and writes nothing; just before the cap it still works.
func TestPatchEnd_PastTheMaxLifetimeIsRefused(t *testing.T) {
	f := newEndWaitFixture(t, types.RunLimits{MaxEndAheadSec: 30 * secDay, UserChangesLimits: true})
	capAt := f.now.Add(-time.Second)
	f.st.run.LifetimeCapAt, f.st.run.LifetimeCapSource = &capAt, types.LimitSourceDeployment
	before, _ := f.stored()
	for _, body := range []string{endsAtBody(f.now.Add(time.Hour)), `{"wait_budget_sec":60}`} {
		w := doSSO(t, f.srv, http.MethodPatch, "/api/v1/runs/"+f.st.run.ID.String(), ownerSession(t), body)
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), reasonRunEndWaitMaxLifetime) {
			t.Errorf("PATCH %s past the cap = %d %s; want 409 %s", body, w.Code, w.Body.String(), reasonRunEndWaitMaxLifetime)
		}
	}
	if after, _ := f.stored(); !after.Equal(*before) {
		t.Errorf("a refused PATCH moved the end %v -> %v", before, after)
	}

	open := f.now.Add(time.Hour)
	f.st.run.LifetimeCapAt = &open
	if code, _ := f.patch(t, ownerSession(t), endsAtBody(f.now.Add(30*time.Minute))); code != http.StatusOK {
		t.Errorf("PATCH before the cap = %d, want 200", code)
	}
}

// endAtEveryDoor posts body to create, preflight and the policy preview as a member under profile with
// WARDYN_RUN_MAX_AGE at maxAge, and returns each door's status and the created run.
func endAtEveryDoor(t *testing.T, profile *types.GovernanceUnit, maxAge time.Duration, body string) (map[string]int, types.AgentRun) {
	t.Helper()
	codes := map[string]int{}
	for _, door := range []string{"/api/v1/runs/preflight", "/api/v1/runs/policy-preview"} {
		srv, _ := runLimitsFixture(t, assignedStore(profile))
		srv.cfg.AgentImages = composeAgentImages
		srv.cfg.RunMaxAge = maxAge
		codes[door] = doSSO(t, srv, http.MethodPost, door, govSession(t, govMemberSub, []string{"eng"}, false), body).Code
	}
	created := createWithEnd(t, profile, maxAge, body)
	codes["/api/v1/runs"] = created.code
	return codes, created.run
}

var endDoorsOK = map[string]int{"/api/v1/runs": http.StatusCreated, "/api/v1/runs/preflight": http.StatusOK, "/api/v1/runs/policy-preview": http.StatusOK}

// WARDYN_RUN_MAX_AGE at 0 is off and a positive age caps the run, at every door.
func TestCreateEnd_RunMaxAgeOffAndOn(t *testing.T) {
	bare := limitsProfile("bare", types.GovernanceLimits{RunLimits: types.RunLimits{AllowNoEnd: true, UserChangesLimits: true}})
	body := endBody("background", `"ends_after_sec":3600`)
	codes, run := endAtEveryDoor(t, bare, 0, body)
	if !maps.Equal(codes, endDoorsOK) || run.LifetimeCapAt != nil {
		t.Errorf("age 0: doors %v, cap %v; want %v and no cap", codes, run.LifetimeCapAt, endDoorsOK)
	}
	codes, run = endAtEveryDoor(t, bare, 48*time.Hour, body)
	if !maps.Equal(codes, endDoorsOK) || run.LifetimeCapAt == nil || !run.LifetimeCapAt.Equal(run.CreatedAt.Add(48*time.Hour)) {
		t.Errorf("age 48h: doors %v, cap %v; want %v and a 48h cap", codes, run.LifetimeCapAt, endDoorsOK)
	}
}

// R2-0140: a negative WARDYN_RUN_MAX_AGE, documented as off, is off at every door: no 500 and no cap.
func TestCreateEnd_NegativeRunMaxAgeIsOff(t *testing.T) {
	bare := limitsProfile("bare", types.GovernanceLimits{RunLimits: types.RunLimits{AllowNoEnd: true, UserChangesLimits: true}})
	codes, run := endAtEveryDoor(t, bare, -time.Second, endBody("background", `"ends_after_sec":3600`))
	if !maps.Equal(codes, endDoorsOK) || run.LifetimeCapAt != nil || run.EndsAt == nil || !run.EndsAt.Equal(run.CreatedAt.Add(time.Hour)) {
		t.Errorf("age -1s: doors %v, ends %v, cap %v; want %v, the hour asked and no cap", codes, run.EndsAt, run.LifetimeCapAt, endDoorsOK)
	}
}
