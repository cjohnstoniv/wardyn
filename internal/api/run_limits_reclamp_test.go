// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const day = 24 * time.Hour

// reclampStore is leaseStore plus store.RunLimitsReclamper and the profile
// list, with the PG conditions of ReclampRunLimits.
type reclampStore struct {
	*leaseStore
	profiles []types.GovernanceProfile
}

func (s *reclampStore) ListGovernanceProfiles(context.Context) ([]types.GovernanceProfile, error) {
	return s.profiles, nil
}

func (s *reclampStore) ListProfiledLiveRuns(ctx context.Context) ([]types.AgentRun, error) {
	run, _ := s.GetRun(ctx, s.run.ID)
	if run.GovernanceProfileID == nil || (run.LostAt != nil && run.LostReason == types.LostEnded) || run.State.IsTerminal() {
		return nil, nil
	}
	return []types.AgentRun{run}, nil
}

func (s *reclampStore) ReclampRunLimits(_ context.Context, from types.AgentRun, toLimits types.RunLimits, toEnd *time.Time, toWait int, endTightenedAt *time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if from.RunLimits != s.run.RunLimits || !sameEnd(from.EndsAt, s.run.EndsAt) || from.WaitBudgetSec != s.run.WaitBudgetSec ||
		(s.run.LostAt != nil && s.run.LostReason == types.LostEnded) || s.state.IsTerminal() {
		return false, nil
	}
	s.run.RunLimits, s.run.EndsAt, s.run.WaitBudgetSec = toLimits, toEnd, toWait
	if endTightenedAt != nil {
		s.run.EndTightenedAt = endTightenedAt
	}
	return true, nil
}

// newReclampFixture is newEndWaitFixture's run, captured under profile with
// limits, ending in endsIn (nil end for 0) and waiting wait seconds.
func newReclampFixture(t *testing.T, limits types.RunLimits, endsIn time.Duration, wait int) (*endWaitFixture, *reclampStore, uuid.UUID) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	profileID := uuid.New()
	run := newFinalizeRun()
	run.CreatedBy, run.WaitBudgetSec, run.RunLimits, run.GovernanceProfileID = endWaitOwner, wait, limits, &profileID
	if endsIn != 0 {
		end := now.Add(endsIn)
		run.EndsAt = &end
	}
	h := newHarness(t)
	st := &reclampStore{leaseStore: &leaseStore{dispatchTestStore: &dispatchTestStore{run: run, state: types.RunRunning}}}
	cfg := baseTestConfig(h, st)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.ApprovalExpiryAfter = 24 * time.Hour
	cfg.Now = func() time.Time { return now }
	return &endWaitFixture{srv: New(cfg), st: st.leaseStore, audit: h.audit, now: now}, st, profileID
}

func (s *reclampStore) setProfile(id uuid.UUID, l types.RunLimits) {
	s.profiles = []types.GovernanceProfile{{ID: id, Name: "members", Limits: types.GovernanceLimits{RunLimits: l}}}
}

func (s *reclampStore) current() types.AgentRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.run
}

func sweepLimits(t *testing.T, f *endWaitFixture) {
	t.Helper()
	if err := f.srv.sweepRunLimits(context.Background()); err != nil {
		t.Fatalf("sweepRunLimits: %v", err)
	}
}

// TestRunLimitsReclamp_TighteningReachesALiveRun: an admin cuts the profile's
// max end from 30 days to 2, its max wait from 8 hours to 1, and takes away the
// gate. On the next sweep the live run takes all three: its end is cut to now +
// 2 days, its wait to an hour, and its captured limits are the tighter ones, so
// the owner can no longer extend past 2 days or shorten at all. Each cut is
// audited once as the system with reason limits_tightened, and the run carries
// end_tightened_at for the banner.
func TestRunLimitsReclamp_TighteningReachesALiveRun(t *testing.T) {
	f, st, profile := newReclampFixture(t,
		types.RunLimits{MaxEndAheadSec: 30 * 86400, MaxWaitSec: 8 * 3600, UserChangesLimits: true}, 20*day, 8*3600)
	st.setProfile(profile, types.RunLimits{MaxEndAheadSec: 2 * 86400, MaxWaitSec: 3600})
	from := *st.current().EndsAt

	sweepLimits(t, f)

	run := st.current()
	cut := f.now.Add(2 * day)
	if run.EndsAt == nil || !run.EndsAt.Equal(cut) || run.WaitBudgetSec != 3600 {
		t.Fatalf("run = ends %v wait %d; want %v and 3600", run.EndsAt, run.WaitBudgetSec, cut)
	}
	want := types.RunLimits{MaxEndAheadSec: 2 * 86400, MaxWaitSec: 3600}
	if run.RunLimits != want {
		t.Errorf("captured limits = %+v, want %+v", run.RunLimits, want)
	}
	if run.EndTightenedAt == nil || !run.EndTightenedAt.Equal(f.now) {
		t.Errorf("end_tightened_at = %v, want %v", run.EndTightenedAt, f.now)
	}

	rows := f.rows(t, "run.end.set")
	if len(rows) != 1 {
		t.Fatalf("run.end.set rows = %d, want 1", len(rows))
	}
	data := leaseAuditData(t, rows[0])
	if rows[0].ActorType != types.ActorSystem || data["reason"] != "limits_tightened" ||
		data["from"] != from.Format(time.RFC3339) || data["to"] != cut.Format(time.RFC3339) ||
		data["max"] != float64(2*86400) || data["profile_id"] != profile.String() {
		t.Errorf("run.end.set = actor %q data %v; want the system, limits_tightened, from %v to %v, max",
			rows[0].ActorType, data, from, cut)
	}
	waits := f.rows(t, "run.wait_budget.set")
	if len(waits) != 1 || leaseAuditData(t, waits[0])["to"] != float64(3600) {
		t.Errorf("run.wait_budget.set rows = %v, want one cutting the wait to 3600", waits)
	}

	code, out := f.patch(t, ownerSession(t), endsAtBody(f.now.Add(20*day)))
	if code != http.StatusOK || out.EndsAt == nil || !out.EndsAt.Equal(cut) {
		t.Errorf("extend to 20 days = %d %+v; want 200 held at %v", code, out, cut)
	}
	if code, _ := f.patch(t, ownerSession(t), endsAtBody(f.now.Add(day))); code != http.StatusForbidden {
		t.Errorf("shorten after the gate was taken = %d, want 403", code)
	}

	sweepLimits(t, f)
	system := 0
	for _, ev := range f.rows(t, "run.end.set") {
		if ev.ActorType == types.ActorSystem {
			system++
		}
	}
	if system != 1 {
		t.Errorf("system run.end.set rows after a second sweep = %d, want still 1", system)
	}
}

// TestRunLimitsReclamp_LooseningNeverReaches: a profile loosened after the run
// started changes nothing on it, and the owner still extends only to the
// captured max. A profile tightened on one field and loosened on another takes
// only the tightened one.
func TestRunLimitsReclamp_LooseningNeverReaches(t *testing.T) {
	captured := types.RunLimits{MaxEndAheadSec: 30 * 86400, MaxWaitSec: 3600}
	f, st, profile := newReclampFixture(t, captured, 20*day, 3600)
	st.setProfile(profile, types.RunLimits{MaxEndAheadSec: 90 * 86400, AllowNoEnd: true, UserChangesLimits: true})

	sweepLimits(t, f)

	if run := st.current(); run.RunLimits != captured || run.EndTightenedAt != nil {
		t.Fatalf("run = %+v tightened %v; want the captured limits untouched", run.RunLimits, run.EndTightenedAt)
	}
	if n := len(f.rows(t, "run.end.set")) + len(f.rows(t, "run.wait_budget.set")); n != 0 {
		t.Errorf("audit rows = %d, want none", n)
	}
	code, out := f.patch(t, ownerSession(t), endsAtBody(f.now.Add(60*day)))
	if limit := f.now.Add(30 * day); code != http.StatusOK || out.EndsAt == nil || !out.EndsAt.Equal(limit) {
		t.Errorf("extend to 60 days = %d %+v; want 200 capped at the captured %v", code, out, limit)
	}

	st.setProfile(profile, types.RunLimits{MaxEndAheadSec: 90 * 86400, MaxWaitSec: 600})
	sweepLimits(t, f)
	if run := st.current(); run.RunLimits.MaxEndAheadSec != 30*86400 || run.RunLimits.MaxWaitSec != 600 || run.WaitBudgetSec != 600 {
		t.Errorf("mixed change = %+v wait %d; want max end kept at 30 days, wait cut to 600", run.RunLimits, run.WaitBudgetSec)
	}
}

// TestRunLimitsReclamp_NoEnd: a run with no end gets one when No end is taken
// away under a max; while No end is still allowed a tighter max leaves it
// without one.
func TestRunLimitsReclamp_NoEnd(t *testing.T) {
	f, st, profile := newReclampFixture(t, types.RunLimits{MaxEndAheadSec: 30 * 86400, AllowNoEnd: true}, 0, 3600)
	st.setProfile(profile, types.RunLimits{MaxEndAheadSec: 7 * 86400, AllowNoEnd: true})
	sweepLimits(t, f)
	if run := st.current(); run.EndsAt != nil || run.RunLimits.MaxEndAheadSec != 7*86400 {
		t.Fatalf("No end still allowed = ends %v limits %+v; want no end under a 7-day max", run.EndsAt, run.RunLimits)
	}

	st.setProfile(profile, types.RunLimits{MaxEndAheadSec: 7 * 86400})
	sweepLimits(t, f)
	if run, want := st.current(), f.now.Add(7*day); run.EndsAt == nil || !run.EndsAt.Equal(want) || run.RunLimits.AllowNoEnd {
		t.Errorf("No end taken away = ends %v limits %+v; want an end at %v", run.EndsAt, run.RunLimits, want)
	}
	if rows := f.rows(t, "run.end.set"); len(rows) != 1 || leaseAuditData(t, rows[0])["from"] != nil {
		t.Errorf("run.end.set rows = %v, want one from No end", rows)
	}
}

// TestRunLimitsReclamp_OnlyTheCapturedProfile: a run is re-clamped only by the
// profile it captured. Another profile tightening, or its own being deleted,
// leaves it as it was.
func TestRunLimitsReclamp_OnlyTheCapturedProfile(t *testing.T) {
	captured := types.RunLimits{MaxEndAheadSec: 30 * 86400}
	f, st, _ := newReclampFixture(t, captured, 20*day, 3600)
	st.setProfile(uuid.New(), types.RunLimits{MaxEndAheadSec: 86400})
	sweepLimits(t, f)
	if run := st.current(); run.RunLimits != captured || !run.EndsAt.Equal(f.now.Add(20*day)) {
		t.Errorf("run = %+v ends %v; want untouched by another profile", run.RunLimits, run.EndsAt)
	}
}
