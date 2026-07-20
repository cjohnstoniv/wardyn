/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { buildSpec, initialWizardState, validateStep, wizardStateFromProposal } from "./wizard-types";
import type { WizardState } from "./wizard-types";
import type { ComposeRunProposal, RunPolicySpec } from "../../../lib/types";
import { SUBSCRIPTION_OAUTH_SECRET } from "../../../lib/types";

// Ephemeral runs: a workspace is OPTIONAL. Basics gates only the batch-needs-a-task
// rule; an interactive run with zero workspaces is valid and buildSpec degrades to
// an empty scratch run (repo "", no workspace mounts/repos).
describe("validateStep — Basics is workspace-optional (ephemeral runs)", () => {
  it("passes an interactive run with no workspace", () => {
    const state = { ...initialWizardState(), workspaces: [], mode: "interactive" as const };
    expect(validateStep("basics", state)).toBeNull();
  });

  it("still requires a task for a batch run", () => {
    const state = { ...initialWizardState(), workspaces: [], mode: "batch" as const, task: "" };
    expect(validateStep("basics", state)).toMatch(/task/i);
  });

  it("buildSpec degrades to an ephemeral scratch run with zero workspaces", () => {
    const { run, inline_policy } = buildSpec({ ...initialWizardState(), workspaces: [] });
    expect(run.repo).toBe("");
    expect(inline_policy.workspace_mounts).toBeUndefined();
    expect(inline_policy.workspace_repos).toBeUndefined();
  });
});

// Regression for the saved-workspace launch bug: a subscription-recorded profile
// carries an api_key grant naming the subscription OAuth sentinel (recordings
// never synthesize the ~/.claude mount). Hydrating it MUST be recognized as
// subscription auth — NOT carried into llmSecretName and re-emitted as an
// x-api-key grant to a secret that doesn't exist (the "references unknown secret"
// launch failure).
describe("wizardStateFromProposal — subscription sentinel recognition", () => {
  const run = { agent: "claude-code", repo: "org/repo", interactive: true } as ComposeRunProposal;
  const spec: RunPolicySpec = {
    allowed_domains: ["api.anthropic.com", "github.com"],
    first_use_approval: "deny_with_review",
    min_confinement_class: "CC2",
    eligible_grants: [
      {
        kind: "api_key",
        scope: { host: "api.anthropic.com", header: "x-api-key", secret_name: SUBSCRIPTION_OAUTH_SECRET, format: "%s" },
        requires_approval: false,
      },
    ],
  };

  it("hydrates the sentinel grant as subscription mode, not an api-key secret", () => {
    const state = wizardStateFromProposal(run, spec);
    expect(state.anthropicAuth).toBe("subscription");
    expect(state.llmSecretName).toBe("");
  });

  it("re-building never emits a dangling api_key grant to the sentinel", () => {
    const { inline_policy } = buildSpec(wizardStateFromProposal(run, spec));
    const apiKey = (inline_policy.eligible_grants ?? []).find((g) => g.kind === "api_key");
    expect(apiKey).toBeUndefined(); // subscription => proxy-injected, no named-secret grant
    expect(inline_policy.allowed_domains).toContain("api.anthropic.com");
  });

  it("reconstructs the resident ~/.claude mount once a subscription dir is set", () => {
    const state = { ...wizardStateFromProposal(run, spec), subscriptionClaudeDir: "/home/op/.claude" };
    const { inline_policy } = buildSpec(state);
    const mount = (inline_policy.workspace_mounts ?? []).find((m) => m.target === "/home/agent/.claude");
    expect(mount?.source).toBe("/home/op/.claude");
  });
});

// Regression for the wizard-contract HIGH finding: under allow-all egress the
// wizard dropped the run's own required hosts AND emitted allowed_domains=[].
// But proxy credential injection fails CLOSED unless the api_key grant's exact
// injection host is in allowed_domains — even under allow-all. So whenever an
// api_key/LLM grant is present, buildSpec MUST always include its injection
// host in allowed_domains, regardless of the allow-all toggle.
describe("buildSpec — allow-all egress + LLM api_key grant", () => {
  function stateWithLlmKey(overrides: Partial<WizardState> = {}): WizardState {
    return {
      ...initialWizardState(),
      // claude-code with an API key => api.anthropic.com injection host.
      agent: "claude-code",
      anthropicAuth: "apikey",
      llmSecretName: "anthropic-key",
      allowAllEgress: true,
      ...overrides,
    };
  }

  it("keeps the anthropic injection host in allowed_domains under allow-all", () => {
    const { inline_policy } = buildSpec(stateWithLlmKey());
    expect(inline_policy.allow_all_egress).toBe(true);
    // The api_key grant must be present...
    const grant = (inline_policy.eligible_grants ?? []).find((g) => g.kind === "api_key");
    expect(grant).toBeTruthy();
    // ...and its injection host must be reachable, else injection fails closed.
    expect(inline_policy.allowed_domains).toContain("api.anthropic.com");
  });

  it("keeps the openai injection host in allowed_domains under allow-all (codex)", () => {
    const { inline_policy } = buildSpec(
      stateWithLlmKey({ agent: "codex-cli", llmSecretName: "openai-key" }),
    );
    expect(inline_policy.allow_all_egress).toBe(true);
    expect(inline_policy.allowed_domains).toContain("api.openai.com");
  });

  it("under allow-all still leaves allowed_domains empty when there is no api_key grant", () => {
    const { inline_policy } = buildSpec(
      stateWithLlmKey({ llmSecretName: "", allowedDomains: ["github.com"] }),
    );
    expect(inline_policy.allow_all_egress).toBe(true);
    // No grant host to pin => allow-all stays a pure deny-list (empty allowlist).
    expect(inline_policy.allowed_domains).toEqual([]);
  });

  it("non-allow-all behavior is unchanged (allowlist + required hosts unioned)", () => {
    const { inline_policy } = buildSpec(
      stateWithLlmKey({ allowAllEgress: false, allowedDomains: ["pypi.org"] }),
    );
    expect(inline_policy.allow_all_egress).toBeUndefined();
    expect(inline_policy.allowed_domains).toContain("pypi.org");
    expect(inline_policy.allowed_domains).toContain("api.anthropic.com");
  });
});

// git_pat grant: broker a stored PAT to git for a non-GitHub host. The grant's
// host is reached over plain CONNECT egress (like github), so buildSpec MUST
// union it into allowed_domains, or the clone gets gated behind first-use
// approval. The grant is emitted only when enabled with both host + secret.
describe("buildSpec — git_pat grant", () => {
  it("emits a git_pat grant with the right scope and unions the host into allowed_domains", () => {
    const { inline_policy } = buildSpec({
      ...initialWizardState(),
      allowAllEgress: false,
      allowedDomains: ["api.anthropic.com"],
      gitPatEnabled: true,
      gitPatHost: "dev.azure.com",
      gitPatSecretName: "ado-pat",
      gitPatUsername: "myuser",
      gitPatRequiresApproval: false,
    });
    const grant = (inline_policy.eligible_grants ?? []).find((g) => g.kind === "git_pat");
    expect(grant).toBeTruthy();
    expect(grant?.scope).toEqual({
      host: "dev.azure.com",
      secret_name: "ado-pat",
      username: "myuser",
    });
    expect(grant?.requires_approval).toBe(false);
    // The host must be reachable over egress.
    expect(inline_policy.allowed_domains).toContain("dev.azure.com");
  });

  it("omits the username from scope when not provided", () => {
    const { inline_policy } = buildSpec({
      ...initialWizardState(),
      gitPatEnabled: true,
      gitPatHost: "gitlab.com",
      gitPatSecretName: "gl-pat",
    });
    const grant = (inline_policy.eligible_grants ?? []).find((g) => g.kind === "git_pat");
    expect(grant?.scope).toEqual({ host: "gitlab.com", secret_name: "gl-pat" });
  });

  it("emits no git_pat grant when disabled or missing host/secret", () => {
    const disabled = buildSpec({
      ...initialWizardState(),
      gitPatEnabled: false,
      gitPatHost: "gitlab.com",
      gitPatSecretName: "gl-pat",
    });
    expect((disabled.inline_policy.eligible_grants ?? []).some((g) => g.kind === "git_pat")).toBe(false);

    const missingSecret = buildSpec({
      ...initialWizardState(),
      gitPatEnabled: true,
      gitPatHost: "gitlab.com",
      gitPatSecretName: "",
    });
    expect((missingSecret.inline_policy.eligible_grants ?? []).some((g) => g.kind === "git_pat")).toBe(false);
  });
});

// Governed command run type: task_mode=exec on the wire, no agent/model
// involved. Default ("agent") must stay backward-compatible (task_mode omitted).
describe("buildSpec — runType (agent run vs governed command)", () => {
  it("defaults to an agent run and omits task_mode", () => {
    const { run } = buildSpec(initialWizardState());
    expect(run.task_mode).toBeUndefined();
  });

  it("emits task_mode: exec for a governed command", () => {
    const { run } = buildSpec({ ...initialWizardState(), runType: "command", task: "npm test" });
    expect(run.task_mode).toBe("exec");
    expect(run.task).toBe("npm test");
  });
});
