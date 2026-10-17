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

// The adopter's measured deployment: one Bedrock provider (an AWS credential, graded powerful), a
// substrate that offers CC1 alone. Every secrets row but secrets_powerful, and every barrier row but
// confinement_cc1, caps nothing there; the egress rows all can, because a run may always narrow its egress.
const adopterPosture = "on this deployment a run under this profile on one of its model providers grades " +
	"egress sealed, reviewed or open; secrets powerful; and confinement CC1 — so only egress_open, " +
	"egress_reviewed, egress_sealed, secrets_powerful and confinement_cc1 can cap it, and secrets_baseline, " +
	"secrets_none, confinement_cc2 and confinement_cc3 cap nothing here"

func TestRubricPostureSentence(t *testing.T) {
	bedrock := bedrockCredGrade{graded: true, host: "bedrock.llm.corp.example"}
	apiKey := bedrockCredGrade{graded: true} // a provider whose credential the secrets axis does not read
	cc1 := []types.ConfinementClass{types.CC1}
	for _, tc := range []struct {
		name string
		r    rubricReach
		want string
	}{
		{"the adopter's deployment", rubricReach{creds: []bedrockCredGrade{bedrock}, providers: true, offered: cc1}, adopterPosture},
		{"the model host declared baseline: its credential grades baseline, not powerful",
			rubricReach{baseline: composer.Baseline{Hosts: []string{"bedrock.llm.corp.example"}}, creds: []bedrockCredGrade{bedrock}, providers: true, offered: cc1},
			"on this deployment a run under this profile on one of its model providers grades egress sealed, reviewed or open; " +
				"secrets baseline or powerful; and confinement CC1 — so only egress_open, egress_reviewed, egress_sealed, secrets_powerful, " +
				"secrets_baseline and confinement_cc1 can cap it, and secrets_none, confinement_cc2 and confinement_cc3 cap nothing here"},
		{"a second provider whose credential is not graded: a run on it holds none",
			rubricReach{creds: []bedrockCredGrade{bedrock, apiKey}, providers: true, offered: cc1},
			"on this deployment a run under this profile on one of its model providers grades egress sealed, reviewed or open; " +
				"secrets none, baseline or powerful; and confinement CC1 — so only egress_open, egress_reviewed, egress_sealed, secrets_powerful, " +
				"secrets_baseline, secrets_none and confinement_cc1 can cap it, and confinement_cc2 and confinement_cc3 cap nothing here"},
		{"the profile's floor cuts the offered classes",
			rubricReach{offered: []types.ConfinementClass{types.CC1, types.CC2}, floor: types.CC2},
			"on this deployment a run under this profile grades egress sealed, reviewed or open; secrets none, baseline or powerful; " +
				"and confinement CC2 — so only egress_open, egress_reviewed, egress_sealed, secrets_powerful, secrets_baseline, secrets_none " +
				"and confinement_cc2 can cap it, and confinement_cc1 and confinement_cc3 cap nothing here"},
		{"classes unknown (runners on, or no executor answer): nothing is pinned",
			rubricReach{},
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
