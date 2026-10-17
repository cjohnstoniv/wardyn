// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestAutonomyOperatorBaselineFreesAPrivateModelHost is the adopter's measured case at both doors: a
// rubric whose secrets_powerful row is L1 and every other row L3, and a Bedrock provider reached at a
// private model host, with that host the run's only allowlisted one. Unset, the run grades egress open
// and secrets powerful and is bound by secrets_powerful at L1, exactly as before the setting existed.
// Declaring the allowlisted host clears egress open alone; declaring the model host too clears the
// secrets bind, and the run resolves L3. The Bedrock credential is not special-cased: it is an api_key to
// a host, graded against the same baseline every api_key is.
func TestAutonomyOperatorBaselineFreesAPrivateModelHost(t *testing.T) {
	const modelHost, allowed = "bedrock.llm.corp.example", "llm.corp.example"
	provider := bedrockKeyTestProvider()
	provider.Bedrock.BaseURL = "https://" + modelHost
	p := govProfile("private-model")
	p.Ceiling.AllowedDomains = []string{allowed}
	l3 := types.AutonomyL3
	p.Limits = types.GovernanceLimits{AutonomyRubric: &types.AutonomyRubric{
		EgressOpen: l3, EgressReviewed: l3, EgressSealed: l3,
		SecretsPowerful: types.AutonomyL1, SecretsBaseline: l3, SecretsNone: l3,
		ConfinementCC1: l3, ConfinementCC2: l3, ConfinementCC3: l3,
	}}
	const body = `{"agent":"claude-code","task":"t","confinement_class":"CC2",` +
		`"inline_policy":{"min_confinement_class":"CC2","allowed_domains":["` + allowed + `"]}}`

	for _, tc := range []struct {
		name        string
		egress      *types.SiteEgress
		wantEgress  types.AutonomyEgressPosture
		wantSecrets types.AutonomySecretsPosture
		wantLevel   types.AutonomyLevel
		wantBound   []string
	}{
		{"unset: graded exactly as before", nil,
			types.AutonomyEgressOpen, types.AutonomySecretsPowerful, types.AutonomyL1, []string{"secrets_powerful"}},
		{"the allowlisted host declared: egress is no longer open", &types.SiteEgress{BaselineHosts: []string{allowed}},
			types.AutonomyEgressSealed, types.AutonomySecretsPowerful, types.AutonomyL1, []string{"secrets_powerful"}},
		{"the model host declared too: no longer bound by secrets_powerful", &types.SiteEgress{BaselineHosts: []string{allowed, modelHost}},
			types.AutonomyEgressSealed, types.AutonomySecretsBaseline, types.AutonomyL3, []string{"egress_sealed", "secrets_baseline", "confinement_cc2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			door := func(path string) (map[string]any, *govEscapeStore, *recRecorder, int, string) {
				srv, st, audit := govEscapeFixture(t, autonomyCapStore(p))
				st.siteConfig = types.SiteConfig{ModelProviders: providerBlock(provider), Egress: tc.egress}
				srv.cfg.Now = func() time.Time { return awsSSOTestFixedNow }
				storeBedrockCredential(t, srv, bedrockLaneBearer, bedrockAutonomyMember)
				w := doSSO(t, srv, http.MethodPost, path, govSession(t, bedrockAutonomyMember, []string{"eng"}, false), body)
				var resp struct {
					Autonomy map[string]any `json:"autonomy"`
				}
				_ = json.Unmarshal(w.Body.Bytes(), &resp)
				return resp.Autonomy, st, audit, w.Code, w.Body.String()
			}
			review, _, _, code, raw := door("/api/v1/runs/preflight")
			if code != http.StatusOK {
				t.Fatalf("preflight = %d, want 200: %s", code, raw)
			}
			_, st, audit, code, raw := door("/api/v1/runs")
			if code != http.StatusCreated {
				t.Fatalf("create = %d, want 201: %s", code, raw)
			}
			launched, _ := autonomyCreateAudit(t, st, audit)["autonomy"].(map[string]any)
			if a, b := mustJSON(review), mustJSON(launched); string(a) != string(b) {
				t.Errorf("Review and launch disagree:\n  review = %s\n  launch = %s", a, b)
			}
			posture, _ := launched["posture"].(map[string]any)
			if got, _ := posture["egress"].(string); got != string(tc.wantEgress) {
				t.Errorf("posture.egress = %q, want %q", got, tc.wantEgress)
			}
			if got, _ := posture["secrets"].(string); got != string(tc.wantSecrets) {
				t.Errorf("posture.secrets = %q, want %q", got, tc.wantSecrets)
			}
			if got, _ := launched["level"].(string); got != string(tc.wantLevel) {
				t.Errorf("level = %q, want %q", got, tc.wantLevel)
			}
			if got := autonomyBoundBy(t, launched); !slices.Equal(got, tc.wantBound) {
				t.Errorf("bound_by = %v, want %v", got, tc.wantBound)
			}
		})
	}
}
