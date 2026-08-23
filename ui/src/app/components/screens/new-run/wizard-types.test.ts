/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import {
  buildSpec,
  initialWizardState,
  isValidDomain,
  primaryWorkspaceId,
  resolvedMountReadOnly,
  gitPatConfigured,
  impliedEgressHosts,
  secretAutoGrants,
} from "./wizard-types";
import type { Workspace } from "../../../lib/types";

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

// A workspace is OPTIONAL: zero selections is a valid ephemeral scratch run, and
// buildSpec has to degrade to one rather than emitting a dangling mount. (This
// used to also cover validateStep's batch-needs-a-task rule; that gate now lives
// in new-run-screen.tsx's `problem` memo, where it can see the whole form — see
// wizard-types.ts's RETIRED note.)
describe("buildSpec is workspace-optional (ephemeral runs)", () => {
  it("degrades to an ephemeral scratch run with zero workspaces", () => {
    const { run, inline_policy } = buildSpec({ ...initialWizardState(), workspaces: [] });
    expect(run.repo).toBe("");
    expect(inline_policy.workspace_mounts).toBeUndefined();
    expect(inline_policy.workspace_repos).toBeUndefined();
  });
});

// D5/claim4: a git_pat switched on with the host OR the secret left blank must
// never widen egress to the typed host — no PAT is ever brokered to reach it, so
// the allowance would be a hole with nothing behind it.
//
// This used to be asserted through validateStep("access")'s error string. That
// was always the weaker test: it checked that the wizard SAID no, not that the
// launched spec was safe. Now that validateStep is gone (see wizard-types.ts's
// RETIRED note), assert the property itself — on gitPatConfigured, the ONE
// predicate buildSpec gates both the grant and the egress union on.
describe("half-configured git_pat neither grants nor widens (D5/claim4)", () => {
  const half = [
    { name: "a host but no stored secret", gitPatHost: "dev.azure.com", gitPatSecretName: "" },
    { name: "a secret but no host", gitPatHost: "", gitPatSecretName: "ado-pat" },
  ];
  for (const c of half) {
    it(`emits no git_pat grant and no implied host for ${c.name}`, () => {
      const state = {
        ...initialWizardState(),
        gitPatEnabled: true,
        gitPatHost: c.gitPatHost,
        gitPatSecretName: c.gitPatSecretName,
      };
      expect(gitPatConfigured(state)).toBe(false);
      const { inline_policy } = buildSpec(state);
      expect((inline_policy.eligible_grants ?? []).some((g) => g.kind === "git_pat")).toBe(false);
      expect(impliedEgressHosts(state).some((h) => h.why === "Git PAT")).toBe(false);
      expect(inline_policy.allowed_domains).not.toContain("dev.azure.com");
    });
  }

  it("is inert when disabled, regardless of host/secret", () => {
    const state = {
      ...initialWizardState(),
      gitPatEnabled: false,
      gitPatHost: "dev.azure.com",
      gitPatSecretName: "ado-pat",
    };
    expect(gitPatConfigured(state)).toBe(false);
    expect(buildSpec(state).inline_policy.allowed_domains).not.toContain("dev.azure.com");
  });

  it("grants AND allows the host once both are set", () => {
    const state = {
      ...initialWizardState(),
      gitPatEnabled: true,
      gitPatHost: "dev.azure.com",
      gitPatSecretName: "ado-pat",
    };
    expect(gitPatConfigured(state)).toBe(true);
    const { inline_policy } = buildSpec(state);
    expect((inline_policy.eligible_grants ?? []).some((g) => g.kind === "git_pat")).toBe(true);
    expect(inline_policy.allowed_domains).toContain("dev.azure.com");
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

// Regression: AddWorkspaceDialog's "Allow writes to this directory" stores
// sources[].writable=true, but nothing creates a `write:<path>` requirement
// row — so resolving write access from requirements ALONE made that checkbox a
// no-op for every run launched from the UI. The mount came out read-only, the
// agent's edits never reached the host, and the run still reported success.
// internal/api/workspace_run.go has always honored src.Writable; this is the
// client mirror agreeing with it.
describe("resolvedMountReadOnly — an explicitly writable source grants write", () => {
  const writableWs = {
    id: "ws-w",
    name: "slugify",
    kind: "local_dir",
    source: "/home/me/slugify",
    status: "scanned",
    created_at: "",
    updated_at: "",
    sources: [{ type: "local_dir", path: "/home/me/slugify", target: "/home/agent/work", writable: true }],
  } as unknown as Workspace;

  const readOnlyWs = {
    ...writableWs,
    sources: [{ type: "local_dir", path: "/home/me/slugify", target: "/home/agent/work" }],
  } as unknown as Workspace;

  it("mounts read-WRITE when the operator ticked the box", () => {
    expect(resolvedMountReadOnly(writableWs, { workspaceId: "ws-w" }, "/home/me/slugify")).toBe(false);
  });

  it("still defaults to read-only when they did not", () => {
    expect(resolvedMountReadOnly(readOnlyWs, { workspaceId: "ws-w" }, "/home/me/slugify")).toBe(true);
  });

  it("lets an explicit per-run readOnly NARROW a writable source back down", () => {
    expect(
      resolvedMountReadOnly(writableWs, { workspaceId: "ws-w", readOnly: true }, "/home/me/slugify"),
    ).toBe(true);
  });
});
