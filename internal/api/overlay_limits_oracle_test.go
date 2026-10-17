// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"math/rand"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The limits meet is judged by the profile writer's own refusals and by the
// door that reads PauseIdleAfterSec, not by the meet's comparator: a composed
// limits object must be one governanceLimitsRefusal accepts, and a composed run
// must pause for idleness at least as early as the base would have paused it.

func randomLimits(r *rand.Rand) types.GovernanceLimits {
	n := func(xs ...int) int { return xs[r.Intn(len(xs))] }
	maxEnd, maxWait := n(0, 600, 7200), n(0, 300, 900)
	l := types.GovernanceLimits{
		MaxConcurrentRuns: n(0, 2, 8), MaxCPUMillis: n(0, 1000, 4000), MaxMemoryMiB: n(0, 512), MaxEphemeralDiskMiB: n(0, 100), MaxDriveSizeMiB: n(0, 100),
		RunLimits: types.RunLimits{
			MaxEndAheadSec: maxEnd, MaxWaitSec: maxWait, AllowNoEnd: r.Intn(2) == 0, UserChangesLimits: r.Intn(2) == 0, PauseIdleAfterSec: n(0, 60, 600),
		},
	}
	if maxEnd > 0 {
		l.DefaultEndSec = n(0, maxEnd)
	}
	if maxWait > 0 {
		l.DefaultWaitSec = n(0, maxWait)
	}
	return l
}

func randomLimitsOverlay(r *rand.Rand) types.LimitsOverlay {
	n := func(xs ...int) *int {
		if r.Intn(2) == 0 {
			return nil
		}
		v := xs[r.Intn(len(xs))]
		return &v
	}
	b := func() *bool {
		if r.Intn(2) == 0 {
			return nil
		}
		v := r.Intn(2) == 0
		return &v
	}
	return types.LimitsOverlay{
		MaxConcurrentRuns: n(0, 1, 4, 9), MaxCPUMillis: n(0, 500, 2000), MaxMemoryMiB: n(0, 256),
		MaxEndAheadSec: n(0, 300, 3600), DefaultEndSec: n(0, 100, 5000), AllowNoEnd: b(),
		MaxWaitSec: n(0, 100, 600), DefaultWaitSec: n(0, 50, 800), UserChangesLimits: b(), PauseIdleAfterSec: n(0, 30, 900),
		DenyInteractive: b(),
	}
}

func TestComposedLimitsPassTheProfileWritersRefusals(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	for i := 0; i < 5000; i++ {
		base, ov := randomLimits(r), randomLimitsOverlay(r)
		if msg := governanceLimitsRefusal(base); msg != "" {
			t.Fatalf("generator produced limits the writer refuses: %s", msg)
		}
		got, _, err := composer.ApplyOverlay(composer.Authority{Limits: base}, composer.Overlay{Limits: ov})
		if err != nil {
			t.Fatalf("case %d: base %+v overlay %+v: %v", i, base, ov, err)
		}
		if msg := governanceLimitsRefusal(got.Limits); msg != "" {
			t.Fatalf("case %d: base %+v overlay %+v composed to limits the writer refuses: %s\n%+v", i, base, ov, msg, got.Limits)
		}
	}
}

// idlePauses restates the sweep's condition (run_pause.go): a quiet run is a
// candidate once it has been quiet for PauseIdleAfterSec, floored at pauseDelayFloor,
// and 0 never pauses an idle run.
func idlePauses(l types.RunLimits, quiet time.Duration) bool {
	return l.PauseIdleAfterSec > 0 && quiet >= max(time.Duration(l.PauseIdleAfterSec)*time.Second, pauseDelayFloor)
}

func TestComposedIdlePauseIsNeverLaterThanTheBases(t *testing.T) {
	r := rand.New(rand.NewSource(11))
	for i := 0; i < 3000; i++ {
		base, ov := randomLimits(r), randomLimitsOverlay(r)
		got, _, err := composer.ApplyOverlay(composer.Authority{Limits: base}, composer.Overlay{Limits: ov})
		if err != nil {
			t.Fatal(err)
		}
		for _, quiet := range []time.Duration{0, time.Minute, 5 * time.Minute, 30 * time.Minute, 4 * time.Hour} {
			if idlePauses(base.RunLimits, quiet) && !idlePauses(got.Limits.RunLimits, quiet) {
				t.Fatalf("case %d: the base pauses a run quiet for %v and the result does not: base %d overlay %v result %d",
					i, quiet, base.PauseIdleAfterSec, ov.PauseIdleAfterSec, got.Limits.PauseIdleAfterSec)
			}
		}
	}
}

// The live-run reclamp and the meet share one tightening, so a running child is
// cut to exactly the bounds a new run under the same composition gets.
func TestLiveReclampAndTheMeetShareOneTightening(t *testing.T) {
	captured := types.RunLimits{MaxEndAheadSec: 7200, MaxWaitSec: 600, PauseIdleAfterSec: 0, AllowNoEnd: true, UserChangesLimits: true, DefaultEndSec: 3600}
	profile := types.RunLimits{MaxEndAheadSec: 1800, MaxWaitSec: 0, PauseIdleAfterSec: 300, AllowNoEnd: false, UserChangesLimits: true}
	live := composer.TightenRunLimits(captured, profile)
	composed, _, err := composer.ApplyOverlay(composer.Authority{Limits: types.GovernanceLimits{RunLimits: captured}}, composer.Overlay{Limits: types.LimitsOverlay{
		MaxEndAheadSec: &profile.MaxEndAheadSec, MaxWaitSec: &profile.MaxWaitSec, PauseIdleAfterSec: &profile.PauseIdleAfterSec, AllowNoEnd: &profile.AllowNoEnd, UserChangesLimits: &profile.UserChangesLimits}})
	if err != nil {
		t.Fatal(err)
	}
	got := composed.Limits.RunLimits
	if got.MaxEndAheadSec != live.MaxEndAheadSec || got.MaxWaitSec != live.MaxWaitSec || got.PauseIdleAfterSec != live.PauseIdleAfterSec ||
		got.AllowNoEnd != live.AllowNoEnd || got.UserChangesLimits != live.UserChangesLimits {
		t.Errorf("a new run under the composition gets %+v, a live run is cut to %+v", got, live)
	}
}
