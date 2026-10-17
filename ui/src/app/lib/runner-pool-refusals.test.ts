/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { PLACEMENT_REASONS } from "./new-run-refusals";
import golden from "./runner-pool-refusals.golden.json";
import {
  BARRIER_LABEL,
  POOL_OFFLINE,
  RUN_END_REASON_SENTENCE,
  RUN_WAIT,
  RUN_WAIT_REASON,
  RUN_TYPE_PLURAL,
  RUNNER_CAPACITY,
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
    const waits = ["WAIT_AT_CAPACITY", "WAIT_NO_RUNNER_FREE", "WAIT_RUNNERS_AT_CAPACITY", "NO_ONLINE_RUNNER", "RUNNER_AT_CAPACITY", "RUNNER_LIMITS_UNSET"];
    expect(golden.map((g) => g.key).sort()).toEqual([...Object.keys(RUNNER_POOL_REFUSAL), ...waits].sort());
    for (const g of golden.filter((x) => !waits.includes(x.key))) {
      expect(RUNNER_POOL_REFUSAL_BY_KEY[g.key as keyof typeof RUNNER_POOL_REFUSAL](...g.args), g.key).toBe(g.text);
    }
  });

  it("each belongs to a reason the server sends", () => {
    expect(Object.keys(RUNNER_POOL_REFUSAL_REASON).sort()).toEqual(Object.keys(RUNNER_POOL_REFUSAL).sort());
    for (const reason of Object.values(RUNNER_POOL_REFUSAL_REASON)) expect(RUNNER_POOL_REASONS).toContain(reason);
    // Every reason has a sentence, so none can reach a person as a bare code.
    expect(new Set(Object.values(RUNNER_POOL_REFUSAL_REASON))).toEqual(new Set(RUNNER_POOL_REASONS));
  });

  it("says a full pool as a wait, not a refusal", () => {
    const g = golden.find((x) => x.key === "WAIT_AT_CAPACITY")!;
    expect(RUN_WAIT.AT_CAPACITY(g.args[0], Number(g.args[1]))).toBe(g.text);
    expect(RUNNER_POOL_REASONS as readonly string[]).not.toContain(RUN_WAIT_REASON.AT_CAPACITY);
    expect(readFileSync(join(root, "internal/runnerpool/wait.go"), "utf8")).toContain(`WaitReason = "${RUN_WAIT_REASON.AT_CAPACITY}"`);
  });

  it("says a run no runner can take yet as a wait too, never as a refusal", () => {
    const g = golden.find((x) => x.key === "WAIT_NO_RUNNER_FREE")!;
    expect(RUN_WAIT.NO_RUNNER_FREE(g.args[0])).toBe(g.text);
    expect(RUNNER_POOL_REASONS as readonly string[]).not.toContain(RUN_WAIT_REASON.NO_RUNNER_FREE);
    expect(readFileSync(join(root, "internal/runnerpool/wait.go"), "utf8")).toContain(`WaitReason = "${RUN_WAIT_REASON.NO_RUNNER_FREE}"`);
  });

  it("says a run whose runners are all at their runs at once as a wait when queued, a refusal when not", () => {
    const w = golden.find((x) => x.key === "WAIT_RUNNERS_AT_CAPACITY")!;
    expect(RUN_WAIT.RUNNERS_AT_CAPACITY(w.args[0])).toBe(w.text);
    expect(RUNNER_POOL_REASONS as readonly string[]).not.toContain(RUN_WAIT_REASON.RUNNERS_AT_CAPACITY);
    expect(readFileSync(join(root, "internal/runnerpool/wait.go"), "utf8")).toContain(`WaitReason = "${RUN_WAIT_REASON.RUNNERS_AT_CAPACITY}"`);
    const g = golden.find((x) => x.key === "RUNNER_AT_CAPACITY")!;
    expect(RUNNER_CAPACITY.RUNNER_AT_CAPACITY(g.args[0])).toBe(g.text);
    expect(PLACEMENT_REASONS).toContain("runner_at_capacity");
    const u = golden.find((x) => x.key === "RUNNER_LIMITS_UNSET")!;
    expect(RUNNER_CAPACITY.RUNNER_LIMITS_UNSET(u.args[0])).toBe(u.text);
    expect(PLACEMENT_REASONS).toContain("runner_limits_unset");
  });

  it("says a pool with no online runner of yours in the server's sentence", () => {
    const g = golden.find((x) => x.key === "NO_ONLINE_RUNNER")!;
    expect(POOL_OFFLINE.NO_ONLINE_RUNNER(g.args[0])).toBe(g.text);
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
      expect(goLimits, reason).toContain(`RunEndReason = "${reason}"`);
      expect(goLimits, reason).toMatch(new RegExp(`:\\s+"${sentence.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}",`));
    }
  });

  it("has a sentence for every end reason the server records, and no other", () => {
    const goReasons = [...goLimits.matchAll(/\bRunEnd\w+\s+RunEndReason = "([a-z_]+)"/g)].map((m) => m[1]);
    expect(goReasons.length).toBeGreaterThanOrEqual(8);
    expect(Object.keys(RUN_END_REASON_SENTENCE).sort()).toEqual([...goReasons].sort());
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
    expect(goReasons).toHaveLength(14);
    expect(new Set(RUNNER_POOL_REASONS)).toEqual(new Set(goReasons));
  });
});
