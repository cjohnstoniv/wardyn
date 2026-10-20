// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// The AWS CLI arm: the gate, dispatch's staging and the sink's re-derivation.

const (
	cliAWSID     = "6f0c1d2e-0000-4000-8000-0000000c0a05"
	cliAWSPortal = "portal.sso.eu-west-1.amazonaws.com"
	cliAWSToken  = "cli-live-sso-token-5be1"
)

func cliAWSRow() *types.CLIAWS {
	return &types.CLIAWS{StartURL: "https://acme.awsapps.com/start", SSORegion: "eu-west-1",
		AccountID: "123456789012", RoleName: "ReadOnly", Region: "eu-west-2"}
}

func cliAWSRowDef() types.ComponentDefinition {
	return types.ComponentDefinition{Hosts: []string{"sts.eu-west-2.amazonaws.com"},
		CLI: &types.ComponentCLI{Kind: types.CLIKindAWS, AWS: cliAWSRow()}}
}

func cliAWSBlob() awsSSOBlob {
	a := cliAWSRow()
	return awsSSOBlob{AccessToken: cliAWSToken, StartURL: a.StartURL, Region: a.SSORegion,
		AccountID: a.AccountID, RoleName: a.RoleName, ExpiresAt: time.Now().Add(2 * time.Hour).UTC(), CapturedAt: time.Now().UTC()}
}

func putCLIBlob(t *testing.T, sec *memSecrets, owner string, b awsSSOBlob) {
	t.Helper()
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := sec.For(owner).Put(context.Background(), cliSecretName(cliAWSID, providerSSOPart), raw); err != nil {
		t.Fatal(err)
	}
}

// cliAWSFixture is the component fixture with the AWS CLI row stored and the
// lane's switch on, as a deployment boots by default.
func cliAWSFixture(t *testing.T) (*componentFixture, map[string]any) {
	t.Helper()
	f := newComponentFixture(t)
	f.srv.cfg.AWSSSOProxyInject = true
	return f, f.org(cliAWSID, cliAWSRowDef())
}

// Through the create door: the sandbox gets the profile and an inert token
// cache, never the session; the portal is allowed, intercepted on :443 and
// carries the header from the run owner's own session, pinned to the row.
func TestAWSCLI_LaunchStagesProfileNotToken(t *testing.T) {
	f, ref := cliAWSFixture(t)
	putCLIBlob(t, f.srv.cfg.Secrets.(*memSecrets), capSub, cliAWSBlob())
	fr := f.srv.cfg.Runner.(*fakeRunner)
	if w := f.ask(t, componentDoors[0], componentBody(ref)); w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s, want 201", w.Code, w.Body.String())
	}
	fr.waitForSandbox(t)
	fr.mu.Lock()
	spec := fr.lastSpec
	fr.mu.Unlock()

	if spec.Env[envAWSProfile] != awsSSOProfileName || spec.Env[envAWSConfigFile] != sandboxAWSDir+"/config" {
		t.Errorf("AWS_PROFILE/AWS_CONFIG_FILE = %q/%q", spec.Env[envAWSProfile], spec.Env[envAWSConfigFile])
	}
	files := decodeSSOFiles(t, spec.Env[awsSSOConfigEnvVar])
	config := files[".aws/config"]
	for _, want := range []string{"[profile wardyn]", "sso_start_url = https://acme.awsapps.com/start", "sso_region = eu-west-1",
		"sso_account_id = 123456789012", "sso_role_name = ReadOnly", "region = eu-west-2"} {
		if !strings.Contains(config, want) {
			t.Errorf("~/.aws/config lacks %q:\n%s", want, config)
		}
	}
	var cache map[string]string
	if err := json.Unmarshal([]byte(files[".aws/sso/cache/"+awsSSOCacheFileName(awsSSOProfileName)+".json"]), &cache); err != nil {
		t.Fatal(err)
	}
	if cache["accessToken"] != awsSSOPlaceholderToken || cache["refreshToken"] != "" || cache["clientSecret"] != "" {
		t.Errorf("token cache = %v, want the inert placeholder and nothing to refresh with", cache)
	}
	if exp, err := time.Parse(time.RFC3339, cache["expiresAt"]); err != nil || time.Until(exp) < 7*24*time.Hour {
		t.Errorf("cache expiry = %q, want far out so the CLI never refreshes it", cache["expiresAt"])
	}
	for k, v := range spec.Env {
		if strings.Contains(v, cliAWSToken) {
			t.Errorf("Env[%s] carries the SSO token", k)
		}
	}
	pc := spec.ProxyConfig
	if !slices.Contains(pc.MITMHosts, cliAWSPortal+":443") || pc.MITMCACertPEM == "" {
		t.Errorf("MITMHosts = %v (CA %t), want the portal on :443 and a per-run CA", pc.MITMHosts, pc.MITMCACertPEM != "")
	}
	if !slices.Contains(pc.Policy.AllowedDomains, cliAWSPortal) {
		t.Errorf("allowed domains %v lack the portal", pc.Policy.AllowedDomains)
	}
	i := slices.IndexFunc(pc.Injection, func(g runner.InjectionGrant) bool { return g.Rule.Host == cliAWSPortal })
	if i < 0 {
		t.Fatalf("no injection rule on the portal: %+v", pc.Injection)
	}
	rule := pc.Injection[i].Rule
	if rule.Header != awsSSOInjectHeader || rule.SecretName != types.AWSSSOAccessTokenSecret || !rule.RequireTLS ||
		rule.PinPath != awsSSORoleCredentialsPath || rule.PinQuery["account_id"] != "123456789012" || rule.PinQuery["role_name"] != "ReadOnly" {
		t.Errorf("portal rule = %+v, want the session header pinned to the row's GetRoleCredentials", rule)
	}
}

// WARDYN_AWS_SSO_PROXY_INJECT off refuses an AWS CLI row at every door, and
// dispatch refuses it too, staging nothing: the off position would put the
// session in the sandbox, so the row is never served that way.
func TestAWSCLI_KillSwitchRefusesNotDegrades(t *testing.T) {
	f, ref := cliAWSFixture(t)
	f.srv.cfg.AWSSSOProxyInject = false
	putCLIBlob(t, f.srv.cfg.Secrets.(*memSecrets), capSub, cliAWSBlob())
	for _, door := range componentDoors {
		w := f.ask(t, door, componentBody(ref))
		if w.Code != http.StatusUnprocessableEntity || reasonOf(t, w) != reasonComponentCLIAWSInjectOff {
			t.Errorf("%s = %d %s, want 422 %s", door, w.Code, w.Body.String(), reasonComponentCLIAWSInjectOff)
		}
	}

	st := &cliRefuseStore{}
	s := &Server{cfg: Config{Store: st, Audit: &recRecorder{}, Now: time.Now}}
	env, policy := map[string]string{}, types.RunPolicySpec{}
	injections, mitm, _, ok := s.authorAWSCLI(context.Background(), types.AgentRun{ID: uuid.New()},
		&awsCLIDispatch{ComponentID: cliAWSID, AWS: *cliAWSRow()}, &policy, env, nil)
	if ok || len(injections) != 0 || len(mitm) != 0 {
		t.Fatalf("dispatch with the switch off = (%v, %v, %t), want a refusal", injections, mitm, ok)
	}
	if len(env) != 0 || len(policy.AllowedDomains) != 0 || len(st.grants) != 0 {
		t.Errorf("a refused dispatch staged env %v, hosts %v, grants %d", env, policy.AllowedDomains, len(st.grants))
	}
	if st.failed != 1 {
		t.Errorf("run marked FAILED %d times, want 1", st.failed)
	}
}

// cliRefuseStore records the grants dispatch authors and the STARTING->FAILED
// CAS a refusal makes (answered unapplied, so no cascade runs).
type cliRefuseStore struct {
	captureGrantStore
	failed int
}

func (c *cliRefuseStore) UpdateRunStateIf(_ context.Context, _ uuid.UUID, from, to types.RunState) (bool, error) {
	if from == types.RunStarting && to == types.RunFailed {
		c.failed++
	}
	return false, nil
}

// One sandbox has one AWS profile: a run already on the Bedrock sign-in lane
// is refused the CLI, rather than one silently replacing the other.
func TestAWSCLI_WithBedrockSignInRefused(t *testing.T) {
	st := &cliRefuseStore{}
	s := &Server{cfg: Config{Store: st, Audit: &recRecorder{}, AWSSSOProxyInject: true, Now: time.Now}}
	env := map[string]string{awsSSOConfigEnvVar: "bedrock's"}
	if _, _, _, ok := s.authorAWSCLI(context.Background(), types.AgentRun{ID: uuid.New()},
		&awsCLIDispatch{ComponentID: cliAWSID, AWS: *cliAWSRow()}, &types.RunPolicySpec{}, env, nil); ok || st.failed != 1 {
		t.Fatalf("ok = %t, failed = %d; want a refusal", ok, st.failed)
	}
	if env[awsSSOConfigEnvVar] != "bedrock's" || len(st.grants) != 0 {
		t.Errorf("the refusal replaced the Bedrock profile or authored a grant")
	}
}

// Not signed in: the launch and Review doors refuse; the preview lists a
// sign_in item for the row and shows the role credentials it would issue.
func TestAWSCLI_SignInRequired(t *testing.T) {
	f, ref := cliAWSFixture(t)
	for _, door := range componentDoors[:2] {
		w := f.ask(t, door, componentBody(ref))
		if w.Code != http.StatusUnprocessableEntity || reasonOf(t, w) != reasonComponentCLISignInRequired {
			t.Errorf("%s = %d %s, want 422 %s", door, w.Code, w.Body.String(), reasonComponentCLISignInRequired)
		}
	}
	ans := decodeAccess(t, componentDoors[2], f.ask(t, componentDoors[2], componentBody(ref)))
	i := slices.IndexFunc(ans.Access.Setup, func(it SetupItem) bool { return it.Fix != nil && it.Fix.Target == signInAWSCLI })
	if i < 0 || ans.Access.Setup[i].Fix.Action != fixSignIn || ans.Access.Setup[i].Fix.ID != cliAWSID {
		t.Fatalf("setup = %+v, want a sign_in item for the row", ans.Access.Setup)
	}
	row, ok := accessRow(ans.Access, cliAWSID)
	if !ok || row.Kind != accessKindCLI || row.Status != accessNeedsSetup {
		t.Errorf("row = %+v (%t), want the CLI row needing setup", row, ok)
	}
	c := slices.IndexFunc(ans.Access.Credentials, func(c client.AccessCredential) bool { return c.Kind == credKindAWSRole })
	if c < 0 || ans.Access.Credentials[c].Delivery != deliveryShortLived || !ans.Access.Credentials[c].Renewed ||
		ans.Access.Credentials[c].AfterEnd != afterEndExpires {
		t.Errorf("credentials = %+v, want short-lived renewed role credentials that expire", ans.Access.Credentials)
	}
	if h, ok := accessHost(ans.Access, cliAWSPortal); !ok || h.Via != viaInjected || h.Verdict != verdictAllowed {
		t.Errorf("portal host = %+v (%t), want injected and allowed", h, ok)
	}
}

// A run attaches one AWS CLI row.
func TestAWSCLI_SecondRowRefused(t *testing.T) {
	f, ref := cliAWSFixture(t)
	putCLIBlob(t, f.srv.cfg.Secrets.(*memSecrets), capSub, cliAWSBlob())
	second := f.org("6f0c1d2e-0000-4000-8000-0000000c0a06", cliAWSRowDef())
	w := f.ask(t, componentDoors[1], componentBody(ref, second))
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "second AWS CLI component") {
		t.Fatalf("two AWS CLI rows = %d %s, want 422", w.Code, w.Body.String())
	}
}

// A run whose model, or an included harness's, comes through an AWS sign-in is
// refused an AWS CLI component at every door, not first at dispatch: the step
// runs at all three and answers 422.
func TestAWSCLI_BedrockSignInRefusedAtTheDoors(t *testing.T) {
	i := slices.IndexFunc(runFoldSteps, func(st runFoldStep) bool {
		return strings.HasSuffix(runtime.FuncForPC(reflect.ValueOf(st.run).Pointer()).Name(), ".stepAWSCLIProfile")
	})
	if i < 0 || runFoldSteps[i].modes != foldAll {
		t.Fatalf("stepAWSCLIProfile is not a fold step at every door")
	}
	id := uuid.MustParse(cliAWSID)
	comps := runComponents{attached: []attachedComponent{{source: componentSourceOrg,
		snapshot: types.RunComponent{ComponentID: &id, Name: "AWS", Definition: cliAWSRowDef()}}}}
	sso := runProviderChoice{chosen: true, provider: types.ModelProvider{Kind: types.ModelProviderBedrockSSO}}
	key := runProviderChoice{chosen: true, provider: types.ModelProvider{Kind: types.ModelProviderAnthropicAPIKey}}
	for _, tc := range []struct {
		name    string
		primary runProviderChoice
		set     []harnessChoice
		comps   runComponents
		refused bool
	}{
		{"the run's provider is a Bedrock sign-in", sso, nil, comps, true},
		{"an included harness's is", key, []harnessChoice{{harness: "codex-cli", choice: sso}}, comps, true},
		{"a key provider", key, nil, comps, false},
		{"no AWS CLI component", sso, nil, runComponents{}, false},
	} {
		w := httptest.NewRecorder()
		f := &runFold{w: w, r: httptest.NewRequest(http.MethodPost, "/", nil), comps: tc.comps, mpChoice: tc.primary, harnessChoices: tc.set}
		ok := (&Server{}).stepAWSCLIProfile(f, &foldRecorder{})
		if ok == tc.refused {
			t.Errorf("%s: step passed = %t, want %t", tc.name, ok, !tc.refused)
		}
		if tc.refused && (w.Code != http.StatusUnprocessableEntity || reasonOf(t, w) != reasonComponentCLIAWSWithBedrockSignIn) {
			t.Errorf("%s: answer %d %s", tc.name, w.Code, w.Body.String())
		}
	}
}
