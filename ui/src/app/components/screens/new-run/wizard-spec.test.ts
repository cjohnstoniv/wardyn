/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// buildSpec + impliedEgressHosts tests — split out of wizard-types.test.ts
// (which stays the source-of-truth test file for WizardState + the wizard's
// own validation/hydration) alongside wizard-spec.ts's own split from
// wizard-types.ts. Whole describe blocks moved verbatim; nothing here is a
// new test.
import { describe, it, expect } from "vitest";
import {
  buildSpec,
  impliedEgressHosts,
  initialWizardState,
} from "./wizard-types";
import type { WizardState } from "./wizard-types";
import type { Workspace, WorkspaceRequirementsMap } from "../../../lib/types";
import { makeWorkspace } from "../../../../test/factories";

function localDirWorkspace(id: string, requirements: WorkspaceRequirementsMap = {}): Workspace {
  return makeWorkspace({
    id,
    name: id,
    kind: "local_dir",
    source: `/home/me/${id}`,
    requirements,
  });
}

// buildSpec's composition-model additions: run.workspaces[] (enabled_optional +
// read_only) and run.integration_id, layered ADDITIVELY onto the existing
// workspace_mounts/repos it already builds (CreateRunRequest.Workspaces is
// metadata-only — attachment itself still flows through those).
describe("buildSpec — composition model: workspaces[] + integration_id", () => {
  it("emits integration_id only when a run override is set", () => {
    expect(buildSpec(initialWizardState()).run.integration_id).toBeUndefined();
    const { run } = buildSpec({ ...initialWizardState(), integrationId: "int-1" });
    expect(run.integration_id).toBe("int-1");
  });

  it("omits run.workspaces entirely when no selection opts into anything", () => {
    const ws = localDirWorkspace("ws-1");
    const { run } = buildSpec(
      { ...initialWizardState(), workspaces: [{ workspaceId: "ws-1" }] },
      [ws],
    );
    expect(run.workspaces).toBeUndefined();
  });

  it("emits one run.workspaces entry per selection that opts into something", () => {
    const ws1 = localDirWorkspace("ws-1");
    const ws2 = localDirWorkspace("ws-2");
    const { run } = buildSpec(
      {
        ...initialWizardState(),
        workspaces: [
          { workspaceId: "ws-1", enabledOptional: ["egress:api.stripe.com"] },
          { workspaceId: "ws-2" }, // no-op — must not appear
        ],
      },
      [ws1, ws2],
    );
    expect(run.workspaces).toEqual([
      { workspace_id: "ws-1", enabled_optional: ["egress:api.stripe.com"] },
    ]);
  });

  it("carries read_only through even with no enabled_optional entries", () => {
    const ws = localDirWorkspace("ws-1");
    const { run } = buildSpec(
      { ...initialWizardState(), workspaces: [{ workspaceId: "ws-1", readOnly: true }] },
      [ws],
    );
    expect(run.workspaces).toEqual([{ workspace_id: "ws-1", read_only: true }]);
  });
});

// The mount buildSpec composes must already show the TRUE resolved write
// access (Review's "exact policy" JSON is this object verbatim) — never a
// placeholder the server silently rewrites later via applyWorkspaceRequirements.
describe("buildSpec — write mode resolves honestly into workspace_mounts[].read_only", () => {
  it("a Required write resolves writable with no enabledOptional/readOnly set", () => {
    const ws = localDirWorkspace("ws-1", {
      "write:/home/me/ws-1": { level: "required", provenance: "operator_set" },
    });
    const { inline_policy } = buildSpec(
      { ...initialWizardState(), workspaces: [{ workspaceId: "ws-1" }] },
      [ws],
    );
    expect(inline_policy.workspace_mounts?.[0].read_only).toBe(false);
  });

  it("a Required write can still be narrowed back to read-only for this run", () => {
    const ws = localDirWorkspace("ws-1", {
      "write:/home/me/ws-1": { level: "required", provenance: "operator_set" },
    });
    const { inline_policy } = buildSpec(
      { ...initialWizardState(), workspaces: [{ workspaceId: "ws-1", readOnly: true }] },
      [ws],
    );
    expect(inline_policy.workspace_mounts?.[0].read_only).toBe(true);
  });

  it("an Optional write NOT enabled stays read-only even if readOnly=false", () => {
    const ws = localDirWorkspace("ws-1", {
      "write:/home/me/ws-1": { level: "optional", provenance: "operator_set" },
    });
    const { inline_policy } = buildSpec(
      { ...initialWizardState(), workspaces: [{ workspaceId: "ws-1", readOnly: false }] },
      [ws],
    );
    // A run may never WIDEN an optional it never enabled via enabledOptional.
    expect(inline_policy.workspace_mounts?.[0].read_only).toBe(true);
  });

  it("an Optional write that IS enabled resolves writable", () => {
    const ws = localDirWorkspace("ws-1", {
      "write:/home/me/ws-1": { level: "optional", provenance: "operator_set" },
    });
    const { inline_policy } = buildSpec(
      {
        ...initialWizardState(),
        workspaces: [{ workspaceId: "ws-1", enabledOptional: ["write:/home/me/ws-1"] }],
      },
      [ws],
    );
    expect(inline_policy.workspace_mounts?.[0].read_only).toBe(false);
  });

  it("no write requirement declared at all defaults to the safe read-only baseline", () => {
    const ws = localDirWorkspace("ws-1");
    const { inline_policy } = buildSpec(
      { ...initialWizardState(), workspaces: [{ workspaceId: "ws-1" }] },
      [ws],
    );
    expect(inline_policy.workspace_mounts?.[0].read_only).toBe(true);
  });
});

// Under allow-all egress, buildSpec must never drop the run's own required
// hosts or emit a bare allowed_domains=[]. Proxy credential injection fails
// CLOSED unless the api_key grant's exact injection host is in
// allowed_domains — even under allow-all. So whenever an api_key/LLM grant is
// present, buildSpec MUST always include its injection host in
// allowed_domains, regardless of the allow-all toggle.
describe("buildSpec — allow-all egress + LLM api_key grant", () => {
  function stateWithLlmKey(overrides: Partial<WizardState> = {}): WizardState {
    return {
      ...initialWizardState(),
      // claude-code with an API key => api.anthropic.com injection host.
      agent: "claude-code",
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

  // BYOA ("none", harness.go's NoManagedAuth row): Wardyn wires that image no
  // model credential and knows no host for it, so a stored secret name must not
  // conjure an Anthropic injection rule — and an Anthropic host in
  // allowed_domains — onto a run that never speaks to Anthropic.
  it("names no host for the BYOA agent, so no api_key grant and no pinned host ship", () => {
    const { inline_policy } = buildSpec(stateWithLlmKey({ agent: "none" }));
    expect((inline_policy.eligible_grants ?? []).some((g) => g.kind === "api_key")).toBe(false);
    expect(inline_policy.allowed_domains ?? []).not.toContain("api.anthropic.com");
    expect(impliedEgressHosts(stateWithLlmKey({ agent: "none" })).map((h) => h.host)).not.toContain(
      "api.anthropic.com",
    );
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
    // D5/claim4: a half-configured PAT must never widen egress to the typed
    // host with no grant to justify it — requiredHosts' union is gated on the
    // SAME predicate as the grant emission above.
    expect(missingSecret.inline_policy.allowed_domains).not.toContain("gitlab.com");

    const missingHost = buildSpec({
      ...initialWizardState(),
      gitPatEnabled: true,
      gitPatHost: "",
      gitPatSecretName: "gl-pat",
    });
    expect((missingHost.inline_policy.eligible_grants ?? []).some((g) => g.kind === "git_pat")).toBe(false);
  });

  // The broker's mint is single-use per grant regardless of
  // RequiresApproval — an approval-gated git_pat authenticates exactly ONE
  // git operation, then a second in the same run 409s with no way to
  // re-approve mid-run. Default off (matching the cached github_token
  // lane's effective behavior); the operator can still opt back in on Access.
  it("defaults gitPatRequiresApproval to false — a fresh wizard's git_pat grant is not approval-gated", () => {
    expect(initialWizardState().gitPatRequiresApproval).toBe(false);
    const { inline_policy } = buildSpec({
      ...initialWizardState(),
      gitPatEnabled: true,
      gitPatHost: "gitlab.com",
      gitPatSecretName: "gl-pat",
    });
    const grant = (inline_policy.eligible_grants ?? []).find((g) => g.kind === "git_pat");
    expect(grant?.requires_approval).toBe(false);
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

// D6/claim3: buildSpec unions these hosts into allowed_domains without the
// operator ever toggling them on the Egress step. impliedEgressHosts is the
// ONE list both buildSpec and step-egress.tsx's "Added by grants:" row read,
// so the two can never drift on what counts as implied.
describe("impliedEgressHosts — the list buildSpec unions and step-egress.tsx renders", () => {
  // ticket: D6 claim3
  it("returns nothing when no grant is active and no repo workspace is selected", () => {
    expect(impliedEgressHosts(initialWizardState())).toEqual([]);
  });

  it("names the model-key host when an LLM secret is selected", () => {
    const state = { ...initialWizardState(), llmSecretName: "anthropic-api-key" };
    expect(impliedEgressHosts(state)).toEqual([{ host: "api.anthropic.com", why: "model key" }]);
  });

  it("names the OpenAI model-key host for codex-cli", () => {
    const state = { ...initialWizardState(), agent: "codex-cli" as const, llmSecretName: "openai-key" };
    expect(impliedEgressHosts(state)).toEqual([{ host: "api.openai.com", why: "model key" }]);
  });

  it("names GitHub access hosts when the GitHub grant is enabled", () => {
    const state = { ...initialWizardState(), githubEnabled: true };
    expect(impliedEgressHosts(state)).toEqual([
      { host: "github.com", why: "GitHub access" },
      { host: "*.githubusercontent.com", why: "GitHub access" },
    ]);
  });

  // Claim 3's sharpest sub-case: selecting a repo workspace silently widens
  // egress even with the GitHub switch untouched — the operator never visits
  // a control that mentions GitHub. Same two hosts, a different "why".
  it("names the SAME hosts 'repo workspace' when a repo is selected without the GitHub grant", () => {
    const repoWs = makeWorkspace({
      id: "ws-1",
      name: "app",
      kind: "repo",
      source: "acme/app",
      created_at: "",
      updated_at: "",
    });
    const state = { ...initialWizardState(), workspaces: [{ workspaceId: "ws-1" }] };
    expect(impliedEgressHosts(state, [repoWs])).toEqual([
      { host: "github.com", why: "repo workspace" },
      { host: "*.githubusercontent.com", why: "repo workspace" },
    ]);
  });

  it("prefers 'GitHub access' over 'repo workspace' when both are true", () => {
    const repoWs = makeWorkspace({
      id: "ws-1",
      name: "app",
      kind: "repo",
      source: "acme/app",
      created_at: "",
      updated_at: "",
    });
    const state = {
      ...initialWizardState(),
      githubEnabled: true,
      workspaces: [{ workspaceId: "ws-1" }],
    };
    expect(impliedEgressHosts(state, [repoWs])).toEqual([
      { host: "github.com", why: "GitHub access" },
      { host: "*.githubusercontent.com", why: "GitHub access" },
    ]);
  });

  // D5/claim4: a host typed with no secret selected must never claim to be
  // "added by grants" — the SAME gate as buildSpec's grant emission.
  it("names the Git PAT host only once host+secret are both configured", () => {
    const configured = {
      ...initialWizardState(),
      gitPatEnabled: true,
      gitPatHost: "dev.azure.com",
      gitPatSecretName: "ado-pat",
    };
    expect(impliedEgressHosts(configured)).toEqual([{ host: "dev.azure.com", why: "Git PAT" }]);

    const halfConfigured = {
      ...initialWizardState(),
      gitPatEnabled: true,
      gitPatHost: "dev.azure.com",
      gitPatSecretName: "",
    };
    expect(impliedEgressHosts(halfConfigured)).toEqual([]);
  });

  it("buildSpec's allowed_domains always contains every host this helper names — one list, can't drift", () => {
    const state = {
      ...initialWizardState(),
      allowedDomains: ["pypi.org"],
      llmSecretName: "anthropic-api-key",
      githubEnabled: true,
      gitPatEnabled: true,
      gitPatHost: "gitlab.com",
      gitPatSecretName: "gl-pat",
    };
    const impliedHosts = impliedEgressHosts(state).map((h) => h.host);
    expect(impliedHosts.length).toBeGreaterThan(0);
    const { inline_policy } = buildSpec(state);
    for (const host of impliedHosts) expect(inline_policy.allowed_domains).toContain(host);
  });
});

