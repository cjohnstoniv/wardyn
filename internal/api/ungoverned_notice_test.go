// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestUngovernedNotice(t *testing.T) {
	unit := &types.GovernanceUnit{ID: uuid.New(), Name: "Contractors", Ceiling: govProfileSpec(), Limits: types.GovernanceLimits{
		MaxCPUMillis: 2000, MaxMemoryMiB: 4096, MaxConcurrentRuns: 3, DenyInteractive: true,
	}}
	server := govServer(&capStore{govProfile: unit, govTier: types.CapabilitySubjectUser})
	operator := governanceCeiling{Operator: true}

	t.Run("a governed caller gets no block", func(t *testing.T) {
		if got := server.ungovernedNotice(governAdminSessionCtx(), governanceCeiling{}); got != nil {
			t.Fatalf("got %+v, want nil", got)
		}
	})
	t.Run("no person has no profile to resolve", func(t *testing.T) {
		got := server.ungovernedNotice(context.Background(), operator)
		if got == nil || !got.NoPerson || got.Unit != "" || len(got.Limits) != 0 {
			t.Fatalf("got %+v, want only no_person", got)
		}
	})
	t.Run("a signed-in admin sees the unit and three limits then more", func(t *testing.T) {
		got := server.ungovernedNotice(governAdminSessionCtx(), operator)
		if got == nil || got.NoPerson || got.Unit != "Contractors" || !got.More ||
			!slices.Equal(got.Limits, []string{"2 CPUs", "4 GiB of memory", "3 runs at once"}) {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("a unit with few limits says no more", func(t *testing.T) {
		small := govServer(&capStore{govProfile: govProfile("Small"), govTier: types.CapabilitySubjectUser})
		got := small.ungovernedNotice(governAdminSessionCtx(), operator)
		if got == nil || got.Unit != "Small" || got.More || !slices.Equal(got.Limits, []string{"no interactive runs"}) {
			t.Fatalf("got %+v", got)
		}
	})
}

func TestUngovernedNoticeStates(t *testing.T) {
	operator := governanceCeiling{Operator: true}
	t.Run("a person whose type resolves to no unit has no profile to compare with", func(t *testing.T) {
		got := govServer(&capStore{}).ungovernedNotice(governAdminSessionCtx(), operator)
		if got == nil || !got.NoProfile || got.Unavailable || got.Unit != "" || len(got.Limits) != 0 {
			t.Fatalf("got %+v, want only no_unit", got)
		}
	})
	t.Run("a failed comparison read is unavailable, never an empty limits list", func(t *testing.T) {
		got := govServer(&capStore{govErr: errors.New("boom")}).ungovernedNotice(governAdminSessionCtx(), operator)
		if got == nil || !got.Unavailable || got.NoProfile || got.Unit != "" || len(got.Limits) != 0 || got.More {
			t.Fatalf("got %+v, want only unavailable", got)
		}
	})
	t.Run("more is set only past three limits", func(t *testing.T) {
		for _, tc := range []struct {
			limits types.GovernanceLimits
			want   []string
			more   bool
		}{
			{types.GovernanceLimits{MaxCPUMillis: 1000, MaxMemoryMiB: 512, MaxConcurrentRuns: 2}, []string{"1 CPU", "512 MiB of memory", "2 runs at once"}, false},
			{types.GovernanceLimits{MaxCPUMillis: 500, MaxMemoryMiB: 512, MaxConcurrentRuns: 2, DenyUIApps: true}, []string{"0.5 CPUs", "512 MiB of memory", "2 runs at once"}, true},
		} {
			unit := &types.GovernanceUnit{ID: uuid.New(), Name: "U", Ceiling: govProfileSpec(), Limits: tc.limits}
			got := govServer(&capStore{govProfile: unit, govTier: types.CapabilitySubjectUser}).ungovernedNotice(governAdminSessionCtx(), operator)
			if got == nil || got.More != tc.more || !slices.Equal(got.Limits, tc.want) {
				t.Fatalf("limits %+v: got %+v, want %v more=%v", tc.limits, got, tc.want, tc.more)
			}
		}
	})
}

// The comparison is preview-only: running it changes nothing the launch checks read. The
// operator's ceiling stays the deployment's, with no unit, before and after, memo or not.
func TestUngovernedNoticeLeavesLaunchAuthorizationUnchanged(t *testing.T) {
	unit := &types.GovernanceUnit{ID: uuid.New(), Name: "Contractors", Ceiling: govProfileSpec(), Limits: types.GovernanceLimits{DenyInteractive: true, MaxCPUMillis: 1000}}
	server := govServer(&capStore{govProfile: unit, govTier: types.CapabilitySubjectUser})
	for name, ctx := range map[string]context.Context{
		"plain": governAdminSessionCtx(),
		"memo":  withCeilingMemo(governAdminSessionCtx()),
	} {
		t.Run(name, func(t *testing.T) {
			before, err := server.effectiveCeiling(ctx)
			if err != nil || !before.Operator || before.Unit != nil {
				t.Fatalf("before: %+v, %v", before, err)
			}
			if got := server.ungovernedNotice(ctx, before); got == nil || got.Unit != "Contractors" {
				t.Fatalf("notice = %+v", got)
			}
			after, err := server.effectiveCeiling(ctx)
			if err != nil || !after.Operator || after.Unit != nil || !reflect.DeepEqual(after.Limits, before.Limits) ||
				!reflect.DeepEqual(specJSON(after.Spec), specJSON(before.Spec)) {
				t.Fatalf("after: %+v, %v; before: %+v", after, err, before)
			}
		})
	}
}

func TestPolicyPreviewUngovernedBlock(t *testing.T) {
	h := newHarness(t)
	w := do(t, h.srv, http.MethodPost, policyPreviewPath, adminToken, `{"agent":"claude-code"}`)
	got := previewResult(t, w).Ungoverned
	if got == nil || !got.NoPerson {
		t.Fatalf("admin token: ungoverned = %+v, want no_person", got)
	}
}
