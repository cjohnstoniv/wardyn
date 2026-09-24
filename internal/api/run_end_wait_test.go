// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const endWaitOwner = "sub-run-owner"

type endWaitFixture struct {
	srv   *Server
	st    *leaseStore
	audit *recRecorder
	now   time.Time
	end   time.Time
}

// newEndWaitFixture is one RUNNING run owned by endWaitOwner under limits,
// ending in a day and waiting an hour, against a fixed clock.
func newEndWaitFixture(t *testing.T, limits types.RunLimits) *endWaitFixture {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	end := now.Add(24 * time.Hour)
	run := newFinalizeRun()
	run.CreatedBy, run.EndsAt, run.WaitBudgetSec, run.RunLimits = endWaitOwner, &end, 3600, limits
	h := newHarness(t)
	st := &leaseStore{dispatchTestStore: &dispatchTestStore{run: run, state: types.RunRunning}}
	cfg := baseTestConfig(h, st)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.ApprovalExpiryAfter = 24 * time.Hour
	cfg.Now = func() time.Time { return now }
	return &endWaitFixture{srv: New(cfg), st: st, audit: h.audit, now: now, end: end}
}

func (f *endWaitFixture) patch(t *testing.T, cookie *http.Cookie, body string) (int, runEndWaitResponse) {
	t.Helper()
	w := doSSO(t, f.srv, http.MethodPatch, "/api/v1/runs/"+f.st.run.ID.String(), cookie, body)
	var out runEndWaitResponse
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return w.Code, out
}

func (f *endWaitFixture) stored() (*time.Time, int) {
	f.st.mu.Lock()
	defer f.st.mu.Unlock()
	return f.st.run.EndsAt, f.st.run.WaitBudgetSec
}

// rows returns the audit rows of action, as outcome → data.
func (f *endWaitFixture) rows(t *testing.T, action string) []types.AuditEvent {
	t.Helper()
	var out []types.AuditEvent
	for _, ev := range f.audit.snapshot() {
		if ev.Action == action {
			out = append(out, ev)
		}
	}
	return out
}

func endsAtBody(at time.Time) string {
	return fmt.Sprintf(`{"ends_at":%q}`, at.Format(time.RFC3339))
}

func ownerSession(t *testing.T) *http.Cookie {
	return ssoSession(t, endWaitOwner, "owner@corp.example", oidc.RoleUser)
}

// TestSetRunEnd_ExtendingIsTheLease: without the gate an owner extends within
// the captured max, an over-ask is capped at now + max and told, and each
// change is audited run.end.set with from, to and max.
func TestSetRunEnd_ExtendingIsTheLease(t *testing.T) {
	f := newEndWaitFixture(t, types.RunLimits{MaxEndAheadSec: 30 * 86400})

	week := f.now.Add(7 * 24 * time.Hour)
	code, out := f.patch(t, ownerSession(t), endsAtBody(week))
	if code != http.StatusOK || out.EndsAt == nil || !out.EndsAt.Equal(week) || len(out.Capped) != 0 {
		t.Fatalf("extend a week = %d %+v; want 200, ends %v, nothing capped", code, out, week)
	}
	if end, _ := f.stored(); end == nil || !end.Equal(week) {
		t.Errorf("stored end = %v, want %v", end, week)
	}

	code, out = f.patch(t, ownerSession(t), endsAtBody(f.now.Add(90*24*time.Hour)))
	limit := f.now.Add(30 * 24 * time.Hour)
	if code != http.StatusOK || out.EndsAt == nil || !out.EndsAt.Equal(limit) ||
		!slices.Equal(out.Capped, []string{"ends_at"}) || out.LatestEnd == nil || !out.LatestEnd.Equal(limit) {
		t.Fatalf("over-ask = %d %+v; want 200, capped at %v and told", code, out, limit)
	}

	rows := f.rows(t, "run.end.set")
	if len(rows) != 2 {
		t.Fatalf("run.end.set rows = %d, want 2", len(rows))
	}
	data := leaseAuditData(t, rows[1])
	if rows[1].Actor != endWaitOwner || rows[1].Outcome != "success" || data["max"] != float64(30*86400) ||
		data["capped"] != true || data["limits_exempt"] != false {
		t.Errorf("second run.end.set = actor %q outcome %q data %v; want the owner, success, max, capped",
			rows[1].Actor, rows[1].Outcome, data)
	}
}

// TestSetRunEnd_TheGate: shortening, No end and changing the wait need the
// run's captured user_changes_limits; without it each is a 403, audited as
// denied, and the run is untouched. With it each lands, still bounded.
func TestSetRunEnd_TheGate(t *testing.T) {
	soon := func(f *endWaitFixture) string { return endsAtBody(f.now.Add(time.Hour)) }
	for _, tc := range []struct {
		name   string
		body   func(*endWaitFixture) string
		action string
	}{
		{"shorten", soon, "run.end.set"},
		{"no end", func(*endWaitFixture) string { return `{"ends_at":null}` }, "run.end.set"},
		{"change the wait", func(*endWaitFixture) string { return `{"wait_budget_sec":600}` }, "run.wait_budget.set"},
	} {
		t.Run(tc.name+" without the gate", func(t *testing.T) {
			f := newEndWaitFixture(t, types.RunLimits{MaxEndAheadSec: 30 * 86400, AllowNoEnd: true})
			if code, _ := f.patch(t, ownerSession(t), tc.body(f)); code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", code)
			}
			if end, wait := f.stored(); end == nil || !end.Equal(f.end) || wait != 3600 {
				t.Errorf("stored = %v %d; want the run untouched", end, wait)
			}
			if rows := f.rows(t, tc.action); len(rows) != 1 || rows[0].Outcome != "denied" {
				t.Errorf("%s rows = %+v; want one denied row", tc.action, rows)
			}
		})
		t.Run(tc.name+" with the gate", func(t *testing.T) {
			f := newEndWaitFixture(t, types.RunLimits{
				MaxEndAheadSec: 30 * 86400, AllowNoEnd: true, MaxWaitSec: 7200, UserChangesLimits: true,
			})
			if code, _ := f.patch(t, ownerSession(t), tc.body(f)); code != http.StatusOK {
				t.Fatalf("status = %d, want 200", code)
			}
			if rows := f.rows(t, tc.action); len(rows) != 1 || rows[0].Outcome != "success" {
				t.Errorf("%s rows = %+v; want one success row", tc.action, rows)
			}
		})
	}

	t.Run("no end stays refused where the limits do not allow it", func(t *testing.T) {
		f := newEndWaitFixture(t, types.RunLimits{UserChangesLimits: true})
		if code, _ := f.patch(t, ownerSession(t), `{"ends_at":null}`); code != http.StatusForbidden {
			t.Errorf("status = %d, want 403", code)
		}
	})

	t.Run("an end on a run with no end is not an extension", func(t *testing.T) {
		f := newEndWaitFixture(t, types.RunLimits{})
		f.st.mu.Lock()
		f.st.run.EndsAt = nil
		f.st.mu.Unlock()
		if code, _ := f.patch(t, ownerSession(t), endsAtBody(f.now.Add(time.Hour))); code != http.StatusForbidden {
			t.Errorf("status = %d, want 403", code)
		}
	})

	t.Run("the wait is capped at the tighter of the profile and the deployment", func(t *testing.T) {
		f := newEndWaitFixture(t, types.RunLimits{MaxWaitSec: 7200, UserChangesLimits: true})
		code, out := f.patch(t, ownerSession(t), `{"wait_budget_sec":999999}`)
		if code != http.StatusOK || out.WaitBudgetSec != 7200 || out.MaxWaitSec != 7200 ||
			!slices.Equal(out.Capped, []string{"wait_budget_sec"}) {
			t.Errorf("= %d %+v; want 200, capped at 7200", code, out)
		}
	})
}

// TestSetRunEnd_WhoIsBounded: a security admin is bounded by their own run's
// captured limits and cannot reach a foreign run; only a super admin is
// exempt, on any run, bounded by the deployment alone.
func TestSetRunEnd_WhoIsBounded(t *testing.T) {
	limits := types.RunLimits{MaxEndAheadSec: 86400 * 2}

	t.Run("a security admin on their own run", func(t *testing.T) {
		f := newEndWaitFixture(t, limits)
		sec := ssoSession(t, endWaitOwner, "sec@corp.example", oidc.RoleSecurityAdmin)
		if code, _ := f.patch(t, sec, endsAtBody(f.now.Add(time.Hour))); code != http.StatusForbidden {
			t.Errorf("shorten = %d, want 403", code)
		}
		if code, out := f.patch(t, sec, endsAtBody(f.now.Add(10*24*time.Hour))); code != http.StatusOK ||
			!slices.Equal(out.Capped, []string{"ends_at"}) {
			t.Errorf("over-ask = %d %+v, want 200 capped", code, out)
		}
	})

	t.Run("a security admin on a foreign run", func(t *testing.T) {
		f := newEndWaitFixture(t, limits)
		sec := ssoSession(t, "sub-sec", "sec@corp.example", oidc.RoleSecurityAdmin)
		if code, _ := f.patch(t, sec, endsAtBody(f.now.Add(36*time.Hour))); code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", code)
		}
	})

	t.Run("a super admin on a foreign run", func(t *testing.T) {
		f := newEndWaitFixture(t, limits)
		admin := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)
		far := f.now.Add(365 * 24 * time.Hour)
		if code, out := f.patch(t, admin, endsAtBody(far)); code != http.StatusOK || !out.EndsAt.Equal(far) {
			t.Errorf("past the max = %d %+v, want 200 at %v", code, out, far)
		}
		if code, out := f.patch(t, admin, `{"ends_at":null,"wait_budget_sec":999999}`); code != http.StatusOK ||
			out.EndsAt != nil || out.WaitBudgetSec != 86400 {
			t.Errorf("no end + wait = %d %+v, want 200, no end, the deployment's 86400", code, out)
		}
		rows := f.rows(t, "run.end.set")
		if len(rows) != 2 || leaseAuditData(t, rows[1])["limits_exempt"] != true {
			t.Errorf("run.end.set rows = %+v; want two, marked limits_exempt", rows)
		}
	})
}

// TestSetRunEnd_Refusals: a finished or kept run, an end in the past and a
// body that changes nothing are refused before anything is written.
func TestSetRunEnd_Refusals(t *testing.T) {
	gated := types.RunLimits{UserChangesLimits: true, AllowNoEnd: true}
	for _, tc := range []struct {
		name  string
		setup func(*endWaitFixture)
		body  func(*endWaitFixture) string
		want  int
	}{
		{"a finished run", func(f *endWaitFixture) { f.st.state = types.RunCompleted },
			func(f *endWaitFixture) string { return `{"ends_at":null}` }, http.StatusConflict},
		{"a kept run", func(f *endWaitFixture) { f.st.run.LostAt, f.st.run.LostReason = &f.now, types.LostEnded },
			func(f *endWaitFixture) string { return endsAtBody(f.now.Add(48 * time.Hour)) }, http.StatusConflict},
		{"an end in the past", func(*endWaitFixture) {},
			func(f *endWaitFixture) string { return endsAtBody(f.now.Add(-time.Minute)) }, http.StatusBadRequest},
		{"no field", func(*endWaitFixture) {}, func(*endWaitFixture) string { return `{}` }, http.StatusBadRequest},
		{"a null wait", func(*endWaitFixture) {},
			func(*endWaitFixture) string { return `{"wait_budget_sec":null}` }, http.StatusBadRequest},
		{"a zero wait", func(*endWaitFixture) {},
			func(*endWaitFixture) string { return `{"wait_budget_sec":0}` }, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newEndWaitFixture(t, gated)
			f.st.mu.Lock()
			tc.setup(f)
			f.st.mu.Unlock()
			if code, _ := f.patch(t, ownerSession(t), tc.body(f)); code != tc.want {
				t.Errorf("status = %d, want %d", code, tc.want)
			}
			if end, wait := f.stored(); end == nil || !end.Equal(f.end) || wait != 3600 {
				t.Errorf("stored = %v %d; want the run untouched", end, wait)
			}
		})
	}
}
