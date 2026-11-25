/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Go->TS wire parity for GET and PUT /governance/approval-counts: the Go json tags of the server's view structs
// (internal/api/governance_approval_counts.go) and of the SDK's (pkg/client/governance_review.go) against the TS
// interfaces in approval-counts.ts, and the approvers and pool status values against the TS unions. Same
// technique as governance-change-note.wire.test.ts (it reads both sources as text).
//
// Run: cd ui && pnpm vitest run src/app/lib/types/approval-counts.wire.test.ts

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

function goJSONTags(src: string, structName: string): string[] {
  const m = new RegExp(`type\\s+${structName}\\s+struct\\s*\\{([\\s\\S]*?)\\n\\}`).exec(src);
  if (!m) throw new Error(`struct ${structName} not found`);
  const tags: string[] = [];
  for (const t of m[1].matchAll(/json:"([^",]+)(?:,[^"]*)?"/g)) {
    if (t[1] !== "-") tags.push(t[1]);
  }
  return tags.sort();
}

function stripComments(src: string): string {
  return src.replace(/\/\*[\s\S]*?\*\//g, "").replace(/\/\/.*$/gm, "");
}

// The property names of one exported TS interface, with those of the interface it extends.
function tsKeys(src: string, name: string): string[] {
  const clean = stripComments(src);
  const m = new RegExp(`export\\s+interface\\s+${name}\\b(?:\\s+extends\\s+([A-Za-z0-9_]+))?\\s*\\{([\\s\\S]*?)\\n\\}`).exec(clean);
  if (!m) throw new Error(`interface ${name} not found`);
  const keys = m[1] ? tsKeys(src, m[1]) : [];
  for (const line of m[2].split(/[;\n]/)) {
    const k = /^\s*(?:readonly\s+)?([A-Za-z_][A-Za-z0-9_]*)\??\s*:/.exec(line);
    if (k) keys.push(k[1]);
  }
  return keys.sort();
}

// The string values of one TS union type alias.
function tsUnion(src: string, name: string): string[] {
  const m = new RegExp(`export\\s+type\\s+${name}\\s*=([^;]*);`).exec(stripComments(src));
  if (!m) throw new Error(`type ${name} not found`);
  return [...m[1].matchAll(/"([^"]+)"/g)].map((v) => v[1]).sort();
}

// The values of Go string constants whose names start with prefix.
function goConsts(src: string, prefix: string): string[] {
  return [...src.matchAll(new RegExp(`\\b${prefix}[A-Za-z]*\\s*=\\s*"([^"]+)"`, "g"))].map((v) => v[1]).sort();
}

describe("Approval counts wire (GET and PUT /governance/approval-counts)", () => {
  const root = repoRoot();
  const goServer = readFileSync(join(root, "internal/api/governance_approval_counts.go"), "utf8");
  const goClient = readFileSync(join(root, "pkg/client/governance_review.go"), "utf8");
  const ts = readFileSync(join(root, "ui/src/app/lib/types/approval-counts.ts"), "utf8");
  const pairs: [server: string, client: string, ts: string][] = [
    ["approvalCountsView", "ApprovalCounts", "ApprovalCounts"],
    ["approvalCountsRule", "ApprovalCountsRule", "ApprovalCountsRule"],
    ["approvalCountsKind", "ApprovalCountsKind", "ApprovalCountsKind"],
    ["approvalCountsPool", "ApprovalCountsPool", "ApprovalCountsPool"],
    ["approvalCountsWrite", "ApprovalCountsWrite", "ApprovalCountsWrite"],
    ["approvalCountsWriteKind", "ApprovalCountsWriteKind", "ApprovalCountsWriteKind"],
  ];

  it.each(pairs)("the server's %s, the SDK's %s and the TypeScript %s carry the same keys", (server, client, tsName) => {
    const keys = goJSONTags(goServer, server);
    expect(keys.length).toBeGreaterThanOrEqual(2); // stale-regex guard
    expect(goJSONTags(goClient, client)).toEqual(keys);
    expect(tsKeys(ts, tsName)).toEqual(keys);
  });

  it("the approvers and pool status values match the TypeScript unions", () => {
    expect(goConsts(goServer, "approvers")).toEqual(tsUnion(ts, "ApprovalCountsApprovers"));
    expect(goConsts(goServer, "poolStatus")).toEqual(tsUnion(ts, "ApprovalCountsPoolStatus"));
  });
});
