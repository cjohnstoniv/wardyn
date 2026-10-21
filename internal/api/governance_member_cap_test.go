// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestGovCovProfileWriteExemptComparesTheMemberCap: a maximum set where the profile authored no size
// IS the member's size (runnerpool.MemberCap), so the exemption compares the member cap, not only
// composer.Leq, which reads a new maximum as tighter. The deployment's size here is the platform
// default, 2000 millicores and 4096 MiB.
func TestGovCovProfileWriteExemptComparesTheMemberCap(t *testing.T) {
	s, _ := govCovServer(t, &govCovStore{})
	profile := func(res *types.ResourceLimits, limits types.GovernanceLimits) types.GovernanceUnit {
		p := govCovStandalone(govCovPID, "p", "pypi.org")
		p.Ceiling.Resources, p.Limits = res, limits
		return p
	}
	body := func(resources, limits string) string {
		b := `{"name":"p","ceiling":{"allowed_domains":["pypi.org"],"min_confinement_class":"CC2"`
		if resources != "" {
			b += `,"resources":` + resources
		}
		b += `}`
		if limits != "" {
			b += `,"limits":` + limits
		}
		return b + `}`
	}
	for _, c := range []struct {
		name string
		cur  types.GovernanceUnit
		body string
		want bool
	}{
		{"an unset CPU maximum set above the default is held", profile(nil, types.GovernanceLimits{}), body("", `{"max_cpu_millis":8000}`), false},
		{"an unset memory maximum set above the default is held", profile(nil, types.GovernanceLimits{}), body("", `{"max_memory_mib":16384}`), false},
		{"a CPU maximum below the default applies", profile(nil, types.GovernanceLimits{}), body("", `{"max_cpu_millis":500}`), true},
		{"a memory maximum below the default applies", profile(nil, types.GovernanceLimits{}), body("", `{"max_memory_mib":1024}`), true},
		{"removing an authored CPU size under a maximum is held",
			profile(&types.ResourceLimits{CPUMillis: 3000}, types.GovernanceLimits{MaxCPUMillis: 4000}), body("", `{"max_cpu_millis":4000}`), false},
		{"removing an authored memory size under a maximum is held",
			profile(&types.ResourceLimits{MemoryMiB: 8192}, types.GovernanceLimits{MaxMemoryMiB: 16384}), body("", `{"max_memory_mib":16384}`), false},
		{"lowering an authored size under the same maximum applies",
			profile(&types.ResourceLimits{CPUMillis: 3000}, types.GovernanceLimits{MaxCPUMillis: 4000}), body(`{"cpu_millis":2500}`, `{"max_cpu_millis":4000}`), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			all := []types.GovernanceUnit{c.cur}
			p, res := govCovBuilt(t, s, all, govCovPID, c.body)
			if got := s.unitWriteExempt(context.Background(), all, p, res); got != c.want {
				t.Errorf("unitWriteExempt = %v, want %v (member cap %+v)", got, c.want, s.resolvedMemberCap(res))
			}
		})
	}
}

// TestGovCovOverlayMayNotRaiseTheMemberCap: an overlay maximum over a base that bounds nothing is
// the member's size, so one above the base's member cap is refused at write, and one below it is
// not. A composition whose base later stops bounding a maximum the overlay named is lowered to the
// base's cap at resolve, two levels down included, and the admin view says so.
func TestGovCovOverlayMayNotRaiseTheMemberCap(t *testing.T) {
	s, _ := govCovServer(t, &govCovStore{})
	pi := func(v int) *int { return &v }
	base := govCovStandalone(govCovBase, "base", "pypi.org")
	child := func(id uuid.UUID, name string, on uuid.UUID, limits types.LimitsOverlay) types.GovernanceUnit {
		p := govCovComposedOn(id, name, on, "pypi.org")
		p.OverlayLimits = &limits
		return p
	}
	for _, c := range []struct {
		name    string
		limits  types.LimitsOverlay
		refused bool
	}{
		{"a CPU maximum above the base's member cap", types.LimitsOverlay{MaxCPUMillis: pi(8000)}, true},
		{"a memory maximum above the base's member cap", types.LimitsOverlay{MaxMemoryMiB: pi(16384)}, true},
		{"a maximum below it", types.LimitsOverlay{MaxCPUMillis: pi(500), MaxMemoryMiB: pi(1024)}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := s.checkGraphWrite(child(govCovPID, "child", govCovBase, c.limits), []types.GovernanceUnit{base})
			var refusal *unitWriteError
			switch {
			case c.refused && (!errors.As(err, &refusal) || refusal.status != http.StatusBadRequest || refusal.reason != reasonGovernanceOverlayInvalid):
				t.Fatalf("checkGraphWrite = %v, want 400 governance_overlay_invalid", err)
			case !c.refused && err != nil:
				t.Fatalf("checkGraphWrite = %v, want no refusal", err)
			}
		})
	}

	t.Run("a base that stops bounding is not exceeded two levels down", func(t *testing.T) {
		bounded := base
		bounded.Limits = types.GovernanceLimits{MaxCPUMillis: 8000, MaxMemoryMiB: 16384}
		mid := child(govCovPID, "mid", govCovBase, types.LimitsOverlay{MaxCPUMillis: pi(6000), MaxMemoryMiB: pi(12288)})
		leafID := uuid.MustParse("dddddddd-0000-0000-0000-0000000000bb")
		leaf := govCovComposedOn(leafID, "leaf", govCovPID, "pypi.org")
		if _, _, err := s.checkGraphWrite(mid, []types.GovernanceUnit{bounded}); err != nil {
			t.Fatalf("the mid overlay is within its base when written: %v", err)
		}
		// The base is then written with no maximum, so its own member cap is the default size.
		res, err := s.composeChain([]types.GovernanceUnit{leaf, mid, base})
		if err != nil {
			t.Fatal(err)
		}
		if got, want := s.resolvedMemberCap(res), (types.ResourceLimits{CPUMillis: 2000, MemoryMiB: 4096}); got != want {
			t.Errorf("leaf member cap = %+v, want the base's %+v", got, want)
		}
		if !strings.Contains(strings.Join(res.AdminWarnings, "\n"), "lowered to it") {
			t.Errorf("admin warnings = %q, want the lowering named", res.AdminWarnings)
		}
		// While the base still bounds them, the overlay's maximums stand.
		res, err = s.composeChain([]types.GovernanceUnit{leaf, mid, bounded})
		if err != nil {
			t.Fatal(err)
		}
		if got, want := s.resolvedMemberCap(res), (types.ResourceLimits{CPUMillis: 6000, MemoryMiB: 12288}); got != want || len(res.AdminWarnings) != 0 {
			t.Errorf("leaf member cap = %+v (warnings %q), want %+v and nothing said", got, res.AdminWarnings, want)
		}
	})
}
