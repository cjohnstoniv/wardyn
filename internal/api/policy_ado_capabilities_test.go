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
// read+code_write) under a policy choosing caps.
func adoLaneWithPolicy(t *testing.T, caps []adoscope.Capability) (adoEntraLane, bool, *adoTestStore, *memAudit) {
	t.Helper()
	st := &adoTestStore{}
	s, audit := newADODispatchServer(st)
	policy := types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"}, AzureDevOpsCapabilities: caps}
	lane, ok := s.authorADOEntraLane(context.Background(), types.AgentRun{ID: uuid.New()}, adoTestRun(t), true,
		adoEntraUngraded(), dispatchLLMPlan{mitmCACertPEM: "CERT", mitmCAKeyPEM: "KEY"}, &policy, map[string]string{}, nil)
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
	for _, want := range []string{`"reason":"capability_ceiling"`, "run policy's azure_devops_capabilities", "policy_admin"} {
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

// The member half, through the real resolveRunPolicy chokepoint: a member's
// inline choice is intersected with THEIR ceiling's list, dropped when that
// list is empty (the field can widen past a row's default_profile without an
// approval, so a silent ceiling is no permission), and inherited when the
// member names none. An operator is the ceiling-setting authority and keeps
// exactly what they wrote.
func TestResolveRunPolicy_MemberADOCapabilitiesClamp(t *testing.T) {
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
		want     []adoscope.Capability
	}{
		{name: "member: intersected with the ceiling's list", profile: profile(r, pr), inline: []adoscope.Capability{r, pa}, want: []adoscope.Capability{r}},
		{name: "member: a silent ceiling drops the choice", profile: profile(), inline: []adoscope.Capability{r, pa}, want: nil},
		{name: "member, unassigned: the default ceiling is silent too", inline: []adoscope.Capability{r, pa}, want: nil},
		{name: "member: none named inherits the ceiling's", profile: profile(r, pr), want: []adoscope.Capability{r, pr}},
		{name: "member: an empty intersection inherits the ceiling's", profile: profile(r, pr), inline: []adoscope.Capability{pa}, want: []adoscope.Capability{r, pr}},
		{name: "operator: unclamped", profile: profile(r), operator: true, inline: []adoscope.Capability{r, pa}, want: []adoscope.Capability{r, pa}},
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
			if !slices.Equal(got.AzureDevOpsCapabilities, tc.want) {
				t.Errorf("azure_devops_capabilities = %v, want %v", got.AzureDevOpsCapabilities, tc.want)
			}
		})
	}
}
