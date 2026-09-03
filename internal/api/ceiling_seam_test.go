// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// flakyCeilingStore answers the FIRST governance resolve and fails every one
// after it — what a concurrent profile edit or a transient store blip produces
// between two of the reads a single create used to make.
type flakyCeilingStore struct {
	store.Store
	calls atomic.Int64
}

func (s *flakyCeilingStore) ResolveGovernanceProfile(context.Context, []string, []string) (*types.GovernanceProfile, types.CapabilitySubjectType, error) {
	if s.calls.Add(1) == 1 {
		return &types.GovernanceProfile{ID: uuid.New(), Name: "walled", Ceiling: types.RunPolicySpec{
			MinConfinementClass: types.CC2, AllowedDomains: []string{"api.anthropic.com"},
		}}, types.CapabilitySubjectUser, nil
	}
	return nil, "", context.DeadlineExceeded
}
func (s *flakyCeilingStore) HasGroupTierAssignments(context.Context) (bool, error) { return false, nil }
func (s *flakyCeilingStore) ListCapabilityGrants(context.Context) ([]types.CapabilityGrant, error) {
	return nil, nil
}

func (s *flakyCeilingStore) ListGroupDenyGrants(context.Context, string) ([]types.CapabilityGrant, error) {
	return nil, nil
}
func (s *flakyCeilingStore) ListCapabilityGrantsFor(context.Context, []string, []string) ([]types.CapabilityGrant, error) {
	return nil, nil
}
func (s *flakyCeilingStore) GetCapabilityEnforcement(context.Context) (map[string]bool, error) {
	return nil, nil
}
func (s *flakyCeilingStore) ListWorkspaces(context.Context) ([]types.Workspace, error) {
	return nil, nil
}
func (s *flakyCeilingStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return types.SiteConfig{}, nil
}

// TestCeilingIsResolvedOncePerRequest is requirement 5's clause — "the three
// ceiling reads per create cannot disagree" — given an implementing mechanism.
//
// It had none. A member create took THREE independent, uncached, untransacted
// reads of governance_assignments (denyMemberGovernance -> resolveRunPolicy ->
// filterMemberGrants) and dispatch a fourth, while resolveRunPolicy's own
// comment asserted "a create must never resolve two different ceilings for one
// request" and governance.go's called the repetition "PF-13's accepted double
// resolution", justified purely on latency. Two in-tree comments, opposite
// invariants, nothing enforcing either.
//
// The consequence is not latency: a security admin NARROWING a profile — the
// incident-response action — can be raced by an in-flight create, landing a run
// whose egress was clamped under the pre-narrowing ceiling while its grants were
// filtered under the post-narrowing one.
//
// The store here fails every resolve after the first, so the count IS the
// assertion: one read means one answer, and any second read shows up as a
// refusal the first would not have given.
func TestCeilingIsResolvedOncePerRequest(t *testing.T) {
	st := &flakyCeilingStore{}
	h := newHarness(t)
	cfg := baseTestConfig(h, st)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.DefaultPolicy = types.RunPolicySpec{MinConfinementClass: types.CC2, AllowedDomains: []string{"api.anthropic.com"}}
	srv := New(cfg)

	// The memo rides the REQUEST context, so this drives the real middleware
	// chain rather than calling the resolver directly — the point is that every
	// site inside one HTTP request shares it.
	member := ssoSession(t, "sub-gov-bob", "bob@corp.example", oidc.RoleMember)
	w := doSSO(t, srv, http.MethodPost, "/api/v1/runs/preflight", member,
		`{"agent":"claude-code","task":"t","inline_policy":{"min_confinement_class":"CC2","allowed_domains":["api.anthropic.com"]}}`)

	if got := st.calls.Load(); got != 1 {
		t.Errorf("governance resolves in ONE request = %d, want 1 — every site must share the request's answer, "+
			"or a profile edit mid-request splits the create across two ceilings", got)
	}
	// And the request succeeds: a second resolve would have failed and turned
	// this into a 500 blaming the caller's policy.
	if w.Code != http.StatusOK {
		t.Errorf("preflight = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// TestCeilingMemoIsPerRequestNotProcessWide is the counterfactual to the memo
// itself: caching ACROSS requests would be the HA blocker effectiveCeiling's own
// note names — a revoked profile still binding on the next call. A second
// request must resolve afresh.
func TestCeilingMemoIsPerRequestNotProcessWide(t *testing.T) {
	st := &flakyCeilingStore{}
	h := newHarness(t)
	cfg := baseTestConfig(h, st)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.DefaultPolicy = types.RunPolicySpec{MinConfinementClass: types.CC2, AllowedDomains: []string{"api.anthropic.com"}}
	srv := New(cfg)
	member := ssoSession(t, "sub-gov-bob", "bob@corp.example", oidc.RoleMember)
	body := `{"agent":"claude-code","task":"t","inline_policy":{"min_confinement_class":"CC2","allowed_domains":["api.anthropic.com"]}}`

	doSSO(t, srv, http.MethodPost, "/api/v1/runs/preflight", member, body)
	first := st.calls.Load()
	doSSO(t, srv, http.MethodPost, "/api/v1/runs/preflight", member, body)
	if second := st.calls.Load(); second == first {
		t.Errorf("the second request reused the first's ceiling (resolves stayed at %d) — the memo must die with "+
			"the request, or revoking a profile would not take effect until restart", first)
	}
}

// TestCeilingRefusalCarriesItsRemedy pins the ONE mapping governance.go names
// "so a resolver failure cannot answer 403 at one site and 500 at the next for
// the same cause".
//
// The STATUS obeyed it; the BODY did not. Two of the eight call sites take only
// the status half (ceilingErrorStatus) and compose their own body from
// err.Error(), which for the unanswerable-snapshot sentinel was the bare string
// "groups_snapshot_stale" — a 403 naming no remedy, on a refusal whose whole
// point is that the human CAN fix it by signing in again. The remedy now travels
// with the error value, so a site that prints it carries the remedy without
// having to know the rule.
func TestCeilingRefusalCarriesItsRemedy(t *testing.T) {
	const remedy = "sign in again"

	// The sentinel itself: every site that prints it gets the remedy.
	if !strings.Contains(errGroupsSnapshotStale.Error(), remedy) {
		t.Errorf("errGroupsSnapshotStale.Error() = %q, want it to name the remedy — a site that composes its own "+
			"body from err.Error() is the case this fixes", errGroupsSnapshotStale)
	}
	// The canonical mapping, unchanged.
	w := httptest.NewRecorder()
	writeCeilingError(w, errGroupsSnapshotStale)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), remedy) {
		t.Errorf("writeCeilingError = %d %s, want 403 naming the remedy", w.Code, w.Body.String())
	}

	// The live seam that composes its own body: GET /secrets, over a deployment
	// with group-tier assignments and a caller whose snapshot is unanswerable.
	st := &capStore{govHasGroupTier: true}
	h := newHarness(t)
	cfg := baseTestConfig(h, st)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Secrets = &memSecrets{m: map[string][]byte{}}
	srv := New(cfg)
	sess := ssoSession(t, "sub-gov-bob", "bob@corp.example", oidc.RoleMember)
	// A nil group snapshot is the unanswerable shape; ssoSession carries none.
	got := doSSO(t, srv, http.MethodGet, "/api/v1/secrets", sess, "")
	if got.Code != http.StatusForbidden {
		t.Fatalf("list secrets = %d, want 403; body=%s", got.Code, got.Body.String())
	}
	if !strings.Contains(got.Body.String(), remedy) {
		t.Errorf("list secrets 403 body = %s\nwant it to name the remedy — the refusal is correct, the message was "+
			"the defect: a human told only \"groups_snapshot_stale\" opens a support ticket", got.Body.String())
	}
}
