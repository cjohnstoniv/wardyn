// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// memberBoundStore is the smallest store that lets BOTH member branches of
// resolveRunPolicy run: a governance profile (so ceiling.Profile != nil, which
// is what scopes the stored branch), a stored policy row for the member to
// select by id, and the capability reads narrowMemberInlinePolicy performs.
type memberBoundStore struct {
	store.Store
	profile *types.GovernanceProfile
	policy  types.RunPolicy
}

func (s *memberBoundStore) ResolveGovernanceProfile(context.Context, []string, []string) (*types.GovernanceProfile, types.CapabilitySubjectType, error) {
	return s.profile, types.CapabilitySubjectUser, nil
}
func (s *memberBoundStore) HasGroupTierAssignments(context.Context) (bool, error) { return false, nil }
func (s *memberBoundStore) GetPolicy(context.Context, uuid.UUID) (types.RunPolicy, error) {
	return s.policy, nil
}
func (s *memberBoundStore) ListCapabilityGrantsFor(context.Context, []string, []string) ([]types.CapabilityGrant, error) {
	return nil, nil
}
func (s *memberBoundStore) GetCapabilityEnforcement(context.Context) (map[string]bool, error) {
	return nil, nil
}

// memberBoundFixture wires one member, one governance ceiling, and one stored
// policy row whose spec is IDENTICAL to the inline body the same member sends,
// so the only difference between the two resolutions is how the content
// arrived. Returns the server, the stored row's id, and that shared spec.
func memberBoundFixture(t *testing.T, memberSpec types.RunPolicySpec) (*Server, *harness, uuid.UUID) {
	t.Helper()
	h := newHarness(t)
	policyID := uuid.New()
	// The ceiling ALLOWS the api_key kind, for exactly one pairing. That is what
	// makes the fixture exercise stage 2 (filterMemberGrants) rather than
	// stopping at stage 1: composer.Clamp drops out-of-ceiling grant KINDS, so a
	// ceiling with no eligible grants at all would never let a pairing reach the
	// check this test is about.
	ceiling := types.RunPolicySpec{
		MinConfinementClass: types.CC2,
		AllowedDomains:      []string{"api.anthropic.com"},
		EligibleGrants: []types.GrantSpec{{
			Kind:  types.GrantAPIKey,
			Scope: mustJSON(map[string]any{"host": "api.anthropic.com", "secret_name": "operator-key"}),
		}},
	}
	st := &memberBoundStore{
		profile: &types.GovernanceProfile{ID: uuid.New(), Name: "walled", Ceiling: ceiling},
		policy:  types.RunPolicy{ID: policyID, Name: "wide", Spec: memberSpec},
	}
	cfg := baseTestConfig(h, st)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.DefaultPolicy = ceiling
	return New(cfg), h, policyID
}

// boundMemberRequest returns a request already carrying a verified MEMBER identity,
// plus the same context to pass as resolveRunPolicy's ctx (the function reads
// both, and they are one context in production).
func boundMemberRequest(t *testing.T) (*http.Request, context.Context) {
	t.Helper()
	ctx := operatorCtx("sub-member", "member@corp.example", oidc.RoleMember)
	return httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(ctx), ctx
}

// dropAudits returns the authz.denied drop events recorded so far, as decoded
// payloads — what an operator reading the stream would actually see.
func dropAudits(t *testing.T, events []types.AuditEvent) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, ev := range events {
		if ev.Action != "authz.denied" {
			continue
		}
		var d map[string]any
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatalf("decode authz.denied data: %v", err)
		}
		out = append(out, d)
	}
	return out
}

// TestMemberBounding_InlineAndStoredBranchesAgree is PF-1 stated as a test:
// "member-selected content is bounded by the member's ceiling whether it
// arrived as a body or as a row id". The same member sends the same wide spec
// twice — once as an inline_policy, once as the id of a stored row holding it
// verbatim — and the CLAMP, the DROPS and the AUDIT must come out identical.
//
// It is the pin for folding the two hand-copied bounding pipelines into
// boundMemberSpec: two copies could drift into a member smuggling through one
// route what the other refuses, and that drift is invisible to any test that
// exercises only one branch.
func TestMemberBounding_InlineAndStoredBranchesAgree(t *testing.T) {
	// Wide on every axis the pipeline bounds: weaker confinement than the
	// ceiling, an extra host, and a stored-secret pairing the operator never
	// eligible-listed (the ceiling carries no eligible grants at all).
	wide := types.RunPolicySpec{
		MinConfinementClass: types.CC1,
		AllowedDomains:      []string{"api.anthropic.com", "evil.example"},
		EligibleGrants: []types.GrantSpec{{
			Kind:  types.GrantAPIKey,
			Scope: mustJSON(map[string]any{"host": "evil.example", "secret_name": "operator-key"}),
		}},
	}

	srv, h, policyID := memberBoundFixture(t, wide)

	inlineReq := wide
	r, ctx := boundMemberRequest(t)
	inlineSpec, inlineID, inlineWarns, ok := srv.resolveRunPolicy(ctx, httptest.NewRecorder(), r,
		&createRunRequest{Agent: "claude-code", Repo: "acme/widgets", InlinePolicy: &inlineReq}, false)
	if !ok {
		t.Fatalf("inline branch refused the request")
	}
	inlineDrops := dropAudits(t, h.audit.events)
	h.audit.events = nil

	r, ctx = boundMemberRequest(t)
	storedSpec, storedID, storedWarns, ok := srv.resolveRunPolicy(ctx, httptest.NewRecorder(), r,
		&createRunRequest{Agent: "claude-code", Repo: "acme/widgets", PolicyID: &policyID}, false)
	if !ok {
		t.Fatalf("stored branch refused the request")
	}
	storedDrops := dropAudits(t, h.audit.events)

	// The clamp: same bounded spec out of both routes.
	if !reflect.DeepEqual(inlineSpec, storedSpec) {
		t.Errorf("branches disagree on the bounded spec:\n inline = %+v\n stored = %+v", inlineSpec, storedSpec)
	}
	// It really was bounded — a test where both branches no-op would pass the
	// comparison above and prove nothing.
	if inlineSpec.MinConfinementClass != types.CC2 {
		t.Errorf("confinement = %q, want it clamped up to the ceiling's %q", inlineSpec.MinConfinementClass, types.CC2)
	}
	if len(inlineSpec.EligibleGrants) != 0 {
		t.Errorf("eligible grants = %+v, want the unlisted pairing dropped", inlineSpec.EligibleGrants)
	}
	// The drops: same warnings surfaced to the member in Review.
	if !reflect.DeepEqual(inlineWarns, storedWarns) {
		t.Errorf("branches disagree on warnings:\n inline = %q\n stored = %q", inlineWarns, storedWarns)
	}
	if len(inlineWarns) == 0 {
		t.Error("no clamp warnings at all — the fixture stopped exercising the pipeline")
	}
	// The audit: same authz.denied stream for the operator.
	if !reflect.DeepEqual(inlineDrops, storedDrops) {
		t.Errorf("branches disagree on the audit stream:\n inline = %+v\n stored = %+v", inlineDrops, storedDrops)
	}
	if len(inlineDrops) == 0 {
		t.Error("no authz.denied audit events — the drop was warned but not recorded")
	}
	// The one difference that is NOT incidental: an inline spec attaches with a
	// nil policy id, a selected row attaches with its own.
	if inlineID != nil {
		t.Errorf("inline branch returned policy id %v, want nil", inlineID)
	}
	if storedID == nil || *storedID != policyID {
		t.Errorf("stored branch returned policy id %v, want %v", storedID, policyID)
	}
}

// TestMemberBounding_ErrorPrefixStaysPerBranch pins the ONE thing the shared
// pipeline still takes from its caller: the error names the input the caller
// actually sent. Folding the branches must not start telling someone who named
// a stored row that their "inline_policy" was invalid.
func TestMemberBounding_ErrorPrefixStaysPerBranch(t *testing.T) {
	// A covered grant kind whose scope will not decode: filterMemberGrants
	// returns 422 here rather than dropping with a warning.
	malformed := types.RunPolicySpec{
		MinConfinementClass: types.CC2,
		AllowedDomains:      []string{"api.anthropic.com"},
		EligibleGrants:      []types.GrantSpec{{Kind: types.GrantAPIKey, Scope: mustJSON([]string{"not-an-object"})}},
	}
	srv, _, policyID := memberBoundFixture(t, malformed)

	inlineReq := malformed
	r, ctx := boundMemberRequest(t)
	w := httptest.NewRecorder()
	if _, _, _, ok := srv.resolveRunPolicy(ctx, w, r,
		&createRunRequest{Agent: "claude-code", Repo: "acme/widgets", InlinePolicy: &inlineReq}, false); ok {
		t.Fatal("inline branch accepted a malformed grant scope")
	}
	if got := w.Body.String(); !jsonErrorHasPrefix(t, got, "invalid inline_policy: ") {
		t.Errorf("inline branch error = %s, want an \"invalid inline_policy: \" prefix", got)
	}

	r, ctx = boundMemberRequest(t)
	w = httptest.NewRecorder()
	if _, _, _, ok := srv.resolveRunPolicy(ctx, w, r,
		&createRunRequest{Agent: "claude-code", Repo: "acme/widgets", PolicyID: &policyID}, false); ok {
		t.Fatal("stored branch accepted a malformed grant scope")
	}
	if got := w.Body.String(); !jsonErrorHasPrefix(t, got, "invalid policy: ") {
		t.Errorf("stored branch error = %s, want an \"invalid policy: \" prefix", got)
	}
}

func jsonErrorHasPrefix(t *testing.T, body, prefix string) bool {
	t.Helper()
	var e struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatalf("decode error body %q: %v", body, err)
	}
	return len(e.Error) >= len(prefix) && e.Error[:len(prefix)] == prefix
}
