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
  runPrefill,
  WIZARD_AGENTS,
} from "./wizard-types";
import { mergeRunSelections } from "./wizard-spec";
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

// PARITY-2: a multi-source workspace has no single kind/source to flatten to
// — internal/store/store.go's deriveWorkspaceMirrors bails (leaves Kind/
// Source empty) whenever len(Sources) != 1. buildSpec must iterate w.sources
// rather than read w.kind/w.source directly — reading those fields directly
// on a multi-source or migrated-ephemeral (migration 0029) workspace would
// silently attach NOTHING, a 400 "mount source is empty" at launch with no
// operator fix available.
describe("buildSpec — multi-source workspaces (PARITY-2)", () => {
  const multiWs = makeWorkspace({
    id: "ws-multi",
    name: "monorepo-plus-scratch",
    // The single-mirror fields a real multi-source record leaves EMPTY —
    // asserting the fix does NOT read these.
    kind: "",
    source: "",
    sources: [
      { type: "local_dir", path: "/home/me/api" },
      { type: "repo", source: "acme/widgets" },
      { type: "ephemeral", target: "/home/agent/scratch" },
    ],
    requirements: { "write:/home/me/api": { level: "required", provenance: "operator_set" } },
  });

  it("emits one workspace_mounts entry per local_dir source and one workspace_repos entry per repo source", () => {
    const { inline_policy } = buildSpec(
      { ...initialWizardState(), workspaces: [{ workspaceId: "ws-multi" }] },
      [multiWs],
    );
    expect(inline_policy.workspace_mounts).toEqual([
      { source: "/home/me/api", target: "/home/agent/work", read_only: false },
    ]);
    expect(inline_policy.workspace_repos).toEqual([{ repo: "acme/widgets" }]);
  });

  it("never emits an empty-source mount or repo (the exact PARITY-2 bug)", () => {
    const { inline_policy } = buildSpec(
      { ...initialWizardState(), workspaces: [{ workspaceId: "ws-multi" }] },
      [multiWs],
    );
    expect(inline_policy.workspace_mounts?.some((m) => m.source === "")).toBe(false);
    expect(inline_policy.workspace_repos?.some((r) => r.repo === "")).toBe(false);
  });

  it("names the run.repo label off the resolved repo, never the empty single-mirror source", () => {
    const { run } = buildSpec(
      { ...initialWizardState(), workspaces: [{ workspaceId: "ws-multi" }] },
      [multiWs],
    );
    expect(run.repo).toBe("acme/widgets");
  });

  // Migration 0029's exact rewrite of a legacy 'container' workspace:
  // sources=[{type:ephemeral,...}] + a custom base_image — mirrors as
  // Kind="ephemeral", Source="".
  it("a purely ephemeral (migrated legacy container) workspace attaches no mount/repo but conveys its identity via workspace_id", () => {
    const ephemeralWs = makeWorkspace({
      id: "ws-eph",
      name: "old-container",
      kind: "ephemeral",
      source: "",
      sources: [{ type: "ephemeral", target: "/home/agent/work" }],
    });
    const { run, inline_policy } = buildSpec(
      { ...initialWizardState(), workspaces: [{ workspaceId: "ws-eph" }] },
      [ephemeralWs],
    );
    expect(inline_policy.workspace_mounts).toBeUndefined();
    expect(inline_policy.workspace_repos).toBeUndefined();
    expect(run.repo).toBe("");
    // Residual PARITY-2: with no mount/repo, referencedWorkspaces can't match
    // the workspace, so its migration-0029 base_image would be dropped unless
    // its identity rides along for seedRequestWorkspace to resolve.
    expect(run.workspace_id).toBe("ws-eph");
  });

  it("does NOT send workspace_id when the selection already resolves to a mount (no double-seed)", () => {
    const ws = localDirWorkspace("ws-local");
    const { run } = buildSpec(
      { ...initialWizardState(), workspaces: [{ workspaceId: "ws-local" }] },
      [ws],
    );
    expect(run.workspace_id).toBeUndefined();
  });

  it("a workspace with no .sources at all still resolves via the legacy single-mirror fallback (fixture/back-compat)", () => {
    const ws = localDirWorkspace("ws-legacy");
    const { inline_policy } = buildSpec(
      { ...initialWizardState(), workspaces: [{ workspaceId: "ws-legacy" }] },
      [ws],
    );
    expect(inline_policy.workspace_mounts).toEqual([
      { source: "/home/me/ws-legacy", target: "/home/agent/work", read_only: true },
    ]);
  });
});


// What the run's MODE puts on the wire — each of these fields must survive
// the trip from form to sandbox, never silently ignored or dropped.
describe("buildSpec — the run mode decides what ships", () => {
  it("an interactive run's task rides as its optional boot seed, and its startup choice", () => {
    const { run } = buildSpec({
      ...initialWizardState(),
      mode: "interactive",
      // Part A1: the task now IS the boot seed — it ships verbatim, trimmed,
      // exactly like a batch run's prompt. Unlike the old "server ignores task
      // for interactive" behavior, there is no separate "leftover from batch
      // mode" case to guard: whatever is in the box when Launch is pressed is
      // what the operator wants seeded.
      task: "  review the failing tests in payments/  ",
      interactiveStart: "agent",
    });
    expect(run.interactive).toBe(true);
    expect(run.task).toBe("review the failing tests in payments/");
    expect(run.interactive_start).toBe("agent");
  });

  it("an interactive run with no seed sends an empty task (today's idle behavior, unchanged)", () => {
    const { run } = buildSpec({ ...initialWizardState(), mode: "interactive", task: "" });
    expect(run.interactive).toBe(true);
    expect(run.task).toBe("");
  });

  it("omits interactive_start for the default shell start", () => {
    const { run } = buildSpec({ ...initialWizardState(), mode: "interactive", interactiveStart: "shell" });
    // "shell" IS the long-standing behavior, so it never needs to go on the
    // wire — and the server's own default has to keep meaning the same thing.
    expect(run.interactive_start).toBeUndefined();
  });

  it("a batch run sends the task as the agent's prompt", () => {
    const { run } = buildSpec({ ...initialWizardState(), mode: "batch", task: "  fix the flaky test  " });
    expect(run.interactive).toBe(false);
    expect(run.task).toBe("fix the flaky test");
    expect(run.interactive_start).toBeUndefined();
  });

  // A shell command must never launch on the (default) interactive mode: the
  // server ignores task_mode for an interactive run, so the sandbox would
  // never run the command and the whole point of the run would vanish silently.
  it("a shell command is unattended even if the mode still says interactive", () => {
    const { run } = buildSpec({
      ...initialWizardState(),
      runType: "command",
      mode: "interactive",
      task: "make test",
    });
    expect(run.interactive).toBe(false);
    expect(run.task_mode).toBe("exec");
    expect(run.task).toBe("make test");
  });

  it("carries the title and description, trimmed, and omits an empty description", () => {
    const { run } = buildSpec({
      ...initialWizardState(),
      title: "  Refund flow  ",
      description: "  ticket 4412  ",
    });
    expect(run.title).toBe("Refund flow");
    expect(run.description).toBe("ticket 4412");
    expect(buildSpec({ ...initialWizardState(), title: "x" }).run.description).toBeUndefined();
  });
});

// A1's seed opt-in and the tool-approval posture — both new CreateRunInput
// fields riding this ONE form increment. Both follow the SAME shape as
// interactive_start above: the wire default is never sent, only the
// non-default explicit choice goes out — so a stale/leftover state field can
// never silently widen what a run is allowed to do.
describe("buildSpec — the boot-seed opt-in and tool-approval posture", () => {
  it("emits seed_auto_tools only for an agent-started seed with the toggle on", () => {
    const { run } = buildSpec({
      ...initialWizardState(),
      mode: "interactive",
      interactiveStart: "agent",
      task: "review the failing tests",
      seedAutoTools: true,
    });
    expect(run.seed_auto_tools).toBe(true);
  });

  it("omits seed_auto_tools when the toggle is off (the wire default)", () => {
    const { run } = buildSpec({
      ...initialWizardState(),
      mode: "interactive",
      interactiveStart: "agent",
      task: "review the failing tests",
      seedAutoTools: false,
    });
    expect(run.seed_auto_tools).toBeUndefined();
  });

  it("omits seed_auto_tools with no seed text, even with the toggle on", () => {
    const { run } = buildSpec({
      ...initialWizardState(),
      mode: "interactive",
      interactiveStart: "agent",
      task: "",
      seedAutoTools: true,
    });
    expect(run.seed_auto_tools).toBeUndefined();
  });

  it("omits seed_auto_tools for a shell startup command, even with the toggle on", () => {
    const { run } = buildSpec({
      ...initialWizardState(),
      mode: "interactive",
      interactiveStart: "shell",
      task: "npm ci && npm run dev",
      seedAutoTools: true,
    });
    expect(run.seed_auto_tools).toBeUndefined();
  });

  it("omits seed_auto_tools for a batch run (structurally can't ride one)", () => {
    const { run } = buildSpec({
      ...initialWizardState(),
      mode: "batch",
      task: "review the failing tests",
      seedAutoTools: true,
    });
    expect(run.seed_auto_tools).toBeUndefined();
  });

  it("emits tool_approvals: hold for an autonomous claude-code run", () => {
    const { run } = buildSpec({
      ...initialWizardState(),
      runType: "agent",
      mode: "batch",
      agent: "claude-code",
      task: "fix the flaky test",
      toolApprovals: "hold",
    });
    expect(run.tool_approvals).toBe("hold");
  });

  it("never emits tool_approvals: auto — the wire default is omitted, not sent", () => {
    const { run } = buildSpec({
      ...initialWizardState(),
      runType: "agent",
      mode: "batch",
      task: "fix the flaky test",
      toolApprovals: "auto",
    });
    expect(run.tool_approvals).toBeUndefined();
  });

  it("omits tool_approvals for codex-cli, even with hold selected (buildSpec re-asserts the server's own rejection)", () => {
    const { run } = buildSpec({
      ...initialWizardState(),
      runType: "agent",
      mode: "batch",
      agent: "codex-cli",
      task: "fix the flaky test",
      toolApprovals: "hold",
    });
    expect(run.tool_approvals).toBeUndefined();
  });

  it("omits tool_approvals for a shell command", () => {
    const { run } = buildSpec({
      ...initialWizardState(),
      runType: "command",
      mode: "batch",
      task: "make test",
      toolApprovals: "hold",
    });
    expect(run.tool_approvals).toBeUndefined();
  });

  it("omits tool_approvals for an interactive run (out of scope structurally)", () => {
    const { run } = buildSpec({
      ...initialWizardState(),
      runType: "agent",
      mode: "interactive",
      agent: "claude-code",
      toolApprovals: "hold",
    });
    expect(run.tool_approvals).toBeUndefined();
  });
});

// mergeRunSelections — /runs/new's post-parse union. The panel's JSON is the
// authored policy; only what it CANNOT know is added back, and only once.
describe("mergeRunSelections — the authored spec wins, the selections are added", () => {
  const base = () => ({
    allowed_domains: ["api.anthropic.com"],
    first_use_approval: "deny_with_review" as const,
    min_confinement_class: "CC2" as const,
  });

  it("never overwrites a key the JSON owns", () => {
    const authored = { ...base(), auto_stop_after_sec: 900, allow_all_egress: true };
    const { spec } = mergeRunSelections(authored, initialWizardState());
    expect(spec.auto_stop_after_sec).toBe(900);
    expect(spec.allow_all_egress).toBe(true);
    expect(spec.first_use_approval).toBe("deny_with_review");
    expect(spec.min_confinement_class).toBe("CC2");
  });

  // Dedupe is deep equality over the FULL canonicalized GrantSpec, so key ORDER
  // and an omitted-vs-written default must not turn one grant into two...
  it("treats a re-spelled identical grant as the same grant", () => {
    const state: WizardState = { ...initialWizardState(), llmSecretName: "anthropic-api-key" };
    expect(buildSpec(state).inline_policy.eligible_grants).toHaveLength(1);
    // Same grant: keys in another order, requires_approval left out (Go
    // defaults it false, which is exactly what the composed one says).
    const authored = {
      ...base(),
      eligible_grants: [
        {
          scope: {
            secret_name: "anthropic-api-key",
            format: "%s",
            host: "api.anthropic.com",
            header: "x-api-key",
          },
          kind: "api_key",
        },
      ],
    } as unknown as Parameters<typeof mergeRunSelections>[0];
    const { spec, added } = mergeRunSelections(authored, state);
    expect(added.grants).toEqual([]);
    expect(spec.eligible_grants).toHaveLength(1);
  });

  // ...but any PARTIAL key would silently DROP a real grant: two grants can
  // agree on kind AND scope and still differ on TTL or requires_approval.
  it("keeps a grant that differs only on ttl_seconds or requires_approval", () => {
    const state: WizardState = { ...initialWizardState(), llmSecretName: "anthropic-api-key" };
    const composed = buildSpec(state).inline_policy.eligible_grants![0];
    const authored = {
      ...base(),
      eligible_grants: [{ ...composed, ttl_seconds: 60 }, { ...composed, requires_approval: true }],
    };
    const { spec, added } = mergeRunSelections(authored, state);
    expect(added.grants).toHaveLength(1);
    expect(spec.eligible_grants).toHaveLength(3);
  });

  // Credential injection only rewrites requests whose host is on the allowlist —
  // under allow_all_egress too. A hand-written api_key grant nobody pinned
  // authenticates nothing, and wizard state cannot know it exists.
  it("pins a JSON-authored api_key host the wizard never saw", () => {
    const authored = {
      ...base(),
      allowed_domains: [],
      allow_all_egress: true,
      eligible_grants: [
        { kind: "api_key", scope: { host: "llm.acme.internal" }, requires_approval: false },
      ],
    };
    const { spec, added } = mergeRunSelections(authored, initialWizardState());
    expect(added.hosts).toEqual(["llm.acme.internal"]);
    expect(spec.allowed_domains).toEqual(["llm.acme.internal"]);
  });

  it("does not re-announce a host the JSON already allows", () => {
    const authored = {
      ...base(),
      eligible_grants: [
        { kind: "api_key", scope: { host: "api.anthropic.com" }, requires_approval: false },
      ],
    };
    const { spec, added } = mergeRunSelections(authored, initialWizardState());
    expect(added.hosts).toEqual([]);
    expect(spec.allowed_domains).toEqual(["api.anthropic.com"]);
  });

  it("adds the Workspace card's mounts, which the JSON cannot know", () => {
    const ws = localDirWorkspace("ws-1");
    const state: WizardState = {
      ...initialWizardState(),
      workspaces: [{ workspaceId: "ws-1", enabledOptional: [] }],
    };
    const { spec, added } = mergeRunSelections(base(), state, [ws]);
    expect(added.mounts).toHaveLength(1);
    expect(spec.workspace_mounts).toEqual(added.mounts);
  });
});

// The member's user drive. The whole contract is what buildSpec DOESN'T emit:
// a run that never ticked the box must produce the byte-identical body it
// produced before the field existed, and `read_only: false` must never be sent
// — the server refuses it as an attempt to widen a read-only allocation, so
// sending it would turn a member's untouched toggle into a refused launch.
describe("buildSpec — drive", () => {
  it("emits nothing at all when the box is unticked", () => {
    const { run } = buildSpec(initialWizardState());
    expect(run.drive).toBeUndefined();
    expect("drive" in run).toBe(false);
  });

  it("emits {enabled:true} and no read_only when the toggle is off", () => {
    const { run } = buildSpec({ ...initialWizardState(), driveEnabled: true });
    expect(run.drive).toEqual({ enabled: true });
  });

  it("adds read_only:true only when the run narrows the mount", () => {
    const { run } = buildSpec({
      ...initialWizardState(),
      driveEnabled: true,
      driveReadOnly: true,
    });
    expect(run.drive).toEqual({ enabled: true, read_only: true });
  });

  it("never emits a drive for a narrowing with no mount asked for", () => {
    const { run } = buildSpec({ ...initialWizardState(), driveReadOnly: true });
    expect(run.drive).toBeUndefined();
  });
});

// B4b — "Start a run like this one"
//
// A re-run button cannot assume the run record holds everything needed: AgentRun
// carries no task_mode, no interactive_start, no seed_auto_tools and no
// tool_approvals, and an inline policy is never persisted at all. So the clone
// reads TWO durable sources — the run row and its `run.create` audit event —
// and NAMES the one thing it cannot carry. A clone that silently dropped
// `tool_approvals: hold` would launch a less supervised run than the one it
// copied, which is exactly the class of failure this product exists to prevent.
describe("runPrefill: the clone carries both sources, and says what it cannot", () => {
  // ticket: B4b
  // A killed run as the two sources actually leave it behind.
  const row = {
    agent: "codex-cli",
    task: "rerun the migration",
    title: "Migration 0062",
    description: "ticket 4412",
    policy_id: "pol-7",
    confinement_class: "CC3" as const,
    interactive: true,
    workspace_ids: ["ws-a", "ws-b"],
  };
  // …and everything createRunAuditData stamps that the row cannot hold.
  const created = {
    task_mode: "exec",
    interactive_start: "shell",
    seed_auto_tools: true,
    tool_approvals: "hold",
    inline_policy: false,
  };

  it("round-trips every field the two sources carry", () => {
    const state = initialWizardState("CC1", runPrefill(row, created).state);

    // From the run ROW.
    expect(state.title).toBe("Migration 0062");
    expect(state.description).toBe("ticket 4412");
    expect(state.agent).toBe("codex-cli");
    expect(state.task).toBe("rerun the migration");
    expect(state.confinementClass).toBe("CC3"); // NOT the "CC1" default above
    expect(state.mode).toBe("interactive");
    expect(state.selectedPolicyId).toBe("pol-7");
    expect(state.workspaces).toEqual([{ workspaceId: "ws-a" }, { workspaceId: "ws-b" }]);

    // From the run.create AUDIT ROW — the half the run record cannot hold, and
    // the half a clone built off the row alone would have silently dropped.
    expect(state.runType).toBe("command"); // task_mode: exec
    expect(state.interactiveStart).toBe("shell");
    expect(state.seedAutoTools).toBe(true);
    expect(state.toolApprovals).toBe("hold");
  });

  it("the DOCUMENTED REMAINDER stays at its fresh-wizard default, never a stale copy", () => {
    const fresh = initialWizardState("CC1");
    const state = initialWizardState("CC1", runPrefill(row, created).state);

    // Credentials and the approvals gating them are minted per run: carrying
    // them over would mint a credential nobody asked for on THIS launch.
    expect(state.githubEnabled).toBe(fresh.githubEnabled);
    expect(state.gitPatEnabled).toBe(fresh.gitPatEnabled);
    expect(state.llmSecretName).toBe(fresh.llmSecretName);
    // Egress and first-use posture live in the policy (carried by policy_id,
    // or named as lost) — never re-derived from the run's own decisions.
    expect(state.allowedDomains).toEqual(fresh.allowedDomains);
    expect(state.firstUseApproval).toBe(fresh.firstUseApproval);
    // AgentRun.image is the RESOLVED sandbox image; WizardState.image is a BYOI
    // BASE, which buildSpec sends as one. Carrying it would turn every clone
    // into a BYOI request — refused where no image builder is wired, re-wrapped
    // where one is, and for a real BYOI run it would ask to wrap that run's own
    // wrapper. No UI field shows it, so the operator could not even see it.
    expect(state.image).toBe("");
    // Request-scoped and on neither source: there is nothing durable to read.
    expect(state.driveEnabled).toBe(fresh.driveEnabled);
    expect(state.integrationId).toBeUndefined();
    expect(state.lifecycle).toBe(fresh.lifecycle);
    expect(state.autoStopMinutes).toBe(fresh.autoStopMinutes);
  });

  it("an INLINE-policy run clones without its policy, and the clone says so", () => {
    // inline_policy runs record `inline_policy: true` and a nil policy id —
    // the document itself is never stored (internal/api/inline_policy.go), so
    // there is nothing to prefill and the wizard must not pretend otherwise.
    const prefill = runPrefill({ ...row, policy_id: undefined }, { ...created, inline_policy: true });

    expect(prefill.inlinePolicy).toBe(true);
    expect(prefill.state.selectedPolicyId).toBeUndefined();
    // The barrier still comes across — it IS on the row — so the clone is not
    // silently weaker than the run it copies on the one axis it can carry.
    expect(initialWizardState("CC1", prefill.state).confinementClass).toBe("CC3");
  });

  it("a saved-policy run carries the reference and raises no ceiling note", () => {
    const prefill = runPrefill(row, created);
    expect(prefill.inlinePolicy).toBe(false);
    expect(prefill.state.selectedPolicyId).toBe("pol-7");
  });

  it("an older trail with no run.create data leaves the request-scoped half at its default", () => {
    // The pre-0.7 case, and the truncated-trail case: absent must read as "not
    // asked for", never as a guess. An invented `tool_approvals: hold` would be
    // as wrong as a dropped one.
    const state = initialWizardState("CC1", runPrefill(row).state);
    expect(state.runType).toBe("agent");
    expect(state.interactiveStart).toBe("agent");
    expect(state.seedAutoTools).toBe(false);
    expect(state.toolApprovals).toBe("auto");
  });

  it("an agent id this build cannot spell leaves the picker on its default", () => {
    // A retired harness on an old run must not write an unlaunchable value
    // into the picker — buildSpec would emit it and create would 400.
    const state = initialWizardState("CC1", runPrefill({ ...row, agent: "cursor" }).state);
    expect(state.agent).toBe(initialWizardState("CC1").agent);
  });

  // …and the check is asked of the ROSTER, so widening it (C-UI reads the
  // harness catalog) carries the clone path with it. A second literal pair is
  // how a new agent ships pickable but unclonable.
  it("clones every agent the roster carries, whatever the roster becomes", () => {
    for (const agent of WIZARD_AGENTS) {
      expect(initialWizardState("CC1", runPrefill({ ...row, agent }).state).agent).toBe(agent);
    }
  });

  // The BYOI trap, stated on the wire body rather than only on the state: this
  // is the assertion that would have caught it.
  it("the built body of a cloned NON-BYOI run carries no image at all", () => {
    const { run } = buildSpec(initialWizardState("CC1", runPrefill(row, created).state));
    expect(run.image).toBeUndefined();
  });

  // The prefill travels as react-router navigation state, which is
  // structured-cloned into history — a function or a class instance on it
  // would throw at navigate() time, in the browser, and nowhere else.
  it("is structured-cloneable, because it rides history state", () => {
    expect(() => structuredClone(runPrefill(row, created))).not.toThrow();
  });

  // And the whole point: the clone is LAUNCHABLE. A prefill that produced a
  // spec create would refuse is a button that looks like a remedy and is not.
  it("produces a spec buildSpec accepts", () => {
    const state = initialWizardState("CC1", runPrefill(row, created).state);
    const { run } = buildSpec(state);
    expect(run.agent).toBe("codex-cli");
    expect(run.confinement_class).toBe("CC3");
  });
});
