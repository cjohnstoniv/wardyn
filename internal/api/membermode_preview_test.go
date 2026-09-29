// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// "View as a new member (not signed in)" — the server half. Clamping only the
// role would leave the admin's own model-provider credential in place, so an
// admin who has signed in could not reach the one state every new member is in.
// One guard at the read chokepoints (readAWSSSOBlob and ownSecret, via
// previewHidesOwnCredential) is what moves every downstream surface into that
// state, and each case below drives a REAL route rather than the predicate,
// because the claim is about what the surfaces answer, not about the bit.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
		V: oidc.SessionCodecVersion, Sub: sub, Email: memberPreviewAdminEmail, Role: role, UserType: "standard",
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

// memberPreviewSrv is a deployment with an SSO console whose one model
// provider is awsSSOTestProvider (or providers, when given): the caller's own
// captured session is seeded by the caller.
func memberPreviewSrv(t *testing.T, providers ...types.ModelProvider) (*Server, *memAudit, *memSecrets, *integStore) {
	t.Helper()
	if providers == nil {
		providers = []types.ModelProvider{awsSSOTestProvider()}
	}
	h := newHarness(t)
	audit := &memAudit{}
	site := types.SiteConfig{}
	if len(providers) > 0 {
		site.ModelProviders = &types.ModelProviders{Providers: providers}
	}
	st := &integStore{govEscapeStore: newGovEscapeStore(&capStore{}), site: site}
	sec := &memSecrets{m: map[string][]byte{}, owned: map[string]map[string][]byte{}}
	cfg := baseTestConfig(h, st)
	cfg.Audit = audit
	cfg.Approvals = h.approvals
	cfg.Broker = h.broker
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Runner = &fakeRunner{}
	cfg.Secrets = sec
	cfg.MaskRegistry = secretmask.NewRegistry()
	cfg.Now = func() time.Time { return awsSSOTestFixedNow }
	cfg.DefaultPolicy = govDeployment()
	return New(cfg), audit, sec, st
}

// putPreviewSession stores owner's own captured session for awsSSOTestProvider.
func putPreviewSession(t *testing.T, sec *memSecrets, owner string) {
	t.Helper()
	raw, err := json.Marshal(awsSSOBlob{
		AccessToken: "admin-access-token", RefreshToken: "refresh-admin-access-token",
		ClientID: "client-id", ClientSecret: "client-secret",
		StartURL: "https://example.awsapps.com/start", Region: "us-east-1",
		AccountID: "123456789012", RoleName: "WardynBedrockRole",
		ExpiresAt: awsSSOTestFixedNow.Add(time.Hour), CapturedAt: awsSSOTestFixedNow.Add(-time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sec.For(owner).Put(context.Background(), providerSecretName(awsSSOTestProviderUID, providerSSOPart), raw); err != nil {
		t.Fatal(err)
	}
}

// previewSetupStatus drives GET /setup/status with one cookie and returns the
// caller's provider_access row for awsSSOTestProvider.
func previewSetupStatus(t *testing.T, srv *Server, cookie *http.Cookie) SetupProviderAccess {
	t.Helper()
	w := doSSO(t, srv, http.MethodGet, "/api/v1/setup/status", cookie, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /setup/status = %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		ProviderAccess []SetupProviderAccess `json:"provider_access"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode /setup/status: %v", err)
	}
	for _, a := range body.ProviderAccess {
		if a.Provider == awsSSOTestProvider().ID {
			return a
		}
	}
	t.Fatalf("provider_access has no row for %s: %+v", awsSSOTestProvider().ID, body.ProviderAccess)
	return SetupProviderAccess{}
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

// TestMemberPreview_SetupStatusGradesNotSignedIn is the finding itself. The
// admin has a live captured AWS sign-in of their own; inside the preview
// /setup/status must grade the FIRST-RUN state — `not_configured`, with the
// sign-in action. The plain mode is the control: `live`.
func TestMemberPreview_SetupStatusGradesNotSignedIn(t *testing.T) {
	srv, _, sec, _ := memberPreviewSrv(t)
	putPreviewSession(t, sec, memberPreviewAdminSub)

	a := previewSetupStatus(t, srv, memberPreviewSession(t, true, true))
	if a.State != modelAccessNotConfigured {
		t.Errorf("provider_access.state = %q, want %q — the preview must reach the state every new member is in",
			a.State, modelAccessNotConfigured)
	}
	if a.Action != modelAccessSignInAction {
		t.Errorf("provider_access.action = %q, want %q", a.Action, modelAccessSignInAction)
	}
	if a.SourceRunID != "" {
		t.Error("the preview published the admin's own capture's run id")
	}

	// CONTROL: plain member mode is unchanged — the admin's own session still
	// grades live.
	if a := previewSetupStatus(t, srv, memberPreviewSession(t, true, false)); a.State != modelAccessLive {
		t.Errorf("plain member mode graded %q, want %q", a.State, modelAccessLive)
	}
}

// TestMemberPreview_NoProviderDowngradesToThePlainMode is F1: the posture is
// GRANTED by the server, never taken from the body. With no model provider on
// the preview hides nothing, so entering it would paint "not signed in" over a
// console that has nothing to sign in to. The toggle answers the PLAIN mode
// instead, /me says so, the audit row carries no marker, and /me tells the
// console not to offer the entry at all.
func TestMemberPreview_NoProviderDowngradesToThePlainMode(t *testing.T) {
	off := awsSSOTestProvider()
	off.Disabled = true
	for name, providers := range map[string][]types.ModelProvider{"no provider block": {}, "every provider off": {off}} {
		t.Run(name, func(t *testing.T) {
			srv, audit, _, _ := memberPreviewSrv(t, providers...)
			admin := memberPreviewSessionAs(t, memberPreviewAdminSub, oidc.RoleAdmin, false, false)

			if me := meBody(t, srv, admin); me["user_preview_available"] != false {
				t.Errorf("/me user_preview_available = %v with no provider on, want false", me["user_preview_available"])
			}
			w := doSSO(t, srv, http.MethodPost, "/api/v1/me/view", admin, `{"view":"user","no_credential":true}`)
			if w.Code != http.StatusOK {
				t.Fatalf("toggle = %d, want 200: %s", w.Code, w.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode toggle response: %v", err)
			}
			if body["user_view"] != true || body["user_view_no_credential"] != false {
				t.Errorf("toggle response = %v, want user_view true and no_credential FALSE", body)
			}
			rows := audit.find("auth.user_view.set")
			if len(rows) != 1 {
				t.Fatalf("auth.user_view.set rows = %d, want 1", len(rows))
			}
			datum := map[string]any{}
			if err := json.Unmarshal(rows[0].Data, &datum); err != nil {
				t.Fatalf("decode audit datum: %v", err)
			}
			if _, present := datum["no_credential"]; present {
				t.Errorf("a downgraded toggle audited no_credential: %v — the key must name the posture the session IS in", datum)
			}
		})
	}

	// THE CONTROL: the same request with a provider on IS granted.
	srv, _, _, _ := memberPreviewSrv(t)
	admin := memberPreviewSessionAs(t, memberPreviewAdminSub, oidc.RoleAdmin, false, false)
	if me := meBody(t, srv, admin); me["user_preview_available"] != true {
		t.Errorf("/me user_preview_available = %v with a provider on, want true", me["user_preview_available"])
	}
	w := doSSO(t, srv, http.MethodPost, "/api/v1/me/view", admin, `{"view":"user","no_credential":true}`)
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode toggle response: %v", err)
	}
	if body["user_view_no_credential"] != true {
		t.Errorf("toggle response = %v, want the posture GRANTED", body)
	}
}

// TestMemberPreview_RealMemberIsNeverGrantedThePosture is F4. SetUserView
// writes a real member no cookie at all, so echoing their request would report
// and AUDIT a posture nobody is in — and would make this row's own
// docs/AUDIT-ACTIONS.md sentence false.
func TestMemberPreview_RealMemberIsNeverGrantedThePosture(t *testing.T) {
	srv, audit, _, _ := memberPreviewSrv(t)
	member := memberPreviewSessionAs(t, "sub-real-member", oidc.RoleUser, false, false)

	w := doSSO(t, srv, http.MethodPost, "/api/v1/me/view", member, `{"view":"user","no_credential":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("member toggle = %d, want 200: %s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode toggle response: %v", err)
	}
	if body["user_view_no_credential"] != false {
		t.Errorf("a real member's request echoed user_view_no_credential:%v, want false — no cookie was written", body["user_view_no_credential"])
	}
	rows := audit.find("auth.user_view.set")
	if len(rows) != 1 {
		t.Fatalf("auth.user_view.set rows = %d, want 1", len(rows))
	}
	datum := map[string]any{}
	if err := json.Unmarshal(rows[0].Data, &datum); err != nil {
		t.Fatalf("decode audit datum: %v", err)
	}
	if _, present := datum["no_credential"]; present {
		t.Errorf("a real member's row carries no_credential: %v", datum)
	}
	if datum["real_role"] != oidc.RoleUser {
		t.Errorf("real_role = %v, want member", datum["real_role"])
	}
	// A member is never offered the control either.
	if me := meBody(t, srv, member); me["user_preview_available"] != false {
		t.Errorf("/me user_preview_available = %v for a member, want false", me["user_preview_available"])
	}
}

// TestMemberPreview_RunCreateRefusedWithTheExistingSentence: no new refusal
// copy. With the credential reading absent, the create door answers the
// sentence it already answers for a person who has not signed in — which is
// the whole reason the fix is one guard at the read rather than a second rule
// per surface.
func TestMemberPreview_RunCreateRefusedWithTheExistingSentence(t *testing.T) {
	srv, _, sec, _ := memberPreviewSrv(t)
	putPreviewSession(t, sec, memberPreviewAdminSub)

	w := doSSO(t, srv, http.MethodPost, "/api/v1/runs", memberPreviewSession(t, true, true),
		`{"agent":"claude-code","task":"ship it"}`)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("create in the preview = %d, want 422: %s", w.Code, w.Body.String())
	}
	if want := connectDenial(awsSSOTestProvider().ID, mpBRNotSignedIn).msg; !strings.Contains(w.Body.String(), want) {
		t.Errorf("body = %q, want the EXISTING refusal %q", w.Body.String(), want)
	}

	// CONTROL: the same admin, plain member mode, still creates the run on their
	// own captured session.
	if w := doSSO(t, srv, http.MethodPost, "/api/v1/runs", memberPreviewSession(t, true, false),
		`{"agent":"claude-code","task":"ship it"}`); w.Code != http.StatusCreated {
		t.Fatalf("plain member mode = %d, want 201: %s", w.Code, w.Body.String())
	}
}

// TestMemberPreview_SignInRefused409: the one door that must refuse rather
// than fail closed. A capture started here would land on the ADMIN'S own
// namespace and overwrite their real session, so the launch is refused.
func TestMemberPreview_SignInRefused409(t *testing.T) {
	srv, audit, _, st := memberPreviewSrv(t)

	w := doSSO(t, srv, http.MethodPost, "/api/v1/model-providers/"+awsSSOTestProvider().ID+"/sign-in",
		memberPreviewSession(t, true, true), "")
	if w.Code != http.StatusConflict {
		t.Fatalf("sign-in in the preview = %d, want 409: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), mpsPreview) {
		t.Errorf("body = %q, want %q", w.Body.String(), mpsPreview)
	}
	if rows := audit.find("harness.login.start"); len(rows) != 0 {
		t.Errorf("a refused sign-in stamped %d harness.login.start row(s)", len(rows))
	}
	st.mu.Lock()
	runs := len(st.runs)
	st.mu.Unlock()
	if runs != 0 {
		t.Errorf("a refused sign-in created %d run row(s)", runs)
	}
}

// TestMemberPreview_ToggleAuditsAndReportsTheVariant drives the real toggle:
// the request field decodes (decodeStrict would 400 an unknown key), /me
// publishes BOTH flags, the audit datum carries no_credential only on the row
// that entered the posture, and enabled:false clears both.
func TestMemberPreview_ToggleAuditsAndReportsTheVariant(t *testing.T) {
	srv, audit, _, _ := memberPreviewSrv(t)
	admin := memberPreviewSessionAs(t, memberPreviewAdminSub, oidc.RoleAdmin, false, false)

	w := doSSO(t, srv, http.MethodPost, "/api/v1/me/view", admin, `{"view":"user","no_credential":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("enter the preview = %d, want 200: %s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode toggle response: %v", err)
	}
	if body["user_view"] != true || body["user_view_no_credential"] != true {
		t.Errorf("toggle response = %v, want both flags true", body)
	}
	on := w.Result().Cookies()
	if len(on) != 1 {
		t.Fatalf("the toggle wrote %d cookies, want 1", len(on))
	}

	me := meBody(t, srv, on[0])
	if me["user_view"] != true || me["user_view_no_credential"] != true {
		t.Errorf("/me = user_view:%v no_credential:%v, want true/true", me["user_view"], me["user_view_no_credential"])
	}

	rows := audit.find("auth.user_view.set")
	if len(rows) != 1 {
		t.Fatalf("auth.user_view.set rows = %d, want 1", len(rows))
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
	w = doSSO(t, srv, http.MethodPost, "/api/v1/me/view", on[0], `{"view":"admin"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("exit = %d, want 200: %s", w.Code, w.Body.String())
	}
	off := w.Result().Cookies()
	if len(off) != 1 {
		t.Fatalf("the exit wrote %d cookies, want 1", len(off))
	}
	me = meBody(t, srv, off[0])
	if me["user_view"] != false || me["user_view_no_credential"] != false {
		t.Errorf("/me after the exit = user_view:%v no_credential:%v, want false/false",
			me["user_view"], me["user_view_no_credential"])
	}
	rows = audit.find("auth.user_view.set")
	if len(rows) != 2 {
		t.Fatalf("auth.user_view.set rows = %d after the exit, want 2", len(rows))
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
// the one run kind the create door's provider check does NOT cover: an exec run
// (isModelRun is false for task_mode exec) is created inside the preview rather
// than refused, and the sandbox it dispatches must carry no AWS SSO material.
//
// Green by construction, and a pin on the direction rather than a reproduction:
// an exec run is not a model run, so no lane credentials it — there is no
// ungated run shape that resolves Bedrock. If an exec lane ever starts
// resolving a model credential, this case reds unless the preview moves with
// it. The first arm
// below is what keeps it from being vacuous — the SAME fixture, on an ordinary
// task run outside the preview, does put the admin's own session into a
// sandbox, so "no AWS SSO material" is an observation and not an empty map.
func TestMemberPreview_NonGatedRunAlsoReadsAbsent(t *testing.T) {
	srv, _, sec, _ := memberPreviewSrv(t)
	putPreviewSession(t, sec, memberPreviewAdminSub)

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

	// THE PIN: the ungated run kind, inside the preview — on its OWN server.
	// Swapping cfg.Runner on the first one raced that run's detached completion
	// watcher, which reads cfg.Runner for its Wait (caught by make test-race).
	srv2, _, sec2, _ := memberPreviewSrv(t)
	putPreviewSession(t, sec2, memberPreviewAdminSub)
	fr2 := srv2.cfg.Runner.(*fakeRunner)
	w := doSSO(t, srv2, http.MethodPost, "/api/v1/runs", memberPreviewSession(t, true, true),
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

// ctxFromCookie drives a cookie through the REAL OIDC middleware and returns the
// context it published — the same context every handler downstream reads, and
// the only honest way to ask a ctx-keyed predicate a question in a test.
func ctxFromCookie(t *testing.T, srv *Server, cookie *http.Cookie) context.Context {
	t.Helper()
	var got context.Context
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = r.Context() })
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	srv.cfg.OIDC.Middleware(next).ServeHTTP(httptest.NewRecorder(), req)
	if got == nil {
		t.Fatal("the middleware published no context — the cookie did not authenticate")
	}
	return got
}

// TestMemberPreview_DispatchResolveReadsAbsent is F3: the DISPATCH-side claim,
// pinned directly. The create door always refuses a model run first, so no
// route reaches the provider's dispatch-time read inside the preview. This
// calls it on the context the real middleware publishes for each cookie, so it
// reds the moment the guard stops firing.
func TestMemberPreview_DispatchResolveReadsAbsent(t *testing.T) {
	srv, _, sec, _ := memberPreviewSrv(t)
	putPreviewSession(t, sec, memberPreviewAdminSub)
	p := awsSSOTestProvider()

	_, d, err := srv.providerBedrockRefusal(ctxFromCookie(t, srv, memberPreviewSession(t, true, true)),
		p, "claude-code", memberPreviewAdminSub, false)
	if err != nil || d.msg != connectDenial(p.ID, mpBRNotSignedIn).msg {
		t.Errorf("inside the preview dispatch read = %q, %v; want not signed in — the admin's own session credentials a run", d.msg, err)
	}

	// CONTROL: the identical call on the PLAIN cookie finds the owner's own
	// session, so the absence above is the guard and not an inert fixture.
	if _, d, err := srv.providerBedrockRefusal(ctxFromCookie(t, srv, memberPreviewSession(t, true, false)),
		p, "claude-code", memberPreviewAdminSub, false); err != nil || d.msg != "" {
		t.Fatalf("the plain mode must still find the owner's own session: %q, %v", d.msg, err)
	}
}
