/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import {
  buildSpec,
  comesWithLine,
  impliedEgressHosts,
  initialWizardState,
  isValidDomain,
  secretAutoGrants,
  validateStep,
  wizardStateFromProposal,
} from "./wizard-types";
import type { RunWorkspaceSelectionWire, WizardState } from "./wizard-types";
import type { ComposeRunProposal, RunPolicySpec, Workspace } from "../../../lib/types";
import { SUBSCRIPTION_OAUTH_SECRET } from "../../../lib/types";

// Workspace.requirements isn't on the shared Workspace TS type yet (see
// wizard-types.ts's own import comment) — cast, matching how the module itself
// reads it.
function localDirWorkspace(id: string, requirements: Record<string, unknown> = {}): Workspace {
  return {
    id,
    name: id,
    kind: "local_dir",
    source: `/home/me/${id}`,
    status: "scanned",
    created_at: "",
    updated_at: "",
    requirements,
  } as Workspace;
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

// Honesty constraint (Stage 4): the TRUST BOUNDARY in
// internal/api/runs_create.go's applyWorkspaceRequirements auto-grants a
// secret ONLY off an operator_set requirement row — a scan_seeded row never
// does, whatever its level. secretAutoGrants is the client-side read of that
// same boundary, so a Required-secret summary can never claim "you get this
// automatically" for one the server will actually skip.
describe("secretAutoGrants — the scan_seeded trust boundary, read client-side", () => {
  it("an operator_set secret requirement auto-grants", () => {
    const ws = localDirWorkspace("ws-1", {
      "secret:DATABASE_URL": { level: "required", provenance: "operator_set" },
    });
    expect(secretAutoGrants(ws, "DATABASE_URL")).toBe(true);
  });

  it("a scan_seeded secret requirement never auto-grants, even though it's Required", () => {
    const ws = localDirWorkspace("ws-1", {
      "secret:DATABASE_URL": { level: "required", provenance: "scan_seeded" },
    });
    expect(secretAutoGrants(ws, "DATABASE_URL")).toBe(false);
  });

  it("a name with no requirement row at all doesn't auto-grant", () => {
    const ws = localDirWorkspace("ws-1", {});
    expect(secretAutoGrants(ws, "NOT_DECLARED")).toBe(false);
  });
});

// Item 4 (found by live driving): enabling an Optional row on the picker
// produced a Review whose "Comes with:" summary never changed — the one
// screen whose whole job is "show what you're launching" silently dropped
// the edit. comesWithLine's second (optional) `sel` argument folds in this
// run's actual enabled_optional set; omitting it (workspace-detail.tsx, which
// has no per-run selection) must stay byte-identical to the old signature.
describe("comesWithLine — reflects this run's enabled Optional rows (Item 4)", () => {
  it("stays byte-identical to the contract-only summary when sel is omitted", () => {
    const ws = localDirWorkspace("ws-1", {
      "secret:DATABASE_URL": { level: "required", provenance: "operator_set" },
      "egress:api.stripe.com": { level: "optional", provenance: "operator_set" },
    });
    expect(comesWithLine(ws)).toBe("1 secret");
  });

  it("an enabled Optional HOST is counted (no trust-boundary gate on egress)", () => {
    const ws = localDirWorkspace("ws-1", {
      "secret:DATABASE_URL": { level: "required", provenance: "operator_set" },
      "egress:telemetry.example.com": { level: "optional", provenance: "scan_seeded" },
    });
    const unset = { workspaceId: "ws-1" };
    const enabled = { workspaceId: "ws-1", enabledOptional: ["egress:telemetry.example.com"] };
    expect(comesWithLine(ws, unset)).toBe("1 secret");
    expect(comesWithLine(ws, enabled)).toBe("1 secret · 1 opted in");
  });

  // Honesty constraint the finding itself calls out: an opted-in secret that
  // secretAutoGrants says the server will actually SKIP (scan_seeded) must
  // never read as though it's coming along.
  it("an enabled Optional SECRET that won't auto-grant (scan_seeded) is NOT counted", () => {
    const ws = localDirWorkspace("ws-1", {
      "secret:UNVERIFIED_TOKEN": { level: "optional", provenance: "scan_seeded" },
    });
    const enabled = { workspaceId: "ws-1", enabledOptional: ["secret:UNVERIFIED_TOKEN"] };
    expect(comesWithLine(ws, enabled)).toBe("nothing beyond the auto-allowed set");
  });

  it("an enabled Optional SECRET that WILL auto-grant (operator_set) IS counted", () => {
    const ws = localDirWorkspace("ws-1", {
      "secret:STRIPE_KEY": { level: "optional", provenance: "operator_set" },
    });
    const enabled = { workspaceId: "ws-1", enabledOptional: ["secret:STRIPE_KEY"] };
    expect(comesWithLine(ws, enabled)).toBe("1 opted in");
  });

  it("write access reflects an enabled Optional write, not just a Required one", () => {
    const ws = localDirWorkspace("ws-1", {
      "write:/home/me/ws-1": { level: "optional", provenance: "scan_seeded" },
    });
    const unset = { workspaceId: "ws-1" };
    const enabled = { workspaceId: "ws-1", enabledOptional: ["write:/home/me/ws-1"] };
    expect(comesWithLine(ws, unset)).toBe("nothing beyond the auto-allowed set");
    expect(comesWithLine(ws, enabled)).toBe("write access");
  });
});

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
// never synthesize the ~/.claude mount — retired anyway, model access now
// resolves from integrations, not a per-run subscription dir). Hydrating it
// must NOT carry the sentinel into llmSecretName and re-emit it as an x-api-key
// grant to a secret that doesn't exist (the "references unknown secret" launch
// failure).
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

  it("never carries the sentinel secret name into llmSecretName", () => {
    const state = wizardStateFromProposal(run, spec);
    expect(state.llmSecretName).toBe("");
  });

  it("re-building never emits a dangling api_key grant to the sentinel", () => {
    const { inline_policy } = buildSpec(wizardStateFromProposal(run, spec));
    const apiKey = (inline_policy.eligible_grants ?? []).find((g) => g.kind === "api_key");
    expect(apiKey).toBeUndefined(); // no real secret name to re-emit a grant for
  });
});

// Item 3 (medium): "Edit in wizard" used to silently drop every Optional
// opt-in the operator made on the AI path — the matched selection only ever
// carried the mount's INFERRED read-only flag, never enabledOptional, because
// wizardStateFromProposal never saw the compose proposal's OWN
// workspace_selections echo (ComposeResponse.proposed.workspace_selections).
describe("wizardStateFromProposal — threads the echoed workspace_selections (Item 3)", () => {
  const ws: Workspace = {
    id: "ws-1",
    name: "app",
    kind: "local_dir",
    source: "/home/me/app",
    status: "scanned",
    created_at: "",
    updated_at: "",
  };
  const run = { agent: "claude-code", repo: "local:app", interactive: true } as ComposeRunProposal;
  const spec: RunPolicySpec = {
    allowed_domains: ["api.anthropic.com"],
    first_use_approval: "deny_with_review",
    min_confinement_class: "CC1",
    workspace_mounts: [{ source: "/home/me/app", target: "/home/agent/work", read_only: false }],
  };

  it("merges enabled_optional from the echo onto the matched selection", () => {
    const echoed: RunWorkspaceSelectionWire[] = [
      { workspace_id: "ws-1", enabled_optional: ["egress:api.stripe.com"] },
    ];
    const state = wizardStateFromProposal(run, spec, [ws], echoed);
    expect(state.workspaces).toEqual([
      { workspaceId: "ws-1", readOnly: false, enabledOptional: ["egress:api.stripe.com"] },
    ]);
  });

  it("the echo's read_only wins over the workMount-inferred value when both are present", () => {
    // workMount.read_only is false (writable) but the echo says true (this
    // run narrowed it) — the echo IS what produced that mount in the first
    // place, so it must win over the inferred fallback.
    const echoed: RunWorkspaceSelectionWire[] = [{ workspace_id: "ws-1", read_only: true }];
    const state = wizardStateFromProposal(run, spec, [ws], echoed);
    expect(state.workspaces).toEqual([{ workspaceId: "ws-1", readOnly: true, enabledOptional: undefined }]);
  });

  it("ignores an echo entry for a different workspace id", () => {
    const echoed: RunWorkspaceSelectionWire[] = [
      { workspace_id: "ws-OTHER", enabled_optional: ["egress:unrelated.example.com"] },
    ];
    const state = wizardStateFromProposal(run, spec, [ws], echoed);
    expect(state.workspaces).toEqual([{ workspaceId: "ws-1", readOnly: false, enabledOptional: undefined }]);
  });

  it("degrades to the workMount-inferred readOnly with no enabledOptional when nothing was echoed (older server / no match)", () => {
    const state = wizardStateFromProposal(run, spec, [ws]); // no 4th arg — same as before Item 3
    expect(state.workspaces).toEqual([{ workspaceId: "ws-1", readOnly: false, enabledOptional: undefined }]);
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
    // D5/claim4 (worse than filed): a half-configured PAT used to still widen
    // egress to the typed host even with no grant to justify it. requiredHosts'
    // union is now gated on the SAME predicate as the grant emission above.
    expect(missingSecret.inline_policy.allowed_domains).not.toContain("gitlab.com");

    const missingHost = buildSpec({
      ...initialWizardState(),
      gitPatEnabled: true,
      gitPatHost: "",
      gitPatSecretName: "gl-pat",
    });
    expect((missingHost.inline_policy.eligible_grants ?? []).some((g) => g.kind === "git_pat")).toBe(false);
  });
});

// D5/claim4: validateStep("access") used to check only the GitHub repo list +
// TTL — a git_pat switched on with a host-or-secret left blank produced no
// error and an enabled Next, yet (pre-fix) still widened egress to the typed
// host with no PAT ever brokered to reach it.
describe("validateStep — Access: half-configured git_pat (D5/claim4)", () => {
  it("errors when enabled with a host but no stored secret", () => {
    const state = {
      ...initialWizardState(),
      gitPatEnabled: true,
      gitPatHost: "dev.azure.com",
      gitPatSecretName: "",
    };
    expect(validateStep("access", state)).toBe("Git PAT needs both a host and a stored secret.");
  });

  it("errors when enabled with a secret but no host", () => {
    const state = {
      ...initialWizardState(),
      gitPatEnabled: true,
      gitPatHost: "",
      gitPatSecretName: "ado-pat",
    };
    expect(validateStep("access", state)).toBe("Git PAT needs both a host and a stored secret.");
  });

  it("passes when disabled, regardless of host/secret", () => {
    const state = { ...initialWizardState(), gitPatEnabled: false, gitPatHost: "", gitPatSecretName: "" };
    expect(validateStep("access", state)).toBeNull();
  });

  it("passes when enabled with both host and secret set", () => {
    const state = {
      ...initialWizardState(),
      gitPatEnabled: true,
      gitPatHost: "dev.azure.com",
      gitPatSecretName: "ado-pat",
    };
    expect(validateStep("access", state)).toBeNull();
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
describe("impliedEgressHosts — the list buildSpec unions and step-egress.tsx renders (D6/claim3)", () => {
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
    const repoWs = {
      id: "ws-1",
      name: "app",
      kind: "repo",
      source: "acme/app",
      status: "scanned",
      created_at: "",
      updated_at: "",
    } as Workspace;
    const state = { ...initialWizardState(), workspaces: [{ workspaceId: "ws-1" }] };
    expect(impliedEgressHosts(state, [repoWs])).toEqual([
      { host: "github.com", why: "repo workspace" },
      { host: "*.githubusercontent.com", why: "repo workspace" },
    ]);
  });

  it("prefers 'GitHub access' over 'repo workspace' when both are true", () => {
    const repoWs = {
      id: "ws-1",
      name: "app",
      kind: "repo",
      source: "acme/app",
      status: "scanned",
      created_at: "",
      updated_at: "",
    } as Workspace;
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

// isValidDomain is a paste-guard, not a shape rule: ValidDomainEntry
// (internal/egress/proxy/policy.go) is the single source of truth, and the
// client being STRICTER than it is what made port-qualified and single-label
// corp hosts untypeable in the wizard. Pin the forms domain_entry_test.go
// accepts so that can't regress.
describe("isValidDomain — never stricter than the server's ValidDomainEntry", () => {
  it("accepts every form the server accepts", () => {
    for (const d of [
      "api.anthropic.com",
      "*.amazonaws.com",
      "example.com:443",
      "*.example.com:443",
      "artifactory.corp:8443",
      "registry:5000",
      "127.0.0.1",
      "::1",
    ]) {
      expect(isValidDomain(d), d).toBe(true);
    }
  });

  it("still catches the obvious slip (empty entry, pasted URL)", () => {
    expect(isValidDomain("")).toBe(false);
    expect(isValidDomain("   ")).toBe(false);
    expect(isValidDomain("https://example.com/api")).toBe(false);
    expect(isValidDomain("example.com /etc")).toBe(false);
  });
});
