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
	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestPolicyPreviewProviderAuthorizationWithoutLiveness(t *testing.T) {
	for kind := range types.ClosedModelProviderKinds {
		t.Run(string(kind), func(t *testing.T) {
			p := keyProvider("private-provider-sentinel", "claude-code")
			p.Kind = kind
			srv := providerRunFixture(t, types.SiteConfig{ModelProviders: providerBlock(p)}, &capStore{}, nil)
			forbidPreviewSideEffects(t, srv)
			srv.cfg.MaxConcurrentRuns = 1
			w := doSSO(t, srv, http.MethodPost, policyPreviewPath, ssoSession(t, "member", "m@example.com", oidc.RoleUser), `{"agent":"claude-code","model_provider":"private-provider-sentinel"}`)
			result := previewResult(t, w)
			if slices.Contains(result.Pending, "model_provider_selection") || strings.Contains(w.Body.String(), p.ID) {
				t.Fatalf("provider fact leaked/missing: %s", w.Body.String())
			}
			if rows := srv.cfg.Audit.(*recRecorder).snapshot(); len(rows) != 0 {
				t.Fatalf("unexpected audit: %+v", rows)
			}
		})
	}
	for _, named := range []bool{false, true} {
		srv := providerRunFixture(t, types.SiteConfig{ModelProviders: providerBlock(keyProvider("hidden-provider", "claude-code"))}, &capStore{enf: map[string]bool{capModelProvider: true}}, nil)
		body := `{"agent":"claude-code"}`
		if named {
			body = `{"agent":"claude-code","model_provider":"hidden-provider"}`
		}
		member := ssoSession(t, "member", "m@example.com", oidc.RoleUser)
		w := doSSO(t, srv, http.MethodPost, policyPreviewPath, member, body)
		if !named {
			result := previewResult(t, w)
			if !slices.Contains(result.Pending, "model_provider_selection") || strings.Contains(w.Body.String(), "hidden-provider") {
				t.Fatalf("pending leaked roster: %s", w.Body.String())
			}
		} else {
			launch := doSSO(t, srv, http.MethodPost, "/api/v1/runs", member, body)
			if w.Code != 422 || w.Body.String() != launch.Body.String() {
				t.Fatalf("selection denial differs: %d %s vs %d %s", w.Code, w.Body.String(), launch.Code, launch.Body.String())
			}
		}
	}
}

func TestPolicyPreviewExpiredCredentialDoesNotRenew(t *testing.T) {
	srv := createRenewalFixture(t)
	calls := renewedOIDC(t)
	token := providerAdminToken(srv, createRenewalOwner)
	forbidPreviewSideEffects(t, srv)
	w := do(t, srv, http.MethodPost, policyPreviewPath, token, createRenewalBody)
	result := previewResult(t, w)
	if calls.Load() != 0 || srv.awsSSOTokenSpent(awsSSOTokenFingerprint("old-refresh-token-1234567890")) {
		t.Fatal("preview spent the stored refresh token")
	}
	if !slices.Contains(result.Pending, previewCredential) || len(result.Warnings) != 0 || strings.Contains(w.Body.String(), "bedrock-sso") {
		t.Fatalf("preview graded credential liveness or exposed a renewal warning: %s", w.Body.String())
	}
}

func TestPolicyPreviewInheritedProviderRefusal(t *testing.T) {
	ws := &types.Workspace{ID: uuid.New(), Name: "allowed", Status: types.WorkspaceScanned,
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: govWorkspaceRepo}}, LLMCred: &types.WorkspaceLLMCred{ProviderRef: "hidden-pin"}}
	srv := providerRunFixture(t, types.SiteConfig{ModelProviders: providerBlock(keyProvider("hidden-pin", "claude-code"))}, &capStore{enf: map[string]bool{capModelProvider: true}}, ws)
	w := doSSO(t, srv, http.MethodPost, policyPreviewPath, ssoSession(t, "member", "m@example.com", oidc.RoleUser), `{"agent":"claude-code","workspace_id":"`+ws.ID.String()+`"}`)
	if w.Code != 403 || strings.Contains(w.Body.String(), "hidden-pin") || strings.Contains(w.Body.String(), `"spec"`) {
		t.Fatalf("pin authorization leaked: %d %s", w.Code, w.Body.String())
	}
}

func TestPolicyPreviewADOWithoutCredentialValuesOrConfiguredHosts(t *testing.T) {
	sc := adoTestSiteConfig(false)
	sc.WorkspaceProviders.Git[0].Entra = &types.ADOEntraConfig{CapabilityCeiling: adoscope.ProfileDefault()}
	sc.ScmHosts = []string{"unrelated.enterprise.invalid"}
	srv := providerRunFixture(t, sc, &capStore{}, nil)
	forbidPreviewSideEffects(t, srv)
	w := doSSO(t, srv, http.MethodPost, policyPreviewPath, ssoSession(t, "member", "m@example.com", oidc.RoleUser), `{"agent":"claude-code","repo":"https://dev.azure.com/contoso/project/_git/repo"}`)
	result := previewResult(t, w)
	if len(result.RepositoryAccess) != 1 || result.RepositoryAccess[0].Kind != "azure_devops" || len(result.RepositoryAccess[0].DefaultProfile) == 0 {
		t.Fatalf("ADO facts: %s", w.Body.String())
	}
	for _, hidden := range []string{scmTestRowID, "unrelated.enterprise.invalid", "credential_source", "tenant_id", "client_id"} {
		if strings.Contains(w.Body.String(), hidden) {
			t.Errorf("leaked %q: %s", hidden, w.Body.String())
		}
	}
}

func TestPolicyPreviewDriveAuthorizationSkipsReadiness(t *testing.T) {
	for _, tc := range []struct {
		name                           string
		paused, noAllocation, readOnly bool
		orgDisabled, profileDisabled   bool
		status                         int
	}{
		{"authorized inaccessible host share", false, false, false, false, false, 200},
		{"paused", true, false, false, false, false, 422},
		{"no allocation", false, true, false, false, false, 422},
		{"readonly widening", false, false, true, false, false, 422},
		{"org disabled", false, false, false, true, false, 422},
		{"profile disabled", false, false, false, false, true, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := driveFixture(func(d *types.UserDrive) {
				d.Backend = types.DriveBackendHostPath
				d.HomeTemplate = types.HomeTemplateSub
				d.HostRoot = "/PRIVATE-PREVIEW-NO-PROBE"
				d.Writable = !tc.readOnly
			})
			st := &driveStore{drive: d, grant: grantFixture(d.ID, nil), tier: types.CapabilitySubjectUser}
			st.grant.Enabled = !tc.paused
			if tc.orgDisabled {
				st.site.WorkspaceProviders = &types.WorkspaceProviders{Storage: &types.StorageProviders{UserDrive: &types.UserDriveProvider{Disabled: true}}}
			}
			if tc.profileDisabled {
				st.profile = &types.GovernanceProfile{ID: uuid.New(), Name: "drive-denied", Ceiling: types.RunPolicySpec{MinConfinementClass: types.CC2}, Limits: types.GovernanceLimits{DenyUserDrive: true}}
			}
			if tc.noAllocation {
				st.drive = nil
				st.grant = nil
			}
			h := newHarness(t)
			cfg := baseTestConfig(h, previewQuotaSpy{Store: previewDriveStore{st}, t: t})
			cfg.DefaultPolicy = types.RunPolicySpec{MinConfinementClass: types.CC2}
			cfg.Runner = previewRunnerSpy{t: t}
			srv := New(cfg)
			r := httptest.NewRequest(http.MethodPost, policyPreviewPath, strings.NewReader(`{"agent":"claude-code","drive":{"enabled":true,"read_only":false}}`)).WithContext(driveMemberCtx(nil, false))
			w := httptest.NewRecorder()
			srv.handlePolicyPreview(w, r)
			if w.Code != tc.status {
				t.Fatalf("got %d %s", w.Code, w.Body.String())
			}
			if tc.status == 200 && !slices.Contains(previewResult(t, w).Pending, "drive_readiness") {
				t.Fatal("drive readiness missing")
			}
			if strings.Contains(w.Body.String(), d.HostRoot) {
				t.Fatal("host root leaked")
			}
		})
	}
}

func TestPolicyPreviewSharesDryRunDenialCoalescer(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg.OIDC = &oidc.Authenticator{}
	rec := &recRecorder{}
	coalescer := &audit.DenialCoalescer{Inner: rec}
	h.srv.cfg.Audit = audit.DelegationRecorder{Inner: audit.DryRunRecorder{Inner: coalescer}}
	h.srv.router = h.srv.routes()
	member := ssoSession(t, "member", "m@example.com", oidc.RoleUser)
	for _, path := range []string{policyPreviewPath, "/api/v1/runs/preflight", policyPreviewPath} {
		w := doSSO(t, h.srv, http.MethodPost, path, member, `{"agent":"claude-code","image":"ungranted"}`)
		if w.Code != 403 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
	rows := rec.snapshot()
	if len(rows) != 1 {
		t.Fatalf("shared refusal wrote %d rows", len(rows))
	}
	var data map[string]any
	if err := json.Unmarshal(rows[0].Data, &data); err != nil || data["dry_run"] != true {
		t.Fatalf("not dry: %s", rows[0].Data)
	}
	coalescer.Flush(context.Background())
	rows = rec.snapshot()
	if len(rows) != 2 || rows[1].Action != audit.CoalesceAction {
		t.Fatalf("summary: %+v", rows)
	}
	if err := json.Unmarshal(rows[1].Data, &data); err != nil || data["count"] != float64(3) {
		t.Fatalf("summary count: %s", rows[1].Data)
	}
}

type previewDriveStore struct{ *driveStore }

func (previewDriveStore) ListWorkspaces(context.Context) ([]types.Workspace, error) { return nil, nil }
