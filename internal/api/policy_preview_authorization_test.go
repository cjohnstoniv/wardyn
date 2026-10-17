// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestPolicyPreviewForeignAttachedSources(t *testing.T) {
	for _, mount := range []bool{false, true} {
		t.Run(map[bool]string{false: "extra repo", true: "direct mount"}[mount], func(t *testing.T) {
			root, project := memberProjectRoot(t)
			srv, st, fr := userDispatchHarness(t, runner.UserMountPolicy{Roots: []string{root}})
			primary := st.put(types.Workspace{OwnedBy: ownerOtherSub, Status: types.WorkspaceScanned})
			foreign := types.Workspace{OwnedBy: ownerMemberSub, Status: types.WorkspaceScanned,
				Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: "private-owner/extra"}}}
			if mount {
				foreign.Sources = []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: project}}
				srv.cfg.DefaultPolicy.WorkspaceMounts = []types.WorkspaceMount{{Source: project, Target: "/home/agent/extra"}}
			}
			id := st.put(foreign)
			body := `{"agent":"claude-code","task":"t","workspace_id":"` + primary.String() + `"}`
			if !mount {
				body = `{"agent":"claude-code","task":"t","workspace_id":"` + primary.String() + `","inline_policy":{"min_confinement_class":"CC2","workspace_repos":[{"repo":"private-owner/extra","target":"/home/agent/extra"}]}}`
			}
			member := ssoSession(t, ownerOtherSub, "other@corp.example", oidc.RoleUser)
			preview := doSSO(t, srv, http.MethodPost, policyPreviewPath, member, body)
			if preview.Code != 422 || errorReason(preview) != reasonWorkspaceSourceNotOnboarded || strings.Contains(preview.Body.String(), `"spec"`) {
				t.Fatalf("foreign attached source: %d %s", preview.Code, preview.Body.String())
			}
			for _, path := range []string{"/api/v1/runs", "/api/v1/runs/preflight"} {
				w := doSSO(t, srv, http.MethodPost, path, member, body)
				if w.Code != preview.Code || w.Body.String() != preview.Body.String() {
					t.Fatalf("%s differs: %d %s vs %s", path, w.Code, w.Body.String(), preview.Body.String())
				}
			}
			st.mu.Lock()
			delete(st.workspaces, id)
			st.mu.Unlock()
			missing := doSSO(t, srv, http.MethodPost, policyPreviewPath, member, body)
			if missing.Code != preview.Code || missing.Body.String() != preview.Body.String() || fr.createCalls != 0 {
				t.Fatalf("foreign/missing source oracle or dispatch: %d %s vs %s", missing.Code, missing.Body.String(), preview.Body.String())
			}
		})
	}
}

func TestPolicyPreviewCapabilityDoorsBeforeFacts(t *testing.T) {
	for _, tc := range []struct {
		name, kind, body string
	}{
		{"agent", capAgent, `{"agent":"claude-code"}`},
		{"policy", capPolicy, `{"agent":"claude-code","policy_id":"` + uuid.NewString() + `"}`},
		{"workspace", capWorkspace, `{"agent":"claude-code","workspace_id":"` + uuid.NewString() + `"}`},
		{"workspace provider", capWorkspaceProvider, `{"agent":"claude-code","repo":"https://dev.azure.com/contoso/project/_git/repo"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := providerRunFixture(t, adoTestSiteConfig(false), &capStore{enf: map[string]bool{tc.kind: true}}, nil)
			forbidPreviewSideEffects(t, srv)
			rec := &recRecorder{}
			srv.cfg.Audit = audit.DryRunRecorder{Inner: rec}
			member := ssoSession(t, "member", "m@example.com", oidc.RoleUser)
			w := doSSO(t, srv, http.MethodPost, policyPreviewPath, member, tc.body)
			if w.Code != 403 || strings.Contains(w.Body.String(), `"spec"`) {
				t.Fatalf("capability door: %d %s", w.Code, w.Body.String())
			}
			rows := rec.snapshot()
			if len(rows) != 1 || rows[0].Action != "authz.denied" {
				t.Fatalf("audit %+v", rows)
			}
			var data map[string]any
			if err := json.Unmarshal(rows[0].Data, &data); err != nil || data["dry_run"] != true {
				t.Fatalf("denial not dry-run: %s", rows[0].Data)
			}
		})
	}
}

func TestPolicyPreviewDraftDoesNotImplyInteractive(t *testing.T) {
	profile := govProfile("no-interactive")
	srv := providerRunFixture(t, types.SiteConfig{}, &capStore{govProfile: profile, govTier: types.CapabilitySubjectUser}, nil)
	forbidPreviewSideEffects(t, srv)
	member := ssoSession(t, "member", "m@example.com", oidc.RoleUser)
	previewResult(t, doSSO(t, srv, http.MethodPost, policyPreviewPath, member, `{"agent":"claude-code"}`))
	w := doSSO(t, srv, http.MethodPost, policyPreviewPath, member, `{"agent":"claude-code","interactive":true}`)
	if w.Code != 403 || errorReason(w) != "governance_profile" {
		t.Fatalf("explicit interactive posture escaped: %d %s", w.Code, w.Body.String())
	}
}

func TestPolicyPreviewSharesAuthenticationAndCSRF(t *testing.T) {
	srv := csrfOIDCServer(t, csrfRedirectURL)
	member := ssoSession(t, "member", "m@example.com", oidc.RoleUser)
	for _, path := range []string{"/api/v1/runs", "/api/v1/runs/preflight", policyPreviewPath} {
		for _, tc := range []struct {
			name, origin string
			cookie       *http.Cookie
			status       int
		}{
			{"unsigned", csrfRedirectHost, nil, 401},
			{"same origin reaches strict decoder", csrfRedirectHost, member, 400},
			{"foreign origin", "https://other.example", member, 403},
		} {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				w := csrfDo(t, srv, http.MethodPost, path, csrfOIDCHost, tc.origin, "", `{`, tc.cookie)
				if w.Code != tc.status {
					t.Fatalf("got %d %s", w.Code, w.Body.String())
				}
				if tc.status == 403 && errorReason(w) != csrfAuditReason {
					t.Fatalf("not the CSRF gate: %s", w.Body.String())
				}
			})
		}
	}
}
