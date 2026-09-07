// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ssoBindingStore is an aws-sso harness-login run PLUS the run's own audit
// trail — the two pieces of TRUSTED server state handleUploadSSOToken must
// bind an uploaded credential to. Self-contained (rather than extending
// ssoLoginRunStore) so this pin compiles unchanged against the pre-fix tree.
type ssoBindingStore struct {
	store.Store
	run    types.AgentRun
	events []types.AuditEvent
}

func (s ssoBindingStore) GetRun(context.Context, uuid.UUID) (types.AgentRun, error) {
	return s.run, nil
}

func (s ssoBindingStore) QueryAuditEvents(context.Context, uuid.UUID, int) ([]types.AuditEvent, error) {
	return s.events, nil
}

// operatorStartURL / operatorRegion are what the OPERATOR asked for: the
// access-portal URL that arrived with POST /setup/harness-login and was
// recorded on harness.login.started, and the boot-config SSO region
// handleHarnessLogin requires before an aws-sso login run may launch.
const (
	operatorStartURL = "https://my-sso.awsapps.com/start"
	operatorRegion   = "us-west-2"
)

// loginRunFor / loginStartedFor are the two pieces of trusted server state an
// aws-sso login run carries: the run row, and the harness.login.started event
// launchHarnessLoginRun wrote with the operator's own access-portal URL.
func loginRunFor(runID uuid.UUID) types.AgentRun {
	return types.AgentRun{ID: runID, Task: harnessLoginTask, Agent: awsSSOAgent}
}

func loginStartedFor(runID uuid.UUID) []types.AuditEvent {
	return []types.AuditEvent{{
		ID: uuid.New(), RunID: &runID, ActorType: types.ActorSystem, Actor: "wardynd",
		Action: "harness.login.started", Target: runID.String(), Outcome: "success",
		Data: mustJSON(map[string]any{
			"provider": awsSSOProvider, "sso_start_url": operatorStartURL,
		}),
	}}
}

// newSSOBindingSrv wires a Server whose boot config and login-run audit trail
// both declare the operator's values, and returns a run token for that run.
func newSSOBindingSrv(t *testing.T) (*Server, *memSecrets, string, uuid.UUID) {
	t.Helper()
	h := newHarness(t)
	runID := uuid.New()
	st := ssoBindingStore{run: loginRunFor(runID), events: loginStartedFor(runID)}
	sec := &memSecrets{m: map[string][]byte{}}
	cfg := baseTestConfig(h, st)
	cfg.Secrets = sec
	cfg.BedrockRegion = operatorRegion
	srv := New(cfg)
	h.srv = srv
	return srv, sec, h.mintRunToken(t, runID), runID
}

func ssoBlobBody(startURL, region, accessToken string) string {
	return `{
		"access_token": "` + accessToken + `",
		"start_url": "` + startURL + `",
		"region": "` + region + `",
		"account_id": "123456789012",
		"role_name": "WardynBedrockRole",
		"expires_at": "2100-01-01T00:00:00Z"
	}`
}

// TestUploadSSOToken_ForeignIdPRejected is the F006 regression. Every guard
// ahead of it authenticates WHICH run may upload (claimsForRunUpload, then
// run.Task/run.Agent against trusted server state) and shape-checks WHAT is
// uploaded (valid, validateSSOStartURL, repoFieldSafe) — none compares the
// blob to the operator's own declaration. So code running INSIDE the vendor
// login sandbox could PUT a structurally perfect blob naming an ATTACKER's
// IdP and region; it lands under the OPERATOR-WIDE reserved harness name and
// resolveBedrockAuth then picks it ahead of the host ~/.aws mount and the
// static-key lanes for every LATER Bedrock run, baking the attacker's
// start_url/account/role into that run's ~/.aws/config and appending the
// attacker region's oidc./portal.sso. hosts to its egress allowlist.
func TestUploadSSOToken_ForeignIdPRejected(t *testing.T) {
	cases := map[string]string{
		"foreign start_url and region": ssoBlobBody("https://attacker.example.com/start", "eu-central-1", "attacker-token"),
		"foreign start_url only":       ssoBlobBody("https://attacker.example.com/start", operatorRegion, "attacker-token"),
		"foreign region only":          ssoBlobBody(operatorStartURL, "eu-central-1", "attacker-token"),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			srv, sec, tok, runID := newSSOBindingSrv(t)
			w := do(t, srv, http.MethodPut, "/api/v1/internal/sso-token/"+runID.String(), tok, body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("%s: code = %d, want 400; body=%s", name, w.Code, w.Body.String())
			}
			if _, ok := sec.m[harnessCredSecretName(awsSSOProvider)]; ok {
				t.Errorf("%s: a blob unbound to the operator's own declaration must not be stored", name)
			}
		})
	}
}

// TestUploadSSOToken_OperatorBoundBlobAccepted keeps the guard honest: the
// blob the operator's own login actually produces still stores.
func TestUploadSSOToken_OperatorBoundBlobAccepted(t *testing.T) {
	srv, sec, tok, runID := newSSOBindingSrv(t)
	w := do(t, srv, http.MethodPut, "/api/v1/internal/sso-token/"+runID.String(), tok,
		ssoBlobBody(operatorStartURL, operatorRegion, "genuine-token"))
	if w.Code != http.StatusNoContent {
		t.Fatalf("operator-bound blob: code = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	if _, ok := sec.m[harnessCredSecretName(awsSSOProvider)]; !ok {
		t.Fatal("operator-bound blob was not stored")
	}
}

// TestUploadSSOToken_SecondCaptureRefused: the login run stays alive until its
// idle auto-stop, so a bound-but-later upload could still replace the
// operator's genuine capture with the attacker's access_token. One capture per
// login run.
func TestUploadSSOToken_SecondCaptureRefused(t *testing.T) {
	srv, sec, tok, runID := newSSOBindingSrv(t)
	path := "/api/v1/internal/sso-token/" + runID.String()

	if w := do(t, srv, http.MethodPut, path, tok, ssoBlobBody(operatorStartURL, operatorRegion, "genuine-token")); w.Code != http.StatusNoContent {
		t.Fatalf("first capture: code = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	w := do(t, srv, http.MethodPut, path, tok, ssoBlobBody(operatorStartURL, operatorRegion, "attacker-token"))
	if w.Code != http.StatusConflict {
		t.Fatalf("second capture: code = %d, want 409; body=%s", w.Code, w.Body.String())
	}
	var blob awsSSOBlob
	if err := json.Unmarshal(sec.m[harnessCredSecretName(awsSSOProvider)], &blob); err != nil {
		t.Fatalf("stored blob: %v", err)
	}
	if blob.AccessToken != "genuine-token" {
		t.Errorf("stored access token = %q, want the FIRST capture to survive", blob.AccessToken)
	}
}
