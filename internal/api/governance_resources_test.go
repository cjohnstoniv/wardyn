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

// The four sizes measured on an adopter's install: an admin and a member with no
// profile got the deployment's 1000m, a member whose profile omitted `resources`
// got the platform's 2000m, and one whose profile set 64000 got 64000m. A member's
// sandbox is the deployment's size, or one the profile explicitly caps.
func TestMemberSandboxSizeNeverEscapesTheDeployment(t *testing.T) {
	deployment := &types.ResourceLimits{CPUMillis: 1000, MemoryMiB: 2048}
	wide := &types.ResourceLimits{CPUMillis: 64000, MemoryMiB: 1 << 20}
	profile := func(res *types.ResourceLimits, lim types.GovernanceLimits) *types.GovernanceProfile {
		return &types.GovernanceProfile{
			ID: uuid.New(), Name: "p",
			Ceiling: types.RunPolicySpec{MinConfinementClass: types.CC2, Resources: res},
			Limits:  lim,
		}
	}
	for _, tc := range []struct {
		name         string
		operator     bool
		profile      *types.GovernanceProfile
		wantCPU, mem int
	}{
		{"admin", true, nil, 1000, 2048},
		{"member with no profile", false, nil, 1000, 2048},
		{"member whose profile omits resources", false, profile(nil, types.GovernanceLimits{}), 1000, 2048},
		{"member whose profile zeroes one field", false, profile(&types.ResourceLimits{CPUMillis: 3000}, types.GovernanceLimits{}), 3000, 2048},
		{"member whose profile sets 64000 and no maximum", false, profile(wide, types.GovernanceLimits{}), 64000, 1 << 20},
		{"member whose profile sets 64000 under a maximum", false,
			profile(wide, types.GovernanceLimits{MaxCPUMillis: 4000, MaxMemoryMiB: 8192}), 4000, 8192},
		{"a maximum below the deployment's size", false,
			profile(nil, types.GovernanceLimits{MaxCPUMillis: 500, MaxMemoryMiB: 1024}), 500, 1024},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			cfg := baseTestConfig(h, &memberBoundStore{profile: tc.profile})
			cfg.OIDC = &oidc.Authenticator{}
			cfg.DefaultPolicy = types.RunPolicySpec{MinConfinementClass: types.CC2, Resources: deployment}
			srv := New(cfg)
			role := oidc.RoleUser
			if tc.operator {
				role = oidc.RoleAdmin
			}
			ctx := operatorCtx("sub-size", "size@corp.example", role)
			r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(ctx)

			// create (dryRun false) and preflight (dryRun true), the no-policy arm.
			for _, dryRun := range []bool{false, true} {
				w := httptest.NewRecorder()
				got, _, _, _, ok := srv.resolveRunPolicy(ctx, w, r, &createRunRequest{Agent: "claude-code"}, dryRun)
				if !ok {
					t.Fatalf("dryRun=%v refused: %d %s", dryRun, w.Code, w.Body.String())
				}
				if got.Resources == nil || got.Resources.CPUMillis != tc.wantCPU || got.Resources.MemoryMiB != tc.mem {
					t.Errorf("dryRun=%v resolved %+v, want %dm/%dMiB", dryRun, got.Resources, tc.wantCPU, tc.mem)
				}
			}

			// dispatch, handed the UNBOUNDED ceiling spec: its own phase holds the maximum.
			gc, err := srv.effectiveCeiling(ctx)
			if err != nil {
				t.Fatal(err)
			}
			res, _, _ := ephemeralDispatch(t, gc.Spec, types.SiteConfig{}, gc)
			if res.CPUMillis != int64(tc.wantCPU) || res.MemoryMiB != int64(tc.mem) {
				t.Errorf("dispatch gave %dm/%dMiB, want %dm/%dMiB", res.CPUMillis, res.MemoryMiB, tc.wantCPU, tc.mem)
			}
		})
	}
}

// A member's own inline request is bounded by the deployment's size when their
// profile omits resources, and by the profile's maximum when it sets one.
func TestMemberInlineRequestBoundByInheritedSize(t *testing.T) {
	prof := &types.GovernanceProfile{
		ID: uuid.New(), Name: "p",
		Ceiling: types.RunPolicySpec{MinConfinementClass: types.CC2},
		Limits:  types.GovernanceLimits{MaxMemoryMiB: 1024},
	}
	h := newHarness(t)
	cfg := baseTestConfig(h, &memberBoundStore{profile: prof})
	cfg.OIDC = &oidc.Authenticator{}
	cfg.DefaultPolicy = types.RunPolicySpec{MinConfinementClass: types.CC2,
		Resources: &types.ResourceLimits{CPUMillis: 1000, MemoryMiB: 2048}}
	srv := New(cfg)
	ctx := operatorCtx("sub-size", "size@corp.example", oidc.RoleUser)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(ctx)
	spec := types.RunPolicySpec{MinConfinementClass: types.CC2,
		Resources: &types.ResourceLimits{CPUMillis: 64000, MemoryMiB: 64000}}
	got, _, _, _, ok := srv.resolveRunPolicy(ctx, httptest.NewRecorder(), r, &createRunRequest{Agent: "claude-code", InlinePolicy: &spec}, false)
	if !ok {
		t.Fatal("refused")
	}
	if got.Resources.CPUMillis != 1000 || got.Resources.MemoryMiB != 1024 {
		t.Errorf("resolved %+v, want 1000m/1024MiB", got.Resources)
	}
}

// The no-policy bound is resource-only: composer.Clamp would drop the profile's
// workspace mounts, and the member who sent no policy is the commonest launch.
func TestNoPolicyMemberKeepsProfileMountsUnderSizeCap(t *testing.T) {
	mount := types.WorkspaceMount{Source: "/srv/data", Target: "/data"}
	prof := &types.GovernanceProfile{
		ID: uuid.New(), Name: "p",
		Ceiling: types.RunPolicySpec{MinConfinementClass: types.CC2, WorkspaceMounts: []types.WorkspaceMount{mount}},
		Limits:  types.GovernanceLimits{MaxCPUMillis: 500},
	}
	h := newHarness(t)
	cfg := baseTestConfig(h, &memberBoundStore{profile: prof})
	cfg.OIDC = &oidc.Authenticator{}
	srv := New(cfg)
	ctx := operatorCtx("sub-size", "size@corp.example", oidc.RoleUser)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(ctx)
	got, _, _, _, ok := srv.resolveRunPolicy(ctx, httptest.NewRecorder(), r, &createRunRequest{Agent: "claude-code"}, false)
	if !ok {
		t.Fatal("refused")
	}
	if len(got.WorkspaceMounts) != 1 || got.WorkspaceMounts[0] != mount {
		t.Errorf("mounts = %+v, want the profile's one mount kept", got.WorkspaceMounts)
	}
	if got.Resources == nil || got.Resources.CPUMillis != 500 {
		t.Errorf("Resources = %+v, want cpu capped to 500", got.Resources)
	}
}

// The sign-in sandbox keeps its own small size even when the ceiling now carries
// an inherited, larger one.
func TestSignInSandboxIgnoresInheritedSize(t *testing.T) {
	srv := &Server{cfg: Config{DefaultPolicy: types.RunPolicySpec{Resources: &types.ResourceLimits{CPUMillis: 1000, MemoryMiB: 2048}}}}
	res := srv.inheritDeploymentResources(nil)
	got := harnessLoginResources(Config{}, governanceCeiling{Spec: types.RunPolicySpec{Resources: res}})
	if got.CPUMillis != 500 || got.MemoryMiB != 512 {
		t.Errorf("sign-in size = %dm/%dMiB, want 500m/512MiB", got.CPUMillis, got.MemoryMiB)
	}
}

func TestGovernanceLimitsRefuseNegativeSizes(t *testing.T) {
	for _, l := range []types.GovernanceLimits{{MaxCPUMillis: -1}, {MaxMemoryMiB: -1}} {
		if governanceLimitsRefusal(l) == "" {
			t.Errorf("%+v was accepted", l)
		}
	}
	if err := validatePolicySpec(types.RunPolicySpec{MinConfinementClass: types.CC2, Resources: &types.ResourceLimits{CPUMillis: -1}}); err == nil {
		t.Error("a negative resources.cpu_millis was accepted")
	}
}
