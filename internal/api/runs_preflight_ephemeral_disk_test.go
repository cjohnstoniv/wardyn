// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// previewEphemeralDiskMiB resolves an inline policy through the DRY-RUN arm of
// resolveRunPolicy — the chokepoint POST /runs/preflight calls — and returns the
// disk_mib the caller is shown.
func previewEphemeralDiskMiB(t *testing.T, requested int, site types.SiteConfig,
	profile *types.GovernanceProfile, operator bool,
) int {
	t.Helper()
	h := newHarness(t)
	st := &memberBoundStore{profile: profile, site: site}
	cfg := baseTestConfig(h, st)
	cfg.OIDC = &oidc.Authenticator{}
	srv := New(cfg)

	role := oidc.RoleMember
	if operator {
		role = oidc.RoleAdmin
	}
	ctx := operatorCtx("sub-parity", "parity@corp.example", role)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs/preflight", nil).WithContext(ctx)

	spec := types.RunPolicySpec{MinConfinementClass: types.CC2}
	if requested > 0 {
		spec.Resources = &types.ResourceLimits{DiskMiB: requested}
	}
	w := httptest.NewRecorder()
	got, _, _, ok := srv.resolveRunPolicy(ctx, w, r, &createRunRequest{Agent: "claude-code", InlinePolicy: &spec}, true)
	if !ok {
		t.Fatalf("preflight resolve refused: %d %s", w.Code, w.Body.String())
	}
	if got.Resources == nil {
		return 0
	}
	return got.Resources.DiskMiB
}

// TestPreflightAndDispatchAgreeOnEphemeralDisk is the pin two code comments
// argued away — "this run cannot disagree about the number" and "sharing the
// function is why there is no test pinning that the two agree".
//
// They did disagree: dispatch clamped by BOTH storage.ephemeral.max_disk_mib and
// the profile's MaxEphemeralDiskMiB and filled a zero from default_disk_mib,
// while the preview applied the profile half alone — so a member holding a
// 100000 MiB policy under a 4096 MiB org maximum previewed 100000 and ran on
// 4096. Same inputs down both paths, one number out.
func TestPreflightAndDispatchAgreeOnEphemeralDisk(t *testing.T) {
	sized := func(maxMiB int) *types.GovernanceProfile {
		return &types.GovernanceProfile{
			ID: uuid.New(), Name: "sized",
			Ceiling: types.RunPolicySpec{MinConfinementClass: types.CC2},
			Limits:  types.GovernanceLimits{MaxEphemeralDiskMiB: maxMiB},
		}
	}
	for _, tc := range []struct {
		name      string
		requested int
		site      types.SiteConfig
		profile   *types.GovernanceProfile
		operator  bool
		want      int
	}{{
		name: "a zero request with no org default stays unbounded on both paths",
		want: 0,
	}, {
		name: "a zero request is FILLED from the org default on both paths",
		site: ephemeralSite(4096, 0), want: 4096,
	}, {
		// The repro, shrunk: the org maximum binds the preview too.
		name:      "a request over the ORG maximum is clamped on both paths",
		requested: 100000, site: ephemeralSite(0, 4096), want: 4096,
	}, {
		// SCOPE half one: the org's number binds EVERY caller, operators included
		// — an operator resolves no profile at all and must still see 4096.
		name:      "an operator is bound by the ORG maximum on both paths",
		requested: 100000, site: ephemeralSite(0, 4096), operator: true, want: 4096,
	}, {
		// SCOPE half two: the profile binds an ASSIGNED member, and the narrower
		// of the two ceilings wins whichever side it is on.
		name:      "a member under an org maximum AND a profile limit takes the org's when it is narrower",
		requested: 100000, site: ephemeralSite(0, 2048), profile: sized(8192), want: 2048,
	}, {
		name:      "…and the profile's when THAT is narrower",
		requested: 100000, site: ephemeralSite(0, 8192), profile: sized(2048), want: 2048,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			preview := previewEphemeralDiskMiB(t, tc.requested, tc.site, tc.profile, tc.operator)

			policy := types.RunPolicySpec{}
			if tc.requested > 0 {
				policy.Resources = &types.ResourceLimits{DiskMiB: tc.requested}
			}
			gc := governanceCeiling{}
			if tc.profile != nil {
				gc = governanceCeiling{Profile: tc.profile, Limits: tc.profile.Limits}
			}
			res, _, _ := ephemeralDispatch(t, policy, tc.site, gc)

			if preview != tc.want {
				t.Errorf("preflight disk_mib = %d, want %d", preview, tc.want)
			}
			if res.DiskMiB != int64(tc.want) {
				t.Errorf("dispatch disk_mib = %d, want %d", res.DiskMiB, tc.want)
			}
			if int64(preview) != res.DiskMiB {
				t.Fatalf("DISAGREE: preflight shows %d MiB, dispatch gives %d MiB", preview, res.DiskMiB)
			}
		})
	}
}
