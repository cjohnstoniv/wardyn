/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { refusalSentence, WORKLOAD_REFUSAL_SENTENCE } from "./workload-refusals";

function repoRoot(): string {
  let dir = resolve(process.cwd());
  for (let i = 0; i < 8; i++) {
    if (existsSync(join(dir, "go.mod"))) return dir;
    dir = dirname(dir);
  }
  throw new Error("go.mod not found");
}

const GO_REASON_FILES = [
  "internal/api/reasons.go",
  "internal/api/reasons_routes.go",
  "internal/runnerpool/reasons.go",
  "internal/placement/reasons.go",
];

describe("workload refusal sentences", () => {
  it("every told reason is a Go constant", () => {
    const go = GO_REASON_FILES.map((f) => readFileSync(join(repoRoot(), f), "utf8")).join("\n");
    for (const reason of Object.keys(WORKLOAD_REFUSAL_SENTENCE)) expect(go, reason).toMatch(new RegExp(`= "${reason}"`));
  });

  it("every workload reason of reasons.go that a workload can carry is told", () => {
    const go = readFileSync(join(repoRoot(), "internal/api/reasons.go"), "utf8");
    const carried = [...go.matchAll(/reasonWorkload(Invalid|SourceConflict|CapReached|PoolMismatch|BindingInactive)\s*=\s*"([a-z_]+)"/g)].map((m) => m[2]);
    expect(carried).toHaveLength(5);
    for (const reason of carried) expect(WORKLOAD_REFUSAL_SENTENCE[reason], reason).toBeTruthy();
  });

  it("every runner pool reason is told", () => {
    const go = readFileSync(join(repoRoot(), "internal/runnerpool/reasons.go"), "utf8");
    const block = /ReasonInvalid[\s\S]*?ReasonFull\s+Reason\s*=\s*"[a-z_]+"/.exec(go)?.[0] ?? "";
    const pool = [...block.matchAll(/Reason\w*\s+Reason\s*=\s*"([a-z_]+)"/g)].map((m) => m[1]);
    expect(pool.length).toBeGreaterThanOrEqual(14);
    for (const reason of pool) expect(WORKLOAD_REFUSAL_SENTENCE[reason], reason).toBeTruthy();
  });

  it("says the table's sentence, else the server's own, else a plain fallback", () => {
    expect(refusalSentence("runner_pool_full", "x")).toBe("The pool is running its limit of runs.");
    expect(refusalSentence("some_other_reason", " The ceiling refused it. ")).toBe("The ceiling refused it.");
    expect(refusalSentence("some_other_reason")).toBe("The launch was refused.");
  });

  it("follows a credential refusal's table sentence with the server's, for the reader the server gives it to", () => {
    const told = WORKLOAD_REFUSAL_SENTENCE.placement_credential;
    const server = "ProxyConfig.Policy.EligibleGrants[0]: a github_token grant is not delivered to this runner. The grant may come from the default policy.";
    expect(told).toBe("A credential in its policy, often a grant from the default policy, cannot be sent to the runner it would land on.");
    // Full tier: the server sentence names the grant and the fix.
    expect(refusalSentence("placement_credential", ` ${server} `)).toBe(`${told} ${server}`);
    // Security tier: the server withholds the sentence, so the table's stands alone.
    expect(refusalSentence("placement_credential", "")).toBe(told);
    expect(refusalSentence("placement_credential")).toBe(told);
  });
});
