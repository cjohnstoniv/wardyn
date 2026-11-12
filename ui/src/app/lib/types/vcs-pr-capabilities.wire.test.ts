/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The pull-request capabilities: Azure DevOps pr_create, pr_vote and pr_complete, and GitHub pr_write and
// pr_review, are Go constants in the grantable set, and the console lists each. The Go side is read from source.

import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { describe, expect, it } from "vitest";

import { GITHUB_CAPABILITY_GROUPS } from "../../components/wardyn/copy/personal-walls";
import { ADO_CAPABILITIES } from "../ado-capabilities";

function repoRoot(): string {
  let dir = resolve(process.cwd());
  for (let i = 0; i < 8; i++) {
    if (existsSync(join(dir, "go.mod"))) return dir;
    dir = dirname(dir);
  }
  throw new Error("go.mod not found");
}

const root = repoRoot();
const read = (p: string) => readFileSync(join(root, p), "utf8");

// goConstants maps each Capability constant to its wire name.
function goConstants(src: string): Map<string, string> {
  const out = new Map<string, string>();
  for (const m of src.matchAll(/^\s*(Cap\w+)\s+Capability\s*=\s*"([^"]+)"/gm)) out.set(m[1], m[2]);
  return out;
}

// goGrantable is the wire name of every constant in the file's grantableCapabilities map.
function goGrantable(src: string): Set<string> {
  const consts = goConstants(src);
  const m = /var grantableCapabilities = map\[Capability\]bool\{([\s\S]*?)\n\}/.exec(src);
  if (!m) throw new Error("grantableCapabilities not found");
  const out = new Set<string>();
  for (const k of m[1].matchAll(/(Cap\w+):\s*true/g)) {
    const wire = consts.get(k[1]);
    if (wire) out.add(wire);
  }
  return out;
}

describe("pull-request capabilities wire", () => {
  it("Azure DevOps pr_create, pr_vote and pr_complete are grantable in Go and listed in the console", () => {
    const go = read("internal/adoscope/capability.go");
    const consts = new Set(goConstants(go).values());
    const grantable = goGrantable(go);
    const listed = new Set(ADO_CAPABILITIES.map((c) => c.cap));
    for (const cap of ["pr", "pr_create", "pr_vote", "pr_complete"]) {
      expect(consts.has(cap), cap).toBe(true);
      expect(grantable.has(cap), cap).toBe(true);
      expect(listed.has(cap), cap).toBe(true);
    }
  });

  it("GitHub pr_write and pr_review are grantable in Go and listed in the console", () => {
    const go = read("internal/ghscope/capability.go");
    const consts = new Set(goConstants(go).values());
    const grantable = goGrantable(go);
    const listed = new Set(GITHUB_CAPABILITY_GROUPS.flatMap((g) => g.caps.map((c) => c.cap)));
    for (const cap of ["pr", "pr_write", "pr_review"]) {
      expect(consts.has(cap), cap).toBe(true);
      expect(grantable.has(cap), cap).toBe(true);
      expect(listed.has(cap), cap).toBe(true);
    }
  });
});
