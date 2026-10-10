/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { describe, expect, it } from "vitest";
import golden from "./runner-pool-refusals.golden.json";
import {
  BARRIER_LABEL,
  HOSTING_LABEL,
  RUN_END_REASON_SENTENCE,
  RUN_TYPE_PLURAL,
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

describe("pool limit wording", () => {
  const goReasons = readFileSync(join(root, "internal/runnerpool/reasons.go"), "utf8");
  const goLimits = readFileSync(join(root, "internal/types/runner_pool_limits.go"), "utf8");

  it("names barriers and run types as the server does", () => {
    for (const [wire, label] of Object.entries(BARRIER_LABEL)) expect(goLimits, wire).toContain(`return "${label}"`);
    for (const [wire, plural] of Object.entries(RUN_TYPE_PLURAL)) expect(goLimits, wire).toContain(`return "${plural}"`);
  });

  it("says the same end-of-life sentence as the run's stored reason", () => {
    for (const [reason, sentence] of Object.entries(RUN_END_REASON_SENTENCE)) {
      expect(goLimits, reason).toContain(`return "${sentence}"`);
    }
    expect(goLimits).toContain('RunEndMaxLifetimeReached RunEndReason = "max_lifetime_reached"');
  });

  it("lists one, two and three allowed barriers the way the server does", () => {
    expect(RUNNER_POOL_REFUSAL.BARRIER_NOT_ALLOWED("P", "CC3", ["CC1"])).toBe("P doesn't allow the Vault barrier. It allows Fence.");
    expect(RUNNER_POOL_REFUSAL.BARRIER_NOT_ALLOWED("P", "CC1", ["CC2", "CC3"])).toBe("P doesn't allow the Fence barrier. It allows Wall and Vault.");
    expect(RUNNER_POOL_REFUSAL.BARRIER_NOT_ALLOWED("P", "CC1", ["CC2", "CC3", "CC1"])).toBe(
      "P doesn't allow the Fence barrier. It allows Wall, Vault and Fence.",
    );
    expect(goReasons).toContain('" and "');
  });
});

describe("reasons", () => {
  it("are the Go closed set of internal/runnerpool/reasons.go", () => {
    const go = readFileSync(join(root, "internal/runnerpool/reasons.go"), "utf8");
    const goReasons = [...go.matchAll(/\bReason\w+\s+Reason = "([a-z_]+)"/g)].map((m) => m[1]);
    expect(goReasons).toHaveLength(12);
    expect(new Set(RUNNER_POOL_REASONS)).toEqual(new Set(goReasons));
  });
});
