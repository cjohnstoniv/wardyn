// Copyright 2026 The Wardyn Authors
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

// A profile's max_ephemeral_disk_mib binds an assigned member and not an ungoverned admin, at every door:
// the Review rail, the preview, create and dispatch. The member rows prove the same fixture does clamp,
// so the admin rows cannot pass because the limit never reached the door.
func TestEphemeralDiskMaximum_BindsAMemberAndNotAnUngovernedAdmin(t *testing.T) {
	const asked, limit = 8192, 2048
	for name, tc := range map[string]struct {
		role string
		want int
	}{"an admin": {oidc.RoleAdmin, asked}, "a member": {oidc.RoleUser, limit}} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			cfg := baseTestConfig(h, &memberBoundStore{profile: &types.GovernanceUnit{
				ID: uuid.New(), Name: "sized", Ceiling: types.RunPolicySpec{MinConfinementClass: types.CC2},
				Limits: types.GovernanceLimits{MaxEphemeralDiskMiB: limit},
			}})
			cfg.OIDC = &oidc.Authenticator{}
			srv := New(cfg)
			ctx := operatorCtx("sub-disk", "disk@corp.example", tc.role)
			r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(ctx)
			inline := types.RunPolicySpec{MinConfinementClass: types.CC2, Resources: &types.ResourceLimits{DiskMiB: asked}}
			got := func(door string, spec types.RunPolicySpec) {
				t.Helper()
				if spec.Resources == nil || spec.Resources.DiskMiB != tc.want {
					t.Errorf("%s: disk_mib = %v, want %d", door, spec.Resources, tc.want)
				}
			}

			gc, err := srv.effectiveCeiling(ctx)
			if err != nil {
				t.Fatal(err)
			}
			review, _, ok := srv.boundUserSpec(ctx, httptest.NewRecorder(), r, inline, gc, "invalid inline_policy: ", true)
			if !ok {
				t.Fatal("the Review rail refused the spec")
			}
			got("Review", review)
			for door, dryRun := range map[string]bool{"preview": true, "create": false} {
				in := inline
				spec, _, _, _, ok := srv.resolveRunPolicy(ctx, httptest.NewRecorder(), r,
					&createRunRequest{Agent: "claude-code", Repo: "acme/widgets", InlinePolicy: &in}, dryRun)
				if !ok {
					t.Fatalf("%s refused the spec", door)
				}
				got(door, spec)
			}
			res, _, _ := ephemeralDispatch(t, types.RunPolicySpec{MinConfinementClass: types.CC2, Resources: &types.ResourceLimits{DiskMiB: asked}}, types.SiteConfig{}, gc)
			if res.DiskMiB != int64(tc.want) {
				t.Errorf("dispatch: disk_mib = %d, want %d", res.DiskMiB, tc.want)
			}
		})
	}
}
