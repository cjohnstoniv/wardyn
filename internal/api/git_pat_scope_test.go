// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const patTestHost = "git.example.com"

func patGrantSpec(fields string) types.GrantSpec {
	scope := `{"host":"` + patTestHost + `","secret_name":"pat"`
	if fields != "" {
		scope += "," + fields
	}
	return types.GrantSpec{Kind: types.GrantGitPAT, Scope: json.RawMessage(scope + "}")}
}

// Each widening of a git_pat scope is refused at governance write, narrowed or
// dropped by Clamp at create and preflight, and removed at resolve. The three
// seams are asked the same question about the same pair, so a widening one of
// them lets through shows up as a disagreement here.
func TestGitPATWideningIsCaughtAtEverySeam(t *testing.T) {
	narrowed := `"repos":["team/app"],"access":"read"`
	for _, tc := range []struct {
		name     string
		ceiling  string
		proposal string
		axis     string // named in the write refusal
	}{
		{"repository substitution", narrowed, `"repos":["team/other"],"access":"read"`, "repos entry"},
		{"read to write", narrowed, `"repos":["team/app"],"access":"write"`, "access"},
		{"api false to true", `"repos":["team/app"],"forge":"gitlab"`, `"repos":["team/app"],"forge":"gitlab","api":true`, "api"},
		{"repos omitted against a narrowed ceiling", narrowed, `"access":"read"`, "repos is omitted"},
		{"access omitted against a read ceiling", narrowed, `"repos":["team/app"]`, "access"},
		{"forge change against a repos ceiling", `"repos":["team/app"],"forge":"gitlab"`, `"repos":["team/app"],"forge":"gitea"`, "forge"},
		{"forge change against an api ceiling", `"api":true,"forge":"gitlab"`, `"forge":"gitea"`, "forge"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ceiling := []types.GrantSpec{patGrantSpec(tc.ceiling)}
			proposal := patGrantSpec(tc.proposal)

			// write
			err := governanceGrantsWithinCeiling([]types.GrantSpec{proposal}, ceiling)
			if err == nil || !strings.Contains(err.Error(), tc.axis) {
				t.Fatalf("governance write = %v, want a refusal naming %q", err, tc.axis)
			}

			// create and preflight: narrowed or dropped, and what is left is inside the ceiling
			clamped, _ := composer.Clamp(types.RunPolicySpec{EligibleGrants: []types.GrantSpec{proposal}},
				types.RunPolicySpec{EligibleGrants: ceiling}, types.GovernanceLimits{})
			for _, g := range clamped.EligibleGrants {
				if cerr := governanceGrantsWithinCeiling([]types.GrantSpec{g}, ceiling); cerr != nil {
					t.Errorf("Clamp kept %s, which the write bound refuses: %v", g.Scope, cerr)
				}
			}

			// resolve
			kept, warns := reintersectGovernanceGrants([]types.GrantSpec{proposal}, ceiling, "p")
			if len(kept) != 0 || len(warns) != 1 {
				t.Errorf("resolve kept %d grants with warnings %q, want the grant removed", len(kept), warns)
			}
		})
	}
}

// A ceiling grant with no repos and no forge dominates a narrower proposal on a
// non-generic forge: every ceiling written before 0.8.6 has no forge key, and this
// is what lets a proposal narrow below it. Clamp keeps the proposal unchanged.
func TestGitPATForgeIsFreeUnderAnUnnarrowedCeiling(t *testing.T) {
	ceiling := []types.GrantSpec{patGrantSpec("")}
	proposal := patGrantSpec(`"forge":"gitlab","repos":["team/app"],"access":"read"`)
	if err := governanceGrantsWithinCeiling([]types.GrantSpec{proposal}, ceiling); err != nil {
		t.Fatalf("write refused it: %v", err)
	}
	clamped, warns := composer.Clamp(types.RunPolicySpec{EligibleGrants: []types.GrantSpec{proposal}},
		types.RunPolicySpec{EligibleGrants: ceiling}, types.GovernanceLimits{})
	if len(clamped.EligibleGrants) != 1 || string(clamped.EligibleGrants[0].Scope) != string(proposal.Scope) || len(warns) != 0 {
		t.Fatalf("Clamp changed it: %v (warnings %q)", clamped.EligibleGrants, warns)
	}
}

// With several same-host ceiling grants, a proposal that needs two of them to
// dominate is refused, whichever order they are listed in.
func TestGitPATGovernanceNeedsOneDominatingCeilingGrant(t *testing.T) {
	wideRepos := patGrantSpec(`"access":"read"`)
	wideAccess := patGrantSpec(`"repos":["team/app"],"access":"write"`)
	proposal := patGrantSpec(`"access":"write"`)
	for name, ceiling := range map[string][]types.GrantSpec{
		"repos grant first":  {wideRepos, wideAccess},
		"access grant first": {wideAccess, wideRepos},
	} {
		if err := governanceGrantsWithinCeiling([]types.GrantSpec{proposal}, ceiling); err == nil {
			t.Errorf("%s: a proposal wide on both axes was accepted against two grants that are each wide on one", name)
		}
	}
	// One grant that is wide on both does dominate it.
	if err := governanceGrantsWithinCeiling([]types.GrantSpec{proposal}, []types.GrantSpec{wideRepos, patGrantSpec("")}); err != nil {
		t.Errorf("a single dominating grant was refused: %v", err)
	}
}

// The write-time refusals, through the policy route. None reaches the store.
func TestCreatePolicyRefusesGitPATScopes(t *testing.T) {
	h := newHarness(t)
	policy := func(grants ...string) string {
		return `{"name":"p","spec":{"min_confinement_class":"CC2","eligible_grants":[` + strings.Join(grants, ",") + `]}}`
	}
	pat := func(host, fields string) string {
		if fields != "" {
			fields = "," + fields
		}
		return `{"kind":"git_pat","scope":{"host":"` + host + `","secret_name":"pat"` + fields + `}}`
	}
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"unknown scope key", policy(pat(patTestHost, `"repo":["a/b"]`)), `unknown field \"repo\"`},
		{"api true", policy(pat(patTestHost, `"api":true,"forge":"gitlab"`)), "not yet available"},
		{"api true on generic", policy(pat(patTestHost, `"api":true`)), "generic"},
		{"access out of enum", policy(pat(patTestHost, `"access":"admin"`)), "access"},
		{"forge out of enum", policy(pat(patTestHost, `"forge":"svn"`)), "forge"},
		{"malformed repos entry", policy(pat(patTestHost, `"repos":["a/../b"]`)), "malformed"},
		{"narrowed repos on dev.azure.com", policy(pat("dev.azure.com", `"repos":["a/b"]`)), "Azure DevOps"},
		{"read on dev.azure.com", policy(pat("dev.azure.com", `"access":"read"`)), "Azure DevOps"},
		{"forge on dev.azure.com", policy(pat("dev.azure.com", `"forge":"gitlab"`)), "Azure DevOps"},
		{"narrowed duplicate, narrowed second", policy(pat(patTestHost, ""), pat(patTestHost, `"repos":["a/b"]`)), "two git_pat grants"},
		{"narrowed duplicate, narrowed first", policy(pat(patTestHost, `"access":"read"`), pat(patTestHost, "")), "two git_pat grants"},
	} {
		w := do(t, h.srv, http.MethodPost, "/api/v1/policies", adminToken, tc.body)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), tc.want) {
			t.Errorf("%s: code = %d body=%s, want 400 containing %q", tc.name, w.Code, w.Body.String(), tc.want)
		}
	}
}

func TestPolicyValidationAcceptsNarrowedGitPAT(t *testing.T) {
	for name, spec := range map[string]types.RunPolicySpec{
		"every axis": {EligibleGrants: []types.GrantSpec{patGrantSpec(`"repos":["team/app","g/*"],"access":"read","forge":"gitlab"`)}},
		// Unnarrowed duplicates keep today's behaviour.
		"unnarrowed duplicates": {EligibleGrants: []types.GrantSpec{patGrantSpec(""), patGrantSpec(`"access":"write"`)}},
		// Different hosts are different grants.
		"narrowed grants on two hosts": {EligibleGrants: []types.GrantSpec{
			patGrantSpec(`"repos":["a/b"]`),
			{Kind: types.GrantGitPAT, Scope: json.RawMessage(`{"host":"other.example.com","secret_name":"pat","repos":["a/b"]}`)},
		}},
	} {
		spec.MinConfinementClass = types.CC2
		if err := validatePolicySpec(spec); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// The boot policy file is strict, and names the stray key. Record-mode
// synthesis reads stored grant specs leniently.
func TestGitPATStrictAtBootAndLenientAtSynthesis(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	body := `{"min_confinement_class":"CC2","eligible_grants":[{"kind":"git_pat","scope":{"host":"` + patTestHost +
		`","secret_name":"pat","repo":["a/b"]}}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPolicySpec(path); err == nil || !strings.Contains(err.Error(), `"repo"`) {
		t.Fatalf("LoadPolicySpec = %v, want an error naming the stray key", err)
	}

	stored := types.RunPolicySpec{
		MinConfinementClass: types.CC2,
		EligibleGrants: []types.GrantSpec{{Kind: types.GrantGitPAT,
			Scope: json.RawMessage(`{"host":"` + patTestHost + `","secret_name":"pat","repo":["a/b"]}`)}},
	}
	if err := validatePolicySpecLenient(stored); err != nil {
		t.Fatalf("synthesis refused a stored grant with a stray key: %v", err)
	}
	if err := validatePolicySpec(stored); err == nil {
		t.Fatal("the write path accepted the same stray key")
	}
}

// The create-time half of the same-host rule, for a spec no write-time check has
// seen (a stored row), and the other half of the lenient-read rule: a stored row
// with an unknown scope key still loads and launches.
func TestCreateRunGitPATStoredPolicies(t *testing.T) {
	srv, st, _ := govEscapeFixture(t, &capStore{})
	srv.cfg.Secrets = &memSecrets{m: map[string][]byte{"pat": []byte("v")}}
	store := func(grants ...types.GrantSpec) string {
		id := uuid.New()
		st.policies[id] = types.RunPolicy{ID: id, Name: id.String(), Spec: types.RunPolicySpec{
			MinConfinementClass: types.CC1, EligibleGrants: grants}}
		return `{"agent":"claude-code","task":"t","policy_id":"` + id.String() + `"}`
	}
	launch := func(path, body string) *httptest.ResponseRecorder {
		return do(t, srv, http.MethodPost, path, adminToken, body)
	}

	dup := store(patGrantSpec(""), patGrantSpec(`"repos":["a/b"]`))
	for _, path := range []string{"/api/v1/runs", "/api/v1/runs/preflight"} {
		w := launch(path, dup)
		if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "two git_pat grants") {
			t.Errorf("%s with narrowed same-host duplicates: code = %d body=%s, want 422", path, w.Code, w.Body.String())
		}
	}

	unnarrowed := store(patGrantSpec(""), patGrantSpec(""))
	if w := launch("/api/v1/runs", unnarrowed); w.Code != http.StatusCreated {
		t.Errorf("unnarrowed duplicates: code = %d body=%s, want 201", w.Code, w.Body.String())
	}

	stray := store(types.GrantSpec{Kind: types.GrantGitPAT,
		Scope: json.RawMessage(`{"host":"` + patTestHost + `","secret_name":"pat","repo":["a/b"]}`)})
	if w := launch("/api/v1/runs", stray); w.Code != http.StatusCreated {
		t.Errorf("stored row with an unknown scope key: code = %d body=%s, want 201", w.Code, w.Body.String())
	}
}

func TestCreateRunGitPATInlineDuplicates(t *testing.T) {
	srv, _, _ := govEscapeFixture(t, &capStore{})
	srv.cfg.Secrets = &memSecrets{m: map[string][]byte{"pat": []byte("v")}}
	body := `{"agent":"claude-code","task":"t","inline_policy":{"min_confinement_class":"CC1","eligible_grants":[` +
		string(mustJSON(patGrantSpec(`"access":"read"`))) + `,` + string(mustJSON(patGrantSpec(""))) + `]}}`
	w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, body)
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "two git_pat grants") {
		t.Fatalf("code = %d body=%s, want 422", w.Code, w.Body.String())
	}
}
