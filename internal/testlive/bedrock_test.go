// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package testlive

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Account ids are built at run time: see redact_test.go.
var (
	memberAccount = strings.Repeat("1", 12)
	otherAccount  = strings.Repeat("2", 12)
)

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func validEnv() map[string]string {
	return map[string]string{
		EnvBedrockAccount: memberAccount, EnvBedrockRole: "role", EnvSSORegion: "us-east-1",
		EnvBedrockRegion: "us-east-1",
	}
}

func TestLoadBedrockConfig(t *testing.T) {
	c, err := LoadBedrockConfig(envOf(validEnv()))
	if err != nil || c.Model != DefaultModel || c.MaxCalls != DefaultMaxCalls {
		t.Fatalf("defaults: %+v, %v", c, err)
	}
	for _, tc := range []struct{ name, key, val string }{
		{"account not 12 digits", EnvBedrockAccount, "12345"},
		{"account unset", EnvBedrockAccount, ""},
		{"role unset", EnvBedrockRole, ""},
		{"model off the allow-list", EnvBedrockModel, "anthropic.claude-opus-4-1-20250805-v1:0"},
		{"model look-alike", EnvBedrockModel, "xus.amazon.nova-micro-v1:0"},
		{"max calls above the hard max", EnvBedrockMaxCalls, "21"},
		{"max calls zero", EnvBedrockMaxCalls, "0"},
		{"max calls not a number", EnvBedrockMaxCalls, "five"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := validEnv()
			env[tc.key] = tc.val
			if _, err := LoadBedrockConfig(envOf(env)); err == nil {
				t.Fatalf("%s=%q was accepted", tc.key, tc.val)
			}
		})
	}
	for _, m := range []string{"anthropic.claude-haiku-4-5-20251001-v1:0", "global.anthropic.claude-haiku-4-5-20251001-v1:0",
		"amazon.nova-micro-v1:0", "us.amazon.nova-micro-v1:0"} {
		env := validEnv()
		env[EnvBedrockModel] = m
		env[EnvBedrockMaxCalls] = "20"
		if _, err := LoadBedrockConfig(envOf(env)); err != nil {
			t.Errorf("allowed model %q refused: %v", m, err)
		}
	}
}

// fakeAWS stands in for the Identity Center portal, STS and Bedrock. STS
// answers with stsAccount; bedrockHits counts calls that reached the model.
func fakeAWS(t *testing.T, stsAccount string) (Endpoints, *atomic.Int64) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/federation/credentials":
			if r.URL.Query().Get("account_id") != memberAccount || r.Header.Get("x-amz-sso_bearer_token") != "sso-token" {
				http.Error(w, "unexpected GetRoleCredentials", http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"roleCredentials":{"accessKeyId":"id","secretAccessKey":"secret","sessionToken":"session"}}`))
		case r.URL.Path == "/":
			if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=id/") {
				http.Error(w, "unsigned", http.StatusForbidden)
				return
			}
			_, _ = w.Write([]byte(`<GetCallerIdentityResponse><GetCallerIdentityResult><Account>` + stsAccount +
				`</Account></GetCallerIdentityResult></GetCallerIdentityResponse>`))
		case strings.HasSuffix(r.URL.Path, "/converse"):
			hits.Add(1)
			_, _ = w.Write([]byte(`{"output":{"message":{"content":[{"text":"pong"}]}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return Endpoints{Portal: srv.URL, STS: srv.URL, Bedrock: srv.URL}, &hits
}

// TestNewMemberBedrock_RefusesAnotherAccount is the money guard: credentials
// that STS places in any account but the capped one never reach a model.
func TestNewMemberBedrock_RefusesAnotherAccount(t *testing.T) {
	cfg, err := LoadBedrockConfig(envOf(validEnv()))
	if err != nil {
		t.Fatal(err)
	}
	ep, hits := fakeAWS(t, otherAccount)
	b, err := NewMemberBedrock(context.Background(), cfg, "sso-token", ep, http.DefaultClient)
	if err == nil || b != nil || !strings.Contains(err.Error(), "refusing to call Bedrock") {
		t.Fatalf("a credential for another account was accepted: %v", err)
	}
	if strings.Contains(err.Error(), otherAccount) || strings.Contains(err.Error(), memberAccount) {
		t.Fatalf("the refusal prints an account id: %v", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("Bedrock was called %d times", hits.Load())
	}
}

func TestBedrock_BudgetAndMaxTokens(t *testing.T) {
	spent.Store(0)
	t.Cleanup(func() { spent.Store(0) })
	env := validEnv()
	env[EnvBedrockMaxCalls] = "2"
	cfg, err := LoadBedrockConfig(envOf(env))
	if err != nil {
		t.Fatal(err)
	}
	ep, hits := fakeAWS(t, memberAccount)
	b, err := NewMemberBedrock(context.Background(), cfg, "sso-token", ep, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Converse(context.Background(), "hi", MaxTokens+1); err == nil {
		t.Fatal("max_tokens above the ceiling was sent")
	}
	for i := 0; i < 2; i++ {
		if text, err := b.Converse(context.Background(), "hi", MaxTokens); err != nil || text != "pong" {
			t.Fatalf("call %d: %q, %v", i+1, text, err)
		}
	}
	if _, err := b.Converse(context.Background(), "hi", 8); err == nil || !strings.Contains(err.Error(), EnvBedrockMaxCalls) {
		t.Fatalf("a call past the budget was not refused: %v", err)
	}
	if hits.Load() != 2 {
		t.Fatalf("Bedrock saw %d calls, want 2", hits.Load())
	}
}

// TestSignV4_Vanilla is the get-vanilla case of AWS's published Signature
// Version 4 test suite.
func TestSignV4_Vanilla(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "https://example.amazonaws.com/", nil)
	now, _ := time.Parse("20060102T150405Z", "20150830T123600Z")
	SignV4(req, nil, Creds{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"},
		"us-east-1", "service", now)
	want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, " +
		"SignedHeaders=host;x-amz-date, Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestSignV4_DoubleEncodesPath(t *testing.T) {
	if got := awsEscape("v1%3A0"); got != "v1%253A0" {
		t.Fatalf("awsEscape = %q", got)
	}
}
