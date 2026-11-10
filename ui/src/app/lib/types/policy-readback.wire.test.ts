// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Source parity for AgentRun.policy_readback: Go's types.RunPolicyReadback and its states
// (internal/types/policy_readback.go) against the TS mirror in ./policy-readback.ts.
import { describe, it, expect } from "vitest";
import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";

function repoRoot(): string {
  let dir = resolve(process.cwd());
  for (let i = 0; i < 8; i++) {
    if (existsSync(join(dir, "go.mod"))) return dir;
    dir = dirname(dir);
  }
  throw new Error("go.mod not found walking up from " + process.cwd());
}

describe("RunPolicyReadback wire parity", () => {
  const root = repoRoot();
  const go = readFileSync(join(root, "internal/types/policy_readback.go"), "utf8");
  const ts = readFileSync(join(root, "ui/src/app/lib/types/policy-readback.ts"), "utf8");

  it("the TS interface has exactly Go's json tags", () => {
    const body = /type\s+RunPolicyReadback\s+struct\s*\{([\s\S]*?)\n\}/.exec(go)?.[1] ?? "";
    const goTags = [...body.matchAll(/json:"([^",]+)(?:,[^"]*)?"/g)].map((m) => m[1]);
    expect(goTags).toContain("confirmed_at");
    const tsBody = /export interface RunPolicyReadback \{([\s\S]*?)\n\}/.exec(ts)?.[1] ?? "";
    const tsKeys = [...tsBody.matchAll(/^\s*([a-z_]+)\??:/gm)].map((m) => m[1]);
    expect(new Set(tsKeys)).toEqual(new Set(goTags));
  });

  it("the TS states are exactly Go's", () => {
    const goStates = [...go.matchAll(/^\s*ProxyStart\w+\s+ProxyStartState\s*=\s*"([a-z]+)"/gm)].map((m) => m[1]);
    const tsStates = [...(/export type ProxyStartState = ([^;]+);/.exec(ts)?.[1] ?? "").matchAll(/"([a-z]+)"/g)].map((m) => m[1]);
    expect(goStates.length).toBe(6);
    expect(new Set(tsStates)).toEqual(new Set(goStates));
  });
});
