// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// "View as a NEW member (not signed in)" (v0.7.5, field report finding 3) — the
// server half. The 0.7.4 mode clamps the ROLE and leaves the admin's own
// per-user AWS session in place, so an admin who has signed in cannot reach the
// one state every new member on a per_user deployment is in. ONE guard at the
// read chokepoint (readAWSSSOBlob, via previewHidesOwnCredential) is what moves
// every downstream surface into that state, and each case below drives a REAL
// route rather than the predicate, because the claim is about what the surfaces
// answer, not about the bit.
//
// Its own file: modelaccess_test.go is past 1000 lines (scripts/check-file-size.sh).

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	memberPreviewAdminSub   = "sub-preview-admin"
	memberPreviewAdminEmail = "preview-admin@corp.example"
)

// memberPreviewSession is memberModeSSOSession (membermode_test.go) with the
// SECOND posture bit — the same hand-rolled payload a zero-value
// *oidc.Authenticator accepts, so every case drives the real cookie branch
// without POSTing the toggle first.
func memberPreviewSession(t *testing.T, mm, noCredential bool) *http.Cookie {
	t.Helper()
	return memberPreviewSessionAs(t, memberPreviewAdminSub, oidc.RoleAdmin, mm, noCredential)
}

func memberPreviewSessionAs(t *testing.T, sub, role string, mm, noCredential bool) *http.Cookie {
	t.Helper()
	payload, err := json.Marshal(oidc.Session{
		V: oidc.SessionCodecVersion, Sub: sub, Email: memberPreviewAdminEmail, Role: role,
		MemberMode: mm, MemberModeNoCredential: noCredential,
		Expiry: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("marshal session: %v", err)
	}
	mac := hmac.New(sha256.New, nil)
	mac.Write(payload)
	return &http.Cookie{
		Name:  "wardyn_session",
		Value: base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)),
	}
}

// memberPreviewSrv is a per_user Bedrock deployment with an SSO console: the
// caller's own captured session is seeded by the caller, and the roster row is
// the one enabled bedrock_sso/per_user row the field report's estate runs.
func memberPreviewSrv(t *testing.T, rows ...types.AgentProvider) (*Server, *memAudit, *scopedSecrets, *integStore) {
	t.Helper()
	if len(rows) == 0 {
		rows = []types.AgentProvider{perUserAWSRow()}
	}
	h := newHarness(t)
	audit := &memAudit{}
	st := &integStore{govEscapeStore: newGovEscapeStore(&capStore{}), site: agentRoster(rows...)}
	sec := newScopedSecrets()
	cfg := baseTestConfig(h, st)
	cfg.Audit = audit
	cfg.Approvals = h.approvals
	cfg.Broker = h.broker
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Runner = &fakeRunner{}
	cfg.Secrets = sec
	cfg.MaskRegistry = secretmask.NewRegistry()
	cfg.BedrockRegion = "us-east-1"
	cfg.BedrockModel = "us.anthropic.claude-sonnet-4-5-20250929-v1:0"
	cfg.Now = func() time.Time { return awsSSOTestFixedNow }
	cfg.DefaultPolicy = govDeployment()
	return New(cfg), audit, sec, st
}

// previewSetupStatus drives GET /setup/status with one cookie and returns the
// two fields this lane is about.
func previewSetupStatus(t *testing.T, srv *Server, cookie *http.Cookie) (SetupModelAccess, []SetupHarness) {
	t.Helper()
	w := doSSO(t, srv, http.MethodGet, "/api/v1/setup/status", cookie, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /setup/status = %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		ModelAccess SetupModelAccess `json:"model_access"`
		Harness     []SetupHarness   `json:"harness"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode /setup/status: %v", err)
	}
	return body.ModelAccess, body.Harness
}

// specSSOPayload is the generated ~/.aws bundle as the dispatched sandbox would
// receive it. It rides SecretEnv (dispatch's splitSecretEnv MOVES credential-
// bearing keys out of Env), so a test that read Env alone would report "absent"
// for every run and pin nothing.
func specSSOPayload(spec runner.SandboxSpec) string {
	if v := spec.SecretEnv[awsSSOConfigEnvVar]; v != "" {
		return v
	}
	return spec.Env[awsSSOConfigEnvVar]
}

func hasAWSHarnessRow(rows []SetupHarness) bool {
	for _, r := range rows {
		if r.Provider == awsSSOProvider {
			return true
		}
	}
	return false
}

// TestMemberPreview_SetupStatusGradesNotSignedIn is the finding itself. The
// admin has a live captured AWS SSO session of their own; inside the preview
// /setup/status must grade the FIRST-RUN state — `not_configured`, with the
// sign-in action — and must publish no captured aws row at all, because a
// console that shows the row is showing the admin their own credential while
// telling them they are seeing a new member's console.
//
// The plain mode is the control, and it is the 0.7.4 behaviour byte for byte:
// `live`, with the row.
func TestMemberPreview_SetupStatusGradesNotSignedIn(t *testing.T) {
	srv, _, sec, _ := memberPreviewSrv(t)
	putScopedSSOBlob(t, sec, memberPreviewAdminSub, awsSSOTestFixedNow.Add(time.Hour), "admin-access-token")

	ma, harness := previewSetupStatus(t, srv, memberPreviewSession(t, true, true))
	if ma.State != modelAccessNotConfigured {
		t.Errorf("model_access.state = %q, want %q — the preview must reach the state every new member is in",
			ma.State, modelAccessNotConfigured)
	}
	if ma.Action != modelAccessSignInAction {
		t.Errorf("model_access.action = %q, want %q", ma.Action, modelAccessSignInAction)
	}
	if hasAWSHarnessRow(harness) {
		t.Error("the preview published a captured aws harness row — the admin's own credential, on a screen claiming to be a new member's")
	}

	// CONTROL: plain member mode is unchanged — the admin's own session still
	// grades live and the row is still there.
	ma, harness = previewSetupStatus(t, srv, memberPreviewSession(t, true, false))
	if ma.State != modelAccessLive {
		t.Errorf("plain member mode graded %q, want %q — the 0.7.4 behaviour must be untouched", ma.State, modelAccessLive)
	}
	if !hasAWSHarnessRow(harness) {
		t.Error("plain member mode dropped the captured aws row")
	}
}

// TestMemberPreview_RunCreateRefusedWithTheExistingSentence: no new refusal
// copy. With the credential reading absent, the create-time mechanism gate
// answers the sentence it already answers for a member who has not signed in —
// which is the whole reason the fix is one guard at the read rather than a
// second rule per surface.
func TestMemberPreview_RunCreateRefusedWithTheExistingSentence(t *testing.T) {
	srv, _, sec, _ := memberPreviewSrv(t)
	putScopedSSOBlob(t, sec, memberPreviewAdminSub, awsSSOTestFixedNow.Add(time.Hour), "admin-access-token")

	w := doSSO(t, srv, http.MethodPost, "/api/v1/runs", memberPreviewSession(t, true, true),
		`{"agent":"claude-code","task":"ship it"}`)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("create in the preview = %d, want 422: %s", w.Code, w.Body.String())
	}
	want := llmMechanismRefusal(perUserAWSRow(), "", false, "")
	if !strings.Contains(w.Body.String(), want) {
		t.Errorf("body = %q, want the EXISTING refusal %q", w.Body.String(), want)
	}

	// CONTROL: the same admin, plain member mode, still creates the run on their
	// own captured session.
	if w := doSSO(t, srv, http.MethodPost, "/api/v1/runs", memberPreviewSession(t, true, false),
		`{"agent":"claude-code","task":"ship it"}`); w.Code != http.StatusCreated {
		t.Fatalf("plain member mode = %d, want 201: %s", w.Code, w.Body.String())
	}
}

// TestMemberPreview_HarnessLoginRefused409: the one door that must refuse
// rather than fail closed. A capture started here would land on the ADMIN'S own
// namespace and overwrite their real session, so the launch is refused — AFTER
// authorizeHarnessLogin, which is what keeps a `shared` deployment answering
// the member's own 403 instead of this sentence.
func TestMemberPreview_HarnessLoginRefused409(t *testing.T) {
	srv, audit, _, st := memberPreviewSrv(t)

	w := doSSO(t, srv, http.MethodPost, "/api/v1/setup/harness-login",
		memberPreviewSession(t, true, true), `{"provider":"`+awsSSOProvider+`"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("harness-login in the preview = %d, want 409: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), memberPreviewSignInRefusal) {
		t.Errorf("body = %q, want %q", w.Body.String(), memberPreviewSignInRefusal)
	}
	if rows := audit.find("harness.login.started"); len(rows) != 0 {
		t.Errorf("a refused sign-in stamped %d harness.login.started row(s)", len(rows))
	}
	st.mu.Lock()
	runs := len(st.runs)
	st.mu.Unlock()
	if runs != 0 {
		t.Errorf("a refused sign-in created %d run row(s)", runs)
	}

	// CONTROL: the refusal sits AFTER the authorization, so a `shared`
	// deployment still answers what a REAL member meets there.
	shared, _, _, _ := memberPreviewSrv(t, types.AgentProvider{
		ID: "claude-code", Mechanism: types.AgentMechanismBedrockSSO,
	})
	w = doSSO(t, shared, http.MethodPost, "/api/v1/setup/harness-login",
		memberPreviewSession(t, true, true), `{"provider":"`+awsSSOProvider+`"}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("shared roster in the preview = %d, want the member's own 403: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), memberPreviewSignInRefusal) {
		t.Error("the preview refusal replaced the member's own harness_login_not_per_user answer")
	}
}

// TestMemberPreview_ToggleAuditsAndReportsTheVariant drives the real toggle:
// the request field decodes (decodeStrict would 400 an unknown key), /me
// publishes BOTH flags, the audit datum carries no_credential only on the row
// that entered the posture, and enabled:false clears both.
func TestMemberPreview_ToggleAuditsAndReportsTheVariant(t *testing.T) {
	srv, audit, _, _ := memberPreviewSrv(t)
	admin := memberPreviewSessionAs(t, memberPreviewAdminSub, oidc.RoleAdmin, false, false)

	w := doSSO(t, srv, http.MethodPost, "/api/v1/me/member-mode", admin, `{"enabled":true,"no_credential":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("enter the preview = %d, want 200: %s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode toggle response: %v", err)
	}
	if body["member_mode"] != true || body["member_mode_no_credential"] != true {
		t.Errorf("toggle response = %v, want both flags true", body)
	}
	on := w.Result().Cookies()
	if len(on) != 1 {
		t.Fatalf("the toggle wrote %d cookies, want 1", len(on))
	}

	me := meBody(t, srv, on[0])
	if me["member_mode"] != true || me["member_mode_no_credential"] != true {
		t.Errorf("/me = member_mode:%v no_credential:%v, want true/true", me["member_mode"], me["member_mode_no_credential"])
	}

	rows := audit.find("auth.member_mode")
	if len(rows) != 1 {
		t.Fatalf("auth.member_mode rows = %d, want 1", len(rows))
	}
	enter := map[string]any{}
	if err := json.Unmarshal(rows[0].Data, &enter); err != nil {
		t.Fatalf("decode audit datum: %v", err)
	}
	if enter["no_credential"] != true || enter["enabled"] != true || enter["real_role"] != oidc.RoleAdmin {
		t.Errorf("enter datum = %v, want enabled/no_credential true and the STAMPED admin role", enter)
	}

	// The exit clears both, and its datum carries no marker — every row a 0.7.4
	// deployment could write stays byte-identical.
	w = doSSO(t, srv, http.MethodPost, "/api/v1/me/member-mode", on[0], `{"enabled":false}`)
	if w.Code != http.StatusOK {
		t.Fatalf("exit = %d, want 200: %s", w.Code, w.Body.String())
	}
	off := w.Result().Cookies()
	if len(off) != 1 {
		t.Fatalf("the exit wrote %d cookies, want 1", len(off))
	}
	me = meBody(t, srv, off[0])
	if me["member_mode"] != false || me["member_mode_no_credential"] != false {
		t.Errorf("/me after the exit = member_mode:%v no_credential:%v, want false/false",
			me["member_mode"], me["member_mode_no_credential"])
	}
	rows = audit.find("auth.member_mode")
	if len(rows) != 2 {
		t.Fatalf("auth.member_mode rows = %d after the exit, want 2", len(rows))
	}
	exit := map[string]any{}
	if err := json.Unmarshal(rows[1].Data, &exit); err != nil {
		t.Fatalf("decode exit datum: %v", err)
	}
	if _, present := exit["no_credential"]; present {
		t.Errorf("the exit row carries no_credential: %v — it is a marker, not a field every row answers", exit)
	}
}

// TestMemberPreview_NonGatedRunAlsoReadsAbsent pins the FAIL-CLOSED direction on
// the one run kind the create-time mechanism gate does NOT cover: an exec run
// (isModelRun is false for task_mode exec, so llmMechanismGateApplies is too)
// is created inside the preview rather than refused, and the sandbox it
// dispatches must carry no AWS SSO material.
//
// GREEN TODAY, deliberately, and it is a REGRESSION pin rather than a
// reproduction: an exec run is not a model run, so no lane credentials it on
// this tree either — there is no ungated run shape that resolves Bedrock today.
// The value is the direction: if an exec lane ever starts resolving a model
// credential, this case reds unless the preview moves with it. The first arm
// below is what keeps it from being vacuous — the SAME fixture, on an ordinary
// task run outside the preview, does put the admin's own session into a
// sandbox, so "no AWS SSO material" is an observation and not an empty map.
func TestMemberPreview_NonGatedRunAlsoReadsAbsent(t *testing.T) {
	srv, _, sec, _ := memberPreviewSrv(t)
	putScopedSSOBlob(t, sec, memberPreviewAdminSub, awsSSOTestFixedNow.Add(time.Hour), "admin-access-token")

	// The fixture DOES credential a sandbox: an ordinary model run in plain
	// member mode carries the admin's own captured session.
	fr := srv.cfg.Runner.(*fakeRunner)
	if w := doSSO(t, srv, http.MethodPost, "/api/v1/runs", memberPreviewSession(t, true, false),
		`{"agent":"claude-code","task":"ship it"}`); w.Code != http.StatusCreated {
		t.Fatalf("plain member mode model run = %d, want 201: %s", w.Code, w.Body.String())
	}
	fr.waitForSandbox(t)
	fr.mu.Lock()
	spec := fr.lastSpec
	fr.mu.Unlock()
	if specSSOPayload(spec) == "" {
		t.Fatalf("the fixture never credentials a sandbox (%s unset on a plain model run) — "+
			"the absence asserted below would prove nothing", awsSSOConfigEnvVar)
	}

	// THE PIN: the ungated run kind, inside the preview.
	fr2 := &fakeRunner{}
	srv.cfg.Runner = runner.Runner(fr2)
	w := doSSO(t, srv, http.MethodPost, "/api/v1/runs", memberPreviewSession(t, true, true),
		`{"agent":"claude-code","task_mode":"exec","task":"echo hi"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("exec run in the preview = %d, want 201 (the gate does not cover it): %s", w.Code, w.Body.String())
	}
	fr2.waitForSandbox(t)
	fr2.mu.Lock()
	spec = fr2.lastSpec
	fr2.mu.Unlock()
	if specSSOPayload(spec) != "" {
		t.Errorf("the preview's own ungated run carries AWS SSO material (%s set) — "+
			"the admin's captured session reached a sandbox", awsSSOConfigEnvVar)
	}
}
