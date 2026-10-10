/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { NEW_RUN_FIXTURES, newRunFixture } from "./new-run-fixtures";
import { NEW_RUN_REASON, PLACEMENT_REASONS, workspaceTargetOverlap, type PlacedTarget } from "./new-run-refusals";
import { OVERRIDE_NARROWING } from "./override-narrowing";
import { buildRunContractWire, overrideItems, pushRuleKey } from "./run-contract-draft";

// The routes the D-UI-a packet draws (M-NR-1, M-NR-2, M-NR-3).
const PACKET_ROUTES = [
  ..."p1-no-runner p1b-unclaimed p2-offline p3-ceiling p4-grants p4-several p5-choose p6-single p7-runners barrier-changed over-cap spec-resources provider-line".split(" ").map((r) => `run/${r}`),
  ..."one scratch two make-primary remove attach attach-none equal nested drive drive-toggled-last fixed-secondary fixed-both clone-collision pin image ado-org optional read-only-forced".split(" ").map((r) => `workspaces/${r}`),
  ...[..."123456789"].map((n) => `access/agent-r${n}`),
  ..."agent agent-locked-l2 agent-no-doc codex git-github git-github-app-missing git-ado-minted git-ado-own-pat git-other-forge git-push-rules custom-needs-input custom-refused source-policy empty add add-local add-empty add-builtin-swap manual-github manual-ado manual-ado-every-repo local-chips git-two-sections git-inactive git-inactive-push-moves git-ado-two-orgs agent-add-host agent-add-secret agent-host-clamped narrow".split(" ").map((r) => `access/${r}`),
  ..."image-agent image-workspace image-build image-org-allowed image-none-allowed image-conflict".split(" ").map((r) => `runner/${r}`),
  "info/title-description",
  ..."review-default review-saved review-custom only-changed only-changed-empty clamped removed narrowed changed-scalar multi-source redacted stale invalid editing templates".split(" ").map((r) => `policy/${r}`),
];

describe("New Run fixtures", () => {
  it("has one fixture for every route the packet draws, once", () => {
    const routes = NEW_RUN_FIXTURES.map((f) => f.route);
    expect(new Set(routes).size).toBe(routes.length);
    expect(PACKET_ROUTES.filter((r) => !routes.includes(r))).toEqual([]);
    expect(() => newRunFixture("run/not-drawn")).toThrow();
  });

  it("answers the way the server does: arrays, known reasons, sentences the server would send", () => {
    for (const f of NEW_RUN_FIXTURES) {
      expect(f.preview.resources ?? [], f.route).toBeInstanceOf(Array);
      expect(f.preview.allowed_images, f.route).toBeInstanceOf(Array);
      for (const row of f.preview.local_placement ?? []) {
        if (!row.local_placeable) expect(PLACEMENT_REASONS, f.route).toContain(row.reason);
      }
      if (f.refusal) {
        expect([...Object.values(NEW_RUN_REASON), ...PLACEMENT_REASONS], f.route).toContain(f.refusal.reason);
        expect(f.refusal.status, f.route).toBeGreaterThanOrEqual(400);
      }
    }
  });

  it("carries each image source and sends a changed image by its allowed ref", () => {
    for (const kind of ["agent", "workspace", "build", "org_allowed"] as const) {
      const f = newRunFixture(`runner/image-${kind.replace("_", "-")}`);
      expect(f.preview.image?.source.kind).toBe(kind);
      expect(f.preview.image?.ref).toBeTruthy();
      if (kind !== "agent") expect(f.preview.image?.source.name).toBeTruthy();
    }
    const f = newRunFixture("runner/image-org-allowed");
    expect(f.preview.allowed_images?.map((image) => image.ref)).toContain(f.preview.image?.ref);
    expect(buildRunContractWire(f.contract, { agent: false, azureDevOps: false, gitPATHosts: [], pushKeys: [] }))
      .toEqual({ allowed_image: f.preview.image?.ref });
    expect(newRunFixture("workspaces/image").tab).toBe("runner");
  });

  it("only drafts overrides the narrowing table lets a request carry", () => {
    for (const f of NEW_RUN_FIXTURES) {
      for (const item of overrideItems(f.contract?.access.overrides ? { ...(f.contract.access.overrides.agent ? { agent: f.contract.access.overrides.agent } : {}), push_rules: f.contract.access.overrides.pushRules } : {})) {
        const row = OVERRIDE_NARROWING.find((r) => r.kind === item.kind && r.op === item.op);
        expect(row?.rule, `${f.route}: ${item.kind} ${item.op}`).not.toBe("refused");
      }
    }
  });

  it("files push-rule overrides by provider and organisation, and sends only the active sections'", () => {
    for (const f of NEW_RUN_FIXTURES) {
      for (const p of f.contract?.access.overrides.pushRules ?? []) expect(p.org, f.route).not.toBe("");
      const keys = (f.contract?.access.overrides.pushRules ?? []).map(pushRuleKey);
      expect(new Set(keys).size, f.route).toBe(keys.length);
    }
    const sent = (route: string) => buildRunContractWire(newRunFixture(route).contract, newRunFixture(route).active ?? { agent: false, azureDevOps: false, gitPATHosts: [], pushKeys: [] });
    expect(sent("access/git-inactive").overrides).toBeUndefined();
    expect(sent("access/git-inactive-push-moves").overrides).toEqual({ push_rules: [{ provider: "github", org: "other", deny_paths: ["ci/"] }] });
    expect(sent("access/agent-add-host").overrides).toEqual({ agent: { add_hosts: ["api.example.com"] } });
    expect(sent("run/p7-runners")).toEqual({ placement: "local" });
  });

  it("the workspace frames show the overlap the shared rule finds, and only then", () => {
    for (const f of NEW_RUN_FIXTURES.filter((x) => x.route.startsWith("workspaces/"))) {
      const placed: PlacedTarget[] = f.drive ? [{ workspace: "your drive", target: "/home/agent/drive" }] : [];
      let found: string | null = null;
      for (const w of f.workspaces ?? []) {
        for (const target of [w.target, ...(w.otherSourceTargets ?? [])]) {
          if (!target) continue;
          found ??= workspaceTargetOverlap({ workspace: w.name, target }, placed);
          placed.push({ workspace: w.name, target });
        }
      }
      if (f.refusal?.reason === NEW_RUN_REASON.WORKSPACE_TARGET_OVERLAP) expect(found, f.route).toBe(f.refusal.text);
      else expect(found, f.route).toBeNull();
    }
  });
});
