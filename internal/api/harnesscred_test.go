// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// getErrStore is a secretstore.Store whose Get returns a fixed error (or value),
// so readHarnessBlob's error-classification branch can be exercised without a
// real backend.
type getErrStore struct {
	getErr error
	val    []byte
}

func (getErrStore) Name() string                                  { return "get-err" }
func (getErrStore) Put(context.Context, string, []byte) error     { return nil }
func (getErrStore) Delete(context.Context, string) error          { return nil }
func (getErrStore) List(context.Context) ([]string, error)        { return nil, nil }
func (s getErrStore) Get(context.Context, string) ([]byte, error) { return s.val, s.getErr }

// For is a no-op: this fake's whole purpose is a fixed Get outcome regardless
// of caller, and no test here scopes it by owner.
func (s getErrStore) For(string) secretstore.Store { return s }

func (getErrStore) DeleteEverywhere(context.Context, []string) (int, error) { return 0, nil }

func (getErrStore) Holders(context.Context, []string) (map[string][]string, error) { return nil, nil }

// TestReadHarnessBlob_InputClasses pins the read discipline of readHarnessBlob
// through its one reader (readAWSSSOBlob): absent, unparseable, and
// structurally-unusable blobs must NEVER read as connected, and only a
// non-ErrNotFound store failure may propagate. A `usable` predicate weakened to
// a bare non-empty check, or a parse error swallowed into found=false, fails
// here.
func TestReadHarnessBlob_InputClasses(t *testing.T) {
	goodSSO, _ := json.Marshal(awsSSOBlob{
		AccessToken: "sso-tok", StartURL: "https://d-1.awsapps.com/start", Region: "us-east-1",
		AccountID: "111122223333", RoleName: "Dev", ExpiresAt: time.Now().Add(time.Hour),
	})
	// A half-written capture: a real access token, but list-accounts came up
	// empty so account/role are missing — awsSSOBlob.valid refuses it, and it
	// must read as ABSENT rather than pre-empt the host-mode Bedrock lane.
	partialSSO, _ := json.Marshal(awsSSOBlob{
		AccessToken: "sso-tok", StartURL: "https://d-1.awsapps.com/start", Region: "us-east-1",
		ExpiresAt: time.Now().Add(time.Hour),
	})

	readers := []struct {
		name     string
		read     func(*Server, context.Context) (bool, error)
		good     []byte
		unusable []byte
	}{
		{
			name: "aws-sso",
			read: func(s *Server, ctx context.Context) (bool, error) {
				_, ok, err := s.readAWSSSOBlob(ctx, awsSSOScope{})
				return ok, err
			},
			good:     goodSSO,
			unusable: partialSSO,
		},
	}

	h := newHarness(t)
	for _, r := range readers {
		classes := []struct {
			name    string
			store   secretstore.Store
			wantOK  bool
			wantErr bool
		}{
			{"nil store is not-connected", nil, false, false},
			{"absent is not-connected", getErrStore{getErr: secretstore.ErrNotFound}, false, false},
			{"wrapped absent is not-connected", getErrStore{getErr: fmt.Errorf("pg: %w", secretstore.ErrNotFound)}, false, false},
			{"decrypt failure propagates", getErrStore{getErr: errors.New("age: no identity matched key")}, false, true},
			{"backend down propagates", getErrStore{getErr: errors.New("dial tcp: connection refused")}, false, true},
			{"malformed blob is an error", getErrStore{val: []byte("{not json")}, false, true},
			{"unusable shape reads as absent", getErrStore{val: r.unusable}, false, false},
			{"connected", getErrStore{val: r.good}, true, false},
		}
		for _, tc := range classes {
			t.Run(r.name+"/"+tc.name, func(t *testing.T) {
				cfg := h.srv.cfg
				cfg.Secrets = tc.store
				ok, err := r.read(New(cfg), context.Background())
				if ok != tc.wantOK {
					t.Fatalf("found = %v, want %v (err %v)", ok, tc.wantOK, err)
				}
				if (err != nil) != tc.wantErr {
					t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
				}
			})
		}
	}
}

func TestManagedSentinelCredsAreInert(t *testing.T) {
	raw, err := base64.StdEncoding.DecodeString(managedSentinelCredsB64())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	var d struct {
		ClaudeAiOauth struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    int64  `json:"expiresAt"`
		} `json:"claudeAiOauth"`
	}
	if uerr := json.Unmarshal(raw, &d); uerr != nil {
		t.Fatalf("unmarshal: %v", uerr)
	}
	if d.ClaudeAiOauth.AccessToken != managedSentinelAccessToken {
		t.Fatalf("access token is not the inert sentinel: %q", d.ClaudeAiOauth.AccessToken)
	}
	if d.ClaudeAiOauth.RefreshToken != "" {
		t.Fatal("sentinel must carry a BLANK refresh token")
	}
	if d.ClaudeAiOauth.ExpiresAt != 4102444800000 {
		t.Fatalf("sentinel expiry must be pinned far out, got %d", d.ClaudeAiOauth.ExpiresAt)
	}
}

func TestHarnessSecretIsReserved(t *testing.T) {
	// reservedSecret covers the wardyn-harness-*-oauth PATTERN, so the retired
	// sign-in blobs and the per-person Azure DevOps ones stay out of the
	// generic secrets API and the injection sink, and so does any future name
	// harnessCredSecretName generates.
	if !reservedSecret(harnessCredSecretName("codex")) {
		t.Fatal("reservedSecret must cover the wardyn-harness-<provider>-oauth pattern for future providers")
	}
}

// HTTP-router-level tests (through the real mux + humanOrAdminAuth)

// harnessCredSrv builds a Server with the harness login/credential routes MOUNTED
// (they mount only when cfg.Secrets != nil) over the given secret store, reusing
// the harness's embedded identity + audit recorder so audit assertions work.
func harnessCredSrv(t *testing.T, sec secretstore.Store) (*harness, *Server) {
	t.Helper()
	h := newHarness(t)
	cfg := h.srv.cfg
	cfg.Secrets = sec
	srv := New(cfg)
	h.srv = srv
	return h, srv
}

func auditHas(events []types.AuditEvent, action string) bool {
	for _, e := range events {
		if e.Action == action {
			return true
		}
	}
	return false
}

// TestHarnessRoutes_RetiredDoorsAreGone: the operator-wide container-login
// launch, the token paste and the disconnect are gone — every person signs in
// through their model provider's own door. Nothing answers on the old paths,
// for an admin or anyone else.
func TestHarnessRoutes_RetiredDoorsAreGone(t *testing.T) {
	_, srv := harnessCredSrv(t, &memSecrets{m: map[string][]byte{}})
	for _, rt := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/setup/harness-login"},
		{http.MethodPut, "/api/v1/setup/harness-credential/anthropic"},
		{http.MethodDelete, "/api/v1/setup/harness-credential/anthropic"},
		{http.MethodDelete, "/api/v1/setup/harness-credential/aws"},
	} {
		if w := do(t, srv, rt.method, rt.path, adminToken, `{"provider":"aws","token":"sk-ant-oat01-x"}`); w.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404 — the retired door must not answer; body=%s", rt.method, rt.path, w.Code, w.Body.String())
		}
	}
}

// TestHarnessLoginEgress_MatchableByTheProxy pins the ONE property every
// harness-login egress entry must have: the proxy's matcher must actually be
// able to match it. That matcher (classifyDomain, internal/egress/proxy) knows
// exactly two forms — a LEADING "*." suffix, or an exact host — so a mid-label
// wildcard ("oidc.*.amazonaws.com") silently degrades into an exact hostname no
// real request can ever equal. It looks like a pre-allow, allows nothing, and
// the flow it exists for is denied on the very hosts it names.
//
// The check is deliberately GENERAL (the whole table, every row, empty and
// populated regions), not a golden list: any future row that reintroduces the
// shape fails here. Teeth: the real builtin evaluator is asked whether a
// CONCRETE host derived from each entry (every "*" label replaced by a real
// label) is allowed — the old strings fail this because
// "oidc.us-east-1.amazonaws.com" never equals the literal "oidc.*.amazonaws.com".
func TestHarnessLoginEgress_MatchableByTheProxy(t *testing.T) {
	for _, agent := range []string{"claude-code", awsSSOAgent} {
		hl, ok := agentHarnessLogin(agent)
		if !ok {
			t.Fatalf("%s must support container login", agent)
		}
		for _, region := range []string{"", "us-east-1", "eu-west-2"} {
			hosts := hl.loginEgress(region, "")
			ev := proxy.NewBuiltinEvaluator(types.RunPolicySpec{AllowedDomains: hosts})
			for _, entry := range hosts {
				// A "*" anywhere but a leading "*." is the defect: classifyDomain
				// keeps it verbatim as an exact host.
				if strings.Contains(strings.TrimPrefix(entry, "*."), "*") {
					t.Errorf("%s/%q: egress entry %q has a mid-label wildcard — the proxy compiles it to an unmatchable exact host",
						agent, region, entry)
					continue
				}
				probe := strings.ReplaceAll(entry, "*", "wardyn-probe")
				v, err := ev.EvaluateHost(context.Background(), egress.Request{Host: probe, Port: 443})
				if err != nil {
					t.Fatalf("evaluate %q: %v", probe, err)
				}
				if v != egress.VerdictAllow {
					t.Errorf("%s/%q: entry %q does not allow %q (verdict %v) — it allows nothing real",
						agent, region, entry, probe, v)
				}
			}
		}
	}
}

// TestHarnessLoginEgress_AWSSSORegionScoped pins the resolved content: with a
// region the three region-scoped SSO endpoints are exact hosts for THAT region,
// and with no configured region nothing regional (and nothing wider, e.g.
// "*.amazonaws.com") is pre-allowed — those hosts go through first-use approval
// instead.
func TestHarnessLoginEgress_AWSSSORegionScoped(t *testing.T) {
	hl, _ := agentHarnessLogin(awsSSOAgent)

	got := hl.loginEgress("eu-west-2", "")
	for _, want := range []string{
		"*.awsapps.com",
		"oidc.eu-west-2.amazonaws.com",
		"portal.sso.eu-west-2.amazonaws.com",
		"device.sso.eu-west-2.amazonaws.com",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("login egress %v is missing %q", got, want)
		}
	}

	bare := hl.loginEgress("", "")
	if len(bare) != 1 || bare[0] != "*.awsapps.com" {
		t.Fatalf("with no configured SSO region the login must pre-allow only the org portal, got %v", bare)
	}
	// The table row itself must stay region-free — a static regional host would
	// be wrong for every other operator.
	for _, entry := range hl.egress {
		if strings.Contains(entry, "amazonaws.com") {
			t.Errorf("static row entry %q is region-scoped; derive it in loginEgress instead", entry)
		}
	}
}

func TestAgentHarnessLogin(t *testing.T) {
	hl, ok := agentHarnessLogin("claude-code")
	if !ok {
		t.Fatal("claude-code must support container login")
	}
	if hl.tokenPrefix != "sk-ant-oat" {
		t.Fatalf("wrong token prefix: %q", hl.tokenPrefix)
	}
	// Codex has no container-login path in v1.
	if _, ok := agentHarnessLogin("codex-cli"); ok {
		t.Fatal("codex-cli must NOT support container login in v1")
	}
}

// TestValidateSSOStartURL: the one operator-supplied value that gets written
// into a file inside the sandbox. A newline would smuggle extra keys into the
// generated INI, so whitespace is rejected outright; everything non-https is
// rejected because the login flow only ever dials https.
func TestValidateSSOStartURL(t *testing.T) {
	for _, ok := range []string{
		"https://my-org.awsapps.com/start",
		"https://my-org.awsapps.com/start#/",
		"https://identitycenter.amazonaws.com/ssoins-1234567890abcdef",
	} {
		if err := validateSSOStartURL(ok); err != nil {
			t.Errorf("validateSSOStartURL(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{
		"",
		"my-org.awsapps.com/start",
		"http://my-org.awsapps.com/start",
		"https://my-org.awsapps.com/start\nsso_region = attacker-region",
		"https://my-org.awsapps.com/start sso_region = x",
	} {
		if err := validateSSOStartURL(bad); err == nil {
			t.Errorf("validateSSOStartURL(%q) = nil, want an error", bad)
		}
	}
}

// TestAWSSSOLoginConfigIsPreLoginOnly: the login sandbox gets CONFIGURATION and
// nothing else — the sso-session block `aws sso login --sso-session wardyn`
// reads, with no profile, no account/role (unknown before the login) and above
// all no token cache. It exists to OBTAIN a credential, so it must never be
// handed one.
func TestAWSSSOLoginConfigIsPreLoginOnly(t *testing.T) {
	got := awsSSOLoginConfigFileContents("https://my-org.awsapps.com/start", "eu-west-2")
	for _, want := range []string{
		"[sso-session " + awsSSOProfileName + "]",
		"sso_start_url = https://my-org.awsapps.com/start",
		"sso_region = eu-west-2",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("pre-login config is missing %q:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"accessToken", "sso_account_id", "sso_role_name", "[profile"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("pre-login config leaks post-login material %q:\n%s", unwanted, got)
		}
	}
}

// TestLoginConfigEnv: the AWS login sandbox is seeded with the pre-login
// ~/.aws/config through the SAME env channel a Bedrock run uses, and with
// NOTHING when either half is unknown or the provider isn't AWS.
func TestLoginConfigEnv(t *testing.T) {
	aws, _ := agentHarnessLogin(awsSSOAgent)
	anthropic, _ := agentHarnessLogin("claude-code")

	env := aws.loginConfigEnv("https://my-org.awsapps.com/start", "eu-west-2")
	rec := env[awsSSOConfigEnvVar]
	if rec == "" {
		t.Fatalf("aws login env is missing %s: %v", awsSSOConfigEnvVar, env)
	}
	if len(env) != 1 {
		t.Errorf("login env carries more than the config record: %v", env)
	}
	relpath, b64, ok := strings.Cut(strings.TrimSpace(rec), "\t")
	if !ok || relpath != ".aws/config" {
		t.Fatalf("record = %q, want a single \".aws/config\\t<base64>\" line", rec)
	}
	raw, derr := base64.StdEncoding.DecodeString(b64)
	if derr != nil {
		t.Fatalf("decode seeded config: %v", derr)
	}
	if got := string(raw); !strings.Contains(got, "sso_start_url = https://my-org.awsapps.com/start") ||
		!strings.Contains(got, "sso_region = eu-west-2") {
		t.Errorf("seeded config does not carry the start URL + region:\n%s", got)
	}

	if got := aws.loginConfigEnv("", "eu-west-2"); got != nil {
		t.Errorf("no start URL must seed nothing, got %v", got)
	}
	if got := aws.loginConfigEnv("https://my-org.awsapps.com/start", ""); got != nil {
		t.Errorf("no region must seed nothing, got %v", got)
	}
	if got := anthropic.loginConfigEnv("https://my-org.awsapps.com/start", "eu-west-2"); got != nil {
		t.Errorf("a non-AWS login must never get an ~/.aws, got %v", got)
	}
}

// TestLaunchHarnessLoginRun_SeedsPinEnv: a sign-in to a pinned bedrock_sso
// provider seeds the pin into the login sandbox's env AND stamps it on the
// run's harness.login.start row.
//
// Both halves matter and they are different claims. The ENV is what lets the
// in-sandbox helper verify the pin against the portal instead of taking
// AccountList[0]; the STAMP is what lets the upload refuse a blob that
// disagrees, and it is read back from the audit row rather than the live
// provider so an edit mid-login cannot re-point a capture already in flight.
func TestLaunchHarnessLoginRun_SeedsPinEnv(t *testing.T) {
	site := credentialSite(ssoProvider())
	p := site.ModelProviders.Providers[0]
	srv, _, audit, _ := signInFixture(t, nil, site)
	runner := srv.cfg.Runner.(*fakeRunner)
	if code, body := signIn(t, srv, ssoSession(t, "sub-member", "member@corp.example", oidc.RoleUser), p.ID); code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", code, body)
	}

	// The POST answers before dispatch (P5), so the spec this reads is composed
	// by the launch goroutine a beat later: waitForSandbox returns when
	// CreateSandbox has actually been called.
	runner.waitForSandbox(t)
	env := runner.lastSandboxEnv()
	if env[awsSSOPinAccountEnvVar] != p.Bedrock.SSOAccountID || env[awsSSOPinRoleEnvVar] != p.Bedrock.SSORoleName {
		t.Errorf("sandbox env = %v, want the pin in %s/%s — without it the helper is back to AccountList[0]",
			env, awsSSOPinAccountEnvVar, awsSSOPinRoleEnvVar)
	}
	// The pre-login ~/.aws/config still rides the same channel; the pin is
	// ADDITIONAL, never a replacement.
	if env[awsSSOConfigEnvVar] == "" {
		t.Errorf("sandbox env = %v, lost the pre-login ~/.aws/config", env)
	}

	stamp := loginStartedStamp(t, audit)
	if stamp.SSOAccountID != p.Bedrock.SSOAccountID || stamp.SSORoleName != p.Bedrock.SSORoleName {
		t.Errorf("harness.login.start stamp = %+v, want the launch-time pin — the upload binds to THIS, not to the live provider", stamp)
	}
}

// TestLoginConfigEnv_NoPinNoEnv: an UNPINNED provider seeds neither variable,
// so a deployment that never pins launches byte-for-byte the sandbox it always
// did.
func TestLoginConfigEnv_NoPinNoEnv(t *testing.T) {
	unpinned := ssoProvider()
	unpinned.Bedrock.SSOAccountID, unpinned.Bedrock.SSORoleName = "", ""
	site := credentialSite(unpinned)
	srv, _, audit, _ := signInFixture(t, nil, site)
	runner := srv.cfg.Runner.(*fakeRunner)
	if code, body := signIn(t, srv, ssoSession(t, "sub-member", "member@corp.example", oidc.RoleUser), unpinned.ID); code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", code, body)
	}
	runner.waitForSandbox(t) // see TestLaunchHarnessLoginRun_SeedsPinEnv
	env := runner.lastSandboxEnv()
	if _, ok := env[awsSSOPinAccountEnvVar]; ok {
		t.Errorf("an unpinned provider seeded %s: %v", awsSSOPinAccountEnvVar, env)
	}
	if _, ok := env[awsSSOPinRoleEnvVar]; ok {
		t.Errorf("an unpinned provider seeded %s: %v", awsSSOPinRoleEnvVar, env)
	}
	if stamp := loginStartedStamp(t, audit); stamp.SSOAccountID != "" || stamp.SSORoleName != "" {
		t.Errorf("an unpinned launch stamped %+v, want no pin — the upload reads an empty stamp pin as 'launched unpinned'", stamp)
	}
}

// loginStartedStamp decodes the launch-time stamp off this run's own
// harness.login.start row — the same read handleUploadSSOToken makes.
func loginStartedStamp(t *testing.T, audit *memAudit) loginRunStamp {
	t.Helper()
	rows := audit.find("harness.login.start")
	if len(rows) != 1 {
		t.Fatalf("harness.login.start rows = %d, want 1", len(rows))
	}
	var stamp loginRunStamp
	if err := json.Unmarshal(rows[0].Data, &stamp); err != nil {
		t.Fatalf("decode harness.login.start data: %v", err)
	}
	return stamp
}

// TestLoginEnv_PinWithNoConfigEnvDoesNotPanic is C-02: the pin is merged into
// the map loginConfigEnv returned, and that map is NIL for a non-AWS flow or a
// half-known config — `maps.Copy` into a nil map panics.
//
// Unreachable through the HTTP door today only because handleProviderSignIn
// refuses a provider with no start URL / region first and validateProviderBedrock
// forbids a pin off a bedrock_sso provider: the safety was two validators away from the panic. This
// calls the merge directly, past both, which is how a future caller reaches it.
func TestLoginEnv_PinWithNoConfigEnvDoesNotPanic(t *testing.T) {
	aws, _ := agentHarnessLogin(awsSSOAgent)
	anthropic, _ := agentHarnessLogin("claude-code")
	pin := awsSSOPin{AccountID: "111111111111", RoleName: "BedrockRunner"}

	for name, env := range map[string]map[string]string{
		// Each of these makes loginConfigEnv return nil.
		"no region":      aws.loginEnv("https://acme.awsapps.com/start", "", pin, ""),
		"no start URL":   aws.loginEnv("", "us-east-1", pin, ""),
		"a non-AWS flow": anthropic.loginEnv("https://acme.awsapps.com/start", "us-east-1", pin, ""),
	} {
		t.Run(name, func(t *testing.T) {
			if env[awsSSOPinAccountEnvVar] != "111111111111" || env[awsSSOPinRoleEnvVar] != "BedrockRunner" {
				t.Errorf("env = %v, want the pin carried even with no ~/.aws/config beside it", env)
			}
		})
	}

	// The whole env, both halves, on the real path.
	full := aws.loginEnv("https://acme.awsapps.com/start", "us-east-1", pin, "")
	if full[awsSSOConfigEnvVar] == "" || full[awsSSOPinAccountEnvVar] == "" {
		t.Errorf("env = %v, want the pre-login config AND the pin", full)
	}
	// And an unpinned launch is byte-identical to what it always was.
	if got := aws.loginEnv("https://acme.awsapps.com/start", "us-east-1", awsSSOPin{}, ""); len(got) != 1 {
		t.Errorf("unpinned env = %v, want only the pre-login ~/.aws/config record", got)
	}
	if got := anthropic.loginEnv("", "", awsSSOPin{}, ""); got != nil {
		t.Errorf("an unpinned non-AWS login seeded %v, want nil", got)
	}
}

// awsSSOScopeDeleteBlob builds a minimal, structurally-valid captured blob so
// readAWSSSOBlob's valid() shape check passes, distinguished by access token.
func awsSSOScopeDeleteBlob(access string) awsSSOBlob {
	return awsSSOBlob{
		AccessToken: access, StartURL: "https://acme.awsapps.com/start", Region: "us-east-1",
		AccountID: "123456789012", RoleName: "WardynBedrockRole", ExpiresAt: awsSSOTestFixedNow.Add(time.Hour),
	}
}

// TestDeleteSpentAWSSSOBlob_ProviderScopeDeletesOnlyItsOwnRow pins dcc0c9622:
// after the model-provider stack, a provider-scoped scope's session lives
// under scope.ssoSecret() (wardyn-provider-<uid>-sso), not the roster's
// harnessCredSecretName(awsSSOProvider). A provider refresh AWS refuses must
// delete only the provider's own row and leave the same person's roster AWS
// sign-in (harnessCredSecretName(awsSSOProvider)) alone.
func TestDeleteSpentAWSSSOBlob_ProviderScopeDeletesOnlyItsOwnRow(t *testing.T) {
	ctx := context.Background()
	s := &Server{cfg: Config{Secrets: &memSecrets{}, Now: func() time.Time { return awsSSOTestFixedNow }}}
	const owner = "alice@example.com"
	rosterScope := awsSSOScope{perUser: true, owner: owner}
	providerScope := awsSSOScope{perUser: true, owner: owner, provider: uuid.NewString()}

	if err := s.storeAWSSSOBlob(ctx, rosterScope, awsSSOScopeDeleteBlob("roster-access-token-1234567890")); err != nil {
		t.Fatalf("seed roster row: %v", err)
	}
	if err := s.storeAWSSSOBlob(ctx, providerScope, awsSSOScopeDeleteBlob("provider-access-token-123456789")); err != nil {
		t.Fatalf("seed provider row: %v", err)
	}

	s.deleteSpentAWSSSOBlob(ctx, providerScope)

	if _, found, err := s.readAWSSSOBlob(ctx, providerScope); err != nil || found {
		t.Errorf("provider row after its own refresh was refused: found=%v err=%v, want gone", found, err)
	}
	roster, found, err := s.readAWSSSOBlob(ctx, rosterScope)
	if err != nil || !found {
		t.Fatalf("roster row after a PROVIDER-scoped refusal: found=%v err=%v, want intact", found, err)
	}
	if roster.AccessToken != "roster-access-token-1234567890" {
		t.Errorf("roster row AccessToken = %q, want the original — a provider-scoped delete touched it", roster.AccessToken)
	}
}

// #1100: the sign-in sandbox gets its own small default, an operator-configured
// override, and stays clamped to the acting principal's governance ceiling.

// TestHarnessLoginResources_Default: no config, no ceiling cap — the compiled-in
// small default (500m/512Mi), never runner.DefaultCPUMillis/DefaultMemoryMiB
// (2000/4096, sized for an agent run).
func TestHarnessLoginResources_Default(t *testing.T) {
	got := harnessLoginResources(Config{}, governanceCeiling{})
	if got.CPUMillis != defaultHarnessLoginCPUMillis || got.MemoryMiB != defaultHarnessLoginMemoryMiB {
		t.Errorf("harnessLoginResources(zero cfg, zero ceiling) = %+v, want {CPUMillis:%d MemoryMiB:%d}",
			got, defaultHarnessLoginCPUMillis, defaultHarnessLoginMemoryMiB)
	}
}

// TestHarnessLoginResources_ConfiguredOverride: a non-zero Config field wins
// over the compiled-in default, under a ceiling that sets no Resources cap.
func TestHarnessLoginResources_ConfiguredOverride(t *testing.T) {
	cfg := Config{HarnessLoginCPUMillis: 750, HarnessLoginMemoryMiB: 1024}
	got := harnessLoginResources(cfg, governanceCeiling{})
	if got.CPUMillis != 750 || got.MemoryMiB != 1024 {
		t.Errorf("harnessLoginResources(configured override) = %+v, want {CPUMillis:750 MemoryMiB:1024}", got)
	}
}

// TestHarnessLoginResources_ClampedToCeiling: the admin-set governance ceiling
// still applies — an operator override (or the default) above the ceiling's own
// Resources cap is clamped DOWN, never bypassed.
func TestHarnessLoginResources_ClampedToCeiling(t *testing.T) {
	cfg := Config{HarnessLoginCPUMillis: 750, HarnessLoginMemoryMiB: 1024}
	ceiling := governanceCeiling{Spec: types.RunPolicySpec{
		Resources: &types.ResourceLimits{CPUMillis: 300, MemoryMiB: 256},
	}}
	got := harnessLoginResources(cfg, ceiling)
	if got.CPUMillis != 300 || got.MemoryMiB != 256 {
		t.Errorf("harnessLoginResources under a 300m/256Mi ceiling = %+v, want {CPUMillis:300 MemoryMiB:256} — "+
			"the sign-in run must not bypass governance", got)
	}
}

// TestHarnessLoginResources_CeilingNeverRaises: a ceiling ABOVE the requested
// size never raises the request — clamping is a ceiling, not a floor.
func TestHarnessLoginResources_CeilingNeverRaises(t *testing.T) {
	ceiling := governanceCeiling{Spec: types.RunPolicySpec{
		Resources: &types.ResourceLimits{CPUMillis: 4000, MemoryMiB: 8192},
	}}
	got := harnessLoginResources(Config{}, ceiling)
	if got.CPUMillis != defaultHarnessLoginCPUMillis || got.MemoryMiB != defaultHarnessLoginMemoryMiB {
		t.Errorf("harnessLoginResources under a wide-open ceiling = %+v, want the small default {CPUMillis:%d MemoryMiB:%d} unchanged",
			got, defaultHarnessLoginCPUMillis, defaultHarnessLoginMemoryMiB)
	}
}

// TestLaunchHarnessLoginRun_DefaultResources: end to end through a provider's
// sign-in door — the composed SandboxSpec carries the small default rather than
// runner.DefaultCPUMillis/DefaultMemoryMiB.
func TestLaunchHarnessLoginRun_DefaultResources(t *testing.T) {
	site := credentialSite(ssoProvider())
	srv, _, _, _ := signInFixture(t, nil, site)
	runner := srv.cfg.Runner.(*fakeRunner)
	if code, body := signIn(t, srv, ssoSession(t, "sub-member", "member@corp.example", oidc.RoleUser), "bedrock-prod"); code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", code, body)
	}
	runner.waitForSandbox(t)
	res := runner.lastSpec.Resources
	if res.CPUMillis != defaultHarnessLoginCPUMillis || res.MemoryMiB != defaultHarnessLoginMemoryMiB {
		t.Errorf("sign-in SandboxSpec.Resources = %+v, want {CPUMillis:%d MemoryMiB:%d}",
			res, defaultHarnessLoginCPUMillis, defaultHarnessLoginMemoryMiB)
	}
}

// TestLaunchHarnessLoginRun_ConfiguredResources: the WARDYN_HARNESS_LOGIN_*
// override (here simulated the way boot wires it: a non-zero Config field)
// reaches the composed SandboxSpec.
func TestLaunchHarnessLoginRun_ConfiguredResources(t *testing.T) {
	site := credentialSite(ssoProvider())
	srv, _, _, _ := signInFixture(t, nil, site)
	srv.cfg.HarnessLoginCPUMillis = 750
	srv.cfg.HarnessLoginMemoryMiB = 1024
	runner := srv.cfg.Runner.(*fakeRunner)
	if code, body := signIn(t, srv, ssoSession(t, "sub-member", "member@corp.example", oidc.RoleUser), "bedrock-prod"); code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", code, body)
	}
	runner.waitForSandbox(t)
	res := runner.lastSpec.Resources
	if res.CPUMillis != 750 || res.MemoryMiB != 1024 {
		t.Errorf("sign-in SandboxSpec.Resources = %+v, want {CPUMillis:750 MemoryMiB:1024} (the configured override)", res)
	}
}
