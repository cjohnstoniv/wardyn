// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestSSOPortalBase_HonoursEndpointOverride pins the LOGIN half of the AWS SSO
// test-endpoint hatch. The control plane already puts AWS_ENDPOINT_URL_SSO on
// the login sandbox (internal/api's ssoInjectEndpointEnv, via loginEnv) and
// already REPLACES the regional portal host in that run's egress allowlist
// (ssoEgressHosts) — so a portal base that ignores the variable dials a host
// the proxy is guaranteed to deny, and the capture dies with blob_shape. That
// is exactly what the kind SSO walk observed.
//
// Unset is the ONLY shape every real deployment ever has, so it is pinned
// first: no environment, no behaviour change, the regional URL.
func TestSSOPortalBase_HonoursEndpointOverride(t *testing.T) {
	const region = "us-east-1"
	const regional = "https://portal.sso.us-east-1.amazonaws.com"
	const fake = "http://wardyn-awsssofake.wardyn.svc.cluster.local:8090"

	cases := []struct {
		name string
		set  bool
		env  string
		want string
	}{
		{name: "unset is the regional portal", set: false, want: regional},
		{name: "empty falls back", set: true, env: "", want: regional},
		{name: "override is honoured", set: true, env: fake, want: fake},
		{name: "trailing slash is trimmed", set: true, env: fake + "/", want: fake},
		{name: "https override is honoured", set: true, env: "https://sso.example.test", want: "https://sso.example.test"},
		// Garbage falls back rather than dialing "" or a relative path: a
		// mistyped hatch on a test cluster must fail as "the real AWS was
		// denied", never as a request to nowhere.
		{name: "not a URL falls back", set: true, env: "not-a-url", want: regional},
		{name: "scheme-less host falls back", set: true, env: "wardyn-awsssofake:8090", want: regional},
		{name: "non-http scheme falls back", set: true, env: "ftp://sso.example.test", want: regional},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(awsEndpointURLSSOEnv, tc.env)
			if !tc.set {
				// t.Setenv cannot unset; it CAN register the restore, so
				// setting-then-unsetting leaves the parent process clean.
				if err := os.Unsetenv(awsEndpointURLSSOEnv); err != nil {
					t.Fatalf("unsetenv: %v", err)
				}
			}
			if got := ssoPortalBase(region); got != tc.want {
				t.Errorf("ssoPortalBase(%q) with %s=%q = %q, want %q", region, awsEndpointURLSSOEnv, tc.env, got, tc.want)
			}
		})
	}
}

// TestEndpointOverrideEnvVarParity is TestPinEnvVarParity's sibling for the
// endpoint variable: the daemon SETS AWS_ENDPOINT_URL_SSO on the login sandbox
// and this helper READS it, as two literals in two packages with nothing else
// tying them together. A rename on either side alone is silent — the helper
// goes back to dialing the real portal, which the hatch's own egress allowlist
// then denies, i.e. the defect this lane fixed.
func TestEndpointOverrideEnvVarParity(t *testing.T) {
	path := filepath.Join("..", "..", "internal", "api", "awssso_endpoint.go")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	m := regexp.MustCompile(`awsEndpointURLSSOEnv\s*=\s*"([^"]+)"`).FindStringSubmatch(string(src))
	if m == nil {
		t.Fatalf("no awsEndpointURLSSOEnv in %s", path)
	}
	if m[1] != awsEndpointURLSSOEnv {
		t.Errorf("endpoint env drift: daemon sets %q, helper reads %q", m[1], awsEndpointURLSSOEnv)
	}
}
