/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import {
  applyProfileSpecToState,
  buildSpec,
  comesWithLine,
  impliedEgressHosts,
  initialWizardState,
  isValidDomain,
  primaryWorkspaceId,
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

// W15-W15e-wizard-roundtrip-3: a recorded/composed spec's git_pat grant used
// to vanish entirely on hydration (only github_token/api_key were read) while
// spec.allowed_domains still carried its host — "Edit in wizard" (and the
// recorded-profile fast-track, which delegates to this same function) would
// re-launch a run that reaches the host with no credential to authenticate.
describe("wizardStateFromProposal — hydrates the git_pat grant, not just github_token/api_key", () => {
  const run = { agent: "claude-code", repo: "local:corp-app", interactive: true } as ComposeRunProposal;
  const spec: RunPolicySpec = {
    allowed_domains: ["dev.azure.com"],
    first_use_approval: "deny_with_review",
    min_confinement_class: "CC2",
    eligible_grants: [
      {
        kind: "git_pat",
        scope: { host: "dev.azure.com", secret_name: "ado-pat", username: "myuser" },
        requires_approval: false,
      },
    ],
  };

  it("carries host/secret/username/requires_approval into the wizard's own git_pat fields", () => {
    const state = wizardStateFromProposal(run, spec);
    expect(state.gitPatEnabled).toBe(true);
    expect(state.gitPatHost).toBe("dev.azure.com");
    expect(state.gitPatSecretName).toBe("ado-pat");
    expect(state.gitPatUsername).toBe("myuser");
    expect(state.gitPatRequiresApproval).toBe(false);
  });

  it("re-building re-emits the SAME git_pat grant, not a dropped one", () => {
    const { inline_policy } = buildSpec(wizardStateFromProposal(run, spec));
    const grant = (inline_policy.eligible_grants ?? []).find((g) => g.kind === "git_pat");
    expect(grant?.scope).toEqual({ host: "dev.azure.com", secret_name: "ado-pat", username: "myuser" });
    // The host stayed in allowed_domains BOTH before and after — the defect
    // was the credential vanishing while the host stayed reachable.
    expect(inline_policy.allowed_domains).toContain("dev.azure.com");
  });

  it("no git_pat grant present -> gitPatEnabled stays false (no false positive)", () => {
    const noGrant: RunPolicySpec = { allowed_domains: [], first_use_approval: "always_deny", min_confinement_class: "CC2" };
    expect(wizardStateFromProposal(run, noGrant).gitPatEnabled).toBe(false);
  });
});

// W15-W15e-wizard-roundtrip-3 (part 2): the finding also names ssh_key —
// this wizard has no editable UI for ssh_key/cloud_sts grants at all, so
// (unlike git_pat, which got dedicated Access fields) they must round-trip
// through a pass-through bucket instead: hydrated verbatim into
// WizardState.opaqueGrants and re-emitted unchanged by buildSpec, never
// silently dropped.
describe("wizardStateFromProposal + buildSpec — pass through grant kinds this wizard can't edit (ssh_key, cloud_sts)", () => {
  const run = { agent: "claude-code", repo: "local:corp-app", interactive: true } as ComposeRunProposal;

  it("carries an ssh_key grant through hydration and back out via buildSpec, unchanged (previously silently dropped)", () => {
    const spec: RunPolicySpec = {
      allowed_domains: ["ssh.dev.azure.com"],
      first_use_approval: "always_deny",
      min_confinement_class: "CC2",
      eligible_grants: [
        {
          kind: "ssh_key",
          scope: { host: "ssh.dev.azure.com", secret_name: "ado-ssh-key" },
          requires_approval: false,
        },
      ],
    };
    const state = wizardStateFromProposal(run, spec);
    expect(state.opaqueGrants).toEqual(spec.eligible_grants);
    const { inline_policy } = buildSpec(state);
    expect(inline_policy.eligible_grants).toEqual(spec.eligible_grants);
  });

  it("carries a cloud_sts grant through the same way", () => {
    const spec: RunPolicySpec = {
      allowed_domains: [],
      first_use_approval: "always_deny",
      min_confinement_class: "CC2",
      eligible_grants: [{ kind: "cloud_sts", scope: { role_arn: "arn:aws:iam::123:role/x" }, requires_approval: true }],
    };
    const { inline_policy } = buildSpec(wizardStateFromProposal(run, spec));
    expect(inline_policy.eligible_grants).toEqual(spec.eligible_grants);
  });

  it("still hydrates git_pat through its own dedicated fields, not the passthrough bucket (no duplicate emission)", () => {
    const spec: RunPolicySpec = {
      allowed_domains: ["dev.azure.com"],
      first_use_approval: "always_deny",
      min_confinement_class: "CC2",
      eligible_grants: [
        { kind: "git_pat", scope: { host: "dev.azure.com", secret_name: "ado-pat" }, requires_approval: false },
      ],
    };
    const state = wizardStateFromProposal(run, spec);
    expect(state.opaqueGrants).toEqual([]); // git_pat is a KNOWN kind — not opaque
    expect(state.gitPatEnabled).toBe(true); // hydrated via its own dedicated fields instead
    const { inline_policy } = buildSpec(state);
    const gitPatGrants = (inline_policy.eligible_grants ?? []).filter((g) => g.kind === "git_pat");
    expect(gitPatGrants).toHaveLength(1); // not duplicated by the passthrough bucket
  });

  it("a fresh wizard state emits no opaque grants", () => {
    const { inline_policy } = buildSpec(initialWizardState());
    expect(inline_policy.eligible_grants).toBeUndefined();
  });
});

// W15-W15e-wizard-roundtrip-4: the two-state read/read+write toggle must
// never WIDEN an asymmetric source scope — re-emitting a pull_requests:write
// grant the recording never had contradicts Record Mode's "reuse can only
// ever subset" claim.
describe("wizardStateFromProposal + buildSpec — github_token scope never widens on round trip", () => {
  const run = { agent: "claude-code", repo: "org/repo", interactive: true } as ComposeRunProposal;

  it("contents:write ALONE (no pull_requests:write) collapses to read-only, not read+write", () => {
    const spec: RunPolicySpec = {
      allowed_domains: ["github.com"],
      first_use_approval: "deny_with_review",
      min_confinement_class: "CC2",
      eligible_grants: [
        { kind: "github_token", scope: { repos: ["org/repo"], permissions: { contents: "write" } }, requires_approval: true },
      ],
    };
    const state = wizardStateFromProposal(run, spec);
    expect(state.githubPermission).toBe("read");
    const { inline_policy } = buildSpec(state);
    const grant = (inline_policy.eligible_grants ?? []).find((g) => g.kind === "github_token");
    expect(grant?.scope?.permissions).toEqual({ contents: "read" });
    expect(grant?.scope?.permissions).not.toHaveProperty("pull_requests");
  });

  it("contents:write AND pull_requests:write together round-trip as read+write, unchanged", () => {
    const spec: RunPolicySpec = {
      allowed_domains: ["github.com"],
      first_use_approval: "deny_with_review",
      min_confinement_class: "CC2",
      eligible_grants: [
        {
          kind: "github_token",
          scope: { repos: ["org/repo"], permissions: { contents: "write", pull_requests: "write" } },
          requires_approval: true,
        },
      ],
    };
    const state = wizardStateFromProposal(run, spec);
    expect(state.githubPermission).toBe("read+write");
    const { inline_policy } = buildSpec(state);
    const grant = (inline_policy.eligible_grants ?? []).find((g) => g.kind === "github_token");
    expect(grant?.scope?.permissions).toEqual({ contents: "write", pull_requests: "write" });
  });
});

// W15-W15e-wizard-roundtrip-7: allowed_domains is a REQUIRED field — `[]` is
// always a real, deliberate value (Record Mode's own synthesized profile
// writes exactly `[]` for a genuine deny-all outcome), never "unset". The
// recorded-profile fast-track skips straight to Review, so a silent rewrite
// here reaches Launch with nobody having seen Egress to catch it.
describe("wizardStateFromProposal — an empty allowed_domains (deny-all) round-trips as empty, not the fresh-wizard default", () => {
  const run = { agent: "claude-code", repo: "org/repo", interactive: true } as ComposeRunProposal;

  it("a genuinely empty allowlist stays empty, not api.anthropic.com", () => {
    const spec: RunPolicySpec = { allowed_domains: [], first_use_approval: "always_deny", min_confinement_class: "CC2" };
    const state = wizardStateFromProposal(run, spec);
    expect(state.allowedDomains).toEqual([]);
    expect(state.allowedDomains).not.toContain("api.anthropic.com");
  });

  it("re-building emits the SAME deny-all shape, not a rewritten allow-list", () => {
    const spec: RunPolicySpec = { allowed_domains: [], first_use_approval: "always_deny", min_confinement_class: "CC2" };
    const { inline_policy } = buildSpec(wizardStateFromProposal(run, spec));
    expect(inline_policy.allowed_domains).toEqual([]);
  });

  it("a non-empty allowlist still round-trips normally (no regression)", () => {
    const spec: RunPolicySpec = {
      allowed_domains: ["github.com"],
      first_use_approval: "deny_with_review",
      min_confinement_class: "CC2",
    };
    expect(wizardStateFromProposal(run, spec).allowedDomains).toEqual(["github.com"]);
  });

  // REGRESSION (a prior fix for this same finding wrote dedupe(spec.allowed_
  // domains) with no null guard, which THROWS on null instead of crashing
  // gracefully): a genuine deny-all recorded profile serves allowed_domains:
  // null on the wire — recordmode.Synthesize leaves the allowlist a NIL slice
  // (`var allowed []string`, never appended to) for a deny-all outcome, and
  // types.go's AllowedDomains has no `omitempty`, so a nil slice serializes
  // to JSON `null`, not `[]`. compose-quick-review.tsx:114's own `?? []`
  // confirms this null shape occurs in practice for the identical field.
  // dedupe()'s `xs.map(...)` throws TypeError on null — this must not crash
  // the recorded-profile fast-track, which skips straight to Review with no
  // screen in between to catch it.
  it("a null allowed_domains (the real deny-all wire shape) does not throw, and is treated as empty", () => {
    const spec = {
      allowed_domains: null,
      first_use_approval: "always_deny",
      min_confinement_class: "CC2",
    } as unknown as RunPolicySpec;
    let state: ReturnType<typeof wizardStateFromProposal> | undefined;
    expect(() => {
      state = wizardStateFromProposal(run, spec);
    }).not.toThrow();
    expect(state?.allowedDomains).toEqual([]);
    expect(() => buildSpec(wizardStateFromProposal(run, spec))).not.toThrow();
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

// W15-W15e-wizard-roundtrip-6: "Edit in wizard" used to silently drop a
// composed devcontainer_repo — the wizard's own Launch (buildSpec) then built
// the plain convention image, a DIFFERENT sandbox than "Approve & launch"
// (which sends result.proposed.run, devcontainer_repo intact, unchanged)
// would have built for the identical proposal.
describe("wizardStateFromProposal + buildSpec — devcontainer_repo round-trips (W15e-6)", () => {
  const spec: RunPolicySpec = {
    allowed_domains: ["api.anthropic.com"],
    first_use_approval: "deny_with_review",
    min_confinement_class: "CC2",
  };

  it("carries devcontainer_repo from the proposal into WizardState", () => {
    const run = {
      agent: "claude-code",
      repo: "org/repo",
      interactive: true,
      devcontainer_repo: "org/devcontainer-repo",
    } as ComposeRunProposal;
    const state = wizardStateFromProposal(run, spec);
    expect(state.devcontainerRepo).toBe("org/devcontainer-repo");
  });

  it("re-emits it on buildSpec, so the wizard's own Launch builds the SAME sandbox", () => {
    const run = {
      agent: "claude-code",
      repo: "org/repo",
      interactive: true,
      devcontainer_repo: "org/devcontainer-repo",
    } as ComposeRunProposal;
    const { run: built } = buildSpec(wizardStateFromProposal(run, spec));
    expect(built.devcontainer_repo).toBe("org/devcontainer-repo");
    expect(built.image).toBeUndefined();
  });

  it("stays absent for a plain proposal with no devcontainer build", () => {
    const run = { agent: "claude-code", repo: "org/repo", interactive: true } as ComposeRunProposal;
    const state = wizardStateFromProposal(run, spec);
    expect(state.devcontainerRepo).toBe("");
    expect(buildSpec(state).run.devcontainer_repo).toBeUndefined();
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

  // W12-W12-B-4: the broker's mint is single-use per grant regardless of
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

// PARITY-2: a multi-source workspace has no single kind/source to flatten to
// — internal/store/store.go's deriveWorkspaceMirrors bails (leaves Kind/
// Source empty) whenever len(Sources) != 1. The old buildSpec read w.kind/
// w.source directly, so a multi-source or migrated-ephemeral (0029) workspace
// silently attached NOTHING — a 400 "mount source is empty" at launch with no
// operator fix available. Iterating w.sources closes it.
describe("buildSpec — multi-source workspaces (PARITY-2)", () => {
  const multiWs = {
    id: "ws-multi",
    name: "monorepo-plus-scratch",
    // The single-mirror fields a real multi-source record leaves EMPTY —
    // asserting the fix does NOT read these.
    kind: "" as unknown as Workspace["kind"],
    source: "",
    status: "scanned",
    created_at: "",
    updated_at: "",
    sources: [
      { type: "local_dir", path: "/home/me/api" },
      { type: "repo", source: "acme/widgets" },
      { type: "ephemeral", target: "/home/agent/scratch" },
    ],
    requirements: { "write:/home/me/api": { level: "required", provenance: "operator_set" } },
  } as Workspace;

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
    const ephemeralWs = {
      id: "ws-eph",
      name: "old-container",
      kind: "ephemeral" as unknown as Workspace["kind"],
      source: "",
      status: "scanned",
      created_at: "",
      updated_at: "",
      sources: [{ type: "ephemeral", target: "/home/agent/work" }],
    } as Workspace;
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

// PARITY-3: the server picks the PRIMARY workspace mounts-then-repos
// (referencedWorkspaces, workspace_run.go) — it walks the resolved spec's
// workspace_mounts in FULL before workspace_repos, so wsRefs[0] is whichever
// SELECTED workspace contributes the FIRST local_dir mount, never simply
// selections[0]. primaryWorkspaceId is the client-side twin.
describe("primaryWorkspaceId — mounts-then-repos, mirroring the server's own pick (PARITY-3)", () => {
  const repoWs = {
    id: "ws-repo",
    name: "api-service",
    kind: "repo",
    source: "acme/api-service",
    status: "scanned",
    created_at: "",
    updated_at: "",
  } as Workspace;
  const localWs = {
    id: "ws-local",
    name: "payments-local",
    kind: "local_dir",
    source: "/home/me/payments",
    status: "scanned",
    created_at: "",
    updated_at: "",
  } as Workspace;

  it("picks the FIRST local_dir-sourced selection even when a repo was attached first", () => {
    const selections = [{ workspaceId: "ws-repo" }, { workspaceId: "ws-local" }];
    expect(primaryWorkspaceId(selections, [repoWs, localWs])).toBe("ws-local");
  });

  it("matches raw selection order when the first selection IS local_dir (the common case)", () => {
    const selections = [{ workspaceId: "ws-local" }, { workspaceId: "ws-repo" }];
    expect(primaryWorkspaceId(selections, [repoWs, localWs])).toBe("ws-local");
  });

  it("falls back to the first repo-sourced selection when nothing local_dir is attached", () => {
    const repoWs2 = { ...repoWs, id: "ws-repo-2", source: "acme/other" };
    const selections = [{ workspaceId: "ws-repo" }, { workspaceId: "ws-repo-2" }];
    expect(primaryWorkspaceId(selections, [repoWs, repoWs2])).toBe("ws-repo");
  });

  // A multi-source workspace's OWN composition decides eligibility
  // (resolvableSources), not the flattened single-mirror kind (PARITY-2) — a
  // local_dir source anywhere in it still makes it primary-eligible.
  it("a multi-source workspace with a local_dir source anywhere in it still counts", () => {
    const mixedWs = {
      id: "ws-mixed",
      name: "mixed",
      kind: "" as unknown as Workspace["kind"],
      source: "",
      status: "scanned",
      created_at: "",
      updated_at: "",
      sources: [
        { type: "repo", source: "acme/widgets" },
        { type: "local_dir", path: "/home/me/widgets" },
      ],
    } as Workspace;
    const selections = [{ workspaceId: "ws-repo" }, { workspaceId: "ws-mixed" }];
    expect(primaryWorkspaceId(selections, [repoWs, mixedWs])).toBe("ws-mixed");
  });

  it("returns undefined when no selection resolves against the fetched list", () => {
    expect(primaryWorkspaceId([{ workspaceId: "ws-gone" }], [])).toBeUndefined();
  });

  it("returns undefined for an empty selection list", () => {
    expect(primaryWorkspaceId([], [repoWs, localWs])).toBeUndefined();
  });
});

// UI-RUN-3: applyProfileSpecToState's own docstring promises to KEEP the
// operator's Basics choices when a saved policy/recorded profile populates
// steps 2-4 — but it rebuilds from initialWizardState and only re-applied
// agent/mode/task/workspaces, silently dropping runType and image. Picking a
// profile after setting up a governed command + BYOI image converted it to
// an agent run on the default image.
describe("applyProfileSpecToState — keeps every Basics choice, incl. runType and image (UI-RUN-3)", () => {
  const spec = {
    allowed_domains: ["api.anthropic.com"],
    first_use_approval: "deny_with_review" as const,
    min_confinement_class: "CC2" as const,
  };

  it("carries runType forward — a governed command must not silently become an agent run", () => {
    const state = { ...initialWizardState(), runType: "command" as const, task: "npm test" };
    const applied = applyProfileSpecToState(state, spec, [], "profile-1");
    expect(applied.runType).toBe("command");
  });

  it("carries the BYOI image forward — it must not be discarded", () => {
    const state = { ...initialWizardState(), image: "ghcr.io/acme/dev@sha256:deadbeef" };
    const applied = applyProfileSpecToState(state, spec, [], "profile-1");
    expect(applied.image).toBe("ghcr.io/acme/dev@sha256:deadbeef");
  });

  it("buildSpec re-emits task_mode: exec and run.image after applying a profile", () => {
    const state = {
      ...initialWizardState(),
      runType: "command" as const,
      image: "ghcr.io/acme/dev@sha256:deadbeef",
      task: "npm test",
    };
    const applied = applyProfileSpecToState(state, spec, [], "profile-1");
    const { run } = buildSpec(applied);
    expect(run.task_mode).toBe("exec");
    expect(run.image).toBe("ghcr.io/acme/dev@sha256:deadbeef");
  });
});
