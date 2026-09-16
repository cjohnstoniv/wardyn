// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"reflect"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// WARDYN_AWS_SSO_ENDPOINT_OVERRIDE is ONE test-only knob that moves every AWS
// SSO derivation together. These tests are the contract:
//
//   - UNSET, every derivation is byte-identical to what it was before the knob
//     existed (the negative control that matters most — the knob must be
//     invisible on every real deployment);
//   - SET, ALL FIVE move: the dispatch egress hosts, the login-run egress hosts
//     (including the device.sso.<r> entry only that flow adds), the CreateToken
//     URL, the login sandbox env and the ssoInject sandbox env.
//
// Five things moving on one switch is the whole point. The gap this closes is
// that each of them hardcoded amazonaws.com in a different file, so a fake
// reachable by one was unreachable by the next.

// theOverride is a plain-HTTP in-cluster Service URL — what the kind SSO walk
// actually sets. http:// on purpose: AWS_ENDPOINT_URL_SSO/_OIDC accept it
// (proven empirically in test/awsssofake/docker.go against the real CLI), and
// the fake serves no TLS.
const theOverride = "http://wardyn-awsssofake.wardyn.svc.cluster.local:8090"

const theOverrideHost = "wardyn-awsssofake.wardyn.svc.cluster.local"

func TestSSOEgressHosts_OverrideUnsetIsByteIdentical(t *testing.T) {
	want := []string{"oidc.eu-west-2.amazonaws.com", "portal.sso.eu-west-2.amazonaws.com"}
	if got := ssoEgressHosts("eu-west-2", ""); !reflect.DeepEqual(got, want) {
		t.Errorf("ssoEgressHosts with no override = %v, want %v", got, want)
	}
}

func TestSSOEgressHosts_OverrideReplacesBoth(t *testing.T) {
	got := ssoEgressHosts("eu-west-2", theOverride)
	want := []string{theOverrideHost}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ssoEgressHosts with the override = %v, want exactly %v — ONE fake backs both services (their paths never collide), so both regional entries collapse to it", got, want)
	}
	for _, h := range got {
		if strings.Contains(h, "amazonaws.com") {
			t.Errorf("an amazonaws.com host survived the override: %q", h)
		}
	}
}

// The login run's egress is derived separately (harnessLogin.loginEgress) and
// adds device.sso.<r> on its own — the entry only the interactive device flow
// needs. It must follow the override too, or the containerized `aws sso login`
// is denied on the very host it dials.
func TestLoginEgress_FollowsTheOverride(t *testing.T) {
	hl, ok := agentHarnessLogin(awsSSOAgent)
	if !ok {
		t.Fatal("no aws-sso harness login row")
	}

	base := hl.loginEgress("eu-west-2", "")
	wantBase := []string{
		"*.awsapps.com",
		"oidc.eu-west-2.amazonaws.com",
		"portal.sso.eu-west-2.amazonaws.com",
		"device.sso.eu-west-2.amazonaws.com",
	}
	if !reflect.DeepEqual(base, wantBase) {
		t.Errorf("loginEgress with no override = %v, want %v (byte-identical to before the knob)", base, wantBase)
	}

	over := hl.loginEgress("eu-west-2", theOverride)
	wantOver := []string{"*.awsapps.com", theOverrideHost}
	if !reflect.DeepEqual(over, wantOver) {
		t.Errorf("loginEgress with the override = %v, want %v — all THREE regional entries (incl. device.sso) collapse to the one fake", over, wantOver)
	}
}

// The CreateToken endpoint a dispatch-time renewal dials.
func TestAWSSSOTokenEndpoint_FollowsTheOverride(t *testing.T) {
	plain := &Server{cfg: Config{}}
	if got, want := plain.awsSSOTokenEndpoint("eu-west-2"), "https://oidc.eu-west-2.amazonaws.com/token"; got != want {
		t.Errorf("awsSSOTokenEndpoint with no override = %q, want %q", got, want)
	}
	over := &Server{cfg: Config{AWSSSOEndpointOverride: theOverride}}
	if got, want := over.awsSSOTokenEndpoint("eu-west-2"), theOverride+"/token"; got != want {
		t.Errorf("awsSSOTokenEndpoint with the override = %q, want %q", got, want)
	}
}

// The LOGIN sandbox env: `aws sso login` inside the login box reads
// AWS_ENDPOINT_URL_SSO_OIDC/AWS_ENDPOINT_URL_SSO, and without them it dials the
// real AWS regardless of what the egress list says.
func TestLoginEnv_CarriesTheEndpointOverride(t *testing.T) {
	hl, ok := agentHarnessLogin(awsSSOAgent)
	if !ok {
		t.Fatal("no aws-sso harness login row")
	}

	plain := hl.loginEnv("https://fake.awsapps.com/start", "eu-west-2", awsSSOPin{}, "")
	for _, k := range []string{awsEndpointURLSSOEnv, awsEndpointURLSSOOIDCEnv} {
		if _, ok := plain[k]; ok {
			t.Errorf("loginEnv with no override carries %s — it must be byte-identical to before the knob", k)
		}
	}

	over := hl.loginEnv("https://fake.awsapps.com/start", "eu-west-2", awsSSOPin{}, theOverride)
	for _, k := range []string{awsEndpointURLSSOEnv, awsEndpointURLSSOOIDCEnv} {
		if over[k] != theOverride {
			t.Errorf("loginEnv[%s] = %q, want %q", k, over[k], theOverride)
		}
	}
	// The non-secret ~/.aws/config it already delivered is untouched.
	if over[awsSSOConfigEnvVar] == "" || over[awsSSOConfigEnvVar] != plain[awsSSOConfigEnvVar] {
		t.Errorf("the override changed the generated ~/.aws/config channel: %q vs %q", over[awsSSOConfigEnvVar], plain[awsSSOConfigEnvVar])
	}
}

// The ssoInject (captured-credential) sandbox env: the AWS SDK inside a Bedrock
// run resolves the SSO cache by calling GetRoleCredentials, and that call goes
// to AWS_ENDPOINT_URL_SSO.
func TestResolveBedrockAuth_SSOInjectCarriesTheEndpointOverride(t *testing.T) {
	for _, tc := range []struct {
		name     string
		override string
	}{
		{"unset", ""},
		{"set", theOverride},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := ssoInjectEndpointEnv(tc.override)
			if tc.override == "" {
				if len(env) != 0 {
					t.Fatalf("ssoInjectEndpointEnv(\"\") = %v, want empty — byte-identical to before the knob", env)
				}
				return
			}
			want := map[string]string{
				awsEndpointURLSSOEnv:     theOverride,
				awsEndpointURLSSOOIDCEnv: theOverride,
			}
			if !reflect.DeepEqual(env, want) {
				t.Errorf("ssoInjectEndpointEnv = %v, want %v", env, want)
			}
		})
	}
}

// ValidateAWSSSOEndpointOverride is the boot gate. Refuse-by-default is the
// whole posture: this knob makes a daemon trust an unsigned HTTP server as AWS
// IAM Identity Center, so it must never be reachable by setting ONE env var.
func TestValidateAWSSSOEndpointOverride(t *testing.T) {
	for _, tc := range []struct {
		name          string
		raw           string
		allowTest     bool
		want          string
		wantErrSubstr string
	}{
		{name: "empty is the default and needs no permission", raw: "", allowTest: false, want: ""},
		{name: "empty stays empty even with the permission", raw: "", allowTest: true, want: ""},
		{
			name: "set without the permission REFUSES", raw: theOverride, allowTest: false,
			wantErrSubstr: "WARDYN_ALLOW_TEST_ENDPOINTS",
		},
		{name: "plain http is accepted (the fake serves no TLS)", raw: theOverride, allowTest: true, want: theOverride},
		{name: "https is accepted", raw: "https://sso.test.internal", allowTest: true, want: "https://sso.test.internal"},
		{name: "one trailing slash is trimmed", raw: theOverride + "/", allowTest: true, want: theOverride},
		{name: "surrounding whitespace is trimmed", raw: "  " + theOverride + "  ", allowTest: true, want: theOverride},
		{name: "a bare host is refused", raw: "wardyn-awsssofake:8090", allowTest: true, wantErrSubstr: "http:// or https://"},
		{name: "a non-http scheme is refused", raw: "ftp://host", allowTest: true, wantErrSubstr: "http:// or https://"},
		{name: "embedded credentials are refused", raw: "http://u:p@host:8090", allowTest: true, wantErrSubstr: "credential"},
		{name: "an empty host is refused", raw: "http:///path", allowTest: true, wantErrSubstr: "host is empty"},
		{name: "a query string is refused", raw: "http://host:8090?a=b", allowTest: true, wantErrSubstr: "query"},
		{name: "a fragment is refused", raw: "http://host:8090#f", allowTest: true, wantErrSubstr: "fragment"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidateAWSSSOEndpointOverride(tc.raw, tc.allowTest)
			if tc.wantErrSubstr != "" {
				if err == nil {
					t.Fatalf("ValidateAWSSSOEndpointOverride(%q, %v) = %q, nil; want an error naming %q", tc.raw, tc.allowTest, got, tc.wantErrSubstr)
				}
				if !strings.Contains(err.Error(), tc.wantErrSubstr) {
					t.Errorf("error = %v, want it to name %q", err, tc.wantErrSubstr)
				}
				if got != "" {
					t.Errorf("a refused override returned %q, want \"\" (fail closed)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateAWSSSOEndpointOverride(%q, %v) errored: %v", tc.raw, tc.allowTest, err)
			}
			if got != tc.want {
				t.Errorf("= %q, want %q", got, tc.want)
			}
		})
	}
}

// The existing GLOBAL AWS_ENDPOINT_URL refusal is untouched: this knob is the
// two SSO-specific variables, never the one that re-points every AWS service.
// (bedrock_test.go and dispatch_siteconfig_test.go pin the same rule from the
// Bedrock side and stay green UNTOUCHED — this is the SSO-side restatement.)
func TestSSOEndpointOverride_IsNeverTheGlobalAWSEndpointURL(t *testing.T) {
	env := ssoInjectEndpointEnv(theOverride)
	if _, bad := env["AWS_ENDPOINT_URL"]; bad {
		t.Error("the SSO override set the GLOBAL AWS_ENDPOINT_URL — that re-points every AWS service, which is exactly what PF-45 refuses")
	}
	hl, _ := agentHarnessLogin(awsSSOAgent)
	if _, bad := hl.loginEnv("https://fake.awsapps.com/start", "eu-west-2", awsSSOPin{}, theOverride)["AWS_ENDPOINT_URL"]; bad {
		t.Error("the login sandbox env set the GLOBAL AWS_ENDPOINT_URL")
	}
}

// An override must not disturb the pin the login run already carries.
func TestLoginEnv_OverrideLeavesThePinAlone(t *testing.T) {
	hl, _ := agentHarnessLogin(awsSSOAgent)
	pin := awsSSOPin{AccountID: "222222222222", RoleName: "WardynDev"}
	plain := hl.loginEnv("https://fake.awsapps.com/start", "eu-west-2", pin, "")
	over := hl.loginEnv("https://fake.awsapps.com/start", "eu-west-2", pin, theOverride)
	for k, v := range plain {
		if over[k] != v {
			t.Errorf("the override changed pre-existing login env %s: %q -> %q", k, v, over[k])
		}
	}
}

// A guard against the knob leaking into the site-config surface: it is a BOOT
// flag, like WARDYN_BEDROCK_BASE_URL and for the same reason (PF-43).
func TestSSOEndpointOverride_IsNotASiteConfigField(t *testing.T) {
	var sc types.SiteConfig
	rt := reflect.TypeOf(sc)
	for i := range rt.NumField() {
		if strings.Contains(strings.ToLower(rt.Field(i).Name), "ssoendpoint") {
			t.Errorf("SiteConfig grew a field %q — the SSO endpoint override is boot-time-only", rt.Field(i).Name)
		}
	}
}
