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
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ─── the Amazon Bedrock model credential (#504) ───────────────────────────────

// bedrockLane is the Amazon Bedrock model provider a fixture's site offers:
// a captured AWS SSO session or a Bedrock API key, each the member's own. The
// empty lane is a site with no model provider at all.
type bedrockLane string

const (
	bedrockLaneNone   bedrockLane = ""
	bedrockLaneSSO    bedrockLane = "sso"
	bedrockLaneBearer bedrockLane = "bearer"
)

// bedrockAutonomyMember is the member every run here is launched by.
const bedrockAutonomyMember = "sub-bedrock"

// bedrockKeyTestProvider is the Bedrock API key provider the bearer lane offers.
func bedrockKeyTestProvider() types.ModelProvider {
	return types.ModelProvider{ID: "bedrock-key", UID: "u-bedrock-key-test", Kind: types.ModelProviderBedrockBearer,
		Bedrock:   &types.BedrockSettings{Region: "us-east-1"},
		Harnesses: []types.ProviderHarness{{Harness: "claude-code", Model: "us.anthropic.claude-sonnet-4-5-20250929-v1:0"}}}
}

// storeBedrockCredential writes owner's OWN credential for the lane's provider
// into owner's secret namespace — the only one a provider run reads, so a
// credential stored under anyone else is invisible to the run.
func storeBedrockCredential(t *testing.T, s *Server, lane bedrockLane, owner string) {
	t.Helper()
	switch lane {
	case bedrockLaneSSO:
		p := awsSSOTestProvider()
		b := providerBedrockSettings(p)
		blob := awsSSOBlob{
			AccessToken: "sso-access-token-" + owner, RefreshToken: "sso-refresh-token-" + owner,
			ClientID: "sso-client-id", ClientSecret: "sso-client-secret-1234567890",
			StartURL: b.SSOStartURL, Region: b.Region, AccountID: b.SSOAccountID, RoleName: b.SSORoleName,
			ExpiresAt: awsSSOTestFixedNow.Add(time.Hour), CapturedAt: awsSSOTestFixedNow.Add(-time.Hour),
		}
		if err := s.storeAWSSSOBlob(context.Background(), chosenProvider{provider: p, owner: owner}.awsScope(), blob); err != nil {
			t.Fatalf("store %q's SSO session: %v", owner, err)
		}
	case bedrockLaneBearer:
		name := providerSecretName(bedrockKeyTestProvider().UID, providerKeyPart)
		if err := s.cfg.Secrets.For(owner).Put(context.Background(), name, []byte("bedrock-bearer-key")); err != nil {
			t.Fatalf("store %q's Bedrock key: %v", owner, err)
		}
	}
}

// bedrockAutonomyFixture is govEscapeFixture whose site offers the lane's
// Bedrock provider, with credOwner's own credential for it stored when set.
// The clock is the SSO tests' fixed one, so a captured session is live and the
// create-time refresh is a no-op on the wire.
func bedrockAutonomyFixture(t *testing.T, p *types.GovernanceProfile, lane bedrockLane, credOwner string, proxyInject bool,
) (*Server, *govEscapeStore, *recRecorder) {
	t.Helper()
	srv, st, audit := govEscapeFixture(t, autonomyCapStore(p))
	switch lane {
	case bedrockLaneSSO:
		st.siteConfig = types.SiteConfig{ModelProviders: providerBlock(awsSSOTestProvider())}
	case bedrockLaneBearer:
		st.siteConfig = types.SiteConfig{ModelProviders: providerBlock(bedrockKeyTestProvider())}
	}
	srv.cfg.Now = func() time.Time { return awsSSOTestFixedNow }
	srv.cfg.AWSSSOProxyInject = proxyInject
	if credOwner != "" {
		storeBedrockCredential(t, srv, lane, credOwner)
	}
	return srv, st, audit
}

// secretsOnlyRubric caps the secrets rows alone, so the level IS the secrets
// grade and a miss on that axis shows up as a wrong LEVEL.
func secretsOnlyRubric(powerful types.AutonomyLevel) *types.AutonomyRubric {
	return &types.AutonomyRubric{
		SecretsNone: types.AutonomyL3, SecretsBaseline: types.AutonomyL3, SecretsPowerful: powerful,
	}
}

const bedrockAutonomyBody = `{"agent":"claude-code","task":"t","confinement_class":"CC2",` +
	`"inline_policy":{"min_confinement_class":"CC2","allowed_domains":["api.anthropic.com"]}}`

// TestAutonomyGradesTheBedrockModelCredentialAtBothDoors is the finding's own
// probe (#504): a run whose model credential is an AWS credential for Amazon
// Bedrock, graded at Review and at launch.
//
// Both doors agreed before the fix — on `secrets=none` — which is why the
// parity assertion alone cannot catch it, and why every row also asserts the
// posture and the level.
//
// The two SSO rows are the residency question: proxy-injected (the sandbox
// holds a placeholder) and sandbox-resident (it holds the session) grade the
// SAME, because the rubric has no residency input and an api_key — always
// proxy-injected — to a non-baseline host is already powerful.
//
// A site with no model provider grades exactly what it did before this fold
// existed. A credential stored under a DIFFERENT member is invisible to the
// run's owner, so the run gets no Bedrock credential at either door and is
// refused (422), not merely graded `secrets=none`.
func TestAutonomyGradesTheBedrockModelCredentialAtBothDoors(t *testing.T) {
	member := func(t *testing.T) *http.Cookie { return govSession(t, bedrockAutonomyMember, []string{"eng"}, false) }
	for _, tc := range []struct {
		name        string
		lane        bedrockLane
		proxyInject bool
		credOwner   string
		wantRefused bool
		wantSecrets types.AutonomySecretsPosture
		wantLevel   types.AutonomyLevel
	}{
		{"captured AWS SSO session, proxy-injected", bedrockLaneSSO, true, bedrockAutonomyMember, false, types.AutonomySecretsPowerful, types.AutonomyL1},
		{"captured AWS SSO session, resident in the sandbox", bedrockLaneSSO, false, bedrockAutonomyMember, false, types.AutonomySecretsPowerful, types.AutonomyL1},
		{"Bedrock API key", bedrockLaneBearer, true, bedrockAutonomyMember, false, types.AutonomySecretsPowerful, types.AutonomyL1},
		{"no model provider at all", bedrockLaneNone, true, "", false, types.AutonomySecretsNone, types.AutonomyL3},
		{"another member's capture is invisible", bedrockLaneSSO, true, "someone-elses-sub", true, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := govProfile("bedrock")
			p.Limits = types.GovernanceLimits{AutonomyRubric: secretsOnlyRubric(types.AutonomyL1)}

			srv, _, _ := bedrockAutonomyFixture(t, p, tc.lane, tc.credOwner, tc.proxyInject)
			w := doSSO(t, srv, http.MethodPost, "/api/v1/runs/preflight", member(t), bedrockAutonomyBody)

			srv2, st2, audit2 := bedrockAutonomyFixture(t, p, tc.lane, tc.credOwner, tc.proxyInject)
			c := doSSO(t, srv2, http.MethodPost, "/api/v1/runs", member(t), bedrockAutonomyBody)

			if tc.wantRefused {
				// The provider's not-signed-in refusal, not merely a 422: any
				// other 422 would pass a status check without the owner-scoped
				// read ever being exercised.
				for door, rec := range map[string]*httptest.ResponseRecorder{"preflight": w, "create": c} {
					if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), mpBRNotSignedIn) {
						t.Errorf("%s = %d %s, want 422 naming the member's own AWS sign-in", door, rec.Code, rec.Body.String())
					}
				}
				return
			}
			if w.Code != http.StatusOK {
				t.Fatalf("preflight = %d, want 200: %s", w.Code, w.Body.String())
			}
			var resp struct {
				Autonomy map[string]any `json:"autonomy"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode preflight: %v", err)
			}

			if c.Code != http.StatusCreated {
				t.Fatalf("create = %d, want 201: %s", c.Code, c.Body.String())
			}
			launched, _ := autonomyCreateAudit(t, st2, audit2)["autonomy"].(map[string]any)

			review, _ := json.Marshal(resp.Autonomy)
			audited, _ := json.Marshal(launched)
			if string(review) != string(audited) {
				t.Errorf("Review and launch disagree:\n  review = %s\n  launch = %s", review, audited)
			}
			posture, _ := launched["posture"].(map[string]any)
			if got, _ := posture["secrets"].(string); got != string(tc.wantSecrets) {
				t.Errorf("posture.secrets = %q, want %q", got, tc.wantSecrets)
			}
			if got, _ := launched["level"].(string); got != string(tc.wantLevel) {
				t.Errorf("level = %q, want %q", got, tc.wantLevel)
			}

			// The derived-hold warning names the credential when it is what
			// graded powerful: the request declared no secret at all.
			var created struct {
				Warnings []string `json:"warnings"`
			}
			if err := json.Unmarshal(c.Body.Bytes(), &created); err != nil {
				t.Fatalf("decode create: %v", err)
			}
			named := slices.ContainsFunc(created.Warnings, func(s string) bool { return strings.Contains(s, "Amazon Bedrock") })
			if named != (tc.wantSecrets == types.AutonomySecretsPowerful) {
				t.Errorf("a 201 warning names Amazon Bedrock = %v, want %v: %q",
					named, tc.wantSecrets == types.AutonomySecretsPowerful, created.Warnings)
			}
		})
	}
}

// TestAutonomyBedrockCredentialCapsLikeAnyPowerfulSecret: a rubric capping
// secrets_powerful below the rung a run would otherwise get refuses and derives
// for the Bedrock credential EXACTLY as it does for a powerful secret the
// request carries itself (a git_pat): the same status, the same refused field,
// the same derived tool_approvals.
func TestAutonomyBedrockCredentialCapsLikeAnyPowerfulSecret(t *testing.T) {
	member := func(t *testing.T) *http.Cookie { return govSession(t, bedrockAutonomyMember, []string{"eng"}, false) }
	pat := types.GrantSpec{Kind: types.GrantGitPAT, Scope: mustJSON(map[string]any{"host": "dev.azure.com", "secret_name": govCorpSecret})}
	patBody := `{"agent":"claude-code","task":"t","confinement_class":"CC2","inline_policy":{"min_confinement_class":"CC2",` +
		`"allowed_domains":["api.anthropic.com"],"eligible_grants":[` + string(mustJSON(pat)) + `]}}`

	type outcome struct {
		code          int
		refused       string
		toolApprovals string
	}
	launch := func(t *testing.T, srv *Server, st *govEscapeStore, audit *recRecorder, body string) outcome {
		t.Helper()
		w := doSSO(t, srv, http.MethodPost, "/api/v1/runs", member(t), body)
		o := outcome{code: w.Code, refused: lastAuthzDenied(audit.snapshot())}
		if w.Code == http.StatusCreated {
			o.toolApprovals, _ = autonomyCreateAudit(t, st, audit)["tool_approvals"].(string)
		}
		return o
	}

	for _, powerful := range []types.AutonomyLevel{types.AutonomyL0, types.AutonomyL1, types.AutonomyL2} {
		t.Run(string(powerful), func(t *testing.T) {
			p := govProfile("bedrock-cap")
			p.Limits = types.GovernanceLimits{AutonomyRubric: secretsOnlyRubric(powerful)}

			srv, st, audit := bedrockAutonomyFixture(t, p, bedrockLaneSSO, bedrockAutonomyMember, true)
			bedrock := launch(t, srv, st, audit, bedrockAutonomyBody)

			p.Ceiling.EligibleGrants = []types.GrantSpec{pat}
			srv2, st2, audit2 := bedrockAutonomyFixture(t, p, bedrockLaneNone, "", true)
			srv2.cfg.DefaultPolicy.EligibleGrants = []types.GrantSpec{pat}
			other := launch(t, srv2, st2, audit2, patBody)

			if bedrock != other {
				t.Errorf("secrets_powerful=%s: the Bedrock credential launched as %+v, a git_pat as %+v", powerful, bedrock, other)
			}
		})
	}
}
