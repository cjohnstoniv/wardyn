// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestRetiredModelEnvRefusal pins which values of a retired model variable
// refuse boot: unset, empty and the 0.7 compose defaults ("false", "off")
// configure nothing and boot; any real value refuses, naming every variable
// set and Settings → Model providers. The hatches that stay (the AWS SSO test
// endpoint pair and the proxy-inject switch) are not retired.
func TestRetiredModelEnvRefusal(t *testing.T) {
	retired := append([]string{
		"WARDYN_ANTHROPIC_BASE_URL", "WARDYN_ANTHROPIC_GATEWAY_HEADER", "WARDYN_ANTHROPIC_GATEWAY_FORMAT",
		"WARDYN_OPENAI_BASE_URL", "WARDYN_OPENAI_GATEWAY_HEADER", "WARDYN_OPENAI_GATEWAY_FORMAT",
		"WARDYN_BEDROCK_REGION", "WARDYN_BEDROCK_MODEL", "WARDYN_BEDROCK_BASE_URL",
		"WARDYN_BEDROCK_AWS_PROFILE", "WARDYN_BEDROCK_AWS_SSO_REGION", "WARDYN_BEDROCK_AWS_DIR",
	}, retiredModelEnvNames...)
	with := func(value string) []string {
		env := []string{"PATH=/usr/bin", "WARDYN_AWS_SSO_PROXY_INJECT=on", "WARDYN_ALLOW_TEST_ENDPOINTS=true"}
		for _, name := range retired {
			env = append(env, name+"="+value)
		}
		return env
	}
	for _, tc := range []struct {
		name    string
		environ []string
		refuses bool
	}{
		{"unset", []string{"PATH=/usr/bin", "WARDYN_AWS_SSO_PROXY_INJECT=off"}, false},
		{"empty", with(""), false},
		{"retired default false", with("false"), false},
		{"retired default off", with("off"), false},
		{"retired default, spaced and upper-case", with(" OFF "), false},
		{"real values", with("https://gw.corp.example"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := refuseRetiredModelEnv(tc.environ)
			if !tc.refuses {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("booted with every retired variable set to a real value")
			}
			for _, name := range retired {
				if !strings.Contains(err.Error(), name) {
					t.Errorf("refusal does not name %s: %v", name, err)
				}
			}
			if !strings.Contains(err.Error(), "Settings → Model providers") {
				t.Errorf("refusal does not name Settings → Model providers: %v", err)
			}
		})
	}
	// One real value among inert ones names that one only.
	err := refuseRetiredModelEnv([]string{"WARDYN_SUBSCRIPTION_INJECT=off", "WARDYN_BEDROCK_MODEL=us.anthropic.claude-x"})
	if err == nil || !strings.Contains(err.Error(), "WARDYN_BEDROCK_MODEL") || strings.Contains(err.Error(), "WARDYN_SUBSCRIPTION_INJECT") {
		t.Fatalf("err = %v, want a refusal naming WARDYN_BEDROCK_MODEL alone", err)
	}
}

// TestBoot_RefusesRetiredModelEnv drives the real boot path, run(), with the
// environment a 0.7 Helm values.yaml renders into the wardynd container (the
// chart copies env and extraEnv verbatim): boot refuses before it opens the
// database, naming the retired variables.
func TestBoot_RefusesRetiredModelEnv(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "values-0.7.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var values struct {
		Env      map[string]string `yaml:"env"`
		ExtraEnv []struct {
			Name  string `yaml:"name"`
			Value string `yaml:"value"`
		} `yaml:"extraEnv"`
	}
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatal(err)
	}
	var set []string
	for name, value := range values.Env {
		t.Setenv(name, value)
		set = append(set, name)
	}
	for _, e := range values.ExtraEnv {
		t.Setenv(e.Name, e.Value)
		set = append(set, e.Name)
	}
	// Unreachable on purpose: a boot that got past the refusal would fail on it
	// instead, and the assertion below says which.
	t.Setenv("WARDYN_PG_DSN", "postgres://wardyn@127.0.0.1:1/wardyn?sslmode=disable")
	resetFlags(t)
	oldArgs := os.Args
	os.Args = []string{"wardynd-test"}
	t.Cleanup(func() { os.Args = oldArgs })

	err = run()
	if err == nil || !strings.Contains(err.Error(), "retired in 0.8.2") {
		t.Fatalf("run() = %v, want the retired-variable refusal", err)
	}
	for _, name := range set {
		if slices.ContainsFunc(retiredModelEnvPrefixes, func(p string) bool { return strings.HasPrefix(name, p) }) ||
			slices.Contains(retiredModelEnvNames, name) {
			if !strings.Contains(err.Error(), name) {
				t.Errorf("refusal does not name %s: %v", name, err)
			}
		}
	}
}
