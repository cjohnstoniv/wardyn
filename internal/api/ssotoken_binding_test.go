// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
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
	run     types.AgentRun
	events  []types.AuditEvent
	siteCfg types.SiteConfig
}

func (s ssoBindingStore) GetRun(context.Context, uuid.UUID) (types.AgentRun, error) {
	return s.run, nil
}

// GetSiteConfig is the roster read the upload handler makes to decide WHOSE
// namespace the capture lands in (0.7.2). Zero value = legacy/shared.
func (s ssoBindingStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return s.siteCfg, nil
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

// ── finding 1: the capture must name the account it was AUTHORIZED to name ───
//
// The reported failure was a capture that named a CONFIDENTLY WRONG AWS
// account: structurally perfect, every guard above satisfied, stored, and then
// baked verbatim into every later Bedrock run's ~/.aws/config — surfacing as
// somebody else's 403 inside an agent terminal, ten retries deep, with no diff
// and no audit row naming the change. awsSSOBlob.valid's own doc already had
// the right reasoning ("accepting it as connected would silently pre-empt a
// lane that might have actually worked") one step short: it required the
// account to be PRESENT, never that it was the RIGHT one.
//
// Two bindings close that, both against trusted server state: the LAUNCH-TIME
// roster pin (read back off this run's own harness.login.started row, never off
// the live roster) and the account the configured Bedrock model lives in.

// pinnedLoginStarted is loginStartedFor PLUS the launch-time account/role pin.
func pinnedLoginStarted(runID uuid.UUID, pinAccount, pinRole string) []types.AuditEvent {
	ev := loginStartedFor(runID)
	ev[0].Data = mustJSON(map[string]any{
		"provider": awsSSOProvider, "sso_start_url": operatorStartURL,
		"sso_account_id": pinAccount, "sso_role_name": pinRole,
	})
	return ev
}

// newPinnedSSOSrv is newSSOBindingSrv with a launch-time pin and the
// operator's configured Bedrock model.
func newPinnedSSOSrv(t *testing.T, pinAccount, pinRole, model string) (*Server, *memSecrets, *harness, string, uuid.UUID) {
	t.Helper()
	h := newHarness(t)
	runID := uuid.New()
	st := ssoBindingStore{run: loginRunFor(runID), events: pinnedLoginStarted(runID, pinAccount, pinRole)}
	sec := &memSecrets{m: map[string][]byte{}}
	cfg := baseTestConfig(h, st)
	cfg.Secrets = sec
	cfg.BedrockRegion = operatorRegion
	cfg.BedrockModel = model
	srv := New(cfg)
	h.srv = srv
	return srv, sec, h, h.mintRunToken(t, runID), runID
}

// ssoBlobFor is ssoBlobBody with the account/role the SANDBOX claims to have
// captured — the one pair a compromised, or merely unlucky, login sandbox is
// free to choose.
func ssoBlobFor(accountID, roleName string) string {
	return `{
		"access_token": "aws-sso-access-token-value",
		"start_url": "` + operatorStartURL + `",
		"region": "` + operatorRegion + `",
		"account_id": "` + accountID + `",
		"role_name": "` + roleName + `",
		"expires_at": "2100-01-01T00:00:00Z"
	}`
}

func putSSOToken(t *testing.T, srv *Server, runID uuid.UUID, tok, body string) (int, string) {
	t.Helper()
	w := do(t, srv, http.MethodPut, "/api/v1/internal/sso-token/"+runID.String(), tok, body)
	return w.Code, w.Body.String()
}

// TestUploadSSOToken_AccountIDNotRowPinRejected: the admin pinned account
// 111111111111; the sandbox uploads a session for 222222222222 — the unrelated
// dev account the operator's cloud team granted. Refused, nothing stored.
func TestUploadSSOToken_AccountIDNotRowPinRejected(t *testing.T) {
	srv, sec, _, tok, runID := newPinnedSSOSrv(t, "111111111111", "BedrockRunner", "")
	code, body := putSSOToken(t, srv, runID, tok, ssoBlobFor("222222222222", "BedrockRunner"))
	if code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400; body=%s", code, body)
	}
	if !strings.Contains(body, "111111111111") || !strings.Contains(body, "222222222222") {
		t.Errorf("refusal = %s, want it to name both what was captured and what is pinned", body)
	}
	if _, ok := sec.m[harnessCredSecretName(awsSSOProvider)]; ok {
		t.Error("a capture that disagrees with the pin must not be stored — it is baked verbatim into every later run's ~/.aws/config")
	}
}

// TestUploadSSOToken_RoleNameNotRowPinRejected: the RIGHT account, the wrong
// role. The finding's own repro had two roles in the wrong account and neither
// could invoke the configured model, so the role half is not decoration.
func TestUploadSSOToken_RoleNameNotRowPinRejected(t *testing.T) {
	srv, sec, _, tok, runID := newPinnedSSOSrv(t, "111111111111", "BedrockRunner", "")
	code, body := putSSOToken(t, srv, runID, tok, ssoBlobFor("111111111111", "ReadOnly"))
	if code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400; body=%s", code, body)
	}
	if !strings.Contains(body, "BedrockRunner") || !strings.Contains(body, "ReadOnly") {
		t.Errorf("refusal = %s, want it to name both roles", body)
	}
	if _, ok := sec.m[harnessCredSecretName(awsSSOProvider)]; ok {
		t.Error("a capture whose ROLE disagrees with the pin must not be stored")
	}
}

// TestUploadSSOToken_AccountIDNotModelARNAccountRejected is ask 3 verbatim:
// "Wardyn holds both halves — the blob's AccountID and the account in
// WARDYN_BEDROCK_MODEL's ARN. When they differ, say so at capture." Unpinned
// row, full model ARN: the mismatch is refusable from server state alone.
func TestUploadSSOToken_AccountIDNotModelARNAccountRejected(t *testing.T) {
	const model = "arn:aws:bedrock:us-west-2:111111111111:inference-profile/us.anthropic.claude-sonnet-4-20250514-v1:0"
	srv, sec, _, tok, runID := newPinnedSSOSrv(t, "", "", model)
	code, body := putSSOToken(t, srv, runID, tok, ssoBlobFor("222222222222", "DevPower"))
	if code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400; body=%s", code, body)
	}
	if !strings.Contains(body, "222222222222") || !strings.Contains(body, "111111111111") {
		t.Errorf("refusal = %s, want it to name the session's account and the model's", body)
	}
	if _, ok := sec.m[harnessCredSecretName(awsSSOProvider)]; ok {
		t.Error("a session for an account the configured model does not live in must not be stored")
	}
}

// TestUploadSSOToken_NonARNModelSkipsAccountBinding is the upgrade guard, and it
// matters more than it looks: WARDYN_BEDROCK_MODEL is most often a BARE
// cross-region inference profile id, which names no account at all. A check
// that failed closed on "no account in the model" would take capture away from
// every such deployment the day they upgraded, to enforce a comparison there is
// no data for.
func TestUploadSSOToken_NonARNModelSkipsAccountBinding(t *testing.T) {
	for _, model := range []string{
		"us.anthropic.claude-sonnet-4-20250514-v1:0",
		"anthropic.claude-3-5-sonnet-20241022-v2:0",
		"",
	} {
		t.Run(model, func(t *testing.T) {
			srv, sec, _, tok, runID := newPinnedSSOSrv(t, "", "", model)
			code, body := putSSOToken(t, srv, runID, tok, ssoBlobFor("222222222222", "DevPower"))
			if code != http.StatusNoContent {
				t.Fatalf("code = %d, want 204 (a model naming no account cannot disagree with one); body=%s", code, body)
			}
			if _, ok := sec.m[harnessCredSecretName(awsSSOProvider)]; !ok {
				t.Error("nothing was stored — a non-ARN model must SKIP the account check, not fail it")
			}
		})
	}
}

// TestUploadSSOToken_PinnedPairAccepted: the pin agrees with the blob and with
// the model's account, so the capture lands exactly as it always did.
func TestUploadSSOToken_PinnedPairAccepted(t *testing.T) {
	const model = "arn:aws:bedrock:us-west-2:111111111111:inference-profile/us.anthropic.claude-sonnet-4-20250514-v1:0"
	srv, sec, _, tok, runID := newPinnedSSOSrv(t, "111111111111", "BedrockRunner", model)
	code, body := putSSOToken(t, srv, runID, tok, ssoBlobFor("111111111111", "BedrockRunner"))
	if code != http.StatusNoContent {
		t.Fatalf("code = %d, want 204; body=%s", code, body)
	}
	if _, ok := sec.m[harnessCredSecretName(awsSSOProvider)]; !ok {
		t.Error("an agreeing capture was not stored")
	}
}

// TestUploadSSOToken_RefusalIsAudited: before this lane every refusal here was a
// bare writeError — the operator's own complaint was "no diff, no audit row
// naming the change, and no warning". A refused capture now leaves a row whose
// reason comes from the FIXED vocabulary and never from sandbox-chosen text.
func TestUploadSSOToken_RefusalIsAudited(t *testing.T) {
	srv, _, h, tok, runID := newPinnedSSOSrv(t, "111111111111", "BedrockRunner", "")
	code, _ := putSSOToken(t, srv, runID, tok, ssoBlobFor("222222222222", "BedrockRunner"))
	if code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", code)
	}
	if !auditHas(h.audit.events, "harness.credential.refused") {
		t.Fatal("a refused capture left no harness.credential.refused row — the refusal is invisible to an incident review")
	}
	for _, ev := range h.audit.events {
		if ev.Action != "harness.credential.refused" {
			continue
		}
		data := string(ev.Data)
		if !strings.Contains(data, refuseReasonAccountRolePin) {
			t.Errorf("refusal row data = %s, want the fixed reason %q", data, refuseReasonAccountRolePin)
		}
		if strings.Contains(data, "222222222222") {
			t.Errorf("refusal row data = %s, carries sandbox-chosen values; the reason vocabulary is closed on purpose", data)
		}
		if ev.Outcome != "failure" {
			t.Errorf("refusal row outcome = %q, want failure", ev.Outcome)
		}
	}
}

// TestBedrockModelAccount is the tree's first ARN parser, and the only question
// it answers is "which account does the configured model live in". Everything
// that is not a full Bedrock ARN with a 12-digit account answers "" — which
// callers read as SKIP, never as a failure.
func TestBedrockModelAccount(t *testing.T) {
	for _, tc := range []struct{ model, want string }{
		{"arn:aws:bedrock:us-west-2:111111111111:inference-profile/us.anthropic.claude-sonnet-4-20250514-v1:0", "111111111111"},
		{"arn:aws:bedrock:us-east-1:222222222222:application-inference-profile/abcd1234", "222222222222"},
		{"arn:aws:bedrock:us-east-1:333333333333:foundation-model/anthropic.claude-3-5-sonnet-20241022-v2:0", "333333333333"},
		// GovCloud / China partitions parse identically — nothing here reads the partition.
		{"arn:aws-us-gov:bedrock:us-gov-west-1:444444444444:inference-profile/x", "444444444444"},
		// A cross-region inference profile id: names no account, so no check.
		{"us.anthropic.claude-sonnet-4-20250514-v1:0", ""},
		{"anthropic.claude-3-5-sonnet-20241022-v2:0", ""},
		{"", ""},
		{"arn:aws:bedrock:us-west-2::inference-profile/x", ""},
		{"arn:aws:bedrock:us-west-2:12345:inference-profile/x", ""},
		{"arn:aws:bedrock:us-west-2:1111111111111:inference-profile/x", ""},
		{"arn:aws:s3:::my-bucket/key:extra", ""},
		{"arn:aws:bedrock", ""},
		{"::::::", ""},
		{"not an arn at all", ""},
	} {
		if got := bedrockModelAccount(tc.model); got != tc.want {
			t.Errorf("bedrockModelAccount(%q) = %q, want %q", tc.model, got, tc.want)
		}
	}
}

// TestAWSSSOPinEnv: an unpinned row seeds NOTHING, so an unpinned launch is
// byte-identical to what it was before this lane.
func TestAWSSSOPinEnv(t *testing.T) {
	if got := awsSSOPinEnv(awsSSOPin{}); got != nil {
		t.Errorf("an empty pin seeded %v, want nil", got)
	}
	if got := awsSSOPinEnv(awsSSOPin{AccountID: "111111111111"}); got != nil {
		t.Errorf("half a pin seeded %v, want nil — pinning the account alone still leaves the role picked for whoever signs in", got)
	}
	got := awsSSOPinEnv(awsSSOPin{AccountID: "111111111111", RoleName: "BedrockRunner"})
	if got[awsSSOPinAccountEnvVar] != "111111111111" || got[awsSSOPinRoleEnvVar] != "BedrockRunner" {
		t.Errorf("pin env = %v, want both halves", got)
	}
	if len(got) != 2 {
		t.Errorf("pin env = %v, want exactly the two pin vars", got)
	}
}
