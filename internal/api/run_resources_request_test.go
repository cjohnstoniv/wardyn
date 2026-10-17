// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// resourceStore is memberBoundStore plus an empty workspace list: the fold reads the workspaces a request
// names, and these requests name none.
type resourceStore struct{ *memberBoundStore }

func (resourceStore) ListWorkspaces(context.Context) ([]types.Workspace, error) { return nil, nil }

// resourceServer is an unpooled server over a store that answers only the caller's governance profile and
// one stored policy row, on a deployment whose default size is 2000m / 4096 MiB.
func resourceServer(t *testing.T, st *memberBoundStore) *Server {
	t.Helper()
	t.Cleanup(func() { runner.SetDefaultLimits(0, 0) })
	runner.SetDefaultLimits(2000, 4096)
	cfg := baseTestConfig(newHarness(t), resourceStore{st})
	cfg.OIDC = &oidc.Authenticator{}
	cfg.DefaultPolicy = types.RunPolicySpec{MinConfinementClass: types.CC2, AllowedDomains: []string{"api.anthropic.com"}}
	return New(cfg)
}

// resourceFold folds req through the preflight door's own steps: the caller's ceiling, then the fold.
func resourceFold(t *testing.T, srv *Server, ctx context.Context, req createRunRequest) runFold {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs/preflight", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	ceiling, denied := srv.denyUserRequest(w, r, req)
	if denied {
		t.Fatalf("denied: %d %s", w.Code, w.Body.String())
	}
	f, ok := srv.foldRunRequest(w, r, foldPreflight, &req, ceiling, "")
	if !ok {
		t.Fatalf("fold refused: %d %s", w.Code, w.Body.String())
	}
	return f
}

func sizeMemberCtx() context.Context {
	return operatorCtx("sub-res", "res@corp.example", oidc.RoleUser)
}

// checkCPU holds the fold's CPU answer and the spec it executes to one expectation.
func checkCPU(t *testing.T, f runFold, requested, applied int, capped bool, src types.LimitSource) {
	t.Helper()
	got := f.place.applied.CPUMillis
	if got.Requested != requested || got.Applied != applied || got.Capped != capped || got.Source != src {
		t.Errorf("cpu = %+v, want requested %d applied %d capped %v from %s", got, requested, applied, capped, src)
	}
	if f.spec.Resources == nil || f.spec.Resources.CPUMillis != applied {
		t.Errorf("executed spec = %+v, want cpu %d", f.spec.Resources, applied)
	}
	if warned := slices.Contains(f.policyWarns, composer.WarnResourcesCapped); warned != capped {
		t.Errorf("cap warning = %v, want %v (%v)", warned, capped, f.policyWarns)
	}
}

// DR-0112 (b): an oversized inline ask is the ASK, and it is never governance's baseline. A member whose
// ceiling sets no size asks inline for 16 CPU: they ask 16, get the deployment's 2 and hear about it.
func TestResourcesOversizedInlineAskIsBoundedNotTrusted(t *testing.T) {
	srv := resourceServer(t, &memberBoundStore{})
	inline := types.RunPolicySpec{MinConfinementClass: types.CC2, AllowedDomains: []string{"api.anthropic.com"},
		Resources: &types.ResourceLimits{CPUMillis: 16000}}
	f := resourceFold(t, srv, sizeMemberCtx(), createRunRequest{Agent: "claude-code", InlinePolicy: &inline})
	checkCPU(t, f, 16000, 2000, true, types.LimitSourceGovernance)
}

// DR-0112 (c): the baseline is the stored policy's size AFTER the member clamp, the ask its size before.
// A profile whose ceiling is 3 CPU, and a stored policy of 6: the member asks 6 and gets 3.
func TestResourcesStoredPolicyBaselineIsTheResolvedSize(t *testing.T) {
	id := uuid.New()
	st := &memberBoundStore{
		profile: &types.GovernanceProfile{ID: uuid.New(), Name: "three",
			Ceiling: types.RunPolicySpec{MinConfinementClass: types.CC2, AllowedDomains: []string{"api.anthropic.com"},
				Resources: &types.ResourceLimits{CPUMillis: 3000}}},
		policy: types.RunPolicy{ID: id, Spec: types.RunPolicySpec{MinConfinementClass: types.CC2, AllowedDomains: []string{"api.anthropic.com"},
			Resources: &types.ResourceLimits{CPUMillis: 6000}}},
	}
	srv := resourceServer(t, st)
	f := resourceFold(t, srv, sizeMemberCtx(), createRunRequest{Agent: "claude-code", PolicyID: &id})
	checkCPU(t, f, 6000, 3000, true, types.LimitSourceGovernance)
	if f.place.applied.CPUMillis.SourceName != "three" {
		t.Errorf("source_name = %q, want the profile's name", f.place.applied.CPUMillis.SourceName)
	}
}

// DR-0112: a policy_overlay that names resources is the ask, as an inline policy's size is: a member
// overlaying 16 CPU on the default asks 16, gets the deployment's 2 and hears about it. An overlay that names
// other keys leaves the ask where it was.
func TestResourcesOverlayAskIsTheAsk(t *testing.T) {
	srv := resourceServer(t, &memberBoundStore{})
	f := resourceFold(t, srv, sizeMemberCtx(), createRunRequest{Agent: "claude-code",
		PolicyOverlay: json.RawMessage(`{"resources":{"cpu_millis":16000}}`)})
	checkCPU(t, f, 16000, 2000, true, types.LimitSourceGovernance)
	f = resourceFold(t, srv, sizeMemberCtx(), createRunRequest{Agent: "claude-code",
		PolicyOverlay: json.RawMessage(`{"allowed_domains":["api.anthropic.com"]}`)})
	if got := f.place.applied.CPUMillis.Requested; got != 0 {
		t.Errorf("cpu requested = %d, want 0 for an overlay that names no resources", got)
	}
}

// An unassigned member's stored policy may be larger than the deployment's default; asking for that same
// size is no widening, so it is neither reduced nor warned, and a request value beats the policy's.
func TestResourcesRequestBeatsTheUntouchedStoredSize(t *testing.T) {
	id := uuid.New()
	st := &memberBoundStore{policy: types.RunPolicy{ID: id, Spec: types.RunPolicySpec{
		MinConfinementClass: types.CC2, AllowedDomains: []string{"api.anthropic.com"},
		Resources: &types.ResourceLimits{CPUMillis: 8000, MemoryMiB: 8192},
	}}}
	srv := resourceServer(t, st)
	f := resourceFold(t, srv, sizeMemberCtx(), createRunRequest{Agent: "claude-code", PolicyID: &id})
	checkCPU(t, f, 8000, 8000, false, "")
	f = resourceFold(t, srv, sizeMemberCtx(), createRunRequest{Agent: "claude-code", PolicyID: &id, Resources: &client.RunResources{CPUMillis: 3000}})
	checkCPU(t, f, 3000, 3000, false, "")
	if m := f.place.applied.MemoryMiB; m.Requested != 8192 || m.Applied != 8192 {
		t.Errorf("memory = %+v, want the policy's 8192 kept where the request names none", m)
	}
}

// An operator stands outside governance: the request contract's bound is their cap.
func TestResourcesOperatorBoundIsTheContractMax(t *testing.T) {
	srv := resourceServer(t, &memberBoundStore{})
	f := resourceFold(t, srv, operatorCtx("sub-op", "op@corp.example", oidc.RoleAdmin),
		createRunRequest{Agent: "claude-code", Resources: &client.RunResources{CPUMillis: maxRunResourceCPU, MemoryMiB: 131072}})
	checkCPU(t, f, maxRunResourceCPU, maxRunResourceCPU, false, "")
	if l := f.place.limits.CPUMillis; l.Cap.Value != types.RunnerPoolCPUMillisMax || l.CapSource != types.LimitSourceDeployment {
		t.Errorf("limits cpu = %+v, want the deployment's contract bound", l)
	}
}

// A profile that sets a maximum and no size of its own (#1949): the maximum is the size, so an ask of
// 8 CPU is kept; an untouched run still gets the deployment's own size (its default policy's 1000m,
// below the platform's 2000m), never more.
func TestResourcesProfileMaximumRaisesTheCapNotTheDefault(t *testing.T) {
	t.Cleanup(func() { runner.SetDefaultLimits(0, 0) })
	runner.SetDefaultLimits(2000, 4096)
	cfg := baseTestConfig(newHarness(t), resourceStore{&memberBoundStore{profile: &types.GovernanceProfile{ID: uuid.New(), Name: "max-eight",
		Ceiling: types.RunPolicySpec{MinConfinementClass: types.CC2, AllowedDomains: []string{"api.anthropic.com"}},
		Limits:  types.GovernanceLimits{MaxCPUMillis: 8000}}}})
	cfg.OIDC = &oidc.Authenticator{}
	cfg.DefaultPolicy = types.RunPolicySpec{MinConfinementClass: types.CC2, AllowedDomains: []string{"api.anthropic.com"},
		Resources: &types.ResourceLimits{CPUMillis: 1000, MemoryMiB: 2048}}
	srv := New(cfg)
	f := resourceFold(t, srv, sizeMemberCtx(), createRunRequest{Agent: "claude-code", Resources: &client.RunResources{CPUMillis: 8000}})
	checkCPU(t, f, 8000, 8000, false, "")
	f = resourceFold(t, srv, sizeMemberCtx(), createRunRequest{Agent: "claude-code"})
	checkCPU(t, f, 0, 1000, false, types.LimitSourceDeployment)
}

// DR-0112 under a deployment whose default policy (4000m / 8192 MiB) is above the platform default:
// the baseline of an inline ask is the member default, the deployment's own size, so an inline ask of
// 4000 is the ASK and is kept, and an inline policy with no size gets that size.
func TestResourcesDeploymentDefaultAbovePlatform(t *testing.T) {
	t.Cleanup(func() { runner.SetDefaultLimits(0, 0) })
	runner.SetDefaultLimits(2000, 4096)
	cfg := baseTestConfig(newHarness(t), resourceStore{&memberBoundStore{profile: &types.GovernanceProfile{ID: uuid.New(), Name: "plain",
		Ceiling: types.RunPolicySpec{MinConfinementClass: types.CC2, AllowedDomains: []string{"api.anthropic.com"}}}}})
	cfg.OIDC = &oidc.Authenticator{}
	cfg.DefaultPolicy = types.RunPolicySpec{MinConfinementClass: types.CC2, AllowedDomains: []string{"api.anthropic.com"},
		Resources: &types.ResourceLimits{CPUMillis: 4000, MemoryMiB: 8192}}
	srv := New(cfg)
	inline := func(cpu int) *types.RunPolicySpec {
		spec := types.RunPolicySpec{MinConfinementClass: types.CC2, AllowedDomains: []string{"api.anthropic.com"}}
		if cpu > 0 {
			spec.Resources = &types.ResourceLimits{CPUMillis: cpu}
		}
		return &spec
	}
	f := resourceFold(t, srv, sizeMemberCtx(), createRunRequest{Agent: "claude-code", InlinePolicy: inline(4000)})
	checkCPU(t, f, 4000, 4000, false, "")
	f = resourceFold(t, srv, sizeMemberCtx(), createRunRequest{Agent: "claude-code", InlinePolicy: inline(0)})
	checkCPU(t, f, 0, 4000, false, types.LimitSourceDeployment)
	if m := f.place.applied.MemoryMiB; m.Applied != 8192 {
		t.Errorf("memory = %+v, want the deployment's 8192", m)
	}
}
