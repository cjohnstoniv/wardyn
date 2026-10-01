// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

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

// TestSetRunEnd_MustBeFutureReasonIsPinned covers the same "an end in the
// past" case TestSetRunEnd_Refusals already exercises, with the wire reason
// asserted as a LITERAL, not the Go const, so a rename of
// reasonRunEndMustBeFuture without updating docs/sdk.md fails here too.
func TestSetRunEnd_MustBeFutureReasonIsPinned(t *testing.T) {
	f := newEndWaitFixture(t, types.RunLimits{UserChangesLimits: true, AllowNoEnd: true})
	w := doSSO(t, f.srv, http.MethodPatch, "/api/v1/runs/"+f.st.run.ID.String(), ownerSession(t), endsAtBody(f.now.Add(-time.Minute)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", w.Code, w.Body.String())
	}
	if got := errorReason(w); got != "run_end_must_be_future" {
		t.Errorf("reason = %q, want the literal %q", got, "run_end_must_be_future")
	}
}

// TestPatchRunEnds_ExtendsALostRun is F1's fix (long-holds design rev 4
// §2.3): a run lost to a reboot or a control-plane outage, even past its own
// end, is still extendable — that is how it becomes revivable again (§4.1).
// A run whose OWN lease ended (LostEnded) is refused once its files are no
// longer kept, which with this fixture's grace of 0 is at once
// (run_ended_revive_test.go covers the grace).
func TestPatchRunEnds_ExtendsALostRun(t *testing.T) {
	for _, reason := range []types.LostReason{types.LostOutage, types.LostReboot} {
		t.Run(string(reason), func(t *testing.T) {
			f := newEndWaitFixture(t, types.RunLimits{MaxEndAheadSec: 30 * 86400})
			past := f.now.Add(-time.Hour)
			f.st.mu.Lock()
			f.st.run.EndsAt, f.st.run.LostAt, f.st.run.LostReason = &past, &past, reason
			f.st.mu.Unlock()

			later := f.now.Add(48 * time.Hour)
			code, out := f.patch(t, ownerSession(t), endsAtBody(later))
			if code != http.StatusOK || out.EndsAt == nil || !out.EndsAt.Equal(later) {
				t.Fatalf("extend a %s run past its end = %d %+v; want 200 at %v", reason, code, out, later)
			}
			end, _ := f.stored()
			if end == nil || !end.Equal(later) {
				t.Fatalf("stored end = %v, want %v", end, later)
			}
			lostAt, lostReason := f.st.lost()
			until, ok := f.srv.keptUntil(types.AgentRun{EndsAt: end, LostAt: lostAt, LostReason: lostReason})
			if !ok || !until.Equal(later) {
				t.Errorf("keptUntil = %v %v; want the new end (grace 0 in this fixture)", until, ok)
			}
		})
	}

	t.Run("a run whose own lease ended, with no files grace, stays refused", func(t *testing.T) {
		f := newEndWaitFixture(t, types.RunLimits{MaxEndAheadSec: 30 * 86400})
		past := f.now.Add(-time.Hour)
		f.st.mu.Lock()
		f.st.run.EndsAt, f.st.run.LostAt, f.st.run.LostReason = &past, &past, types.LostEnded
		f.st.mu.Unlock()
		if code, _ := f.patch(t, ownerSession(t), endsAtBody(f.now.Add(24*time.Hour))); code != http.StatusConflict {
			t.Errorf("status = %d, want 409", code)
		}
	})
}

// profileErrStore is the reclamp store whose profile list can fail.
type profileErrStore struct {
	*reclampStore
	err error
}

func (s profileErrStore) ListGovernanceProfiles(ctx context.Context) ([]types.GovernanceProfile, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.reclampStore.ListGovernanceProfiles(ctx)
}

// patchRaw is patch with the response body as sent, for what it must not carry.
func (f *endWaitFixture) patchRaw(t *testing.T, cookie *http.Cookie, body string) (int, map[string]any, string) {
	t.Helper()
	w := doSSO(t, f.srv, http.MethodPatch, "/api/v1/runs/"+f.st.run.ID.String(), cookie, body)
	var m map[string]any
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return w.Code, m, w.Body.String()
}

// #1322: a PATCH the run's captured cap cut back says when the admin has since
// loosened the launch profile, so the toast can say so. Display only: the cap
// that binds stays the captured one.
func TestSetRunEnd_CapLoosenedIsToldNotApplied(t *testing.T) {
	captured := types.RunLimits{MaxEndAheadSec: 2 * 86400, MaxWaitSec: 3600, UserChangesLimits: true}
	const liveSec = 29*86400 + 7 // a number the response must never carry
	for _, tc := range []struct {
		name    string
		profile *types.RunLimits // nil: no profile row
		listErr error
		atCap   bool // the run already ends at the cap: nothing is written, so no owner re-check reads the profile
		want    bool
	}{
		{name: "loosened", profile: ptr(types.RunLimits{MaxEndAheadSec: liveSec}), want: true},
		{name: "loosened to no limit", profile: ptr(types.RunLimits{}), want: true},
		{name: "tightened", profile: ptr(types.RunLimits{MaxEndAheadSec: 86400})},
		{name: "unchanged", profile: ptr(types.RunLimits{MaxEndAheadSec: 2 * 86400})},
		{name: "deleted", profile: nil, atCap: true},
		{name: "unreadable", profile: ptr(types.RunLimits{MaxEndAheadSec: liveSec}), listErr: errors.New("db down"), atCap: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			endsIn := day
			if tc.atCap {
				endsIn = 2 * day
			}
			f, st, profileID := newReclampFixture(t, captured, endsIn, 1800)
			if tc.profile != nil {
				st.setProfile(profileID, *tc.profile)
			}
			f.srv.cfg.Store = profileErrStore{reclampStore: st, err: tc.listErr}
			code, m, raw := f.patchRaw(t, ownerSession(t), endsAtBody(f.now.Add(10*day)))
			if code != http.StatusOK {
				t.Fatalf("PATCH = %d %s, want 200 at the captured cap whatever the profile read said", code, raw)
			}
			if got, _ := m["ends_cap_loosened"].(bool); got != tc.want {
				t.Errorf("ends_cap_loosened = %v, want %v (%s)", m["ends_cap_loosened"], tc.want, raw)
			}
			if _, present := m["ends_cap_loosened"]; present != tc.want {
				t.Errorf("ends_cap_loosened present = %v, want %v: only ever sent true", present, tc.want)
			}
			if strings.Contains(raw, "2505607") {
				t.Errorf("response carries the live profile's number: %s", raw)
			}
			if end, _ := f.stored(); end == nil || !end.Equal(f.now.Add(2*day)) {
				t.Errorf("stored end = %v, want the captured bound %v", end, f.now.Add(2*day))
			}
			if tc.atCap {
				return
			}
			rows := f.rows(t, "run.end.set")
			if len(rows) != 1 {
				t.Fatalf("run.end.set rows = %d, want 1", len(rows))
			}
			data := leaseAuditData(t, rows[0])
			if _, present := data["ends_cap_loosened"]; present != tc.want || data["capped"] != true {
				t.Errorf("run.end.set data = %v; want ends_cap_loosened present only when true, capped", data)
			}
		})
	}

	t.Run("an over-ask made after the loosening still stores the captured bound", func(t *testing.T) {
		f, st, profileID := newReclampFixture(t, captured, day, 1800)
		st.setProfile(profileID, types.RunLimits{MaxEndAheadSec: 30 * 86400})
		for i := 0; i < 2; i++ {
			if code, out := f.patch(t, ownerSession(t), endsAtBody(f.now.Add(20*day))); code != http.StatusOK ||
				out.EndsAt == nil || !out.EndsAt.Equal(f.now.Add(2*day)) {
				t.Fatalf("over-ask %d = %d %+v, want the captured %v", i, code, out, f.now.Add(2*day))
			}
		}
	})

	// The comparison is against the profile the run launched under, never
	// another one in the list (the owner's current assignment may be a looser or
	// tighter profile than the one the run was captured from).
	t.Run("the launch profile decides, not another profile", func(t *testing.T) {
		for _, tc := range []struct {
			name         string
			launch, nowB int
			want         bool
		}{
			{"launch unchanged, another looser", 2 * 86400, 30 * 86400, false},
			{"launch loosened, another tighter", 30 * 86400, 86400, true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				f, st, profileID := newReclampFixture(t, captured, 2*day, 1800)
				st.profiles = []types.GovernanceProfile{
					{ID: uuid.New(), Name: "owner's current", Limits: types.GovernanceLimits{RunLimits: types.RunLimits{MaxEndAheadSec: tc.nowB}}},
					{ID: profileID, Name: "launch", Limits: types.GovernanceLimits{RunLimits: types.RunLimits{MaxEndAheadSec: tc.launch}}},
				}
				code, m, raw := f.patchRaw(t, ownerSession(t), endsAtBody(f.now.Add(10*day)))
				if _, present := m["ends_cap_loosened"]; code != http.StatusOK || present != tc.want {
					t.Errorf("PATCH = %d %s; want ends_cap_loosened present = %v", code, raw, tc.want)
				}
			})
		}
	})

	t.Run("only the wait capped", func(t *testing.T) {
		f, st, profileID := newReclampFixture(t, captured, day, 1800)
		st.setProfile(profileID, types.RunLimits{MaxEndAheadSec: 30 * 86400, MaxWaitSec: 8 * 3600})
		code, m, raw := f.patchRaw(t, ownerSession(t), `{"wait_budget_sec":7200}`)
		if code != http.StatusOK || len(m["capped"].([]any)) != 1 || m["capped"].([]any)[0] != "wait_budget_sec" {
			t.Fatalf("PATCH = %d %s, want 200 with only the wait capped", code, raw)
		}
		if _, present := m["ends_cap_loosened"]; present {
			t.Errorf("ends_cap_loosened sent when only the wait was capped: %s", raw)
		}
	})

	t.Run("an exempt caller", func(t *testing.T) {
		f, st, profileID := newReclampFixture(t, captured, day, 1800)
		st.setProfile(profileID, types.RunLimits{MaxEndAheadSec: 30 * 86400})
		admin := ssoSession(t, endWaitOwner, "admin@corp.example", oidc.RoleAdmin)
		code, m, raw := f.patchRaw(t, admin, endsAtBody(f.now.Add(100*365*day)))
		if code != http.StatusOK || len(m["capped"].([]any)) != 1 {
			t.Fatalf("PATCH = %d %s, want 200 capped at the deployment's bound", code, raw)
		}
		if _, present := m["ends_cap_loosened"]; present {
			t.Errorf("ends_cap_loosened sent to a caller the run's limits never bound: %s", raw)
		}
	})

	t.Run("a run with no launch profile", func(t *testing.T) {
		f := newEndWaitFixture(t, captured)
		code, m, raw := f.patchRaw(t, ownerSession(t), endsAtBody(f.now.Add(10*day)))
		if _, present := m["ends_cap_loosened"]; code != http.StatusOK || present || len(m["capped"].([]any)) != 1 {
			t.Errorf("PATCH = %d %s, want 200 capped, not loosened", code, raw)
		}
	})
}
