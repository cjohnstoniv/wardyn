// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestPolicyPreviewPostgresOwnershipAndOracle(t *testing.T) {
	h, secrets := newRunOwnerPGHarness(t)
	ctx := context.Background()
	h.srv.cfg.DefaultPolicy.EligibleGrants = nil
	st := h.srv.cfg.Store
	member := ssoSession(t, "preview-b", "b@example.com", oidc.RoleUser)
	ws, err := st.CreateWorkspace(ctx, types.Workspace{ID: uuid.New(), Name: "private-project", OwnedBy: "preview-a", Status: types.WorkspaceScanned,
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: "private-owner/private-project"}}, CreatedAt: time.Now(), UpdatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	request := func(id uuid.UUID) string {
		return `{"agent":"claude-code","task":"t","workspace_id":"` + id.String() + `"}`
	}
	preview := doSSO(t, h.srv, http.MethodPost, policyPreviewPath, member, request(ws.ID))
	missing := doSSO(t, h.srv, http.MethodPost, policyPreviewPath, member, request(uuid.New()))
	launch := doSSO(t, h.srv, http.MethodPost, "/api/v1/runs", member, request(ws.ID))
	if preview.Code != 404 || preview.Body.String() != missing.Body.String() || preview.Body.String() != launch.Body.String() {
		t.Fatalf("foreign preview/launch/missing: %d %s / %s / %s", preview.Code, preview.Body.String(), missing.Body.String(), launch.Body.String())
	}
	owner := ssoSession(t, "preview-a", "a@example.com", oidc.RoleUser)
	allowed := previewResult(t, doSSO(t, h.srv, http.MethodPost, policyPreviewPath, owner, request(ws.ID)))
	if len(allowed.RepositoryAccess) != 1 || len(allowed.Spec.WorkspaceRepos) != 1 {
		t.Fatalf("authorized facts missing: %+v", allowed)
	}

	h.srv.cfg.DefaultPolicy.AllowedDomains = []string{"attacker.example"}
	h.srv.cfg.DefaultPolicy.EligibleGrants = []types.GrantSpec{{Kind: types.GrantAPIKey}}
	body := `{"agent":"claude-code","inline_policy":{"min_confinement_class":"CC2","eligible_grants":[{"kind":"api_key","scope":{"host":"attacker.example","secret_name":"hidden-operator"}}]}}`
	before := doSSO(t, h.srv, http.MethodPost, policyPreviewPath, member, body)
	previewResult(t, before)
	if err := secrets.Put(ctx, "hidden-operator", []byte("private-value-sentinel")); err != nil {
		t.Fatal(err)
	}
	h.srv.cfg.Secrets = previewSecretsSpy{Store: secrets, t: t}
	after := doSSO(t, h.srv, http.MethodPost, policyPreviewPath, member, body)
	previewResult(t, after)
	if before.Body.String() != after.Body.String() || strings.Contains(after.Body.String(), "hidden-operator") {
		t.Fatalf("operator secret existence changed preview: %s / %s", before.Body.String(), after.Body.String())
	}
	runs, err := st.ListRuns(ctx)
	if err != nil || len(runs) != 0 {
		t.Fatalf("preview persisted runs: %v, %v", runs, err)
	}
}
