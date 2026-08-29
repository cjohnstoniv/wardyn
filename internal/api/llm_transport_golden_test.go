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
	// workspaceID + interactive drive the modelRun discriminator: a
	// workspace-linked NON-interactive run is a scan run (execs wardyn-scan,
	// never calls a model), so it must not be handed resident Bedrock creds.
	// A workspace-linked INTERACTIVE run is Record Mode — a human driving a
	// real agent — so it stays a model run. Without these two fields every
	// cell was an ordinary run and `modelRun := true` passed the whole suite.
	workspaceID *uuid.UUID
	interactive bool
	// taskMode drives the same modelRun discriminator: "exec" is the BYOA/CI
	// plain-command lane ("no agent, no LLM credentials"), so it makes no model
	// call and must receive NO injected credential even with a managed/resident
	// subscription wired. "" is the ordinary agent-harness run.
	taskMode string
	// task, when set to harnessLoginTask, models a `claude setup-token` login
	// box (run.Task, not taskMode) — the harnessLogin discriminator.
	task string
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

		// (b') gateway-aware opt-out: an internal model gateway is configured
		// AND a Wardyn-managed subscription blob is ALSO wired, but the api-key
		// injection targets the GATEWAY host (not api.anthropic.com) — exactly
		// what ensureLLMGrant now authors under a gateway. managed must still
		// stay off: hasAnthropicAPIKeyInjection must recognize the gateway host,
		// not just the hardcoded public one.
		{
			name:  "claude-code/gateway-configured-with-api-key-injection-managed-false",
			agent: "claude-code",
			cfg: Config{
				LLMGateways:  map[string]string{"api.anthropic.com": "https://llm-gateway.corp.internal"},
				ManagedToken: fakeSubProvider{tok: subscription.Token{Value: "managed-tok"}},
				Secrets:      &memSecrets{m: map[string][]byte{"anthropic-api-key": []byte("sk-ant-test")}},
			},
			injections: []runner.InjectionGrant{{
				GrantID: uuid.New(),
				Rule:    egress.InjectionRule{Host: "llm-gateway.corp.internal", Header: "x-api-key", Format: "%s", SecretName: "anthropic-api-key"},
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

		// (d') the SAME managed-subscription blob, but this is a task-mode=exec
		// run (BYOA/CI plain command). Its contract is "no LLM credentials", so
		// managed injection must NOT fire — inject_managed stays false and
		// api.anthropic.com is NOT appended to egress. Pins the exec no-model-call
		// gate against the (d) cell just above, which DOES inject.
		{
			name:     "claude-code/managed-subscription-blob-present-exec-suppressed",
			agent:    "claude-code",
			cfg:      Config{ManagedToken: fakeSubProvider{tok: subscription.Token{Value: "managed-tok"}}},
			taskMode: "exec",
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

		// (h) SCAN RUN: workspace-linked and non-interactive. It execs
		// wardyn-scan and never calls a model, so modelRun is false and the
		// resident SigV4 creds must NOT be placed in the sandbox even though
		// Bedrock is fully configured. This cell is the discriminator itself:
		// flip modelRun to an unconditional true and only this cell moves.
		{
			name:        "claude-code/scan-run-bedrock-configured",
			agent:       "claude-code",
			workspaceID: ptrUUID(),
			cfg: Config{
				BedrockRegion: "us-east-1", BedrockModel: "us.anthropic.claude-sonnet-4-5-20250929-v1:0",
				Secrets: &memSecrets{m: map[string][]byte{
					bedrockAccessKeyIDSecret:     []byte("AKIATESTTESTTESTTEST"),
					bedrockSecretAccessKeySecret: []byte("wJalrXUtnFEMItesttesttesttesttesttestKEY"),
				}},
				MaskRegistry: secretmask.NewRegistry(),
			},
		},

		// (i) RECORD RUN: workspace-linked but INTERACTIVE — a human driving a
		// real agent in an attach shell, so it IS a model run and Bedrock
		// resolves exactly as it does for an ordinary run. Same inputs as (h)
		// apart from the interactive flag; the pair pins both sides of the
		// discriminator, so collapsing it in either direction fails.
		{
			name:        "claude-code/record-run-interactive-bedrock-configured",
			agent:       "claude-code",
			workspaceID: ptrUUID(),
			interactive: true,
			cfg: Config{
				BedrockRegion: "us-east-1", BedrockModel: "us.anthropic.claude-sonnet-4-5-20250929-v1:0",
				Secrets: &memSecrets{m: map[string][]byte{
					bedrockAccessKeyIDSecret:     []byte("AKIATESTTESTTESTTEST"),
					bedrockSecretAccessKeySecret: []byte("wJalrXUtnFEMItesttesttesttesttesttestKEY"),
				}},
				MaskRegistry: secretmask.NewRegistry(),
			},
		},

		// (j) PRECEDENCE: every lane wired at once — resident subscription
		// mount, managed blob, Bedrock, and an api-key injection. The
		// documented order is host-staged mount > managed > Bedrock > api-key,
		// so the resident mount must win and the rest must stay off. An
		// inversion anywhere in that chain shows up here and nowhere else.
		{
			name:  "claude-code/all-lanes-wired-resident-wins",
			agent: "claude-code",
			cfg: Config{
				SubscriptionToken: fakeSubProvider{tok: subscription.Token{Value: "resident-tok"}},
				ManagedToken:      fakeSubProvider{tok: subscription.Token{Value: "managed-tok"}},
				BedrockRegion:     "us-east-1", BedrockModel: "us.anthropic.claude-sonnet-4-5-20250929-v1:0",
				Secrets: &memSecrets{m: map[string][]byte{
					bedrockAPIKeySecret: []byte("bedrock-bearer-test"),
					"anthropic-api-key": []byte("sk-ant-test"),
				}},
				MaskRegistry: secretmask.NewRegistry(),
			},
			mounts: []types.WorkspaceMount{{Target: claudeCredTarget}},
			injections: []runner.InjectionGrant{{
				GrantID: uuid.New(),
				Rule:    egress.InjectionRule{Host: "api.anthropic.com", Header: "x-api-key", Format: "%s", SecretName: "anthropic-api-key"},
			}},
		},

		// (j') W12-W12-C-1: a HARNESS LOGIN run (run.Task = harnessLoginTask —
		// `claude setup-token` in the attach shell, no credential yet) with
		// Bedrock fully configured must get NOTHING Bedrock-shaped: bedrock_ready
		// and inject_bedrock_bearer both false, no ~/.aws mount, no bearer MITM.
		// Same Bedrock config as (f) (bearer mode); the only difference is
		// task=harnessLoginTask. Before the fix, resolveBedrockAuth ran
		// unconditionally and this cell was byte-identical to (f) apart from
		// sandbox_env — i.e. the login box still got a minted bearer grant.
		{
			name:  "claude-code/harness-login-bedrock-configured-gets-nothing",
			agent: "claude-code",
			task:  harnessLoginTask,
			cfg: Config{
				BedrockRegion: "us-east-1", BedrockModel: "us.anthropic.claude-sonnet-4-5-20250929-v1:0",
				Secrets:      &memSecrets{m: map[string][]byte{bedrockAPIKeySecret: []byte("bedrock-bearer-test")}},
				MaskRegistry: secretmask.NewRegistry(),
			},
		},

		// (k) MANAGED vs API-KEY opt-out: the managed fallback must NOT
		// silently replace an operator's explicit api-key injection. Both are
		// present and no resident mount is staged, so managed stays off.
		{
			name:  "claude-code/managed-yields-to-explicit-api-key",
			agent: "claude-code",
			cfg: Config{
				ManagedToken: fakeSubProvider{tok: subscription.Token{Value: "managed-tok"}},
				Secrets:      &memSecrets{m: map[string][]byte{"anthropic-api-key": []byte("sk-ant-test")}},
			},
			injections: []runner.InjectionGrant{{
				GrantID: uuid.New(),
				Rule:    egress.InjectionRule{Host: "api.anthropic.com", Header: "x-api-key", Format: "%s", SecretName: "anthropic-api-key"},
			}},
		},
	}
}

// ptrUUID returns a pointer to a fresh UUID — a workspace-linked run's
// WorkspaceID. The value never reaches the golden (only its presence changes
// resolveLLMTransport's behavior), so a random id keeps the fixture stable.
func ptrUUID() *uuid.UUID { id := uuid.New(); return &id }

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
		// The matrix pins the TRANSPORT logic, so every cell runs in the single-user
		// desktop posture that permits a shared subscription at all. The off-posture
		// half is TestLLMTransport_NoSharedSubscriptionOffPosture below, which asserts
		// the whole matrix collapses to "no subscription credential" — cheaper than
		// doubling this golden, and it fails loudly if a new lane appears.
		c.cfg.SubscriptionPostureOK = true
		srv := New(c.cfg)
		run := types.AgentRun{ID: uuid.New(), Agent: c.agent, Task: c.task, CreatedBy: "alice@example.com", WorkspaceID: c.workspaceID}
		policy := &types.RunPolicySpec{
			AllowedDomains:  []string{"git.example.com"},
			WorkspaceMounts: c.mounts,
		}
		sandboxEnv := map[string]string{}
		llm := srv.resolveLLMTransport(context.Background(), run, policy, sandboxEnv, c.injections,
			c.interactive, c.taskMode, "http://wardyn-proxy:3128", nil)
		if _, dup := got[c.name]; dup {
			t.Fatalf("duplicate cell name %q", c.name)
		}
		got[c.name] = snapshotLLMTransport(llm, sandboxEnv, policy.AllowedDomains)
	}
	compareOrUpdateGolden(t, "testdata/llm_transport_golden.json", got)
}

// Off-posture, EVERY cell of the transport matrix must resolve to no shared
// subscription credential — both the resident lane and the managed fallback,
// across every agent, task mode and credential environment the golden covers.
//
// This is the cheap high-coverage negative: a new lane, or a new cell that wires
// a credential some other way, fails here without anyone having to remember to
// think about posture. Asserting the collapse is stronger than asserting a
// handful of named cases, because the thing being defended is "no path reaches
// it", not "these paths do not".
func TestLLMTransport_NoSharedSubscriptionOffPosture(t *testing.T) {
	for _, c := range llmGoldenCases() {
		c.cfg.SubscriptionPostureOK = false
		srv := New(c.cfg)
		run := types.AgentRun{ID: uuid.New(), Agent: c.agent, Task: c.task, CreatedBy: "alice@example.com", WorkspaceID: c.workspaceID}
		policy := &types.RunPolicySpec{
			AllowedDomains:  []string{"git.example.com"},
			WorkspaceMounts: c.mounts,
		}
		sandboxEnv := map[string]string{}
		llm := srv.resolveLLMTransport(context.Background(), run, policy, sandboxEnv, c.injections,
			c.interactive, c.taskMode, "http://wardyn-proxy:3128", nil)
		if llm.injectSub {
			t.Errorf("%s: injectSub true off-posture — one operator's subscription would be injected into this run", c.name)
		}
		if llm.injectManaged {
			t.Errorf("%s: injectManaged true off-posture — the managed fallback is the broadest sharing path and needs no opt-in", c.name)
		}
	}
}
