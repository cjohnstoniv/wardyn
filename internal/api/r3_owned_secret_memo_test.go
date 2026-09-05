// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// r3PlainStore answers the reads the member run pipeline makes with "nothing
// configured", so Config.DefaultPolicy IS the caller's ceiling and the only
// variable in the test below is the request body.
type r3PlainStore struct {
	store.Store
}

func (r3PlainStore) ResolveGovernanceProfile(context.Context, []string, []string) (*types.GovernanceProfile, types.CapabilitySubjectType, error) {
	return nil, "", nil
}
func (r3PlainStore) HasGroupTierAssignments(context.Context) (bool, error) { return false, nil }
func (r3PlainStore) ListCapabilityGrants(context.Context) ([]types.CapabilityGrant, error) {
	return nil, nil
}
func (r3PlainStore) ListGroupDenyGrants(context.Context, string) ([]types.CapabilityGrant, error) {
	return nil, nil
}
func (r3PlainStore) ListCapabilityGrantsFor(context.Context, []string, []string) ([]types.CapabilityGrant, error) {
	return nil, nil
}
func (r3PlainStore) GetCapabilityEnforcement(context.Context) (map[string]bool, error) {
	return nil, nil
}
func (r3PlainStore) ListWorkspaces(context.Context) ([]types.Workspace, error) { return nil, nil }
func (r3PlainStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return types.SiteConfig{}, nil
}

// countingSecretStore counts the OWNER-scoped List calls the member pipeline
// makes — the read ownsSecret performs. It is a count, not a stopwatch: the
// defect is "one member's request body chooses how many Postgres round trips the
// handler makes", and the count is that exactly.
type countingSecretStore struct {
	owner     string
	operator  []string
	own       map[string][]string
	ownerList *atomic.Int64
	opList    *atomic.Int64
}

func newCountingSecretStore(operator []string, own map[string][]string) *countingSecretStore {
	return &countingSecretStore{operator: operator, own: own, ownerList: &atomic.Int64{}, opList: &atomic.Int64{}}
}

func (c *countingSecretStore) Name() string { return "counting" }
func (c *countingSecretStore) Put(context.Context, string, []byte) error {
	return nil
}
func (c *countingSecretStore) Get(context.Context, string) ([]byte, error) {
	return []byte("v"), nil
}
func (c *countingSecretStore) Delete(context.Context, string) error { return nil }
func (c *countingSecretStore) List(context.Context) ([]string, error) {
	if c.owner == "" {
		c.opList.Add(1)
		return c.operator, nil
	}
	c.ownerList.Add(1)
	return c.own[c.owner], nil
}
func (c *countingSecretStore) For(owner string) secretstore.Store {
	cp := *c
	cp.owner = owner
	return &cp
}

// r3MemberPreflightBody builds a member inline_policy carrying n api_key grants,
// each naming the member's OWN model key paired with the model-provider host the
// policy already allows — the 6c own-key arm, which is the arm that calls
// ownsSecret.
func r3MemberPreflightBody(t *testing.T, n int) string {
	t.Helper()
	grants := make([]types.GrantSpec, 0, n)
	for i := 0; i < n; i++ {
		scope, err := json.Marshal(map[string]string{
			"host": "api.anthropic.com", "secret_name": "alice-model-key", "header": "X-Api-Key",
		})
		if err != nil {
			t.Fatal(err)
		}
		grants = append(grants, types.GrantSpec{Kind: types.GrantAPIKey, Scope: scope})
	}
	spec := types.RunPolicySpec{
		MinConfinementClass: types.CC2,
		AllowedDomains:      []string{"api.anthropic.com"},
		EligibleGrants:      grants,
	}
	body, err := json.Marshal(map[string]any{"agent": "claude-code", "task": "t", "inline_policy": spec})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// TestOwnedSecretReadsAreFlatInCallerInput is F067's growth law for the half
// capBatch did not close.
//
// capBatch made the egress loop's store reads flat in len(allowed_domains) and
// maxAllowedDomainsPerSpec capped that list — but the member pipeline's OTHER
// caller-sized list, spec.eligible_grants, has no cap at all and buys an
// UNMEMOIZED For(owner).List per grant at three separate sites
// (filterMemberGrants' 6c own-key arm, narrowMemberInlinePolicy's ownership
// exemption, validateInlineSecretRefs' unknown-name arm). So "per-request cost
// stays O(1) store reads regardless of body content" was still false: it just
// moved lists.
//
// The assertion is CONSTANCY, not a budget: the owner-scoped read count must not
// depend on len(EligibleGrants) at all.
func TestOwnedSecretReadsAreFlatInCallerInput(t *testing.T) {
	run := func(t *testing.T, n int) (int64, int) {
		t.Helper()
		h := newHarness(t)
		cfg := baseTestConfig(h, r3PlainStore{})
		cfg.OIDC = &oidc.Authenticator{}
		cfg.DefaultPolicy = types.RunPolicySpec{
			MinConfinementClass: types.CC2,
			AllowedDomains:      []string{"api.anthropic.com"},
			EligibleGrants: []types.GrantSpec{{
				Kind:  types.GrantAPIKey,
				Scope: json.RawMessage(`{"host":"api.anthropic.com","secret_name":"operator-key","header":"X-Api-Key"}`),
			}},
		}
		sec := newCountingSecretStore(nil, map[string][]string{
			"sub-gov-bob": {"alice-model-key"},
		})
		cfg.Secrets = sec
		srv := New(cfg)
		member := ssoSession(t, "sub-gov-bob", "bob@corp.example", oidc.RoleMember)
		w := doSSO(t, srv, http.MethodPost, "/api/v1/runs/preflight", member, r3MemberPreflightBody(t, n))
		if w.Code != http.StatusOK {
			t.Fatalf("n=%d: preflight = %d, want 200; body=%s", n, w.Code, w.Body.String())
		}
		return sec.ownerList.Load(), w.Code
	}

	base, _ := run(t, 1)
	for _, n := range []int{10, 200, 2000} {
		got, _ := run(t, n)
		if got != base {
			t.Errorf("n=%d: owner-scoped secret List calls = %d, want %d (the n=1 count) — a member's "+
				"request body must not choose how many store round trips one request makes", n, got, base)
		}
	}
	if base > 2 {
		t.Errorf("even n=1 made %d owner-scoped reads; the pipeline should resolve the owner's names once per request", base)
	}
}
