/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { ADO_CAPABILITIES, ADO_CAPABILITY_GROUPS, ADO_DEFAULT_PROFILE } from "./ado-capabilities";

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

  it("lists capabilities in the approved per-area mock's order, area by area", () => {
    expect(ADO_CAPABILITIES.map((c) => c.cap)).toEqual([
      "code_read",
      "code_write",
      "pr",
      "policy_admin",
      "policy_bypass",
      "repo_admin",
      "work_read",
      "work_write",
      "work_admin",
      "wiki_read",
      "wiki_write",
      "build_read",
      "build_execute",
      "build_admin",
      "release_read",
      "release_execute",
      "release_admin",
      "serviceendpoint_read",
      "serviceendpoint_admin",
      "library_read",
      "packaging_read",
      "packaging_write",
      "packaging_manage",
      "test_read",
      "project_read",
      "identity_read",
      "project_admin",
      "security_admin",
    ]);
    expect(ADO_CAPABILITY_GROUPS.map((g) => g.id)).toEqual([
      "repos",
      "boards",
      "wiki",
      "pipelines",
      "artifacts",
      "test_plans",
      "organization",
    ]);
    const groupOrder = ADO_CAPABILITY_GROUPS.map((g) => g.id);
    const seen = ADO_CAPABILITIES.map((c) => groupOrder.indexOf(c.group));
    expect(seen).toEqual([...seen].sort((a, b) => a - b));
  });

  it("flags High risk and the eleven reads exactly as the approved mock does", () => {
    expect(ADO_CAPABILITIES.filter((c) => c.highRisk).map((c) => c.cap)).toEqual([
      "policy_admin",
      "policy_bypass",
      "repo_admin",
      "work_admin",
      "build_admin",
      "release_admin",
      "serviceendpoint_admin",
      "packaging_manage",
      "project_admin",
      "security_admin",
    ]);
    const reads = ADO_CAPABILITIES.filter((c) => c.read).map((c) => c.cap);
    expect(reads).toHaveLength(11);
    expect(reads.every((c) => c.endsWith("_read"))).toBe(true);
    for (const c of ADO_CAPABILITIES) expect(ADO_CAPABILITY_GROUPS.some((g) => g.id === c.group)).toBe(true);
  });

  it("empties to Go's ProfileDefault", () => {
    const src = readFileSync(join(repoRoot(), "internal/adoscope/capability.go"), "utf8");
    const body = /func ProfileDefault\(\) \[\]Capability \{ return \[\]Capability\{([^}]*)\} \}/.exec(src);
    expect(body).not.toBeNull();
    const consts = new Map<string, string>();
    for (const m of src.matchAll(/^\s*(Cap\w+)\s+Capability = "([^"]+)"/gm)) consts.set(m[1], m[2]);
    expect(body![1].split(",").map((c) => consts.get(c.trim()))).toEqual([...ADO_DEFAULT_PROFILE]);
  });
});
