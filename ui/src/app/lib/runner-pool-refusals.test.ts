/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { describe, expect, it } from "vitest";
import golden from "./runner-pool-refusals.golden.json";
import {
  HOSTING_LABEL,
  RUNNER_POOL_REASONS,
  RUNNER_POOL_REFUSAL,
  RUNNER_POOL_REFUSAL_BY_KEY,
  RUNNER_POOL_REFUSAL_REASON,
} from "./runner-pool-refusals";

function repoRoot(): string {
  let dir = resolve(process.cwd());
  for (let i = 0; i < 8; i++) {
    if (existsSync(join(dir, "go.mod"))) return dir;
    dir = dirname(dir);
  }
  throw new Error("go.mod not found");
}

const root = repoRoot();

describe("runner pool refusal sentences", () => {
  it("are the golden bytes the server's twin is pinned to", () => {
    expect(golden.map((g) => g.key).sort()).toEqual(Object.keys(RUNNER_POOL_REFUSAL).sort());
    for (const g of golden) {
      expect(RUNNER_POOL_REFUSAL_BY_KEY[g.key as keyof typeof RUNNER_POOL_REFUSAL](...g.args), g.key).toBe(g.text);
    }
  });

  it("each belongs to a reason the server sends", () => {
    expect(Object.keys(RUNNER_POOL_REFUSAL_REASON).sort()).toEqual(Object.keys(RUNNER_POOL_REFUSAL).sort());
    for (const reason of Object.values(RUNNER_POOL_REFUSAL_REASON)) expect(RUNNER_POOL_REASONS).toContain(reason);
    // Every reason has a sentence, so none can reach a person as a bare code.
    expect(new Set(Object.values(RUNNER_POOL_REFUSAL_REASON))).toEqual(new Set(RUNNER_POOL_REASONS));
  });

  it("names the hosting types as the server does", () => {
    const go = readFileSync(join(root, "internal/runnerpool/reasons.go"), "utf8");
    for (const [wire, label] of Object.entries(HOSTING_LABEL)) {
      expect(go, wire).toContain(`return "${label}"`);
    }
    expect(RUNNER_POOL_REFUSAL.REQUIRED(""), "no hosting type known").toBe(RUNNER_POOL_REFUSAL.REQUIRED_ANY());
  });
});

describe("reasons", () => {
  it("are the Go closed set of internal/runnerpool/reasons.go", () => {
    const go = readFileSync(join(root, "internal/runnerpool/reasons.go"), "utf8");
    const goReasons = [...go.matchAll(/\bReason\w+\s+Reason = "([a-z_]+)"/g)].map((m) => m[1]);
    expect(goReasons).toHaveLength(9);
    expect(new Set(RUNNER_POOL_REASONS)).toEqual(new Set(goReasons));
  });
});
