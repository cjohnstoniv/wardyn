// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

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

// TestGroundAPIKeySecretNames_StampsProviderHeaderWhenAbsent is
// W15-W15b-composer-pipeline-2: mapProposedGrant (schema.go) builds an
// api_key grant's scope from ONLY {host, secret_name} — a composer proposal's
// grant NEVER carries header/format. injectionRuleFromScope then defaults an
// empty header/format to "Authorization"/"Bearer %s" — correct for OpenAI,
// WRONG for Anthropic (x-api-key, bare key). Grounding must stamp the known
// provider's own header+format so the proxy injects what Anthropic's API
// actually accepts, not the generic Bearer default.
func TestGroundAPIKeySecretNames_StampsProviderHeaderWhenAbsent(t *testing.T) {
	headerFormatOf := func(t *testing.T, g types.GrantSpec) (header, format string) {
		t.Helper()
		var scope map[string]any
		if err := json.Unmarshal(g.Scope, &scope); err != nil {
			t.Fatalf("decode scope: %v", err)
		}
		h, _ := scope["header"].(string)
		f, _ := scope["format"].(string)
		return h, f
	}
	// Exactly the shape mapProposedGrant actually produces: no header/format.
	bareAPIKey := func(host, secret string) types.GrantSpec {
		return types.GrantSpec{Kind: types.GrantAPIKey, Scope: mustJSON(map[string]any{
			"host": host, "secret_name": secret})}
	}
	spec := &types.RunPolicySpec{
		EligibleGrants: []types.GrantSpec{
			bareAPIKey("api.anthropic.com", "anthropic-api-key"), // known LLM host, no header -> stamped
			bareAPIKey("api.openai.com", "openai-api-key"),       // known LLM host, no header -> stamped
			bareAPIKey("api.example.com", "custom-key"),          // unknown host -> left to the generic default
		},
	}
	warns := groundAPIKeySecretNames(spec)

	if h, f := headerFormatOf(t, spec.EligibleGrants[0]); h != "x-api-key" || f != "%s" {
		t.Errorf("anthropic grant header/format = %q/%q, want x-api-key/%%s (a Bearer default 401s against api.anthropic.com)", h, f)
	}
	if h, f := headerFormatOf(t, spec.EligibleGrants[1]); h != "Authorization" || f != "Bearer %s" {
		t.Errorf("openai grant header/format = %q/%q, want Authorization/Bearer %%s", h, f)
	}
	if h, f := headerFormatOf(t, spec.EligibleGrants[2]); h != "" || f != "" {
		t.Errorf("unknown-host grant header/format = %q/%q, want left empty (unknown provider, no canonical answer)", h, f)
	}
	foundWarn := false
	for _, w := range warns {
		if strings.Contains(w, "carried no header/format") {
			foundWarn = true
		}
	}
	if !foundWarn {
		t.Errorf("expected a header/format grounding warning, got %v", warns)
	}
}

// TestBrokeredRunWithholdsGitPATGrantEnv is the git_pat half of "brokered means
// single-lane", the sibling of TestBrokeredRunWithholdsSSHGrantEnv. It exists
// because git_pat used to be exempt from all three seams on the reasoning that
// such a grant was "already dead twice over" — one of those two deaths being
// wardyn-git-helper's in-sandbox refusal, which binds only a caller that asks
// GIT for the credential. An agent that POSTs the mint route never meets it, so
// the surviving barrier was a name-keyed egress deny that does not bind a raw-IP
// CONNECT under allow_all_egress. Withholding the grant id is what makes the
// credential absent rather than merely inconvenient.
//
// The drop is scoped, not a blanket: only a BROKERED forge's host, only on a run
// that is actually brokered, and the caller's map is never mutated.
func TestBrokeredRunWithholdsGitPATGrantEnv(t *testing.T) {
	run := types.AgentRun{ID: uuid.New()}
	brokered := map[string]uuid.UUID{"acme/widgets": uuid.New()}
	apply := func(patGrants map[string]string, gitGrants map[string]uuid.UUID) (map[string]string, []string) {
		env := map[string]string{}
		_, dropped := applyDispatchModeEnv(env, run, false, "", nil, patGrants, nil, gitGrants)
		return env, dropped
	}

	// Brokered run, git_pat for the brokered forge + one for another host: the
	// brokered host is withheld and REPORTED (the caller warns + audits on it);
	// the unrelated host's PAT still ships — that is the lane git_pat exists for.
	mixed := map[string]string{"github.com": uuid.NewString(), "dev.azure.com": uuid.NewString()}
	env, dropped := apply(mixed, brokered)
	if !slices.Equal(dropped, []string{"github.com"}) {
		t.Errorf("dropped = %v, want [github.com] — a silent drop leaves the operator with no explanation", dropped)
	}
	if got := env["WARDYN_GIT_PAT_GRANTS"]; strings.Contains(got, "github.com") || !strings.Contains(got, "dev.azure.com") {
		t.Errorf("WARDYN_GIT_PAT_GRANTS = %q, want the brokered forge withheld and dev.azure.com kept", got)
	}
	if len(mixed) != 2 {
		t.Errorf("the caller's git_pat grant map was mutated: %v", mixed)
	}

	// Brokered run whose ONLY git_pat is for a host the broker does NOT serve:
	// nothing is withheld and the map ships verbatim. The broker is github.com-only,
	// so ADO/GitLab have no brokered alternative — breaking this lane on a run that
	// merely also holds a github_token grant would be a pure regression.
	adoOnly := map[string]string{"dev.azure.com": uuid.NewString()}
	env, dropped = apply(adoOnly, brokered)
	if dropped != nil {
		t.Errorf("dropped = %v, want nil — dev.azure.com is not a brokered forge", dropped)
	}
	if got := env["WARDYN_GIT_PAT_GRANTS"]; got != string(mustJSON(adoOnly)) {
		t.Errorf("WARDYN_GIT_PAT_GRANTS = %q, want the map unchanged (%q)", got, mustJSON(adoOnly))
	}

	// Brokered run whose ONLY git_pat is a GitHub host (bare, subdomain, or an
	// odd spelling): the env var must be ABSENT, not an empty map. Two consumers
	// tell those apart — wardyn-git-helper's patGrantForHost branches on the key
	// existing, and agent-run's provision_git_helper_secret gates on
	// `[[ -n "${WARDYN_GIT_PAT_GRANTS:-}" ]]`, to which the string "{}" is
	// NON-empty. Emitting "{}" would provision a git-helper secret for a run with
	// no PAT lane left.
	for _, host := range []string{"github.com", "api.github.com", "GitHub.com."} {
		env, dropped := apply(map[string]string{host: uuid.NewString()}, brokered)
		if _, ok := env["WARDYN_GIT_PAT_GRANTS"]; ok {
			t.Errorf("%s: WARDYN_GIT_PAT_GRANTS = %q, want it unset entirely (not %q)", host, env["WARDYN_GIT_PAT_GRANTS"], "{}")
		}
		if !slices.Equal(dropped, []string{host}) {
			t.Errorf("%s: dropped = %v, want [%s]", host, dropped, host)
		}
	}

	// NOT brokered: the operator's git_pat is untouched, nothing to report. A
	// GitHub PAT on a run with no broker map is the documented un-brokered lane
	// (wardyn-git-helper mints it; no GitHub deny is injected).
	env, dropped = apply(map[string]string{"github.com": uuid.NewString()}, nil)
	if !strings.Contains(env["WARDYN_GIT_PAT_GRANTS"], "github.com") || dropped != nil {
		t.Errorf("non-brokered run lost its git_pat grant: env=%q dropped=%v", env["WARDYN_GIT_PAT_GRANTS"], dropped)
	}
}

// TestDispatch_BrokeredGitPATDropIsAuditedAndNeverReachesTheSandbox runs the
// git_pat drop through a REAL dispatch — the sibling of the ssh_key test of the
// same shape. It pins the two things the unit test above cannot: the env the
// runner is actually handed (the grant id must not be in the SandboxSpec at all
// — wardyn-git-helper reads it from there) and the audit event that makes the
// drop non-silent.
func TestDispatch_BrokeredGitPATDropIsAuditedAndNeverReachesTheSandbox(t *testing.T) {
	fr := &fakeRunner{}
	srv, _, audit, run := dispatchTeardownFixture(t, fr, types.RunPending)
	run.Task = "" // no agent exec / completion watcher; this test is about dispatch

	srv.dispatchRun(context.Background(), run, dispatchParams{
		RunToken: "run-token", Image: "wardyn/claude-code:latest",
		Policy:       types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"}, MinConfinementClass: types.CC1},
		GitGrants:    map[string]uuid.UUID{"acme/widgets": uuid.New()},
		GitPATGrants: map[string]string{"github.com": uuid.NewString()},
	})

	if v, ok := fr.lastSpec.Env["WARDYN_GIT_PAT_GRANTS"]; ok {
		t.Errorf("the sandbox was handed WARDYN_GIT_PAT_GRANTS=%q on a brokered run — the agent can mint a resident GitHub PAT", v)
	}
	ev := findAudit(audit.events, run.ID, "run.git_pat.brokered_forge", "failure")
	if ev == nil {
		t.Fatalf("the git_pat drop was SILENT: no run.git_pat.brokered_forge event; events=%s", auditDump(audit.events, run.ID))
	}
	if !strings.Contains(string(ev.Data), "github.com") {
		t.Errorf("audit event does not name the dropped host: %s", ev.Data)
	}
}

// TestInternalMint_BrokeredForgeRefusesGitPAT is the MINT-time half for git_pat
// — and the one that matters most, because it is the seam the old "dead twice
// over" reasoning missed. The proxy's own mint refusal (isBrokeredGitGrant)
// iterates the github_token grant ids only, so a POST naming the git_pat grant
// was forwarded and answered WITH THE PAT. The refusal must land BEFORE the
// broker: that is what proves no transaction opened and no approval-gated
// grant's single-use slot was consumed.
func TestInternalMint_BrokeredForgeRefusesGitPAT(t *testing.T) {
	patGrant := func(runID, id uuid.UUID, host string) types.CredentialGrant {
		return types.CredentialGrant{ID: id, RunID: runID, Spec: types.GrantSpec{
			Kind:  types.GrantGitPAT,
			Scope: mustJSON(map[string]any{"host": host, "secret_name": "gh-pat"}),
		}}
	}
	ghGrant := func(runID uuid.UUID) types.CredentialGrant {
		return types.CredentialGrant{ID: uuid.New(), RunID: runID, Spec: types.GrantSpec{
			Kind:  types.GrantGitHubToken,
			Scope: mustJSON(map[string]any{"repos": []string{"acme/widgets"}}),
		}}
	}

	cases := []struct {
		name     string
		host     string
		withGH   bool
		noStore  bool
		wantCode int
	}{
		{"github git_pat co-granted with a github_token is refused", "github.com", true, false, http.StatusForbidden},
		{"a github SUBDOMAIN git_pat is refused too", "api.github.com", true, false, http.StatusForbidden},
		{"a git_pat for another host is untouched by a github_token", "dev.azure.com", true, false, http.StatusOK},
		{"a github git_pat with no github_token is the unbrokered lane", "github.com", false, false, http.StatusOK},
		{"no Store fails OPEN — defence in depth, not a gate", "github.com", true, true, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			runID, patID := uuid.New(), uuid.New()
			grants := []types.CredentialGrant{patGrant(runID, patID, tc.host)}
			if tc.withGH {
				grants = append(grants, ghGrant(runID))
			}
			if !tc.noStore {
				h.srv.cfg.Store = grantsStore{grants: grants}
			}
			tok := h.mintRunToken(t, runID)
			w := do(t, h.srv, http.MethodPost, "/api/v1/internal/credentials/mint", tok,
				`{"grant_id":"`+patID.String()+`"}`)
			if w.Code != tc.wantCode {
				t.Fatalf("mint code = %d, want %d; body=%s", w.Code, tc.wantCode, w.Body.String())
			}
			if tc.wantCode != http.StatusForbidden {
				if h.broker.lastCall == nil {
					t.Fatalf("the mint never reached the broker — the guard over-refused")
				}
				return
			}
			if h.broker.lastCall != nil {
				t.Errorf("a REFUSED mint still called the broker: an approval-gated grant's single-use slot could have been burned")
			}
			if !strings.Contains(w.Body.String(), "single-lane") {
				t.Errorf("refusal must name the rule, got: %s", w.Body.String())
			}
		})
	}
}
