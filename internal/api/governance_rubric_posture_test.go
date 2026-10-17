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

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The adopter's measured deployment: one Bedrock provider serving Claude Code (an AWS credential,
// graded powerful), a substrate that offers CC1 alone. A run on Codex CLI or Your own tools resolves
// no provider and carries no credential, so every secrets row can still bind; what the provider forces
// is said on its own, for runs on it.
const adopterPosture = "on this deployment a run under this profile grades egress sealed, reviewed or open; " +
	"secrets none, baseline or powerful; and confinement CC1 — so only egress_open, egress_reviewed, egress_sealed, " +
	"secrets_powerful, secrets_baseline, secrets_none and confinement_cc1 can cap it, and confinement_cc2 and " +
	"confinement_cc3 cap nothing here; a run on one of its model providers always grades secrets powerful"

func TestRubricPostureSentence(t *testing.T) {
	bedrock := bedrockCredGrade{graded: true, host: "bedrock.llm.corp.example"}
	none := bedrockCredGrade{graded: true} // a run on no provider, or on one whose credential is not graded
	cc1 := []types.ConfinementClass{types.CC1}
	for _, tc := range []struct {
		name string
		r    rubricReach
		want string
	}{
		{"the adopter's deployment: a no-provider harness can launch, so secrets_none binds",
			rubricReach{launch: []bedrockCredGrade{none, none, bedrock}, provider: []bedrockCredGrade{bedrock}, offered: cc1}, adopterPosture},
		{"the model host declared baseline: a run on the provider grades baseline, not powerful",
			rubricReach{baseline: composer.Baseline{Hosts: []string{"bedrock.llm.corp.example"}},
				launch: []bedrockCredGrade{none, bedrock}, provider: []bedrockCredGrade{bedrock}, offered: cc1},
			"on this deployment a run under this profile grades egress sealed, reviewed or open; secrets none, baseline or powerful; " +
				"and confinement CC1 — so only egress_open, egress_reviewed, egress_sealed, secrets_powerful, secrets_baseline, " +
				"secrets_none and confinement_cc1 can cap it, and confinement_cc2 and confinement_cc3 cap nothing here; " +
				"a run on one of its model providers always grades secrets baseline or powerful"},
		// Not reachable on today's catalog, which always offers Your own tools; the arithmetic is still
		// what a deployment whose every launchable run sits on a Bedrock provider would get.
		{"every launchable run on a Bedrock provider: secrets_none and secrets_baseline are inert",
			rubricReach{launch: []bedrockCredGrade{bedrock}, provider: []bedrockCredGrade{bedrock}, offered: cc1},
			"on this deployment a run under this profile grades egress sealed, reviewed or open; secrets powerful; " +
				"and confinement CC1 — so only egress_open, egress_reviewed, egress_sealed, secrets_powerful and confinement_cc1 " +
				"can cap it, and secrets_baseline, secrets_none, confinement_cc2 and confinement_cc3 cap nothing here"},
		{"a second provider whose credential is not graded: nothing forced on provider runs either",
			rubricReach{launch: []bedrockCredGrade{none, bedrock, none}, provider: []bedrockCredGrade{bedrock, none}, offered: cc1},
			"on this deployment a run under this profile grades egress sealed, reviewed or open; secrets none, baseline or powerful; " +
				"and confinement CC1 — so only egress_open, egress_reviewed, egress_sealed, secrets_powerful, secrets_baseline, " +
				"secrets_none and confinement_cc1 can cap it, and confinement_cc2 and confinement_cc3 cap nothing here"},
		{"the profile's floor cuts the offered classes",
			rubricReach{launch: []bedrockCredGrade{none}, offered: []types.ConfinementClass{types.CC1, types.CC2}, floor: types.CC2},
			"on this deployment a run under this profile grades egress sealed, reviewed or open; secrets none, baseline or powerful; " +
				"and confinement CC2 — so only egress_open, egress_reviewed, egress_sealed, secrets_powerful, secrets_baseline, secrets_none " +
				"and confinement_cc2 can cap it, and confinement_cc1 and confinement_cc3 cap nothing here"},
		{"classes unknown (runners on, or no executor answer): nothing is pinned",
			rubricReach{launch: []bedrockCredGrade{none}},
			"on this deployment a run under this profile grades egress sealed, reviewed or open; secrets none, baseline or powerful; " +
				"and confinement CC1, CC2 or CC3 — so every rubric row can cap it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := rubricPostureSentence(tc.r); got != tc.want {
				t.Errorf("sentence:\n got  %q\n want %q", got, tc.want)
			}
		})
	}
}

// Every catalog harness is a launchable run shape: one a provider serves folds that provider's
// credential, and one no provider serves (Codex CLI here) or with no model gateway (Your own tools)
// folds none.
func TestLaunchModelCredentialsCountEveryHarness(t *testing.T) {
	provider := bedrockKeyTestProvider() // serves claude-code only
	launch, onProvider := launchModelCredentials(types.SiteConfig{ModelProviders: providerBlock(provider)})
	host := providerBedrockRuntimeHost(provider)
	hosts := func(gs []bedrockCredGrade) []string {
		var out []string
		for _, g := range gs {
			out = append(out, g.host)
		}
		slices.Sort(out)
		return out
	}
	if got, want := hosts(launch), []string{"", "", host}; !slices.Equal(got, want) {
		t.Errorf("launch credential hosts = %q, want %q (codex-cli and none fold no credential)", got, want)
	}
	if got := hosts(onProvider); !slices.Equal(got, []string{host}) {
		t.Errorf("provider credential hosts = %q, want only %q", got, host)
	}
	if launch, onProvider := launchModelCredentials(types.SiteConfig{}); len(onProvider) != 0 || len(launch) != len(harnessCatalog) ||
		slices.ContainsFunc(launch, func(g bedrockCredGrade) bool { return g.host != "" }) {
		t.Errorf("no providers: launch = %+v provider = %+v, want one empty grade per harness", launch, onProvider)
	}
}

// rubricSiteStore is the fake governance store with a site config, which the advisory reads.
type rubricSiteStore struct {
	*govCrudStore
	site types.SiteConfig
}

func (s *rubricSiteStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return s.site, nil
}

// The write response carries the sentence beside the omission warnings, for a profile with a rubric
// row and only then; self-hosted runners on make the barrier classes unknowable, so nothing is pinned.
func TestGovernanceProfileWriteAdvisesTheRubricPosture(t *testing.T) {
	provider := bedrockKeyTestProvider()
	provider.Bedrock.BaseURL = "https://bedrock.llm.corp.example"
	write := func(t *testing.T, site types.SiteConfig, body string) []string {
		t.Helper()
		h := newHarness(t)
		cfg := baseTestConfig(h, &rubricSiteStore{govCrudStore: &govCrudStore{}, site: site})
		cfg.Runner = &fakeRunner{capsClasses: []types.ConfinementClass{types.CC1}}
		w := do(t, New(cfg), http.MethodPost, "/api/v1/governance/profiles", adminToken, body)
		if w.Code != http.StatusCreated {
			t.Fatalf("POST = %d %s, want 201", w.Code, w.Body.String())
		}
		var resp governanceProfileResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		return resp.Warnings
	}
	const rubric = `{"name":"r","ceiling":{"min_confinement_class":"CC1"},"limits":{"autonomy_rubric":{` +
		`"egress_open":"L3","egress_reviewed":"L3","egress_sealed":"L3","secrets_powerful":"L1","secrets_baseline":"L3",` +
		`"secrets_none":"L3","confinement_cc1":"L3"}}}`
	site := types.SiteConfig{ModelProviders: providerBlock(provider)}
	if got := write(t, site, rubric); !slices.Contains(got, adopterPosture) {
		t.Errorf("warnings = %q, want the posture sentence %q", got, adopterPosture)
	}
	posture := func(ws []string) bool {
		return slices.ContainsFunc(ws, func(s string) bool { return strings.HasPrefix(s, "on this deployment") })
	}
	if got := write(t, site, `{"name":"r","ceiling":{"min_confinement_class":"CC1"}}`); posture(got) {
		t.Errorf("no rubric, yet warnings = %q", got)
	}
	runners := site
	runners.Runners = &types.RunnerSettings{Enabled: true}
	got := write(t, runners, rubric)
	if !slices.ContainsFunc(got, func(s string) bool { return strings.Contains(s, "and confinement CC1, CC2 or CC3 —") }) {
		t.Errorf("runners on: warnings = %q, want every barrier class reachable", got)
	}
}
