// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/subscription"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// This file pins resolveLLMTransport's behavior (the single chokepoint that
// decides which mutually-exclusive Anthropic/OpenAI transport a run gets: a
// host-staged subscription mount, the Wardyn-managed subscription, Bedrock, or
// the api-key gateway) with a golden-file matrix over agent x credential
// environment. It exists so the D4/D5 gut (removing verify/setup-commands
// orchestration) can prove it left resolveLLMTransport's own behavior for
// surviving run kinds byte-identical — see the GATE note in
// internal/api/testdata/llm_transport_golden.json's sibling test.
//
// Regenerate with: WARDYN_UPDATE_GOLDEN=1 go test ./internal/api/ -run Golden

// goldenUpdate reports whether golden fixtures in this package should be
// (re)written instead of compared. Shared by every golden test in the package.
func goldenUpdate() bool { return os.Getenv("WARDYN_UPDATE_GOLDEN") == "1" }

// compareOrUpdateGolden marshals got as indented JSON and either writes it to
// path (WARDYN_UPDATE_GOLDEN=1) or compares it byte-for-byte against the
// existing file, failing with a diff-friendly message otherwise. Shared by
// every golden test in this package (small enough that a generic helper beats
// duplicating the read/write/compare dance per fixture).
func compareOrUpdateGolden(t *testing.T, path string, got any) {
	t.Helper()
	want, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatalf("marshal golden %s: %v", path, err)
	}
	want = append(want, '\n')
	if goldenUpdate() {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, want, 0o644); err != nil {
			t.Fatalf("write golden %s: %v", path, err)
		}
		return
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (regenerate with WARDYN_UPDATE_GOLDEN=1 go test ./internal/api/ -run 'Golden|CheckIds')", path, err)
	}
	if string(onDisk) != string(want) {
		t.Errorf("golden mismatch for %s (if this is an INTENTIONAL behavior change, regenerate with "+
			"WARDYN_UPDATE_GOLDEN=1 go test ./internal/api/ -run 'Golden|CheckIds' and review the diff before committing "+
			"it):\n--- on disk ---\n%s\n--- recomputed ---\n%s", path, onDisk, want)
	}
}

// fakeSubProvider is a minimal subscription.Provider fake: Current and Peek
// both return the same fixed (token, err) pair — enough to model a wired
// resident/managed subscription for resolveLLMTransport, which only asks
// whether the provider is non-nil (injectSub) or Peek()s it (managedInjectReady).
type fakeSubProvider struct {
	tok subscription.Token
	err error
}

func (f fakeSubProvider) Current(context.Context) (subscription.Token, error) { return f.tok, f.err }
func (f fakeSubProvider) Peek() (subscription.Token, error)                   { return f.tok, f.err }

// llmTransportGoldenCell is the exported-field snapshot of one
// resolveLLMTransport call: llmTransport's unexported fields copied out (JSON
// can't marshal unexported fields even from within the same package), the
// sandboxEnv mutations it made, and policy.AllowedDomains afterward.
type llmTransportGoldenCell struct {
	Subscription        bool `json:"subscription"`
	InjectSub           bool `json:"inject_sub"`
	InjectManaged       bool `json:"inject_managed"`
	HarnessLogin        bool `json:"harness_login"`
	BedrockReady        bool `json:"bedrock_ready"`
	InjectBedrockBearer bool `json:"inject_bedrock_bearer"`
	// Bedrock sub-fields; meaningful only when BedrockReady (zero values otherwise).
	BedrockBearer   bool   `json:"bedrock_bearer,omitempty"`
	BedrockAWSMount bool   `json:"bedrock_aws_mount,omitempty"`
	BedrockRegion   string `json:"bedrock_region,omitempty"`
	BedrockModel    string `json:"bedrock_model,omitempty"`

	SandboxEnv     map[string]string `json:"sandbox_env"`
	AllowedDomains []string          `json:"allowed_domains"`
}

func snapshotLLMTransport(llm llmTransport, sandboxEnv map[string]string, allowedDomains []string) llmTransportGoldenCell {
	doms := append([]string(nil), allowedDomains...)
	sort.Strings(doms) // order isn't the property under test; membership + baseline preservation is
	return llmTransportGoldenCell{
		Subscription:        llm.subscription,
		InjectSub:           llm.injectSub,
		InjectManaged:       llm.injectManaged,
		HarnessLogin:        llm.harnessLogin,
		BedrockReady:        llm.bedrockReady,
		InjectBedrockBearer: llm.injectBedrockBearer,
		BedrockBearer:       llm.bedrock.bearer,
		BedrockAWSMount:     llm.bedrock.awsMount,
		BedrockRegion:       llm.bedrock.region,
		BedrockModel:        llm.bedrock.model,
		SandboxEnv:          sandboxEnv,
		AllowedDomains:      doms,
	}
}

// llmGoldenCase is one matrix cell: an agent paired with a credential
// environment shape resolveLLMTransport can feasibly be driven through without
// a real secret store/subscription/AWS. Not a strict 2x7 cross product — Bedrock
// and the subscription/managed paths are Claude-only by construction
// (resolveBedrockAuth/managedInjectReady both gate on agent=="claude-code"), so
// pairing them with codex-cli would test nothing real.
type llmGoldenCase struct {
	name       string
	agent      string
	cfg        Config
	injections []runner.InjectionGrant
	mounts     []types.WorkspaceMount
}

func llmGoldenCases() []llmGoldenCase {
	return []llmGoldenCase{
		// (a) nothing configured, both agents: the api-key placeholder fallback.
		{name: "claude-code/nothing-configured", agent: "claude-code"},
		{name: "codex-cli/nothing-configured", agent: "codex-cli"},

		// (b) an anthropic-api-key secret is present AND an injection for it was
		// already proposed (mirrors runs_create.go's ensureLLMGrant running BEFORE
		// dispatch) — the interesting assertion is the OPT-OUT: this pre-existing
		// injection is what suppresses the managed-subscription fallback below.
		{
			name:  "claude-code/anthropic-api-key-injection-present",
			agent: "claude-code",
			cfg:   Config{Secrets: &memSecrets{m: map[string][]byte{"anthropic-api-key": []byte("sk-ant-test")}}},
			injections: []runner.InjectionGrant{{
				GrantID: uuid.New(),
				Rule:    egress.InjectionRule{Host: "api.anthropic.com", Header: "x-api-key", Format: "%s", SecretName: "anthropic-api-key"},
			}},
		},

		// (c) an openai-api-key secret is present. resolveLLMTransport does not
		// itself branch on secret presence for api-key mode (that's the proxy's
		// injection-resolve job, at request time) — this cell exists to LOCK IN
		// that fact: it is byte-identical to codex-cli/nothing-configured.
		{
			name:  "codex-cli/openai-api-key-present",
			agent: "codex-cli",
			cfg:   Config{Secrets: &memSecrets{m: map[string][]byte{"openai-api-key": []byte("sk-oai-test")}}},
		},

		// (d) a Wardyn-managed subscription blob is present (container-login
		// setup-token) — the compose-mode fallback path.
		{
			name:  "claude-code/managed-subscription-blob-present",
			agent: "claude-code",
			cfg:   Config{ManagedToken: fakeSubProvider{tok: subscription.Token{Value: "managed-tok"}}},
		},

		// (e) a resident subscription token is wired AND the policy's ceiling
		// blesses the host-staged ~/.claude mount — the host-mode path, which
		// outranks managed/Bedrock/api-key.
		{
			name:   "claude-code/resident-subscription-token-wired",
			agent:  "claude-code",
			cfg:    Config{SubscriptionToken: fakeSubProvider{tok: subscription.Token{Value: "resident-tok"}}},
			mounts: []types.WorkspaceMount{{Target: claudeCredTarget}},
		},

		// (f) Bedrock BEARER mode: a bedrock-api-key secret is present, so the
		// proxy-injected (never-resident) bearer path wins over the resident
		// SigV4 fallback.
		{
			name:  "claude-code/bedrock-bearer",
			agent: "claude-code",
			cfg: Config{
				BedrockRegion: "us-east-1", BedrockModel: "us.anthropic.claude-sonnet-4-5-20250929-v1:0",
				Secrets:      &memSecrets{m: map[string][]byte{bedrockAPIKeySecret: []byte("bedrock-bearer-test")}},
				MaskRegistry: secretmask.NewRegistry(),
			},
		},

		// (g) Bedrock resident static SigV4 keys: no bearer, no captured SSO, no
		// ~/.aws mount configured, so resolveBedrockAuth falls all the way to the
		// resident-key fallback.
		{
			name:  "claude-code/bedrock-static-keys",
			agent: "claude-code",
			cfg: Config{
				BedrockRegion: "us-east-1", BedrockModel: "us.anthropic.claude-sonnet-4-5-20250929-v1:0",
				Secrets: &memSecrets{m: map[string][]byte{
					bedrockAccessKeyIDSecret:     []byte("AKIATESTTESTTESTTEST"),
					bedrockSecretAccessKeySecret: []byte("wJalrXUtnFEMItesttesttesttesttesttestKEY"),
				}},
				MaskRegistry: secretmask.NewRegistry(),
			},
		},
	}
}

// TestLLMTransportGolden drives resolveLLMTransport directly (not through the
// full dispatchRun/CreateSandbox path — this is a unit-level snapshot of one
// phase, not an integration test, so no Store/Runner is needed) over the
// matrix above and pins the result. A baseline "git.example.com" AllowedDomains
// entry (representing some already-resolved clone/scan egress) is present on
// every cell's input policy so additions the LLM phase makes (Bedrock hosts,
// the managed-subscription host) are visible against a stable floor, and so the
// managed-subscription fallback's "non-empty egress" gate is satisfied uniformly.
func TestLLMTransportGolden(t *testing.T) {
	got := map[string]llmTransportGoldenCell{}
	for _, c := range llmGoldenCases() {
		srv := New(c.cfg)
		run := types.AgentRun{ID: uuid.New(), Agent: c.agent, CreatedBy: "alice@example.com"}
		policy := &types.RunPolicySpec{
			AllowedDomains:  []string{"git.example.com"},
			WorkspaceMounts: c.mounts,
		}
		sandboxEnv := map[string]string{}
		llm := srv.resolveLLMTransport(context.Background(), run, policy, sandboxEnv, c.injections,
			false /* interactive */, "http://wardyn-proxy:3128", nil)
		if _, dup := got[c.name]; dup {
			t.Fatalf("duplicate cell name %q", c.name)
		}
		got[c.name] = snapshotLLMTransport(llm, sandboxEnv, policy.AllowedDomains)
	}
	compareOrUpdateGolden(t, "testdata/llm_transport_golden.json", got)
}
