/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { ADO_CAPABILITIES, ADO_CAPABILITY_GROUPS } from "./ado-capabilities";

function repoRoot(): string {
  let dir = resolve(process.cwd());
  for (let i = 0; i < 8; i++) {
    if (existsSync(join(dir, "go.mod"))) return dir;
    dir = dirname(dir);
  }
  throw new Error("go.mod not found walking up from " + process.cwd());
}

describe("ADO_CAPABILITIES — the console's copy of adoscope's grantable set", () => {
  it("is exactly grantableCapabilities in internal/adoscope/capability.go", () => {
    const src = readFileSync(join(repoRoot(), "internal/adoscope/capability.go"), "utf8");
    const block = /var grantableCapabilities = map\[Capability\]bool\{([\s\S]*?)\n\}/.exec(src);
    expect(block).not.toBeNull();
    const consts = new Map<string, string>();
    for (const m of src.matchAll(/^\s*(Cap\w+)\s+Capability = "([^"]+)"/gm)) consts.set(m[1], m[2]);
    const grantable = [...block![1].matchAll(/(Cap\w+):\s*true/g)].map((m) => consts.get(m[1]));
    expect(ADO_CAPABILITIES.map((c) => c.cap).sort()).toEqual([...grantable].sort());
  });

  it("lists capabilities in the approved mock's order, group by group", () => {
    expect(ADO_CAPABILITIES.map((c) => c.cap)).toEqual([
      "read",
      "code_write",
      "pr",
      "work_write",
      "wiki_write",
      "build_execute",
      "packaging_write",
      "policy_admin",
      "policy_bypass",
      "repo_admin",
      "security_admin",
      "serviceendpoint_admin",
      "build_admin",
      "project_admin",
    ]);
    const groupOrder = ADO_CAPABILITY_GROUPS.map((g) => g.id);
    const seen = ADO_CAPABILITIES.map((c) => groupOrder.indexOf(c.group));
    expect(seen).toEqual([...seen].sort((a, b) => a - b));
  });

  it("puts every capability in a declared group, and every high-risk one in the high-risk group", () => {
    const groups = new Set(ADO_CAPABILITY_GROUPS.map((g) => g.id));
    for (const c of ADO_CAPABILITIES) {
      expect(groups.has(c.group)).toBe(true);
      expect(c.highRisk).toBe(c.group === "high_risk");
    }
  });
});
