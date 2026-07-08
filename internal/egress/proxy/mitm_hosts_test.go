// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import "testing"

// TestIsMITMHost_OperatorWidening pins the trust boundary of the isMITMHost
// widening: the built-in LLM hosts are always MITM-eligible; an OPERATOR-configured
// corp artifact host becomes eligible; any OTHER host stays excluded (no wildcard,
// no suffix match), and the empty allowlist is LLM-only.
func TestIsMITMHost_OperatorWidening(t *testing.T) {
	p := newProxy(Options{
		MITMHosts: []string{"Artifactory.Corp", "nexus.internal.example.com", " ", ""},
	})

	// Built-in LLM hosts: always eligible.
	for _, h := range []string{anthropicHost, openaiHost, "API.ANTHROPIC.COM", "api.anthropic.com."} {
		if !p.isMITMHost(h) {
			t.Errorf("isMITMHost(%q) = false, want true (built-in LLM host)", h)
		}
	}

	// Operator-configured corp hosts: eligible, case-insensitive + trailing-dot tolerant.
	for _, h := range []string{"artifactory.corp", "ARTIFACTORY.CORP", "artifactory.corp.", "nexus.internal.example.com"} {
		if !p.isMITMHost(h) {
			t.Errorf("isMITMHost(%q) = false, want true (operator corp host)", h)
		}
		if !p.isCorpMITMHost(h) {
			t.Errorf("isCorpMITMHost(%q) = false, want true", h)
		}
	}

	// Corp guard excludes the built-in LLM hosts (routing separation).
	if p.isCorpMITMHost(anthropicHost) {
		t.Errorf("isCorpMITMHost(anthropic) = true; LLM hosts must not be treated as corp")
	}

	// Arbitrary / near-miss hosts: excluded — NO wildcard, NO suffix widening.
	for _, h := range []string{
		"evil.com",
		"artifactory.corp.evil.com", // suffix attack must NOT match
		"notartifactory.corp",       // not an exact match
		"registry.npmjs.org",
		"",
	} {
		if p.isMITMHost(h) {
			t.Errorf("isMITMHost(%q) = true, want false (not operator-configured)", h)
		}
	}

	// Empty allowlist: LLM-only.
	bare := newProxy(Options{})
	if bare.isMITMHost("artifactory.corp") {
		t.Errorf("empty MITMHosts must be LLM-only, but corp host was eligible")
	}
	if !bare.isMITMHost(anthropicHost) {
		t.Errorf("empty MITMHosts must still MITM the built-in LLM hosts")
	}
}

// TestChannelForHost_NonLLMIsGeneric: a corp artifact host maps to ChannelGeneric
// so the LLM content scanner never runs against package-registry traffic.
func TestChannelForHost_NonLLMIsGeneric(t *testing.T) {
	if got := channelForHost("artifactory.corp"); string(got) == "" {
		t.Fatalf("channelForHost returned empty channel")
	}
	// classifyLLM(Generic, ...) must be scanNone regardless of method/path.
	if classifyLLM(channelForHost("artifactory.corp"), "POST", "some/path") != scanNone {
		t.Errorf("corp artifact host must classify as scanNone (no LLM scanning)")
	}
	// LLM hosts keep their channels.
	if classifyLLM(channelForHost(anthropicHost), "POST", "v1/messages") != scanMessages {
		t.Errorf("anthropic /messages must still classify as scanMessages")
	}
}

// fakeCA is a non-nil *certAuthority stand-in so mitmLLMHost's `p.ca != nil`
// gate is satisfied without minting a real cert (we only assert the boolean).
func fakeCA(t *testing.T) *certAuthority {
	t.Helper()
	certPEM, keyPEM := genTestCA(t)
	ca, err := newCertAuthority(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("newCertAuthority: %v", err)
	}
	return ca
}

// TestMITMLLMHost_GatedOnIntent is the security-review regression (LOW): a per-run
// CA can be minted purely for artifact-token injection, so the built-in LLM hosts
// must be TLS-MITM'd ONLY when LLM MITM is actually intended for the run (mitmLLM)
// — never merely because a CA exists. Corp artifact hosts stay unaffected (they go
// through isCorpMITMHost, a separate branch).
func TestMITMLLMHost_GatedOnIntent(t *testing.T) {
	ca := fakeCA(t)

	// Artifact-only run: CA present (for corp-token injection) but LLM MITM NOT
	// intended → a direct CONNECT to Anthropic/OpenAI must stay opaque.
	artifactOnly := newProxy(Options{CA: ca, MITMHosts: []string{"artifactory.corp"}, MITMLLM: false})
	for _, h := range []string{anthropicHost, openaiHost} {
		if artifactOnly.mitmLLMHost(h) {
			t.Errorf("artifact-only run (mitmLLM=false) must NOT MITM LLM host %q despite a CA being present", h)
		}
	}

	// Subscription/intercept_tls run: LLM MITM intended → LLM hosts are terminated.
	llmIntent := newProxy(Options{CA: ca, MITMLLM: true})
	for _, h := range []string{anthropicHost, openaiHost} {
		if !llmIntent.mitmLLMHost(h) {
			t.Errorf("run with LLM MITM intent (mitmLLM=true) must MITM LLM host %q", h)
		}
	}

	// No CA at all → never MITM, regardless of intent flag.
	noCA := newProxy(Options{MITMLLM: true})
	if noCA.mitmLLMHost(anthropicHost) {
		t.Errorf("no CA configured must never MITM an LLM host")
	}
}
