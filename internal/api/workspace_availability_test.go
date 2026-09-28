// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// availGET fires GET /api/v1/workspaces/{id} as the given caller and decodes
// available_to_you (#1267).
func availGET(t *testing.T, srv *Server, id uuid.UUID, session *http.Cookie, bearer string) bool {
	t.Helper()
	var w *httptest.ResponseRecorder
	if bearer != "" {
		w = do(t, srv, http.MethodGet, "/api/v1/workspaces/"+id.String(), bearer, "")
	} else {
		w = doSSO(t, srv, http.MethodGet, "/api/v1/workspaces/"+id.String(), session, "")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("GET /workspaces/%s = %d, want 200: %s", id, w.Code, w.Body.String())
	}
	var got struct {
		AvailableToYou bool `json:"available_to_you"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode workspace: %v", err)
	}
	return got.AvailableToYou
}

// availPreflightAllowed fires the REAL POST /api/v1/runs/preflight against ws
// as the same caller and reports whether the launch decide path admitted it
// (200) or refused it (anything else). execMode sends `task_mode:"exec"` —
// createDoorIsModelRun's own gate (runs_dispatch_llm.go's `isModelRun`
// answers false for it, ignoring `agent` entirely) — the Shell/exec shape
// that never asks the server's model-provider door, which is exactly the run
// type available_to_you must agree with over ITS two arms.
func availPreflightAllowed(t *testing.T, srv *Server, id uuid.UUID, session *http.Cookie, bearer string, execMode bool) bool {
	t.Helper()
	taskMode := ""
	if execMode {
		taskMode = `,"task_mode":"exec"`
	}
	body := fmt.Sprintf(`{"agent":"claude-code","task":"t","workspace_id":%q%s}`, id, taskMode)
	var w *httptest.ResponseRecorder
	if bearer != "" {
		w = do(t, srv, http.MethodPost, "/api/v1/runs/preflight", bearer, body)
	} else {
		w = doSSO(t, srv, http.MethodPost, "/api/v1/runs/preflight", session, body)
	}
	return w.Code == http.StatusOK
}

// TestWorkspaceAvailableToCaller pins #1267's available_to_you against the
// SAME decide path launch runs, over the two arms that apply to EVERY run
// type: the workspace capability itself (unrestricted; restricted with a
// named allow; restricted with only a wildcard allow, which lets nobody in
// once a value is restricted — decide's own step 3), a git-provider pin the
// caller lacks, and an operator's exemption. A model-provider pin, with or
// without a block, is asserted to NOT move the flag at all — that arm stays
// client-side, isAgent-gated (#1249); TestWorkspaceAvailableToCaller_
// ModelProviderArmStaysClientSide is the divergence proof this table cannot
// show on its own.
//
// Every case ALSO fires the real preflight, as a Shell/exec launch (the run
// type available_to_you's two arms both apply to unconditionally), with the
// SAME caller and workspace, and asserts available_to_you agrees with what
// preflight actually decided: the field is worthless if it can say
// "available" for a workspace the launch door refuses, or the reverse.
func TestWorkspaceAvailableToCaller(t *testing.T) {
	ephemeral := []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeEphemeral, Target: "/home/agent/work"}}
	for _, tc := range []struct {
		name          string
		site          types.SiteConfig
		build         func(id uuid.UUID) *capStore
		sources       []types.WorkspaceSource
		llmCred       *types.WorkspaceLLMCred
		operator      bool
		wantAvailable bool
	}{
		{
			name:          "an unrestricted workspace",
			build:         func(uuid.UUID) *capStore { return &capStore{} },
			sources:       ephemeral,
			wantAvailable: true,
		},
		{
			name: "a restricted workspace with a named allow for the caller",
			build: func(id uuid.UUID) *capStore {
				return &capStore{
					restricted: restrictedOne(capWorkspace, id.String()),
					grants: []types.CapabilityGrant{
						grant(types.CapabilitySubjectUser, govMemberSub, capWorkspace, id.String(), types.CapabilityAllow),
					},
				}
			},
			sources:       ephemeral,
			wantAvailable: true,
		},
		{
			name: "a restricted workspace plus a wildcard allow — wildcard lets nobody in",
			build: func(id uuid.UUID) *capStore {
				return &capStore{
					restricted: restrictedOne(capWorkspace, id.String()),
					grants: []types.CapabilityGrant{
						grant(types.CapabilitySubjectAll, "", capWorkspace, capWildcard, types.CapabilityAllow),
					},
				}
			},
			sources:       ephemeral,
			wantAvailable: false,
		},
		{
			name: "a git-provider pin the caller lacks",
			site: admitSite(),
			build: func(uuid.UUID) *capStore {
				return &capStore{grants: []types.CapabilityGrant{
					grant(types.CapabilitySubjectUser, govMemberSub, capWorkspaceProvider, admitRowID, types.CapabilityDeny),
				}}
			},
			sources:       []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: admitOnRow}},
			wantAvailable: false,
		},
		{
			name: "a model-provider pin the caller lacks, WITH a model-provider block, does not move the flag",
			site: types.SiteConfig{ModelProviders: providerBlock(keyProvider("anthropic", "claude-code"))},
			build: func(uuid.UUID) *capStore {
				return &capStore{grants: []types.CapabilityGrant{
					grant(types.CapabilitySubjectUser, govMemberSub, capModelProvider, "anthropic", types.CapabilityDeny),
				}}
			},
			sources:       ephemeral,
			llmCred:       &types.WorkspaceLLMCred{ProviderRef: "anthropic"},
			wantAvailable: true,
		},
		{
			name: "the identical pin, WITHOUT any model-provider block, also does not move the flag",
			build: func(uuid.UUID) *capStore {
				return &capStore{grants: []types.CapabilityGrant{
					grant(types.CapabilitySubjectUser, govMemberSub, capModelProvider, "anthropic", types.CapabilityDeny),
				}}
			},
			sources:       ephemeral,
			llmCred:       &types.WorkspaceLLMCred{ProviderRef: "anthropic"},
			wantAvailable: true,
		},
		{
			name: "an admin caller is exempt from a restriction that walls every member",
			build: func(id uuid.UUID) *capStore {
				return &capStore{
					restricted: restrictedOne(capWorkspace, id.String()),
					grants: []types.CapabilityGrant{
						grant(types.CapabilitySubjectAll, "", capWorkspace, capWildcard, types.CapabilityAllow),
					},
				}
			},
			sources:       ephemeral,
			operator:      true,
			wantAvailable: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := &types.Workspace{
				ID: uuid.New(), Name: "avail-ws", Status: types.WorkspaceScanned,
				Sources: tc.sources, LLMCred: tc.llmCred,
			}
			cs := tc.build(ws.ID)
			srv := providerRunFixture(t, tc.site, cs, ws)

			var session *http.Cookie
			var bearer string
			if tc.operator {
				// The admin BEARER token, not an SSO admin session: an SSO
				// admin session is in the Admin view and cannot launch or
				// preview a launch (refuseAdminViewLaunch) — see
				// firePreflightAndCreate's own comment.
				bearer = adminToken
			} else {
				session = govSession(t, govMemberSub, []string{"eng"}, false)
			}

			available := availGET(t, srv, ws.ID, session, bearer)
			if available != tc.wantAvailable {
				t.Errorf("available_to_you = %v, want %v", available, tc.wantAvailable)
			}
			// execMode=true: a Shell/exec launch, the run type both of
			// available_to_you's arms apply to unconditionally.
			allowed := availPreflightAllowed(t, srv, ws.ID, session, bearer, true)
			if available != allowed {
				t.Errorf("available_to_you = %v but a Shell/exec preflight allowed = %v — the two must agree", available, allowed)
			}
		})
	}
}

// TestWorkspaceAvailableToCaller_ModelProviderArmStaysClientSide is the
// divergence proof TestWorkspaceAvailableToCaller's table cannot show on its
// own: available_to_you stays TRUE for a workspace pinned to a model
// provider the caller is denied, because that arm is deliberately excluded
// from the flag (workspace_availability.go's own doc comment). A Shell/exec
// preflight agrees (it never asks the model-provider door either), but a
// REAL agent preflight against the identical workspace and caller is
// refused — the server's launch gate still enforces the pin; the console's
// own client-side arm (workspaceModelProviderUnavailable, isAgent-gated,
// wizard-types.ts) is what has to show that refusal for an agent run, since
// available_to_you was never meant to.
func TestWorkspaceAvailableToCaller_ModelProviderArmStaysClientSide(t *testing.T) {
	site := types.SiteConfig{ModelProviders: providerBlock(keyProvider("anthropic", "claude-code"))}
	ws := &types.Workspace{
		ID: uuid.New(), Name: "avail-model-pin", Status: types.WorkspaceScanned,
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeEphemeral, Target: "/home/agent/work"}},
		LLMCred: &types.WorkspaceLLMCred{ProviderRef: "anthropic"},
	}
	cs := &capStore{grants: []types.CapabilityGrant{
		grant(types.CapabilitySubjectUser, govMemberSub, capModelProvider, "anthropic", types.CapabilityDeny),
	}}
	srv := providerRunFixture(t, site, cs, ws)
	session := govSession(t, govMemberSub, []string{"eng"}, false)

	if available := availGET(t, srv, ws.ID, session, ""); !available {
		t.Fatalf("available_to_you = false, want true — a model-provider pin must not move the flag")
	}
	if allowed := availPreflightAllowed(t, srv, ws.ID, session, "", true); !allowed {
		t.Errorf("a Shell/exec preflight was refused — the model-provider door must never apply to one")
	}
	if allowed := availPreflightAllowed(t, srv, ws.ID, session, "", false); allowed {
		t.Errorf("an agent preflight was allowed — the denied model-provider pin should have refused it " +
			"(available_to_you correctly said nothing about this; the client's own arm is what must show it)")
	}
}
