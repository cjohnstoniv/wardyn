// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"regexp"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/api"
)

// credentialSwitches are the credential protections that ship off by default: each variable, the value
// that turns it on, how wardynd reads the parsed flag, and the api.Config line main.go sets from it.
var credentialSwitches = []struct {
	env, on string
	read    func(f *bootFlags) bool
	main    string // a regexp over main.go
}{
	{"WARDYN_ENV_SECRET_PLACEHOLDERS", "true", func(f *bootFlags) bool { return *f.envSecretPlaceholders },
		`(?m)^\s*EnvSecretPlaceholders:\s+\*f\.envSecretPlaceholders,$`},
	{"WARDYN_FILE_SECRET_PLACEHOLDERS", "true", func(f *bootFlags) bool { return *f.fileSecretPlaceholders },
		`(?m)^\s*FileSecretPlaceholders:\s+\*f\.fileSecretPlaceholders,$`},
	{"WARDYN_RUNNER_SECRET_PLACEHOLDERS", "true", func(f *bootFlags) bool { return *f.runnerSecretPlaceholders },
		`(?m)^\s*RunnerSecretPlaceholders:\s+\*f\.runnerSecretPlaceholders,$`},
	{"WARDYN_PROXY_RESPONSE_SCRUB", "true", func(f *bootFlags) bool { return *f.proxyResponseScrub },
		`(?m)^\s*ScrubProxyResponses:\s+\*f\.proxyResponseScrub,$`},
	{"WARDYN_GIT_PAT_BROKER_OFF_REFUSED", "true", func(f *bootFlags) bool { return *f.gitPATBrokerOffRefused },
		`(?m)^\s*RefuseGitPATBrokerOff:\s+\*f\.gitPATBrokerOffRefused,$`},
	{"WARDYN_AWS_SIGV4_AT_PROXY", "on", func(f *bootFlags) bool { return api.ResolveAWSSigV4AtProxy(*f.awsSigV4AtProxy) },
		`(?m)^\s*AWSSigV4AtProxy:\s+api\.ResolveAWSSigV4AtProxy\(\*f\.awsSigV4AtProxy\),$`},
}

// TestCredentialSwitchesDefaultOff pins each credential switch off when its variable is unset, empty or
// set to an off spelling, and on when set to its on spelling. Turning one on by default fails the
// "unset" and "empty" cases of that switch only.
func TestCredentialSwitchesDefaultOff(t *testing.T) {
	oldArgs := os.Args
	t.Cleanup(func() { os.Args = oldArgs })
	for _, sw := range credentialSwitches {
		for _, c := range []struct {
			name string
			env  *string // nil: unset
			want bool
		}{
			{"unset", nil, false},
			{"empty", new(string), false},
			{"off", ptr("false"), false},
			{"on", ptr(sw.on), true},
		} {
			t.Run(sw.env+"/"+c.name, func(t *testing.T) {
				for _, other := range credentialSwitches {
					ensureUnset(t, other.env)
				}
				if c.env != nil {
					t.Setenv(sw.env, *c.env)
				}
				os.Args = []string{"wardynd-test"}
				resetFlags(t)
				if got := sw.read(parseBootFlags()); got != c.want {
					t.Fatalf("%s (%s) reads as on=%v, want %v", sw.env, c.name, got, c.want)
				}
			})
		}
	}
}

// TestCredentialSwitchesReachConfig pins main.go's line for each switch, so the readings above are what
// api.Config gets.
func TestCredentialSwitchesReachConfig(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, sw := range credentialSwitches {
		if !regexp.MustCompile(sw.main).Match(src) {
			t.Errorf("main.go no longer sets api.Config from %s as %s; update credentialSwitches", sw.env, sw.main)
		}
	}
}

func ptr(s string) *string { return &s }
