// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

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
	p := newProxy(Options{})
	if got := p.channelForHost("artifactory.corp"); string(got) == "" {
		t.Fatalf("channelForHost returned empty channel")
	}
	// classifyLLM(Generic, ...) must be scanNone regardless of method/path.
	if classifyLLM(p.channelForHost("artifactory.corp"), "POST", "some/path") != scanNone {
		t.Errorf("corp artifact host must classify as scanNone (no LLM scanning)")
	}
	// LLM hosts keep their channels.
	if classifyLLM(p.channelForHost(anthropicHost), "POST", "v1/messages") != scanMessages {
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

// TestMITMLLMHost_GatedOnIntent: a per-run
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
		if artifactOnly.mitmLLMHost(h, 443) {
			t.Errorf("artifact-only run (mitmLLM=false) must NOT MITM LLM host %q despite a CA being present", h)
		}
	}

	// Subscription/intercept_tls run: LLM MITM intended → LLM hosts are terminated.
	llmIntent := newProxy(Options{CA: ca, MITMLLM: true})
	for _, h := range []string{anthropicHost, openaiHost} {
		if !llmIntent.mitmLLMHost(h, 443) {
			t.Errorf("run with LLM MITM intent (mitmLLM=true) must MITM LLM host %q", h)
		}
	}

	// No CA at all → never MITM, regardless of intent flag.
	noCA := newProxy(Options{MITMLLM: true})
	if noCA.mitmLLMHost(anthropicHost, 443) {
		t.Errorf("no CA configured must never MITM an LLM host")
	}
}

// TestIsCorpMITMHost_ReservedLLMHostsExcludedEvenWhenListed: handleConnect
// dispatches the corp-artifact MITM branch (guarded by
// isCorpMITMHost) BEFORE it ever reaches the isLLMHost/mitmLLMHost intent gate, so
// isCorpMITMHost is the only thing standing between a misconfigured/admin-authored
// mitmHosts entry naming a reserved LLM hostname and that entry silently routing
// real Anthropic/OpenAI traffic through the corp-artifact-token MITM branch —
// bypassing the mitmLLM intent gate entirely and swapping a corp registry token
// onto LLM traffic. Unlike TestIsMITMHost_OperatorWidening's corp-guard check
// (which never actually seeds mitmHosts with an LLM host, so it can't catch this),
// this test puts anthropicHost/openaiHost directly IN mitmHosts and asserts
// isCorpMITMHost still refuses them.
func TestIsCorpMITMHost_ReservedLLMHostsExcludedEvenWhenListed(t *testing.T) {
	p := newProxy(Options{
		MITMHosts: []string{"artifactory.corp", anthropicHost, openaiHost, strings.ToUpper(anthropicHost)},
	})

	for _, h := range []string{anthropicHost, openaiHost, "API.ANTHROPIC.COM", "api.openai.com."} {
		if p.isCorpMITMHost(h) {
			t.Errorf("isCorpMITMHost(%q) = true, want false: a reserved LLM host must never be treated as a corp artifact host, even when present in mitmHosts", h)
		}
	}
	// A genuine corp host in the same allowlist is unaffected.
	if !p.isCorpMITMHost("artifactory.corp") {
		t.Errorf("isCorpMITMHost(artifactory.corp) = false, want true (real corp host must still work)")
	}
}

// Two spellings of one IP literal share every host-keyed table, in both directions: an IPv4-mapped,
// expanded or compressed request host reaches the terminate-and-inspect entry, its port and scheme,
// the model channel and the injection rule configured under another spelling, exactly as the
// canonical spelling does; another address shares none of them.
func TestHostTables_IPLiteralSpellingsAreOneHost(t *testing.T) {
	p := newProxy(Options{RunID: uuid.New(), Policy: CompilePolicy(types.RunPolicySpec{}),
		MITMHosts:       []string{"http://10.0.0.5:8443", "[2001:0db8:0000:0000:0000:0000:0000:0001]:443", "::ffff:10.0.0.9"},
		LLMChannelHosts: map[string]string{"2001:db8::2": anthropicHost}})
	for _, h := range []string{"10.0.0.5", "::ffff:10.0.0.5", "::FFFF:a00:5", "0:0:0:0:0:ffff:10.0.0.5"} {
		if !p.isCorpMITMHost(h) || !p.mitmPortAllowed(h, 8443) || p.mitmPortAllowed(h, 443) || !p.mitmPlaintextUpstream(h, 8443) {
			t.Errorf("%q: eligible %v, port 8443 %v, port 443 %v, plaintext %v; want the 10.0.0.5:8443 plain-HTTP entry",
				h, p.isCorpMITMHost(h), p.mitmPortAllowed(h, 8443), p.mitmPortAllowed(h, 443), p.mitmPlaintextUpstream(h, 8443))
		}
	}
	for _, h := range []string{"2001:db8::1", "2001:DB8:0:0::1"} {
		if !p.isCorpMITMHost(h) || !p.mitmPortAllowed(h, 443) || p.mitmPlaintextUpstream(h, 443) {
			t.Errorf("%q: want the expanded IPv6 TLS entry on 443", h)
		}
	}
	if !p.isCorpMITMHost("10.0.0.9") {
		t.Error("10.0.0.9 missed the entry configured as ::ffff:10.0.0.9")
	}
	if p.isCorpMITMHost("10.0.0.6") || p.isCorpMITMHost("::ffff:10.0.0.6") || p.isCorpMITMHost("2001:db8::3") {
		t.Error("another address matched an entry")
	}
	want := p.channelForHost(anthropicHost)
	for _, h := range []string{"2001:db8::2", "2001:0DB8:0000::0002"} {
		if !p.isLLMHost(h) || p.channelForHost(h) != want {
			t.Errorf("%q: model host %v, channel %v; want the channel configured as 2001:db8::2", h, p.isLLMHost(h), p.channelForHost(h))
		}
	}
	g := uuid.New()
	inj := &injector{byHost: map[string]*injEntry{"10.0.0.5": {grantID: g}}}
	for _, h := range []string{"10.0.0.5", "::ffff:10.0.0.5", "10.0.0.5."} {
		if got, ok := inj.grantIDFor(h); !ok || got != g {
			t.Errorf("injection rule for %q = (%v, %v), want the 10.0.0.5 rule", h, got, ok)
		}
	}
	if h, port := splitHostPort("[::ffff:10.0.0.5]:443", 443); h != "10.0.0.5" || port != 443 {
		t.Errorf("splitHostPort = (%q, %d), want the canonical (10.0.0.5, 443)", h, port)
	}
}
