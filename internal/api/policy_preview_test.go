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

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const policyPreviewPath = "/api/v1/runs/policy-preview"

func previewResult(t *testing.T, w *httptest.ResponseRecorder) policyPreviewResponse {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("preview = %d %s", w.Code, w.Body.String())
	}
	var result policyPreviewResponse
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Provisional || result.Pending == nil || result.Warnings == nil || result.RepositoryAccess == nil || result.Provenance == nil {
		t.Fatalf("missing provisional/array contract: %s", w.Body.String())
	}
	return result
}

func TestPolicyPreviewDraftAndShapeRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, body, reason string
		status             int
	}{
		{"draft", `{"agent":"claude-code"}`, "", 200},
		{"exec", `{"agent":"claude-code","task_mode":"exec","task":"echo hi"}`, "", 200},
		{"interactive", `{"agent":"claude-code","interactive":true}`, "", 200},
		{"unknown field", `{"agent":"claude-code","unknown":true}`, reasonInvalidRequestBody, 400},
		{"missing agent", `{}`, reasonAgentRequired, 400},
		{"reserved", `{"agent":"claude-code","task":"harness login"}`, reasonRunTaskReserved, 400},
		{"class", `{"agent":"claude-code","confinement_class":"CC7"}`, reasonConfinementClassUnknown, 400},
		{"floor", `{"agent":"claude-code","confinement_class":"CC1"}`, reasonConfinementClassConflict, 422},
		{"task mode", `{"agent":"claude-code","task_mode":"other"}`, reasonTaskModeUnknown, 400},
		{"interactive start", `{"agent":"claude-code","interactive_start":"other"}`, reasonInteractiveStartUnknown, 400},
		{"approvals enum", `{"agent":"claude-code","tool_approvals":"other"}`, reasonToolApprovalsUnknown, 400},
		{"codex hold", `{"agent":"codex-cli","tool_approvals":"hold"}`, reasonToolApprovalsHoldUnsupportedAgent, 400},
		{"interactive hold", `{"agent":"claude-code","interactive":true,"tool_approvals":"hold"}`, reasonToolApprovalsHoldInteractiveConflict, 400},
		{"retired integration", `{"agent":"claude-code","integration_id":"retired"}`, reasonIntegrationIDRetired, 422},
		{"text controls", `{"agent":"claude-code","title":"bad\u0000"}`, reasonRunFieldControlChar, 400},
		{"text limit", `{"agent":"claude-code","title":"` + strings.Repeat("x", 201) + `"}`, reasonRunFieldTooLong, 400},
		{"xor", `{"agent":"claude-code","inline_policy":{},"policy_id":"` + uuid.NewString() + `"}`, reasonInlinePolicyXOR, 400},
		{"provider id", `{"agent":"claude-code","model_provider":"BAD ID"}`, reasonModelProviderIDInvalid, 400},
		{"provider no block", `{"agent":"claude-code","model_provider":"other"}`, reasonModelProviderNoBlockConfigured, 422},
		{"provider exec", `{"agent":"claude-code","task_mode":"exec","model_provider":"other"}`, reasonModelProviderNotApplicable, 400},
		{"image xor", `{"agent":"claude-code","image":"base","devcontainer_repo":"acme/project"}`, reasonImageDevcontainerExclusive, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			w := do(t, h.srv, http.MethodPost, policyPreviewPath, adminToken, tc.body)
			if w.Code != tc.status || errorReason(w) != tc.reason {
				t.Fatalf("got %d %s, want %d %s", w.Code, w.Body.String(), tc.status, tc.reason)
			}
			if tc.status == 200 {
				result := previewResult(t, w)
				for _, p := range []policyPreviewPending{"credential_liveness", "autonomy", "tool_approvals", "runner_confinement", "dispatch_egress"} {
					if !slices.Contains(result.Pending, p) {
						t.Errorf("pending lacks %s", p)
					}
				}
				if tc.name == "draft" && (!slices.Contains(result.Pending, "task") || !slices.Contains(result.Pending, "model_provider_selection")) {
					t.Errorf("draft pending=%v", result.Pending)
				}
			}
			if len(h.audit.snapshot()) != 0 {
				t.Fatalf("preview wrote audit: %+v", h.audit.snapshot())
			}
		})
	}
}

func TestPolicyPreviewWorkspaceIsolation(t *testing.T) {
	root, project := memberProjectRoot(t)
	srv, st, fr := userDispatchHarness(t, runner.UserMountPolicy{Roots: []string{root}})
	foreign := memberOwnedWorkspace(st, ownerMemberSub, project)
	member := ssoSession(t, ownerOtherSub, "other@corp.example", oidc.RoleUser)
	body := func(id uuid.UUID) string {
		return `{"agent":"claude-code","task":"t","workspace_id":"` + id.String() + `"}`
	}
	got := doSSO(t, srv, http.MethodPost, policyPreviewPath, member, body(foreign))
	for _, other := range []*httptest.ResponseRecorder{
		doSSO(t, srv, http.MethodPost, policyPreviewPath, member, body(uuid.New())),
		doSSO(t, srv, http.MethodPost, "/api/v1/runs", member, body(foreign)),
		doSSO(t, srv, http.MethodPost, "/api/v1/runs/preflight", member, body(foreign)),
	} {
		if got.Code != 404 || got.Code != other.Code || got.Body.String() != other.Body.String() {
			t.Fatalf("foreign/missing/launch mismatch: %d %s versus %d %s", got.Code, got.Body.String(), other.Code, other.Body.String())
		}
	}
	if strings.Contains(got.Body.String(), project) || strings.Contains(got.Body.String(), `"spec"`) || fr.createCalls != 0 {
		t.Fatal("foreign resource leaked or dispatched")
	}
	// An inert override neither attaches nor resolves a workspace ID.
	srv.cfg.Store = previewWorkspaceLookupSpy{ownerStore: st, t: t}
	base := doSSO(t, srv, http.MethodPost, policyPreviewPath, member, `{"agent":"claude-code"}`)
	for _, id := range []uuid.UUID{foreign, uuid.New()} {
		w := doSSO(t, srv, http.MethodPost, policyPreviewPath, member, `{"agent":"claude-code","workspaces":[{"workspace_id":"`+id.String()+`"}]}`)
		previewResult(t, w)
		if w.Body.String() != base.Body.String() {
			t.Errorf("inert override changed facts: %s", w.Body.String())
		}
	}
	srv.cfg.Store = st
	// The owner sees only the redacted effective mount.
	owner := ssoSession(t, ownerMemberSub, "owner@corp.example", oidc.RoleUser)
	w := doSSO(t, srv, http.MethodPost, policyPreviewPath, owner, body(foreign))
	result := previewResult(t, w)
	if !result.Redacted || strings.Contains(w.Body.String(), project) || len(result.Spec.WorkspaceMounts) != 1 {
		t.Fatalf("unsafe mount facts: %s", w.Body.String())
	}
}

func TestPolicyPreviewSecretGuessIsNotAnOracle(t *testing.T) {
	var outputs []string
	for _, exists := range []bool{false, true} {
		h := newHarness(t)
		secrets := &memSecrets{m: map[string][]byte{}}
		if exists {
			secrets.m["hidden-operator-secret"] = []byte("NEVER-READ")
		}
		h.srv.cfg.Secrets = secrets
		h.srv.cfg.OIDC = &oidc.Authenticator{}
		h.srv.cfg.DefaultPolicy = types.RunPolicySpec{MinConfinementClass: types.CC2, AllowedDomains: []string{"attacker.example"}, EligibleGrants: []types.GrantSpec{{Kind: types.GrantGitPAT}}}
		h.srv.router = h.srv.routes()
		w := doSSO(t, h.srv, http.MethodPost, policyPreviewPath, ssoSession(t, "member", "m@example.com", oidc.RoleUser), `{"agent":"claude-code","inline_policy":{"min_confinement_class":"CC2","eligible_grants":[{"kind":"git_pat","scope":{"host":"attacker.example","secret_name":"hidden-operator-secret"}}]}}`)
		result := previewResult(t, w)
		if len(result.Spec.EligibleGrants) != 0 || strings.Contains(w.Body.String(), "hidden-operator-secret") {
			t.Fatalf("secret leaked: %s", w.Body.String())
		}
		outputs = append(outputs, w.Body.String())
	}
	if outputs[0] != outputs[1] {
		t.Fatalf("secret existence changed response:\n%s\n%s", outputs[0], outputs[1])
	}
}

type previewSecretsSpy struct {
	secretstore.Store
	t *testing.T
}

func (s previewSecretsSpy) For(owner string) secretstore.Store {
	return previewSecretsSpy{Store: s.Store.For(owner), t: s.t}
}
func (s previewSecretsSpy) Get(context.Context, string) ([]byte, error) {
	s.t.Fatal("preview read a credential value")
	return nil, nil
}
func (s previewSecretsSpy) Put(context.Context, string, []byte) error {
	s.t.Fatal("preview wrote/renewed a credential")
	return nil
}
func (s previewSecretsSpy) Delete(context.Context, string) error {
	s.t.Fatal("preview deleted a credential")
	return nil
}

type previewRunnerSpy struct {
	runner.Runner
	t *testing.T
}

func (s previewRunnerSpy) Capabilities(context.Context) (runner.Capabilities, error) {
	s.t.Fatal("preview probed runner capabilities")
	return runner.Capabilities{}, nil
}
func (s previewRunnerSpy) ProbeDrive(context.Context, types.DriveMount) (runner.DriveProbe, error) {
	s.t.Fatal("preview probed drive")
	return runner.DriveProbe{}, nil
}

type previewQuotaSpy struct {
	store.Store
	t *testing.T
}

func (s previewQuotaSpy) CountActiveRunsBy(context.Context, string) (int, error) {
	s.t.Fatal("preview counted member runs")
	return 0, nil
}
func (s previewQuotaSpy) CountNonTerminalRuns(context.Context) (int, error) {
	s.t.Fatal("preview counted deployment runs")
	return 0, nil
}
