// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// adoLaneWithPolicy drives the one dispatch call the override lives in,
// authorADOEntraLane, for the fixture row (ceiling read+code_write+pr, default
// read+code_write) under an OPERATOR's policy choosing caps (a member's is
// bounded further; policy_ado_standing_test.go).
func adoLaneWithPolicy(t *testing.T, caps []adoscope.Capability) (adoEntraLane, bool, *adoTestStore, *memAudit) {
	t.Helper()
	st := &adoTestStore{}
	s, audit := newADODispatchServer(st)
	policy := types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"}, AzureDevOpsCapabilities: caps}
	lane, ok := s.authorADOEntraLane(context.Background(), types.AgentRun{ID: uuid.New()}, adoTestRun(t), true,
		adoEntraUngraded(), adoStandingBound{operator: true}, dispatchLLMPlan{mitmCACertPEM: "CERT", mitmCAKeyPEM: "KEY"}, &policy, map[string]string{}, nil)
	return lane, ok, st, audit
}

func grantSnapshotCaps(t *testing.T, st *adoTestStore) [][]adoscope.Capability {
	t.Helper()
	var out [][]adoscope.Capability
	for _, g := range st.grants {
		var sc struct {
			Snapshot adoEntraScopeSnapshot `json:"snapshot"`
		}
		if err := json.Unmarshal(g.Spec.Scope, &sc); err != nil {
			t.Fatalf("grant scope: %v", err)
		}
		out = append(out, sc.Snapshot.Capabilities)
	}
	return out
}

// The policy's choice replaces the row's default_profile — in the REST gate and
// in every grant snapshot the resolver reads — and can narrow below it.
func TestADOEntraLane_PolicyCapabilitiesReplaceDefaultProfile(t *testing.T) {
	for name, want := range map[string][]adoscope.Capability{
		"narrower than the default": {adoscope.CapRead},
		"wider than the default":    {adoscope.CapRead, adoscope.CapPR},
	} {
		lane, ok, st, _ := adoLaneWithPolicy(t, want)
		if !ok || lane.gate == nil {
			t.Fatalf("%s: dispatch refused a choice inside the ceiling", name)
		}
		if !slices.Equal(lane.gate.Capabilities, want) {
			t.Errorf("%s: gate capabilities = %v, want the policy's %v", name, lane.gate.Capabilities, want)
		}
		snaps := grantSnapshotCaps(t, st)
		if len(snaps) == 0 {
			t.Fatalf("%s: no grant authored", name)
		}
		for i, got := range snaps {
			if !slices.Equal(got, want) {
				t.Errorf("%s: grant[%d] snapshot capabilities = %v, want %v", name, i, got, want)
			}
		}
	}
}

// Absent is today's behaviour: the row's default_profile, byte for byte.
func TestADOEntraLane_NoPolicyCapabilitiesKeepsDefaultProfile(t *testing.T) {
	lane, ok, st, _ := adoLaneWithPolicy(t, nil)
	want := []adoscope.Capability{adoscope.CapRead, adoscope.CapCodeWrite}
	if !ok || lane.gate == nil || !slices.Equal(lane.gate.Capabilities, want) {
		t.Fatalf("ok=%v gate=%+v, want the default profile %v", ok, lane.gate, want)
	}
	for i, got := range grantSnapshotCaps(t, st) {
		if !slices.Equal(got, want) {
			t.Errorf("grant[%d] snapshot capabilities = %v, want %v", i, got, want)
		}
	}
}

// A policy naming a capability outside the row's ceiling is refused at launch,
// with nothing authored and a sentence naming the policy as the source — never
// silently granted, never silently narrowed.
func TestADOEntraLane_PolicyCapabilityOutsideCeilingIsRefused(t *testing.T) {
	lane, ok, st, audit := adoLaneWithPolicy(t, []adoscope.Capability{adoscope.CapRead, adoscope.CapPolicyAdmin})
	if ok || lane.gate != nil || len(st.grants) != 0 {
		t.Fatalf("ok=%v gate=%+v grants=%d, want a refusal with nothing authored", ok, lane.gate, len(st.grants))
	}
	if !slices.Equal(st.failTo, []types.RunState{types.RunFailed}) {
		t.Errorf("run transitions = %v, want one to FAILED", st.failTo)
	}
	rows := audit.find("run.create")
	if len(rows) != 1 {
		t.Fatalf("run.create audit rows = %d, want 1", len(rows))
	}
	data := string(rows[0].Data)
	for _, want := range []string{`"reason":"capability_ceiling"`,
		"Can't launch with this policy. It grants “Change branch policies” for Azure DevOps, which is outside what your administrator allows on this provider."} {
		if !strings.Contains(data, want) {
			t.Errorf("refusal %s does not say %q", data, want)
		}
	}
}

// An unknown capability never reaches dispatch: the policy door refuses it with
// its own reason, not the bucket.
func TestCreatePolicy_UnknownADOCapabilityIsItsOwnReason(t *testing.T) {
	body := `{"name":"x","spec":{"min_confinement_class":"CC2","allowed_domains":[],"first_use_approval":"always_deny",` +
		`"azure_devops_capabilities":["read","admin"]}}`
	w := httptest.NewRecorder()
	(&Server{}).handleCreatePolicy(w, httptest.NewRequest(http.MethodPost, "/api/v1/policies", strings.NewReader(body)))
	var got struct {
		Error  string `json:"error"`
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != http.StatusBadRequest || got.Reason != reasonADOCapabilityUnknown ||
		!strings.Contains(got.Error, `azure_devops_capabilities[1]: "admin"`) {
		t.Fatalf("status=%d body=%s, want 400 %s naming the entry", w.Code, w.Body.String(), reasonADOCapabilityUnknown)
	}
}

func TestValidatePolicySpec_ADOCapabilities(t *testing.T) {
	spec := func(caps ...adoscope.Capability) types.RunPolicySpec {
		return types.RunPolicySpec{MinConfinementClass: types.CC2, AzureDevOpsCapabilities: caps}
	}
	for _, ok := range []types.RunPolicySpec{spec(), spec(adoscope.CapRead, adoscope.CapCodeWrite, adoscope.CapPR),
		spec(adoscope.CapRead, adoscope.CapPolicyAdmin)} {
		if err := validatePolicySpec(ok); err != nil {
			t.Errorf("%v refused: %v", ok.AzureDevOpsCapabilities, err)
		}
	}
	// A refused catalogue value is a Capability too, and still not grantable.
	for _, bad := range []adoscope.Capability{"admin", "", adoscope.CapDeniedTokens, adoscope.CapUnclassifiedWrite} {
		if err := validatePolicySpec(spec(adoscope.CapRead, bad)); err == nil ||
			specRefusalReason(err, reasonPolicyRequestInvalid) != reasonADOCapabilityUnknown {
			t.Errorf("%q: err=%v, want a refusal carrying %s", bad, err, reasonADOCapabilityUnknown)
		}
	}
	if got := specRefusalReason(validatePolicySpec(types.RunPolicySpec{}), reasonPolicyRequestInvalid); got != reasonPolicyRequestInvalid {
		t.Errorf("an unrelated refusal took reason %q, want the bucket", got)
	}
}

// The member half, through the real resolveRunPolicy chokepoint: resolve no
// longer narrows a member's azure_devops_capabilities, because their bound
// needs the row's default_profile, which only dispatch knows. What resolve must
// keep is the difference between an explicit list and none at all, so dispatch
// never reads a narrowed-to-nothing choice as "use the default".
func TestResolveRunPolicy_MemberADOCapabilitiesReachDispatchAsChosen(t *testing.T) {
	r, pr, pa := adoscope.CapRead, adoscope.CapPR, adoscope.CapPolicyAdmin
	profile := func(caps ...adoscope.Capability) *types.GovernanceProfile {
		return &types.GovernanceProfile{ID: uuid.New(), Name: "ado",
			Ceiling: types.RunPolicySpec{MinConfinementClass: types.CC2, AzureDevOpsCapabilities: caps}}
	}
	for _, tc := range []struct {
		name     string
		profile  *types.GovernanceProfile
		operator bool
		inline   []adoscope.Capability
	}{
		{name: "member: outside the ceiling's list", profile: profile(r, pr), inline: []adoscope.Capability{r, pa}},
		{name: "member: a silent ceiling", profile: profile(), inline: []adoscope.Capability{r}},
		{name: "member, unassigned", inline: []adoscope.Capability{r, pa}},
		{name: "member: none named stays unset", profile: profile(r, pr)},
		{name: "operator", profile: profile(r), operator: true, inline: []adoscope.Capability{r, pa}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			cfg := baseTestConfig(h, &memberBoundStore{profile: tc.profile})
			cfg.OIDC = &oidc.Authenticator{}
			srv := New(cfg)
			role := oidc.RoleUser
			if tc.operator {
				role = oidc.RoleAdmin
			}
			ctx := operatorCtx("sub-ado", "ado@corp.example", role)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/runs/preflight", nil).WithContext(ctx)
			spec := types.RunPolicySpec{MinConfinementClass: types.CC2, AzureDevOpsCapabilities: tc.inline}
			w := httptest.NewRecorder()
			got, _, _, ok := srv.resolveRunPolicy(ctx, w, req, &createRunRequest{Agent: "claude-code", InlinePolicy: &spec}, true)
			if !ok {
				t.Fatalf("resolve refused: %d %s", w.Code, w.Body.String())
			}
			if !slices.Equal(got.AzureDevOpsCapabilities, tc.inline) {
				t.Errorf("azure_devops_capabilities = %v, want the choice %v", got.AzureDevOpsCapabilities, tc.inline)
			}
		})
	}
}

// A member under a profile whose list is [read pr], on the fixture row
// (default_profile [read code_write]): naming nothing, or sending no policy,
// dispatches the row's default_profile — the ceiling's list is never inherited
// unasked. Naming only what neither the default nor the list permits refuses
// the launch instead of falling back to the default.
func TestMemberADOCapabilities_CeilingListIsNeverInherited(t *testing.T) {
	prof := &types.GovernanceProfile{ID: uuid.New(), Name: "ado", Ceiling: types.RunPolicySpec{
		MinConfinementClass: types.CC2, AzureDevOpsCapabilities: []adoscope.Capability{adoscope.CapRead, adoscope.CapPR}}}
	inline := func(caps ...adoscope.Capability) *types.RunPolicySpec {
		return &types.RunPolicySpec{MinConfinementClass: types.CC2, AzureDevOpsCapabilities: caps}
	}
	for name, tc := range map[string]struct {
		req     *createRunRequest
		refused bool
	}{
		"inline, nothing chosen":          {req: &createRunRequest{Agent: "claude-code", InlinePolicy: inline()}},
		"no policy at all":                {req: &createRunRequest{Agent: "claude-code"}},
		"inline, disjoint [policy_admin]": {req: &createRunRequest{Agent: "claude-code", InlinePolicy: inline(adoscope.CapPolicyAdmin)}, refused: true},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			cfg := baseTestConfig(h, &memberBoundStore{profile: prof})
			cfg.OIDC = &oidc.Authenticator{}
			srv := New(cfg)
			ctx := operatorCtx("sub-ado", "ado@corp.example", oidc.RoleUser)
			r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(ctx)
			w := httptest.NewRecorder()
			spec, _, _, ok := srv.resolveRunPolicy(ctx, w, r, tc.req, true)
			if !ok {
				t.Fatalf("resolve refused: %d %s", w.Code, w.Body.String())
			}
			ceiling, err := srv.effectiveCeiling(ctx)
			if err != nil {
				t.Fatal(err)
			}
			st := &adoTestStore{}
			ds, _ := newADODispatchServer(st)
			lane, ok := ds.authorADOEntraLane(context.Background(), types.AgentRun{ID: uuid.New()}, adoTestRun(t), true,
				adoEntraUngraded(), adoStandingFor(ceiling), dispatchLLMPlan{mitmCACertPEM: "CERT", mitmCAKeyPEM: "KEY"}, &spec, map[string]string{}, nil)
			if tc.refused {
				if ok || lane.gate != nil || len(st.grants) != 0 {
					t.Errorf("dispatched gate = %+v (ok %v), want the launch refused", lane.gate, ok)
				}
				return
			}
			want := []adoscope.Capability{adoscope.CapRead, adoscope.CapCodeWrite}
			if !ok || lane.gate == nil || !slices.Equal(lane.gate.Capabilities, want) {
				t.Errorf("dispatched gate = %+v (ok %v), want the row's default_profile %v", lane.gate, ok, want)
			}
		})
	}
}

// A launch preset's inline_policy crosses the same validator, so it answers the
// unknown capability with the same named reason.
func TestPresetInlinePolicy_UnknownADOCapabilityIsItsOwnReason(t *testing.T) {
	req := presetRequest{}
	req.Request.Agent = "claude-code"
	req.Request.InlinePolicy = &types.RunPolicySpec{MinConfinementClass: types.CC2,
		AzureDevOpsCapabilities: []adoscope.Capability{"admin"}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/admin/presets", nil)
	if (&Server{}).validatePresetRequest(w, r, "ado", req) {
		t.Fatal("a preset naming an unknown capability was accepted")
	}
	var got struct {
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != http.StatusBadRequest || got.Reason != reasonADOCapabilityUnknown {
		t.Fatalf("status=%d body=%s, want 400 %s", w.Code, w.Body.String(), reasonADOCapabilityUnknown)
	}
}

// The refusal names every capability past the ceiling by its console name.
func TestADOPolicyPastCeiling_NamesEachCapability(t *testing.T) {
	one := adoPolicyPastCeiling([]adoscope.Capability{adoscope.CapServiceEndpointAdmin})
	if want := "Can't launch with this policy. It grants “Manage service connections” for Azure DevOps, which is outside " +
		"what your administrator allows on this provider. Ask an admin to widen the ceiling, or pick a different saved policy."; one != want {
		t.Errorf("one:\n got %q\nwant %q", one, want)
	}
	three := adoPolicyPastCeiling([]adoscope.Capability{adoscope.CapWikiWrite, adoscope.CapPolicyAdmin, adoscope.CapProjectAdmin})
	if !strings.Contains(three, "It grants “Wiki”, “Change branch policies” and “Manage projects” for Azure DevOps") {
		t.Errorf("three: %q", three)
	}
}
