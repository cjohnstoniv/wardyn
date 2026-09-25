// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ─── the Amazon Bedrock model credential (#504) ───────────────────────────────

// bedrockLane seeds one of the Bedrock credential lanes into a fixture's
// operator namespace (no roster row: the legacy scope every lane reads), or
// nothing at all.
type bedrockLane string

const (
	bedrockLaneNone   bedrockLane = ""
	bedrockLaneSSO    bedrockLane = "sso"
	bedrockLaneBearer bedrockLane = "bearer"
	bedrockLaneKeys   bedrockLane = "keys"
)

func seedBedrockLane(t *testing.T, srv *Server, lane bedrockLane) {
	t.Helper()
	sec, ok := srv.cfg.Secrets.(*memSecrets)
	if !ok {
		t.Fatalf("fixture secrets are %T, not *memSecrets", srv.cfg.Secrets)
	}
	switch lane {
	case bedrockLaneSSO:
		putAWSSSOBlob(t, srv, awsSSOTestFixedNow.Add(time.Hour))
	case bedrockLaneBearer:
		sec.m[bedrockAPIKeySecret] = []byte("bedrock-bearer-key")
	case bedrockLaneKeys:
		sec.m[bedrockAccessKeyIDSecret] = []byte("AKIAEXAMPLE")
		sec.m[bedrockSecretAccessKeySecret] = []byte("secret-access-key")
	}
}

// bedrockAutonomyFixture is govEscapeFixture with Amazon Bedrock configured (a
// region and a model) and the given lane seeded. The clock is the SSO tests'
// fixed one, so a captured session is live and the create-time refresh is a
// no-op on the wire.
func bedrockAutonomyFixture(t *testing.T, p *types.GovernanceProfile, configured bool, lane bedrockLane, proxyInject bool,
) (*Server, *govEscapeStore, *recRecorder) {
	t.Helper()
	srv, st, audit := govEscapeFixture(t, autonomyCapStore(p))
	if configured {
		srv.cfg.BedrockRegion = "us-east-1"
		srv.cfg.BedrockModel = "us.anthropic.claude-sonnet-4-5-20250929-v1:0"
	}
	srv.cfg.Now = func() time.Time { return awsSSOTestFixedNow }
	srv.cfg.AWSSSOProxyInject = proxyInject
	seedBedrockLane(t, srv, lane)
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
// probe: a run whose model credential is an AWS credential for Amazon Bedrock,
// graded at Review and at launch.
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
// The last two rows are the golden: a deployment with Bedrock configured and no
// credential resolving, and one with no Bedrock at all, grade exactly what they
// did before this fold existed.
func TestAutonomyGradesTheBedrockModelCredentialAtBothDoors(t *testing.T) {
	member := func(t *testing.T) *http.Cookie { return govSession(t, "sub-bedrock", []string{"eng"}, false) }
	for _, tc := range []struct {
		name        string
		configured  bool
		lane        bedrockLane
		proxyInject bool
		wantSecrets types.AutonomySecretsPosture
		wantLevel   types.AutonomyLevel
	}{
		{"captured AWS SSO session, proxy-injected", true, bedrockLaneSSO, true, types.AutonomySecretsPowerful, types.AutonomyL1},
		{"captured AWS SSO session, resident in the sandbox", true, bedrockLaneSSO, false, types.AutonomySecretsPowerful, types.AutonomyL1},
		{"Bedrock bearer key", true, bedrockLaneBearer, true, types.AutonomySecretsPowerful, types.AutonomyL1},
		{"resident SigV4 keys", true, bedrockLaneKeys, true, types.AutonomySecretsPowerful, types.AutonomyL1},
		{"Bedrock configured, no credential resolves", true, bedrockLaneNone, true, types.AutonomySecretsNone, types.AutonomyL3},
		{"no Bedrock at all", false, bedrockLaneSSO, true, types.AutonomySecretsNone, types.AutonomyL3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := govProfile("bedrock")
			p.Limits = types.GovernanceLimits{AutonomyRubric: secretsOnlyRubric(types.AutonomyL1)}

			srv, _, _ := bedrockAutonomyFixture(t, p, tc.configured, tc.lane, tc.proxyInject)
			w := doSSO(t, srv, http.MethodPost, "/api/v1/runs/preflight", member(t), bedrockAutonomyBody)
			if w.Code != http.StatusOK {
				t.Fatalf("preflight = %d, want 200: %s", w.Code, w.Body.String())
			}
			var resp struct {
				Autonomy map[string]any `json:"autonomy"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode preflight: %v", err)
			}

			srv2, st2, audit2 := bedrockAutonomyFixture(t, p, tc.configured, tc.lane, tc.proxyInject)
			c := doSSO(t, srv2, http.MethodPost, "/api/v1/runs", member(t), bedrockAutonomyBody)
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
	member := func(t *testing.T) *http.Cookie { return govSession(t, "sub-bedrock", []string{"eng"}, false) }
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

			srv, st, audit := bedrockAutonomyFixture(t, p, true, bedrockLaneSSO, true)
			bedrock := launch(t, srv, st, audit, bedrockAutonomyBody)

			p.Ceiling.EligibleGrants = []types.GrantSpec{pat}
			srv2, st2, audit2 := bedrockAutonomyFixture(t, p, false, bedrockLaneNone, true)
			srv2.cfg.DefaultPolicy.EligibleGrants = []types.GrantSpec{pat}
			other := launch(t, srv2, st2, audit2, patBody)

			if bedrock != other {
				t.Errorf("secrets_powerful=%s: the Bedrock credential launched as %+v, a git_pat as %+v", powerful, bedrock, other)
			}
		})
	}
}

// TestBedrockCredentialIsAuthoredOnlyAsGraded is the freeze: the gate grades
// the credential from the create-time resolution, and dispatch resolves it
// again, minutes later (the image resolve, a devcontainer build). A captured
// AWS sign-in stored in between used to reach a run graded `secrets=none`.
//
// The assertion is on what dispatch HANDED THE RUNNER, never on the posture:
// the audit row records the grade, so only the sandbox spec tells the escape
// from a correct run. The reverse edit — the credential removed after the
// gate — only over-caps the run and must still launch.
func TestBedrockCredentialIsAuthoredOnlyAsGraded(t *testing.T) {
	const portal = "portal.sso.us-east-1.amazonaws.com"
	for _, tc := range []struct {
		name        string
		atTheGate   bedrockLane
		wantRefused bool
	}{
		{"a sign-in captured after the gate never reaches the run", bedrockLaneNone, true},
		{"a sign-in removed after the gate only caps the run", bedrockLaneSSO, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := govProfile("bedrock-span")
			p.Limits = types.GovernanceLimits{AutonomyRubric: secretsOnlyRubric(types.AutonomyL1)}
			srv, st, audit := bedrockAutonomyFixture(t, p, true, tc.atTheGate, true)
			st.onCreateRun = func() {
				sec, _ := srv.cfg.Secrets.(*memSecrets)
				if tc.atTheGate == bedrockLaneNone {
					seedBedrockLane(t, srv, bedrockLaneSSO)
					return
				}
				delete(sec.m, harnessCredSecretName(awsSSOProvider))
			}

			w := doSSO(t, srv, http.MethodPost, "/api/v1/runs", govSession(t, "sub-bedrock", []string{"eng"}, false), bedrockAutonomyBody)
			if w.Code != http.StatusCreated {
				t.Fatalf("create = %d, want 201: %s", w.Code, w.Body.String())
			}
			var created struct {
				ID uuid.UUID `json:"id"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
				t.Fatalf("decode create: %v", err)
			}
			runID := created.ID
			state, hosts := adoDispatchedInjectionHosts(t, srv, runID)

			if tc.wantRefused {
				if slices.Contains(hosts, portal) {
					t.Errorf("dispatch injected the AWS SSO session into %s for a run graded without it (hosts %v)", portal, hosts)
				}
				if state != types.RunFailed {
					t.Errorf("run state = %s, want FAILED", state)
				}
				if ev := findAudit(audit.snapshot(), runID, "run.create", "failure"); ev == nil || !strings.Contains(string(ev.Data), "autonomy_grade_drift") {
					t.Errorf("no run.create failure row naming autonomy_grade_drift")
				}
				return
			}
			if state == types.RunFailed {
				t.Errorf("run state = FAILED: a credential removed after the gate only over-caps the run")
			}
		})
	}
}
