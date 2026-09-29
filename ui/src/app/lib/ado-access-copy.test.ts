/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { ADO_CAPABILITIES } from "./ado-capabilities";
import { ADO_CAP_COPY } from "./workspace-providers-copy";

function repoRoot(): string {
  let dir = resolve(process.cwd());
  for (let i = 0; i < 8; i++) {
    if (existsSync(join(dir, "go.mod"))) return dir;
    dir = dirname(dir);
  }
  throw new Error("go.mod not found walking up from " + process.cwd());
}

// The dispatch refusal names a capability in Go (adoscope.ShortLabel); the
// console names it in workspace-providers-copy.ts. One name per capability, whichever side speaks.
describe("ADO_CAP_COPY names — the same names Go's launch refusal uses", () => {
  it("names every grantable capability exactly as adoscope's shortLabels does", () => {
    const src = readFileSync(join(repoRoot(), "internal/adoscope/capability.go"), "utf8");
    const consts = new Map<string, string>();
    for (const m of src.matchAll(/^\s*(Cap\w+)\s+Capability = "([^"]+)"/gm)) consts.set(m[1], m[2]);
    const block = /var shortLabels = map\[Capability\]string\{([\s\S]*?)\n\}/.exec(src);
    expect(block).not.toBeNull();
    const goNames: Record<string, string> = {};
    for (const m of block![1].matchAll(/(Cap\w+):\s*"([^"]+)"/g)) goNames[consts.get(m[1])!] = m[2];
    expect(Object.fromEntries(Object.entries(ADO_CAP_COPY).map(([cap, c]) => [cap, c.name]))).toEqual(goNames);
    expect(Object.keys(ADO_CAP_COPY).sort()).toEqual(ADO_CAPABILITIES.map((c) => c.cap).sort());
  });
});

// Owner ruling 2026-09-28: policy_bypass is ONLY a pull request completed with
// bypassPolicy, and the run's own-branch rule is Wardyn's, never a branch
// policy Wardyn read. The copy must not say otherwise, in the console or in the
// Go labels a refusal quotes.
describe("ADO capability copy — no branch policy Wardyn never read", () => {
  it("pins the owner-approved names and consequence lines", () => {
    expect(ADO_CAP_COPY.policy_bypass).toEqual({
      name: "Bypass branch policies",
      consequence: "Complete a pull request without its required reviewers or checks.",
    });
    expect(ADO_CAP_COPY.code_write.name).toBe("Push to the run's own branch");
    expect(ADO_CAP_COPY.code_write.consequence).toContain("outside this run's own branch");
  });

  it("never says a branch is protected, or that no policy protects it", () => {
    const labels = /var labels = map\[Capability\]string\{([\s\S]*?)\n\}/.exec(
      readFileSync(join(repoRoot(), "internal/adoscope/capability.go"), "utf8"),
    );
    expect(labels).not.toBeNull();
    const all = [...Object.values(ADO_CAP_COPY).map((c) => c.consequence), labels![1]].join("\n");
    expect(all).not.toMatch(/no policy protects|protected branch|policy-protected|past a branch policy/i);
  });
});
