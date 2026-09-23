// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ephemeralDispatch drives ONE dispatch and returns the Resources the runner
// actually received plus the audit trail — the two places the effective
// ephemeral size is observable.
//
// Driven straight at dispatchRun with a resolved dispatchCeiling, the way every
// lane delivers one (the runWalledDispatch precedent, which cannot express a
// ceiling whose LIMITS matter and whose deny list is empty).
func ephemeralDispatch(t *testing.T, policy types.RunPolicySpec, site types.SiteConfig,
	gc governanceCeiling,
) (runner.Resources, []types.AuditEvent, uuid.UUID) {
	t.Helper()
	fr := &fakeRunner{}
	srv, st, audit, run := dispatchTeardownFixture(t, fr, types.RunPending)
	srv.cfg.Store = ceilingDispatchStore{dispatchTestStore: st, site: site}
	run.Task = "" // no agent exec / completion watcher: this is about composition
	srv.dispatchRun(context.Background(), run, ceilingForDispatch(gc, adoEntraUngraded()), dispatchParams{
		RunToken: "run-token", Image: "wardyn/claude-code:latest", Policy: policy,
	})
	return fr.lastSpec.Resources, audit.events, run.ID
}

// ephemeralSite is an org storage-provider block; zero fields stay absent, so a
// case can set a default without a maximum and vice versa.
func ephemeralSite(defaultMiB, maxMiB int) types.SiteConfig {
	return types.SiteConfig{WorkspaceProviders: &types.WorkspaceProviders{
		Storage: &types.StorageProviders{Ephemeral: &types.EphemeralProvider{
			DefaultDiskMiB: defaultMiB, MaxDiskMiB: maxMiB,
		}},
	}}
}

// assignedCeiling is an ASSIGNED profile carrying one ephemeral limit and nothing
// else — the shape that makes MaxEphemeralDiskMiB bind at all (ceilingForDispatch
// returns no limits for an unassigned principal).
func assignedCeiling(maxEphemeralDiskMiB int) governanceCeiling {
	return governanceCeiling{
		Profile: &types.GovernanceProfile{Name: "sized"},
		Limits:  types.GovernanceLimits{MaxEphemeralDiskMiB: maxEphemeralDiskMiB},
	}
}

// TestDispatchEphemeralDisk_Precedence is §6.3's precedence table, at the one
// site that implements it.
//
// request/policy disk_mib → FILLED from the org default_disk_mib when that is
// zero → CLAMPED to min(provider max_disk_mib, the profile's
// MaxEphemeralDiskMiB), zeros meaning "no bound" on either ceiling. Never a
// refusal: disk_mib is authored on POLICIES, so a 422 would break every stored
// policy the day an admin first writes a limit.
func TestDispatchEphemeralDisk_Precedence(t *testing.T) {
	for _, tc := range []struct {
		name       string
		requested  int
		site       types.SiteConfig
		ceiling    governanceCeiling
		want       int
		wantFilled bool
	}{{
		name: "an authored request beats the org default", requested: 1024,
		site: ephemeralSite(4096, 0), want: 1024,
	}, {
		name: "a zero request is FILLED from the org default", requested: 0,
		site: ephemeralSite(4096, 0), want: 4096, wantFilled: true,
	}, {
		// The whole reason the fill is the ONLY fill: a maximum bounds a request
		// and never invents one. Filling from max_disk_mib would hand every
		// request-less run a non-zero DiskMiB, and the docker driver fails the
		// create closed on overlay2-over-ext4 — Docker Desktop, WSL2, stock Ubuntu.
		name: "a zero request with NO org default stays unbounded", requested: 0,
		site: ephemeralSite(0, 4096), ceiling: assignedCeiling(2048), want: 0,
	}, {
		name: "a request over the provider maximum is clamped to it", requested: 8192,
		site: ephemeralSite(0, 4096), want: 4096,
	}, {
		name: "a request over an ASSIGNED profile's maximum is clamped to it", requested: 8192,
		ceiling: assignedCeiling(2048), want: 2048,
	}, {
		// Both ceilings, one expression: the narrower of the two wins whichever
		// side it is on.
		name: "the NARROWER of the two ceilings binds", requested: 8192,
		site: ephemeralSite(0, 4096), ceiling: assignedCeiling(2048), want: 2048,
	}, {
		name: "a FILLED default is clamped by the provider maximum too", requested: 0,
		site: ephemeralSite(8192, 4096), want: 4096, wantFilled: true,
	}, {
		// SCOPE, the asymmetry stated once: MaxEphemeralDiskMiB binds ASSIGNED
		// members only — an operator short-circuits at effectiveCeiling's step 1
		// and an unassigned member resolves no limits at all, so ceilingForDispatch
		// carries none for either.
		name: "an unassigned principal is EXEMPT from the governance maximum", requested: 8192,
		ceiling: governanceCeiling{Limits: types.GovernanceLimits{MaxEphemeralDiskMiB: 2048}},
		want:    8192,
	}, {
		// …and is NOT exempt from the provider's: max_disk_mib is the ORG's
		// number and binds every caller.
		name: "an unassigned principal is still bound by the PROVIDER maximum", requested: 8192,
		site:    ephemeralSite(0, 4096),
		ceiling: governanceCeiling{Limits: types.GovernanceLimits{MaxEphemeralDiskMiB: 2048}},
		want:    4096,
	}, {
		// PROVENANCE STOPS AT THIS SITE. composer.Clamp's capField already fills a
		// zero request UP TO a profile SPEC's resources.disk_mib at create, so a
		// non-zero size can reach dispatch without the member typing it — and it is
		// still REQUESTED, not filled: an admin wrote that number on a policy, which
		// is exactly the input the docker driver is right to refuse a create over.
		// Only the ORG DEFAULT is a fill.
		name: "a size that arrived from a profile's spec cap is REQUESTED, not filled", requested: 4096,
		ceiling: assignedCeiling(8192), want: 4096,
	}, {
		name: "no providers and no limits is byte-for-byte today", requested: 8192,
		want: 8192,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			policy := types.RunPolicySpec{}
			if tc.requested > 0 {
				policy.Resources = &types.ResourceLimits{DiskMiB: tc.requested}
			}
			res, events, runID := ephemeralDispatch(t, policy, tc.site, tc.ceiling)
			if res.DiskMiB != int64(tc.want) {
				t.Errorf("sandbox DiskMiB = %d, want %d", res.DiskMiB, tc.want)
			}
			if res.DiskMiBFilled != tc.wantFilled {
				t.Errorf("DiskMiBFilled = %v, want %v", res.DiskMiBFilled, tc.wantFilled)
			}
			// The effective number is DISCLOSED to every caller through the
			// envelope — the audit is why the clamp can be silent at the API.
			ev := findAudit(events, runID, "run.policy.resolve", "success")
			if ev == nil {
				t.Fatalf("no run.policy.resolve envelope; events=%s", auditDump(events, runID))
			}
			var spec types.RunPolicySpec
			if err := json.Unmarshal(ev.Data, &spec); err != nil {
				t.Fatalf("envelope is not a RunPolicySpec: %v (%s)", err, ev.Data)
			}
			got := 0
			if spec.Resources != nil {
				got = spec.Resources.DiskMiB
			}
			if got != tc.want {
				t.Errorf("run.policy.resolve disk_mib = %d, want %d — the envelope is the only disclosure a member gets", got, tc.want)
			}
			// PROVENANCE rides the same envelope, so a reader can tell a size the
			// org filled in (degrades to uncapped on a host that cannot keep it)
			// from one a policy demanded (refused at create there). Absent — not
			// false — on every requested run: the key is omitempty.
			var datum map[string]any
			if err := json.Unmarshal(ev.Data, &datum); err != nil {
				t.Fatalf("envelope is not an object: %v (%s)", err, ev.Data)
			}
			filled, present := datum["disk_mib_filled"]
			if tc.wantFilled != (present && filled == true) {
				t.Errorf("run.policy.resolve disk_mib_filled = %v (present=%v), want filled=%v", filled, present, tc.wantFilled)
			}
		})
	}
}

// TestDispatchEphemeralDisk_NeverWritesThroughTheCallersResources is the aliasing
// pin. dispatchRun's `policy` is a SHALLOW copy of the caller's spec, so its
// Resources POINTER is still the caller's — and for a run that authored no policy
// of its own that pointer is the process-global default/ceiling spec. An in-place
// DiskMiB write would therefore have re-sized every subsequent run on the
// deployment.
func TestDispatchEphemeralDisk_NeverWritesThroughTheCallersResources(t *testing.T) {
	shared := &types.ResourceLimits{DiskMiB: 8192}
	res, _, _ := ephemeralDispatch(t, types.RunPolicySpec{Resources: shared}, ephemeralSite(0, 4096), governanceCeiling{})
	if res.DiskMiB != 4096 {
		t.Fatalf("sandbox DiskMiB = %d, want 4096 (clamped)", res.DiskMiB)
	}
	if shared.DiskMiB != 8192 {
		t.Errorf("the caller's ResourceLimits was mutated to %d — every later run under this spec is now re-sized", shared.DiskMiB)
	}
}

// TestDispatchEphemeralDisk_CeilingReassertCarriesTheClamp: run.policy.resolve
// records a policy, not whose ceiling shaped it. When a profile applies, its
// re-assertion row is where the SIZE half becomes attributable — and it stays
// absent for a profile that sets no size, so a pre-0.7.2 profile's row is
// byte-identical.
func TestDispatchEphemeralDisk_CeilingReassertCarriesTheClamp(t *testing.T) {
	policy := types.RunPolicySpec{Resources: &types.ResourceLimits{DiskMiB: 8192}}

	_, events, runID := ephemeralDispatch(t, policy, types.SiteConfig{}, assignedCeiling(2048))
	ev := findAudit(events, runID, "run.ceiling.reassert", "success")
	if ev == nil {
		t.Fatalf("no run.ceiling.reassert row; events=%s", auditDump(events, runID))
	}
	// A FRESH value per unmarshal, deliberately: json.Unmarshal leaves a pointer
	// field untouched when the key is absent, so a reused struct would report the
	// PREVIOUS row's keys as present on the "no limit" case below.
	type sizeKeys struct {
		Max       *int `json:"max_ephemeral_disk_mib"`
		Effective *int `json:"ephemeral_disk_mib"`
	}
	var data sizeKeys
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		t.Fatalf("row data is not an object: %v (%s)", err, ev.Data)
	}
	if data.Max == nil || *data.Max != 2048 {
		t.Errorf("max_ephemeral_disk_mib = %v, want 2048", data.Max)
	}
	if data.Effective == nil || *data.Effective != 2048 {
		t.Errorf("ephemeral_disk_mib = %v, want the clamped 2048", data.Effective)
	}

	// A profile with no size limit: neither key appears.
	_, events, runID = ephemeralDispatch(t, policy, types.SiteConfig{},
		governanceCeiling{Profile: &types.GovernanceProfile{Name: "unsized"}})
	ev = findAudit(events, runID, "run.ceiling.reassert", "success")
	if ev == nil {
		t.Fatalf("no run.ceiling.reassert row for an assigned profile; events=%s", auditDump(events, runID))
	}
	var after sizeKeys
	if err := json.Unmarshal(ev.Data, &after); err != nil {
		t.Fatalf("row data is not an object: %v (%s)", err, ev.Data)
	}
	if after.Max != nil || after.Effective != nil {
		t.Errorf("a profile that sets no ephemeral limit still emitted the size keys (%v, %v)", after.Max, after.Effective)
	}
}

// TestBoundMemberSpec_PreviewsTheGovernanceEphemeralLimit is the PREFLIGHT half
// of the same number.
//
// Dispatch binds MaxEphemeralDiskMiB for every lane; a member meets it first in
// the Review rail and POST /runs/preflight, both of which run their spec through
// boundMemberSpec → composer.Clamp. The two share one min() (composer.CapDiskMiB),
// so what is left to get wrong — and what this pins — is the THREADING: the
// member bounding pipeline takes the whole resolved ceiling precisely so the
// limit beside the spec reaches the clamp instead of a silent zero.
//
// DiskMiB == 0 is also the k8s "no ephemeral-storage keys at all" case, pinned on
// the created pod by lane S1 (internal/runner/k8s/sandbox_test.go).
func TestBoundMemberSpec_PreviewsTheGovernanceEphemeralLimit(t *testing.T) {
	h, _ := newSecretsHarness(t)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs/preflight", nil)
	ceiling := governanceCeiling{
		Spec:    types.RunPolicySpec{MinConfinementClass: types.CC2},
		Profile: &types.GovernanceProfile{Name: "sized"},
		Limits:  types.GovernanceLimits{MaxEphemeralDiskMiB: 2048},
	}
	spec, warns, ok := h.srv.boundMemberSpec(context.Background(), httptest.NewRecorder(), r,
		types.RunPolicySpec{MinConfinementClass: types.CC2, Resources: &types.ResourceLimits{DiskMiB: 8192}},
		ceiling, "invalid inline_policy: ", true)
	if !ok {
		t.Fatal("boundMemberSpec refused the spec")
	}
	if spec.Resources == nil || spec.Resources.DiskMiB != 2048 {
		t.Fatalf("previewed disk_mib = %v, want the same 2048 the run gets", spec.Resources)
	}
	var capped bool
	for _, w := range warns {
		capped = capped || strings.Contains(w, composer.WarnResourcesCapped)
	}
	if !capped {
		t.Errorf("warns = %v, want %q — the member is told, in the clamp's own words", warns, composer.WarnResourcesCapped)
	}
}

// TestResolveRunPolicy_DefaultArmPreviewsTheEphemeralCap is the arm composer.
// Clamp cannot reach, and it is the commonest member request there is: no
// policy_id, no inline_policy, so resolvePolicy hands back the PROFILE's own
// ceiling spec — which an admin may have written wider than the limit standing
// beside it. The member bounding pipeline runs only for a SELECTED row, so
// without a clamp here Review previewed one size and the run got another, with
// nothing said to the member (dispatch's disclosure is a log line and an audit
// row, neither of which is a preview).
//
// Pinned as preview == dispatch on the same inputs, which is the claim.
func TestResolveRunPolicy_DefaultArmPreviewsTheEphemeralCap(t *testing.T) {
	const authored, limit = 8192, 2048
	profileCeiling := types.RunPolicySpec{
		MinConfinementClass: types.CC2,
		Resources:           &types.ResourceLimits{DiskMiB: authored},
	}
	h := newHarness(t)
	st := &memberBoundStore{profile: &types.GovernanceProfile{
		ID: uuid.New(), Name: "sized", Ceiling: profileCeiling,
		Limits: types.GovernanceLimits{MaxEphemeralDiskMiB: limit},
	}}
	cfg := baseTestConfig(h, st)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.DefaultPolicy = profileCeiling
	srv := New(cfg)

	r, ctx := boundMemberRequest(t)
	spec, policyID, warns, ok := srv.resolveRunPolicy(ctx, httptest.NewRecorder(), r,
		&createRunRequest{Agent: "claude-code", Repo: "acme/widgets"}, true)
	if !ok {
		t.Fatal("the default arm refused a member request carrying no policy at all")
	}
	if policyID != nil {
		t.Fatalf("policy_id = %v, want nil — this is the no-policy arm", policyID)
	}
	if spec.Resources == nil || spec.Resources.DiskMiB != limit {
		t.Fatalf("previewed disk_mib = %v, want %d", spec.Resources, limit)
	}
	var capped bool
	for _, w := range warns {
		capped = capped || strings.Contains(w, composer.WarnResourcesCapped)
	}
	if !capped {
		t.Errorf("warns = %q, want %q — a silent clamp is the defect", warns, composer.WarnResourcesCapped)
	}
	// The profile's own ceiling spec must not have been re-sized under it: every
	// later run resolving the same profile reads that value.
	if profileCeiling.Resources.DiskMiB != authored {
		t.Errorf("the profile's ceiling Resources was mutated to %d", profileCeiling.Resources.DiskMiB)
	}

	// …and the number the run actually gets, from the same inputs.
	res, _, _ := ephemeralDispatch(t, profileCeiling, types.SiteConfig{}, assignedCeiling(limit))
	if res.DiskMiB != int64(spec.Resources.DiskMiB) {
		t.Errorf("preview shows %d but dispatch gives %d", spec.Resources.DiskMiB, res.DiskMiB)
	}

	// THE COUNTERFACTUAL: an OPERATOR is the ceiling-setting authority and is
	// exempt from the limit, here exactly as at dispatch.
	opCtx := operatorCtx("sub-admin", "admin@corp.example", oidc.RoleAdmin)
	opReq := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(opCtx)
	spec, _, _, ok = srv.resolveRunPolicy(opCtx, httptest.NewRecorder(), opReq,
		&createRunRequest{Agent: "claude-code", Repo: "acme/widgets"}, true)
	if !ok {
		t.Fatal("the operator request was refused")
	}
	if spec.Resources == nil || spec.Resources.DiskMiB != authored {
		t.Errorf("operator previewed disk_mib = %v, want the unclamped %d", spec.Resources, authored)
	}
}
