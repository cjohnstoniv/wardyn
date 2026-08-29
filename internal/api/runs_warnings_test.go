// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// runWarnStore is capStore (capability rows) plus the run methods
// handleCreateRun needs to reach its 201 with no runner and no Postgres —
// CreateGrant only matters for a request whose EligibleGrants actually
// survives narrowing (a member's own-key grant, say); the narrowing-only
// tests below never reach it.
type runWarnStore struct {
	*capStore
	created types.AgentRun
}

func (s *runWarnStore) ListRuns(context.Context) ([]types.AgentRun, error) { return nil, nil }

func (s *runWarnStore) ListWorkspaces(context.Context) ([]types.Workspace, error) { return nil, nil }

func (s *runWarnStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return types.SiteConfig{}, nil
}

func (s *runWarnStore) SetRunImage(context.Context, uuid.UUID, string) error { return nil }

func (s *runWarnStore) CreateGrant(_ context.Context, g types.CredentialGrant) (types.CredentialGrant, error) {
	return g, nil
}

func (s *runWarnStore) CreateRun(_ context.Context, run types.AgentRun) (types.AgentRun, error) {
	s.created = run
	return run, nil
}

// TestCreateRun_SurfacesMemberNarrowingWarnings: a member launching an inline
// policy whose egress host they do not hold gets a 201 — the run is narrowed,
// never refused — carrying a warning that NAMES the dropped host.
//
// This is the wire the console depends on: it launches without preflighting, so
// before this the drop was invisible everywhere except `wardyn run --dry-run`,
// while the permissions copy and OPERATIONS both promised "a warning naming it".
func TestCreateRun_SurfacesMemberNarrowingWarnings(t *testing.T) {
	h := newHarness(t)
	st := &runWarnStore{capStore: &capStore{enf: map[string]bool{capEgressHost: true}}}
	cfg := baseTestConfig(h, st)
	cfg.OIDC = &oidc.Authenticator{}
	// The operator ceiling carries both hosts, so composer.Clamp keeps them and
	// the ONLY thing that can drop one is the member's own capability set.
	cfg.DefaultPolicy = types.RunPolicySpec{
		MinConfinementClass: types.CC2,
		AllowedDomains:      []string{"api.anthropic.com", "evil.example.com"},
	}
	srv := New(cfg)

	body := `{"agent":"claude-code","task":"t","inline_policy":{"min_confinement_class":"CC2","allowed_domains":["api.anthropic.com","evil.example.com"]}}`
	w := doSSO(t, srv, http.MethodPost, "/api/v1/runs",
		ssoSession(t, "sub-warn-member", "dev@corp.example", oidc.RoleMember), body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201 (a drop narrows, it does not refuse): %s", w.Code, w.Body.String())
	}
	var got createRunResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v; body=%s", err, w.Body.String())
	}
	var named bool
	for _, warn := range got.Warnings {
		if strings.Contains(warn, "evil.example.com") {
			named = true
		}
	}
	if !named {
		t.Fatalf("warnings = %v, want one naming the dropped host evil.example.com", got.Warnings)
	}
	// The narrowing itself (that the host really is gone from the resolved
	// spec) is capability_seams_test.go's; this test's whole job is that the
	// caller HEARS about it.
	if st.created.ID == (types.AgentRun{}).ID {
		t.Fatal("no run was persisted, so the 201 above proved nothing")
	}
}
