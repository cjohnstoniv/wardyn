// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"reflect"
	"strings"
	"testing"
)

// TestParseAgentImages_ValidatesHarnessSetKeys: a malformed harness-set key in
// WARDYN_AGENT_IMAGES stops the daemon at boot rather than at a launch.
func TestParseAgentImages_ValidatesHarnessSetKeys(t *testing.T) {
	if _, _, err := parseAgentImages(`{"claude-code":"wardyn/agent-claude-code:local","claude-code+codex-cli":"wardyn/agent-harnesses:local"}`); err != nil {
		t.Fatalf("a valid set key: %v", err)
	}
	for _, raw := range []string{`{"codex-cli+claude-code":"x"}`, `{"claude-code+":"x"}`, `{"claude-code+claude-code":"x"}`} {
		if _, _, err := parseAgentImages(raw); err == nil {
			t.Errorf("%s was accepted", raw)
		}
	}
}

// TestParseAgentImages_SplitsArchVariants: `<key>@<arch>` entries leave the plain map, which stays the
// one-ref-per-key map every reader expects, and land in the variants.
func TestParseAgentImages_SplitsArchVariants(t *testing.T) {
	plain, variants, err := parseAgentImages(`{"base":"m/b:1","base@arm64":"m/b:1-arm64","claude-code+codex-cli":"m/s:1","claude-code+codex-cli@arm64":"m/s:1-arm64","base@amd64":"m/b:1-amd64"}`)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"base": "m/b:1", "claude-code+codex-cli": "m/s:1"}; !reflect.DeepEqual(plain, want) {
		t.Errorf("plain = %v, want %v", plain, want)
	}
	want := map[string]map[string]string{
		"arm64": {"base": "m/b:1-arm64", "claude-code+codex-cli": "m/s:1-arm64"},
		"amd64": {"base": "m/b:1-amd64"},
	}
	if !reflect.DeepEqual(variants, want) {
		t.Errorf("variants = %v, want %v", variants, want)
	}

	if _, _, err = parseAgentImages(`{"a+b":"m/s:1","a+b@arm64":"m/s:1-arm64"}`); err != nil {
		t.Errorf("a set key with an @arch variant: %v", err)
	}
	plain, variants, err = parseAgentImages(`{"claude-code":"m/cc:1"}`)
	if err != nil || variants != nil || !reflect.DeepEqual(plain, map[string]string{"claude-code": "m/cc:1"}) {
		t.Errorf("a config with no variants = %v, %v, %v; want it unchanged and nil variants", plain, variants, err)
	}
}

func TestParseAgentImages_RefusesBadVariants(t *testing.T) {
	for _, raw := range []string{
		`{"base":"m/b","base@riscv64":"m/b-r"}`,
		`{"base":"m/b","base@":"m/b-r"}`,
		`{"x":"m/x","x@arm64":"m/a","y@arm64":"m/y"}`, // y has no plain entry
		`{"base@arm64":"m/b"}`,
		`{"base":"m/b","base@arm64":""}`,
		`{"base":"m/b","@arm64":"m/b-a"}`,
		`{"base":"m/b","base@arm64@arm64":"m/b-a"}`,
		`{"base@x":"m/b","base@x@arm64":"m/b-a"}`,
	} {
		if _, _, err := parseAgentImages(raw); err == nil {
			t.Errorf("%s was accepted", raw)
		}
	}
}

func TestParseBootImages_ProxyImagePerArch(t *testing.T) {
	empty, agentImages := "", ""
	arm := "reg.example/wardyn-proxy:1-arm64"
	f := &bootFlags{agentImagesJSON: &agentImages, proxyImage: &empty, proxyImageAMD64: &empty, proxyImageARM64: &arm}
	_, _, byArch, err := parseBootImages(f)
	if err != nil || len(byArch) != 1 || byArch["arm64"] != arm {
		t.Fatalf("byArch = %v, %v; want only the arm64 entry", byArch, err)
	}
	bad := "a b"
	f.proxyImageARM64 = &bad
	if _, _, _, err = parseBootImages(f); err == nil || !strings.Contains(err.Error(), "WARDYN_PROXY_IMAGE_ARM64") {
		t.Errorf("an unsafe ref = %v; want a refusal naming WARDYN_PROXY_IMAGE_ARM64", err)
	}
}

func TestParseBootImages_PlainProxyImage(t *testing.T) {
	empty, agentImages, bad := "", "", "a b"
	f := &bootFlags{agentImagesJSON: &agentImages, proxyImage: &bad, proxyImageAMD64: &empty, proxyImageARM64: &empty}
	if _, _, _, err := parseBootImages(f); err == nil || !strings.Contains(err.Error(), "WARDYN_PROXY_IMAGE ") {
		t.Errorf("an unsafe plain ref = %v; want a refusal naming WARDYN_PROXY_IMAGE", err)
	}
}
