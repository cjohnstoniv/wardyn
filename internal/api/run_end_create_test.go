// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	secHour = 3600
	secDay  = 24 * secHour
)

var (
	// A profile that offers No end and bounds a finite one at a week.
	govWeek     = types.RunLimits{MaxEndAheadSec: 7 * secDay, AllowNoEnd: true, UserChangesLimits: true}
	govOpen     = types.RunLimits{AllowNoEnd: true, UserChangesLimits: true}
	govNoNoEnd  = types.RunLimits{MaxEndAheadSec: 7 * secDay, UserChangesLimits: true}
	govNoGate   = types.RunLimits{MaxEndAheadSec: 7 * secDay, AllowNoEnd: true}
	govDefDay   = types.RunLimits{MaxEndAheadSec: 7 * secDay, DefaultEndSec: secDay, AllowNoEnd: true, UserChangesLimits: true}
	govDefDayNo = types.RunLimits{MaxEndAheadSec: 7 * secDay, DefaultEndSec: secDay}
)

func poolWithLifetime(name string, bg, ia types.RunnerPoolDuration) *types.RunnerPool {
	amount := func(def, c int) *types.RunnerPoolAmount { return &types.RunnerPoolAmount{Default: def, Cap: c} }
	return &types.RunnerPool{ID: uuid.New(), Name: name, Limits: &types.RunnerPoolLimits{
		Background:  &types.RunnerPoolBackgroundLimits{CPUMillis: amount(1000, 4000), MemoryMiB: amount(1024, 8192), Lifetime: &bg},
		Interactive: &types.RunnerPoolInteractiveLimits{CPUMillis: amount(1000, 4000), MemoryMiB: amount(1024, 8192), Lifetime: &ia, Idle: &types.RunnerPoolDuration{DefaultSec: 1800, MaxSec: 7200}},
		Barriers:    []types.ConfinementClass{types.CC1},
	}}
}

// monthPool bounds both run types at 30 days, defaulting to a day.
func monthPool() *types.RunnerPool {
	d := types.RunnerPoolDuration{DefaultSec: secDay, MaxSec: 30 * secDay}
	return poolWithLifetime("Big builds", d, d)
}

// unlimitedPool allows an unlimited lifetime with no default: the shape that
// leaves an untouched run with nothing to default to.
func unlimitedPool() *types.RunnerPool {
	d := types.RunnerPoolDuration{Unlimited: true}
	return poolWithLifetime("App servers", d, d)
}

// TestPlanCreateEnd_Table is every outcome row of the create-time end (design §3.3,
// with O4-B), each source binding in turn.
func TestPlanCreateEnd_Table(t *testing.T) {
	const noEnd = -1
	type want struct {
		endSec    int // noEnd for a run with no end
		capSec    int
		capped    bool
		source    types.LimitSource
		refusal   int // the status of a refusal; 0 for none
		reason    string
		sentences []string // every one must be in the refusal message
	}
	bg, ia := types.RunnerPoolRunBackground, types.RunnerPoolRunInteractive
	for _, tc := range []struct {
		name string
		in   createEndInput
		want want
	}{
		{"neither, a finite default", createEndInput{runType: bg, governed: true, gov: govDefDay},
			want{endSec: secDay, source: types.LimitSourceGovernance}},
		{"neither, default unlimited: Background gets No end", createEndInput{runType: bg, governed: true, gov: govOpen},
			want{endSec: noEnd}},
		{"neither, a profile that bounds nothing and offers nothing: today's no end", createEndInput{runType: bg, governed: true},
			want{endSec: noEnd}},
		{"neither, the pool's own default", createEndInput{runType: bg, governed: true, gov: govOpen, pool: monthPool()},
			want{endSec: secDay, capSec: 30 * secDay, source: types.LimitSourcePool}},
		{"a finite ask within every cap", createEndInput{runType: bg, governed: true, gov: govWeek, askedSec: 3 * secDay},
			want{endSec: 3 * secDay}},
		{"a finite ask past governance's lease is cut and names it", createEndInput{runType: bg, governed: true, gov: govWeek, askedSec: 30 * secDay},
			want{endSec: 7 * secDay, capped: true, source: types.LimitSourceGovernance}},
		{"a finite ask past the pool's maximum is cut and names it", createEndInput{runType: bg, governed: true, gov: govOpen, pool: monthPool(), askedSec: 90 * secDay},
			want{endSec: 30 * secDay, capSec: 30 * secDay, capped: true, source: types.LimitSourcePool}},
		{"a finite ask past the deployment's age is cut and names it", createEndInput{runType: bg, governed: true, gov: govOpen, maxAgeSec: 14 * secDay, askedSec: 30 * secDay},
			want{endSec: 14 * secDay, capSec: 14 * secDay, capped: true, source: types.LimitSourceDeployment}},
		{"the lowest maximum binds when governance is the lowest", createEndInput{runType: bg, governed: true, gov: govWeek, pool: monthPool(), maxAgeSec: 60 * secDay, askedSec: 20 * secDay},
			want{endSec: 7 * secDay, capSec: 30 * secDay, capped: true, source: types.LimitSourceGovernance}},
		{"the lowest maximum binds when the deployment is the lowest", createEndInput{runType: bg, governed: true, gov: govWeek, pool: monthPool(), maxAgeSec: 2 * secDay, askedSec: 5 * secDay},
			want{endSec: 2 * secDay, capSec: 2 * secDay, capped: true, source: types.LimitSourceDeployment}},
		{"O4-B: an end before the default needs no gate", createEndInput{runType: bg, governed: true, gov: govDefDayNo, askedSec: secHour},
			want{endSec: secHour}},
		{"O4-B: a finite end when the default is No end needs no gate", createEndInput{runType: bg, governed: true, askedSec: 2 * secHour},
			want{endSec: 2 * secHour}},
		{"no_end when the default is already No end grants nothing and is allowed", createEndInput{runType: bg, governed: true, noEnd: true},
			want{endSec: noEnd}},
		{"no_end, a finite default, every source permits it", createEndInput{runType: bg, governed: true, gov: govDefDay, noEnd: true},
			want{endSec: noEnd}},
		{"no_end, the pool bounds the lifetime", createEndInput{runType: bg, governed: true, gov: govOpen, pool: monthPool(), noEnd: true},
			want{refusal: http.StatusForbidden, reason: reasonRunEndNoEndNotAllowed, sentences: []string{"Big builds"}}},
		{"no_end, the deployment bounds the lifetime", createEndInput{runType: bg, governed: true, gov: govOpen, maxAgeSec: 14 * secDay, noEnd: true},
			want{refusal: http.StatusForbidden, reason: reasonRunEndNoEndNotAllowed, sentences: []string{"this deployment"}}},
		{"no_end, governance lacks allow_no_end", createEndInput{runType: bg, governed: true, gov: govNoNoEnd, noEnd: true},
			want{refusal: http.StatusForbidden, reason: reasonRunEndNoEndNotAllowed}},
		{"no_end, governance lacks the gate", createEndInput{runType: bg, governed: true, gov: govNoGate, noEnd: true},
			want{refusal: http.StatusForbidden, reason: reasonRunLimitsGateDenied}},
		{"a super admin skips governance for No end", createEndInput{runType: bg, governed: false, gov: govNoNoEnd, noEnd: true},
			want{endSec: noEnd}},
		{"a super admin is still bound by the pool", createEndInput{runType: bg, governed: false, pool: monthPool(), noEnd: true},
			want{refusal: http.StatusForbidden, reason: reasonRunEndNoEndNotAllowed, sentences: []string{"Big builds"}}},
		{"a super admin's over-ask is cut to the pool, not to governance", createEndInput{runType: bg, governed: false, gov: govNoNoEnd, pool: monthPool(), askedSec: 90 * secDay},
			want{endSec: 30 * secDay, capSec: 30 * secDay, capped: true, source: types.LimitSourcePool}},
		{"Interactive, neither: 24 hours", createEndInput{runType: ia, governed: true, gov: govOpen},
			want{endSec: secDay, source: types.LimitSourceDeployment}},
		{"Interactive, a pool of unlimited lifetime and no default: 24 hours", createEndInput{runType: ia, governed: true, gov: govOpen, pool: unlimitedPool()},
			want{endSec: secDay, source: types.LimitSourceDeployment}},
		{"Interactive, an explicit No end where permitted", createEndInput{runType: ia, governed: true, gov: govOpen, pool: unlimitedPool(), noEnd: true},
			want{endSec: noEnd}},
		{"Interactive, No end needs the permissions too", createEndInput{runType: ia, governed: true, gov: govNoGate, noEnd: true},
			want{refusal: http.StatusForbidden, reason: reasonRunLimitsGateDenied}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, refusal, err := planCreateEnd(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if tc.want.refusal != 0 {
				if refusal == nil || refusal.status != tc.want.refusal || refusal.body.Reason != tc.want.reason {
					t.Fatalf("refusal = %+v, want %d %s", refusal, tc.want.refusal, tc.want.reason)
				}
				for _, s := range tc.want.sentences {
					if !strings.Contains(refusal.body.Error, s) {
						t.Errorf("refusal %q does not name %q", refusal.body.Error, s)
					}
				}
				return
			}
			if refusal != nil {
				t.Fatalf("refused: %+v", refusal.body)
			}
			if tc.want.endSec == noEnd {
				if got.endSec != nil {
					t.Errorf("end = %d s, want No end", *got.endSec)
				}
			} else if got.endSec == nil || *got.endSec != tc.want.endSec {
				t.Errorf("end = %v, want %d s", got.endSec, tc.want.endSec)
			}
			if got.capSec != tc.want.capSec || got.capped != tc.want.capped || got.source != tc.want.source {
				t.Errorf("cap %d s capped %v source %q, want %d %v %q", got.capSec, got.capped, got.source, tc.want.capSec, tc.want.capped, tc.want.source)
			}
		})
	}
}

// TestCreateEnd_InteractiveNeverDefaultsToNoEnd: an Interactive run no source gives a
// lifetime default gets a finite one, 24 hours, whatever the pool and the profile say
// about No end (O7-B; the pool-limits review's probe P1).
func TestCreateEnd_InteractiveNeverDefaultsToNoEnd(t *testing.T) {
	if runnerpool.InteractiveDefaultLifetimeSec != 24*secHour {
		t.Fatalf("the Interactive fallback = %d s, want 24 hours", runnerpool.InteractiveDefaultLifetimeSec)
	}
	for name, in := range map[string]createEndInput{
		"no pool, a profile that offers No end":    {runType: types.RunnerPoolRunInteractive, governed: true, gov: govOpen},
		"a pool with no limits block":              {runType: types.RunnerPoolRunInteractive, governed: true, gov: govOpen, pool: &types.RunnerPool{Name: "Plain"}},
		"a pool of unlimited lifetime, no default": {runType: types.RunnerPoolRunInteractive, governed: true, gov: govOpen, pool: unlimitedPool()},
		"a profile with no limits at all":          {runType: types.RunnerPoolRunInteractive, governed: true},
		"a super admin":                            {runType: types.RunnerPoolRunInteractive, governed: false},
		"an older client's run (no stored mode)":   {runType: runTypeForEnd(""), governed: true},
	} {
		got, refusal, err := planCreateEnd(in)
		want := 24 * secHour
		if in.runType == types.RunnerPoolRunBackground {
			// The older-client shape is decided as Background and keeps today's no end.
			if err != nil || refusal != nil || got.endSec != nil {
				t.Errorf("%s: end = %v (%v, %v), want today's No end", name, got.endSec, err, refusal)
			}
			continue
		}
		if err != nil || refusal != nil || got.endSec == nil || *got.endSec != want {
			t.Errorf("%s: end = %v (%v, %v), want %d s", name, got.endSec, err, refusal, want)
		}
	}
}

// TestCreateEnd_BackgroundMayDefaultToNoEnd (O7-A): a Background run no source gives
// a default gets No end, as it does today.
func TestCreateEnd_BackgroundMayDefaultToNoEnd(t *testing.T) {
	for name, in := range map[string]createEndInput{
		"a pool of unlimited lifetime": {runType: types.RunnerPoolRunBackground, governed: true, gov: govOpen, pool: unlimitedPool()},
		"no pool, a bare profile":      {runType: types.RunnerPoolRunBackground, governed: true},
		"a super admin":                {runType: types.RunnerPoolRunBackground, governed: false},
	} {
		got, refusal, err := planCreateEnd(in)
		if err != nil || refusal != nil || got.endSec != nil || got.capSec != 0 {
			t.Errorf("%s: end %v cap %d (%v, %v), want No end and no cap", name, got.endSec, got.capSec, err, refusal)
		}
	}
	// The run type is the stored experience; an older client's run (none) is Background.
	for exp, want := range map[types.RunExperience]types.RunnerPoolRunType{
		types.ExperienceBackground: types.RunnerPoolRunBackground, types.ExperienceInteractive: types.RunnerPoolRunInteractive, "": types.RunnerPoolRunBackground,
	} {
		if got := runTypeForEnd(exp); got != want {
			t.Errorf("runTypeForEnd(%q) = %q, want %q", exp, got, want)
		}
	}
}

// TestPlanCreateEnd_SameLatestEndAsPatch: one finite ask is cut to the same end at
// create and in a PATCH made the same instant, and by the same limit.
func TestPlanCreateEnd_SameLatestEndAsPatch(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for _, tc := range []struct {
		name string
		in   createEndInput
	}{
		{"governance alone", createEndInput{governed: true, gov: govWeek}},
		{"the pool alone", createEndInput{governed: true, gov: govOpen, pool: monthPool()}},
		{"the deployment alone", createEndInput{governed: true, gov: govOpen, maxAgeSec: 14 * secDay}},
		{"governance lower than the pool", createEndInput{governed: true, gov: govWeek, pool: monthPool()}},
		{"the pool lower than governance", createEndInput{governed: true, gov: types.RunLimits{MaxEndAheadSec: 90 * secDay}, pool: monthPool()}},
		{"nothing but the run-limit ceiling", createEndInput{governed: true, gov: govOpen}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := tc.in
			in.runType, in.askedSec = types.RunnerPoolRunBackground, maxRunLimitSec
			created, refusal, err := planCreateEnd(in)
			if err != nil || refusal != nil || created.endSec == nil {
				t.Fatalf("create = %+v, %v, %v", created, refusal, err)
			}
			run := types.AgentRun{ID: uuid.New(), EndsAt: &now, RunLimits: in.gov}
			if created.capSec > 0 {
				capAt := now.Add(time.Duration(created.capSec) * time.Second)
				run.LifetimeCapAt, run.LifetimeCapSource = &capAt, created.capSource
			}
			far := now.Add(time.Duration(maxRunLimitSec) * time.Second)
			p := planRunEndWait(run, runEndWaitRequest{EndsAt: &far}, map[string]bool{"ends_at": true}, !in.governed, now, 0)
			if p.refusal != "" || p.resp.EndsAt == nil {
				t.Fatalf("patch = %+v", p)
			}
			if got := int(p.resp.EndsAt.Sub(now) / time.Second); got != *created.endSec {
				t.Errorf("patch ends %d s out, create %d s", got, *created.endSec)
			}
			if p.resp.LatestEnd != nil && created.source != "" && p.resp.LatestEndSource != created.source {
				t.Errorf("patch names %q, create %q", p.resp.LatestEndSource, created.source)
			}
		})
	}
}

// TestGovernanceSourceAgreesWithPlanEnd: what runnerpool reads from a profile's run
// limits is what planEnd enforces at an end change: the finite cap, and No end as
// its own answer.
func TestGovernanceSourceAgreesWithPlanEnd(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	cur := now.Add(time.Hour)
	for _, max := range []int{0, 7 * secDay} {
		for _, allow := range []bool{false, true} {
			for _, gate := range []bool{false, true} {
				l := types.RunLimits{MaxEndAheadSec: max, AllowNoEnd: allow, UserChangesLimits: gate}
				gov, err := runnerpool.GovernanceSource(types.GovernanceLimits{RunLimits: l}, types.ResourceLimits{}, types.ResourceLimits{}, types.ResourceLimits{CPUMillis: 1000, MemoryMiB: 1024})
				if err != nil {
					t.Fatal(err)
				}
				name := fmt.Sprintf("max %d s, allow no end %v, gate %v", max, allow, gate)

				far := now.Add(time.Duration(maxRunLimitSec) * 2 * time.Second)
				var p endWaitPlan
				p.planEnd(&cur, &far, l, runCap{}, now)
				if p.refusal != "" || p.resp.EndsAt == nil {
					t.Fatalf("%s: a far end = %+v", name, p)
				}
				gotCap := int(p.resp.EndsAt.Sub(now) / time.Second)
				switch {
				case gov.LifetimeSec.Max != nil && *gov.LifetimeSec.Max != gotCap:
					t.Errorf("%s: runnerpool bounds a finite end at %d s, planEnd at %d s", name, *gov.LifetimeSec.Max, gotCap)
				case gov.LifetimeSec.Max == nil && gotCap != maxRunLimitSec:
					t.Errorf("%s: runnerpool leaves the finite end unbounded, planEnd bounds it at %d s", name, gotCap)
				}

				var np endWaitPlan
				np.planEnd(&cur, nil, l, runCap{}, now)
				planEndAllows := np.refusal == ""
				if gov.NoEnd == nil || *gov.NoEnd != planEndAllows {
					t.Errorf("%s: runnerpool says No end %v, planEnd allows it: %v", name, gov.NoEnd, planEndAllows)
				}
			}
		}
	}
}

// TestStepRun_DeploymentMaxAgeCapsPatch: a server-launched run (newStepRun, the
// door record, scan, harness login and probe runs take) has no pool, so the
// deployment's maximum age is its only cap. It is stamped at create, and a PATCH
// can neither give the run No end nor push it past that age, a super admin
// included.
func TestStepRun_DeploymentMaxAgeCapsPatch(t *testing.T) {
	h := newHarness(t)
	srv := New(baseTestConfig(h, &dispatchTestStore{}))
	srv.cfg.RunMaxAge = 14 * 24 * time.Hour
	limits := types.RunLimits{MaxEndAheadSec: secDay, AllowNoEnd: true, UserChangesLimits: true}
	gov := stepRunGoverned(governanceCeiling{Limits: types.GovernanceLimits{RunLimits: limits}}, false, false)
	run, _, err := srv.newStepRun(context.Background(), uuid.New(), "owner", "source scan", types.CC2, gov, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := run.CreatedAt.Add(14 * 24 * time.Hour); run.LifetimeCapAt == nil || !run.LifetimeCapAt.Equal(want) || run.LifetimeCapSource != types.LimitSourceDeployment {
		t.Fatalf("lifetime_cap_at = %v (%q), want created_at + 14d from the deployment", run.LifetimeCapAt, run.LifetimeCapSource)
	}

	now := run.CreatedAt.Add(time.Hour)
	end := now.Add(time.Hour)
	run.EndsAt = &end
	if p := planRunEndWait(run, runEndWaitRequest{}, map[string]bool{"ends_at": true}, false, now, 0); p.status != http.StatusForbidden || p.refusalReason != reasonRunEndNoEndNotAllowed {
		t.Errorf("a member's No end = %d %q, want 403 %s", p.status, p.refusalReason, reasonRunEndNoEndNotAllowed)
	}
	far := now.Add(400 * 24 * time.Hour)
	p := planRunEndWait(run, runEndWaitRequest{EndsAt: &far}, map[string]bool{"ends_at": true}, true, now, 0)
	if p.refusal != "" || p.resp.EndsAt == nil || !p.resp.EndsAt.Equal(run.LifetimeCapAt.Truncate(time.Second)) || p.resp.LatestEndSource != types.LimitSourceDeployment {
		t.Errorf("a super admin past max age = %+v, want the end at the cap %v, the deployment's", p.resp, run.LifetimeCapAt)
	}

	srv.cfg.RunMaxAge = 0
	if r, _, err := srv.newStepRun(context.Background(), uuid.New(), "owner", "source scan", types.CC2, gov, nil); err != nil || r.LifetimeCapAt != nil || r.LifetimeCapSource != "" {
		t.Errorf("with no max age: cap %v (%q), %v; want none", r.LifetimeCapAt, r.LifetimeCapSource, err)
	}
}

// TestPlanEnd_PastLifetimeCapRefuses: a run that reaches its lifetime cap between the handler's check
// and the plan gets the handler's own 409, never an end in the past that reads as a successful change.
func TestPlanEnd_PastLifetimeCapRefuses(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	cur, passed, asked := now.Add(-2*time.Minute), now.Add(-time.Minute), now.Add(time.Hour)
	var p endWaitPlan
	p.planEnd(&cur, &asked, types.RunLimits{UserChangesLimits: true}, runCap{at: &passed, source: types.LimitSourceDeployment}, now)
	if p.status != http.StatusConflict || p.refusalReason != reasonRunEndWaitMaxLifetime || p.endChanged {
		t.Fatalf("a later end past the lifetime cap = %+v, want the handler's own 409 %s", p, reasonRunEndWaitMaxLifetime)
	}
}
