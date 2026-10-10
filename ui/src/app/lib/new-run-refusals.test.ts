/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Go/TS byte parity for the New Run refusals. Run:
//   cd ui && pnpm vitest run src/app/lib/new-run-refusals.test.ts

import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { describe, expect, it } from "vitest";
import golden from "./workspace-refusals.golden.json";
import {
  NEW_RUN_REASON,
  PLACEMENT_REASONS,
  WORKSPACE_REFUSAL,
  WORKSPACE_REFUSAL_BY_KEY,
  WORKSPACE_REFUSAL_REASON,
  workspaceTargetOverlap,
  workspaceTargetShapeOk,
} from "./new-run-refusals";

function repoRoot(): string {
  let dir = resolve(process.cwd());
  for (let i = 0; i < 8; i++) {
    if (existsSync(join(dir, "go.mod"))) return dir;
    dir = dirname(dir);
  }
  throw new Error("go.mod not found");
}

const root = repoRoot();

describe("workspace refusal sentences", () => {
  it("are the golden bytes the server's twin is pinned to", () => {
    expect(golden.map((g) => g.key).sort()).toEqual(Object.keys(WORKSPACE_REFUSAL).sort());
    for (const g of golden) {
      expect(WORKSPACE_REFUSAL_BY_KEY[g.key as keyof typeof WORKSPACE_REFUSAL](...g.args), g.key).toBe(g.text);
    }
  });

  it("each belongs to a reason the server sends", () => {
    for (const reason of Object.values(WORKSPACE_REFUSAL_REASON)) {
      expect(Object.values(NEW_RUN_REASON)).toContain(reason);
    }
  });
});

describe("reasons", () => {
  it("the contract's reasons are the Go constants of internal/api/reasons.go", () => {
    const go = readFileSync(join(root, "internal/api/reasons.go"), "utf8");
    const block = /The 0\.9 New Run request contract[\s\S]*?\n\t\/\/ validateImageBuildRequest/.exec(go)?.[0] ?? "";
    const goReasons = [...block.matchAll(/=\s*"([a-z_]+)"/g)].map((m) => m[1]);
    expect(goReasons.length).toBeGreaterThanOrEqual(10);
    expect(new Set(Object.values(NEW_RUN_REASON))).toEqual(new Set(goReasons));
  });

  it("the placement reasons are the Go closed set", () => {
    const go = readFileSync(join(root, "internal/placement/reasons.go"), "utf8");
    const goReasons = [...go.matchAll(/\bReason\w+\s+Reason = "([a-z_]+)"/g)].map((m) => m[1]);
    expect(goReasons).toHaveLength(26);
    expect(new Set(PLACEMENT_REASONS)).toEqual(new Set(goReasons));
  });
});

describe("workspace target rules", () => {
  it("accepts and refuses the shapes the server does", () => {
    for (const ok of ["/home/agent/work", "/home/agent/a/b"]) expect(workspaceTargetShapeOk(ok), ok).toBe(true);
    for (const bad of ["work", "/home/agent", "/home/agent/", "/home/agent/a/", "/home/agent/../etc", "/home/agent/./a", "/home/agent//a", "/etc/x", "/home/agentx/a"]) {
      expect(workspaceTargetShapeOk(bad), bad).toBe(false);
    }
  });

  it("finds equal and nested targets across workspaces, never within one", () => {
    const a = { workspace: "payments", target: "/home/agent/work" };
    expect(workspaceTargetOverlap({ workspace: "docs", target: "/home/agent/work" }, [a])).toBe(WORKSPACE_REFUSAL.OVERLAP_EQUAL("/home/agent/work", "payments"));
    expect(workspaceTargetOverlap({ workspace: "docs", target: "/home/agent/work/api" }, [a])).toBe(
      WORKSPACE_REFUSAL.OVERLAP_NESTED("/home/agent/work/api", "payments", "/home/agent/work"),
    );
    expect(workspaceTargetOverlap({ workspace: "docs", target: "/home/agent" + "/w" }, [{ workspace: "payments", target: "/home/agent/w/x" }])).toBe(
      WORKSPACE_REFUSAL.OVERLAP_NESTED("/home/agent/w/x", "docs", "/home/agent/w"),
    );
    expect(workspaceTargetOverlap({ workspace: "docs", target: "/home/agent/worker" }, [a])).toBeNull();
    expect(workspaceTargetOverlap({ workspace: "payments", target: "/home/agent/work/api" }, [a])).toBeNull();
  });
});
