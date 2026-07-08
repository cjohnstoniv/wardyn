// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func gitPATPolicy(host, secret string) types.RunPolicySpec {
	return types.RunPolicySpec{
		MinConfinementClass: types.CC2,
		EligibleGrants: []types.GrantSpec{{
			Kind:  types.GrantGitPAT,
			Scope: mustJSON(map[string]any{"host": host, "secret_name": secret}),
		}},
	}
}

// TestValidatePolicySpec_GitPAT asserts the write-time invariants for git_pat:
// a valid grant passes; empty host, empty secret_name, and a reserved secret
// name are rejected (fail closed — a policy must not exfiltrate a platform key).
func TestValidatePolicySpec_GitPAT(t *testing.T) {
	if err := validatePolicySpec(gitPATPolicy("dev.azure.com", "ado-pat")); err != nil {
		t.Fatalf("valid git_pat grant rejected: %v", err)
	}

	bad := []struct {
		name string
		spec types.RunPolicySpec
	}{
		{"empty-host", gitPATPolicy("", "ado-pat")},
		{"empty-secret", gitPATPolicy("dev.azure.com", "")},
		{"reserved-secret", gitPATPolicy("dev.azure.com", "wardyn-signing-key")},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			if err := validatePolicySpec(c.spec); err == nil {
				t.Fatalf("%s: expected validatePolicySpec to reject, got nil", c.name)
			}
		})
	}
}

// TestValidateInlineSecretRefs_GitPAT asserts a git_pat grant referencing an
// unknown secret is rejected at create time (422), and a present one passes.
func TestValidateInlineSecretRefs_GitPAT(t *testing.T) {
	h, _ := newSecretsHarness(t) // memSecrets seeded with "anthropic-api-key"
	ctx := context.Background()

	present := gitPATPolicy("gitlab.com", "anthropic-api-key") // reuse the seeded name
	if code, err := h.srv.validateInlineSecretRefs(ctx, present); err != nil || code != 0 {
		t.Fatalf("present git_pat secret: code=%d err=%v, want (0,nil)", code, err)
	}

	missing := gitPATPolicy("gitlab.com", "no-such-pat")
	if code, err := h.srv.validateInlineSecretRefs(ctx, missing); err == nil || code != http.StatusUnprocessableEntity {
		t.Fatalf("missing git_pat secret: code=%d err=%v, want (422,err)", code, err)
	}

	reserved := gitPATPolicy("gitlab.com", "wardyn-session-key")
	if code, err := h.srv.validateInlineSecretRefs(ctx, reserved); err == nil || code != http.StatusUnprocessableEntity {
		t.Fatalf("reserved git_pat secret: code=%d err=%v, want (422,err)", code, err)
	}
}

// TestADOEgressDomains asserts the ADO egress-bundle mapping used by the
// git_pat lane (runs.go handleCreateRun): a host matching either published ADO
// hostname (modern dev.azure.com or a legacy org.visualstudio.com) returns
// BOTH hosts (an org may clone via one while ADO's API surface uses the
// other); any other host (GitHub, GitLab, a bare GHES host) returns nil — the
// ADO bundle must never leak onto an unrelated git_pat grant.
func TestADOEgressDomains(t *testing.T) {
	cases := []struct {
		host string
		want []string
	}{
		{"dev.azure.com", []string{"dev.azure.com", "*.visualstudio.com"}},
		{"Dev.Azure.Com", []string{"dev.azure.com", "*.visualstudio.com"}},  // case-insensitive
		{"dev.azure.com.", []string{"dev.azure.com", "*.visualstudio.com"}}, // trailing dot tolerated
		{"myorg.visualstudio.com", []string{"dev.azure.com", "*.visualstudio.com"}},
		{"github.com", nil},
		{"gitlab.com", nil},
		{"ghes.corp.internal", nil},
		{"", nil},
	}
	for _, c := range cases {
		got := adoEgressDomains(c.host)
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("adoEgressDomains(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}

// TestGroundGitPATGrants_KeepDrop asserts compose grounding keeps a git_pat
// grant only when its host matches a detected non-github remote and drops it
// otherwise; non-git_pat grants pass through untouched.
func TestGroundGitPATGrants_KeepDrop(t *testing.T) {
	spec := &types.RunPolicySpec{
		EligibleGrants: []types.GrantSpec{
			{Kind: types.GrantGitPAT, Scope: mustJSON(map[string]any{"host": "dev.azure.com", "secret_name": "ado"})},
			{Kind: types.GrantGitPAT, Scope: mustJSON(map[string]any{"host": "gitlab.com", "secret_name": "gl"})},
			{Kind: types.GrantAPIKey, Scope: mustJSON(map[string]any{"host": "api.anthropic.com", "secret_name": "k"})},
		},
	}
	groundGitPATGrants(spec, []string{"dev.azure.com"}) // only ADO detected

	var patHosts []string
	apiKeys := 0
	for _, g := range spec.EligibleGrants {
		switch g.Kind {
		case types.GrantGitPAT:
			host, _, _, _ := gitPATScopeFields(g.Scope)
			patHosts = append(patHosts, host)
		case types.GrantAPIKey:
			apiKeys++
		}
	}
	if len(patHosts) != 1 || patHosts[0] != "dev.azure.com" {
		t.Fatalf("kept git_pat hosts = %v, want [dev.azure.com] (gitlab dropped)", patHosts)
	}
	if apiKeys != 1 {
		t.Fatalf("api_key grant count = %d, want 1 (untouched)", apiKeys)
	}
}

// TestGroundAPIKeySecretNames asserts compose grounding rewrites api_key secret
// names the store can never hold (secretNameRE) — canonical name for known
// provider hosts, mechanical sanitize otherwise — and leaves storable names and
// other grant kinds untouched.
func TestGroundAPIKeySecretNames(t *testing.T) {
	apiKey := func(host, secret string) types.GrantSpec {
		return types.GrantSpec{Kind: types.GrantAPIKey, Scope: mustJSON(map[string]any{
			"host": host, "header": "x-api-key", "format": "%s", "secret_name": secret})}
	}
	secretOf := func(t *testing.T, g types.GrantSpec) string {
		t.Helper()
		var scope map[string]any
		if err := json.Unmarshal(g.Scope, &scope); err != nil {
			t.Fatalf("decode scope: %v", err)
		}
		s, _ := scope["secret_name"].(string)
		return s
	}
	spec := &types.RunPolicySpec{
		EligibleGrants: []types.GrantSpec{
			apiKey("api.anthropic.com", "ANTHROPIC_API_KEY"), // known host -> canonical
			apiKey("api.example.com", "My Custom_KEY"),       // unknown host -> sanitized
			apiKey("api.anthropic.com", "my-anthropic-key"),  // already storable -> untouched
			apiKey("api.example.com", "***"),                 // nothing storable -> untouched (validation catches)
			{Kind: types.GrantGitPAT, Scope: mustJSON(map[string]any{"host": "gitlab.com", "secret_name": "GL_PAT"})}, // wrong kind -> untouched
		},
	}
	warns := groundAPIKeySecretNames(spec)
	if got := secretOf(t, spec.EligibleGrants[0]); got != "anthropic-api-key" {
		t.Errorf("known host: secret_name = %q, want anthropic-api-key", got)
	}
	if got := secretOf(t, spec.EligibleGrants[1]); got != "my-custom-key" {
		t.Errorf("unknown host: secret_name = %q, want my-custom-key", got)
	}
	if got := secretOf(t, spec.EligibleGrants[2]); got != "my-anthropic-key" {
		t.Errorf("storable name: secret_name = %q, want untouched my-anthropic-key", got)
	}
	if got := secretOf(t, spec.EligibleGrants[3]); got != "***" {
		t.Errorf("unsanitizable name: secret_name = %q, want untouched ***", got)
	}
	if got := secretOf(t, spec.EligibleGrants[4]); got != "GL_PAT" {
		t.Errorf("git_pat grant: secret_name = %q, want untouched GL_PAT", got)
	}
	if len(warns) != 2 {
		t.Errorf("warns = %v, want exactly 2 normalization warnings", warns)
	}
}
