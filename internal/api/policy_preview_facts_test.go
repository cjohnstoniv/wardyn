// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestPolicyPreviewSafeWarnings(t *testing.T) {
	safe := []string{
		composer.WarnResourcesCapped,
		"push_rules inherited from the operator's policy",
		"allow_all_egress disabled: operator policy does not permit allow-all egress",
		"git_push_any_branch disabled: operator policy keeps push branch-namespace confinement on",
		`confinement raised from "CC1" to operator minimum "CC2"`,
		`first_use_approval raised from "wait_for_review" to operator minimum "always_deny"`,
		"auto_stop_after_sec capped to operator maximum 300s",
		"dropped 2 proposed workspace mount(s): host mounts are operator-authored, never composer-proposed",
		"github grant: dropped 2 repo(s) outside operator scope",
		"push_rules.max_file_size_mib capped to operator maximum 5",
	}
	unsafe := []string{
		`dropped egress host "PRIVATE-HOST"`,
		`dropped api_key grant naming secret "PRIVATE-SECRET"`,
		`workspace mount "/PRIVATE-PATH" dropped`,
		`provider PRIVATE-PROVIDER will renew at launch`,
		`first_use_approval raised from "private_secret" to operator minimum "always_deny"`,
		"auto_stop_after_sec capped to operator maximum PRIVATE-NAMEs",
	}
	if got := previewSafeWarnings(append(slices.Clone(safe), unsafe...)); !slices.Equal(got, safe) {
		t.Fatalf("warning disclosure: %v", got)
	}
	h := newHarness(t)
	h.srv.cfg.OIDC = &oidc.Authenticator{}
	h.srv.router = h.srv.routes()
	w := doSSO(t, h.srv, http.MethodPost, policyPreviewPath, ssoSession(t, "member", "m@example.com", oidc.RoleUser),
		`{"agent":"claude-code","inline_policy":{"min_confinement_class":"CC1","allowed_domains":["PRIVATE-HOST"],"workspace_mounts":[{"source":"/PRIVATE-PATH","target":"/home/agent/x"}],"eligible_grants":[{"kind":"git_pat","scope":{"host":"PRIVATE-HOST","secret_name":"PRIVATE-SECRET"}}]}}`)
	result := previewResult(t, w)
	if len(result.Warnings) == 0 || strings.Contains(w.Body.String(), "PRIVATE-") {
		t.Fatalf("post-clamp warnings leaked: %s", w.Body.String())
	}
}

func TestPolicyPreviewRedactionPreservesPATEmptyAxis(t *testing.T) {
	for _, empty := range []bool{false, true} {
		scope := map[string]any{"host": "git.example", "secret_name": "PRIVATE-SECRET"}
		if empty {
			scope["repos"] = []string{}
		}
		spec := types.RunPolicySpec{MinConfinementClass: types.CC2,
			EligibleGrants:  []types.GrantSpec{{Kind: types.GrantGitPAT, Scope: mustJSON(scope)}},
			LLMInspection:   &types.LLMInspectionSpec{WorkspaceSecretValues: []string{"PRIVATE-VALUE"}},
			WorkspaceMounts: []types.WorkspaceMount{{Source: "/PRIVATE-MOUNT", Target: "/home/agent/work"}}}
		before := mustJSON(spec)
		facts := policyPreviewFacts(createRunRequest{}, spec, policySourceRecord{Kind: policyKindInline}, nil, types.SiteConfig{}, runProviderChoice{}, runComponents{}, composer.Baseline{})
		body := string(mustJSON(facts))
		if !facts.Redacted || strings.Contains(body, "PRIVATE-") || strings.Contains(body, "workspace_secret_values") || facts.Spec.AllowedDomains == nil {
			t.Fatalf("redaction: %s", body)
		}
		var got map[string]any
		if err := json.Unmarshal(facts.Spec.EligibleGrants[0].Scope, &got); err != nil {
			t.Fatal(err)
		}
		repos, present := got["repos"]
		if present != empty || (empty && !reflect.DeepEqual(repos, []any{})) {
			t.Fatalf("repos lost omitted/empty distinction: %s", body)
		}
		if string(before) != string(mustJSON(spec)) {
			t.Fatal("redacting preview mutated the authored spec")
		}
	}
}

func TestPolicyPreviewSourceProvenance(t *testing.T) {
	for _, kind := range []string{policyKindDefault, policyKindProfile, policyKindStored, policyKindInline} {
		t.Run(kind, func(t *testing.T) {
			cs := &capStore{}
			if kind == policyKindProfile {
				cs.govProfile = govProfile("assigned-name")
				cs.govTier = types.CapabilitySubjectUser
			}
			srv := providerRunFixture(t, types.SiteConfig{}, cs, nil)
			body, id := `{"agent":"claude-code","task":"t"}`, uuid.New()
			if kind == policyKindStored {
				srv.cfg.Store.(*integStore).policies[id] = types.RunPolicy{ID: id, Name: "stored-name", Spec: govDeployment()}
				body = `{"agent":"claude-code","task":"t","policy_id":"` + id.String() + `"}`
			} else if kind == policyKindInline {
				body = `{"agent":"claude-code","task":"t","inline_policy":{"min_confinement_class":"CC2"}}`
			}
			forbidPreviewSideEffects(t, srv)
			w := doSSO(t, srv, http.MethodPost, policyPreviewPath, ssoSession(t, "member", "m@example.com", oidc.RoleUser), body)
			result := previewResult(t, w)
			if result.Source.Kind != kind || (result.Source.PolicyID != nil) != (kind == policyKindStored) {
				t.Fatalf("source: %s", w.Body.String())
			}
			if kind == policyKindStored && (result.Source.Name != "stored-name" || *result.Source.PolicyID != id) {
				t.Fatalf("stored source: %+v", result.Source)
			}
			if kind == policyKindProfile && (result.Source.Name != "assigned-name" || strings.Contains(w.Body.String(), cs.govProfile.ID.String())) {
				t.Fatalf("profile source leaked identity: %s", w.Body.String())
			}
		})
	}
}

func TestPolicyPreviewADOLegacyGrantDoesNotReadOwnPAT(t *testing.T) {
	site := adoTestSiteConfig(false)
	site.WorkspaceProviders.Git = []types.GitProvider{ownPATTestRow()}
	srv := providerRunFixture(t, site, &capStore{}, nil)
	srv.cfg.DefaultPolicy.EligibleGrants = []types.GrantSpec{{Kind: types.GrantGitPAT, Scope: mustJSON(types.GitPATScope{Host: "dev.azure.com", SecretName: "legacy-pat"})}}
	member := ssoSession(t, "member", "m@example.com", oidc.RoleUser)
	body := `{"agent":"claude-code","task":"t","repo":"https://dev.azure.com/contoso/project/_git/repo"}`
	preflight := doSSO(t, srv, http.MethodPost, "/api/v1/runs/preflight", member, body)
	if preflight.Code != 422 || !strings.Contains(preflight.Body.String(), adoOwnPATNotAddedRefusal) {
		t.Fatalf("fixture must reach the ADO credential check: %d %s", preflight.Code, preflight.Body.String())
	}
	// A present sealed own-token row forces Get if the readiness helper is reached.
	if err := srv.cfg.Secrets.For("member").Put(context.Background(), adoOwnPATSecretName(ownPATRowID), []byte("PRIVATE-CREDENTIAL")); err != nil {
		t.Fatal(err)
	}
	forbidPreviewSideEffects(t, srv)
	w := doSSO(t, srv, http.MethodPost, policyPreviewPath, member, body)
	result := previewResult(t, w)
	if strings.Contains(w.Body.String(), "legacy-pat") || !slices.Contains(result.Pending, previewCredential) {
		t.Fatalf("legacy grant facts: %s", w.Body.String())
	}
}

func TestPolicyPreviewADOPolicyEmptyUsesRowDefaults(t *testing.T) {
	sc := adoTestSiteConfig(false)
	sc.WorkspaceProviders.Git[0] = ownPATTestRow()
	sc.WorkspaceProviders.Git[0].Entra.DefaultProfile = nil
	ws := &types.Workspace{ID: uuid.New(), Status: types.WorkspaceScanned, Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: "https://dev.azure.com/contoso/project/_git/repo"}}}
	srv := providerRunFixture(t, sc, &capStore{}, ws)
	forbidPreviewSideEffects(t, srv)
	member := ssoSession(t, "member", "m@example.com", oidc.RoleUser)
	var outputs []string
	for _, axis := range []string{"", `,"azure_devops_capabilities":[]`} {
		body := `{"agent":"claude-code","workspace_id":"` + ws.ID.String() + `","inline_policy":{"min_confinement_class":"CC2"` + axis + `}}`
		w := doSSO(t, srv, http.MethodPost, policyPreviewPath, member, body)
		result := previewResult(t, w)
		wantProfile := []adoscope.Capability{adoscope.CapCodeRead, adoscope.CapProjectRead}
		wantCeiling := []adoscope.Capability{adoscope.CapCodeRead, adoscope.CapCodeWrite, adoscope.CapPR, adoscope.CapProjectRead}
		if len(result.RepositoryAccess) != 1 || !slices.Equal(result.RepositoryAccess[0].DefaultProfile, wantProfile) || !slices.Equal(result.RepositoryAccess[0].CapabilityCeiling, wantCeiling) {
			t.Fatalf("resolved row defaults: %s", w.Body.String())
		}
		outputs = append(outputs, w.Body.String())
	}
	if outputs[0] != outputs[1] {
		t.Fatalf("ADO absent/empty must use same defaults: %s vs %s", outputs[0], outputs[1])
	}
}
