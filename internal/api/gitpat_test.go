// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
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
