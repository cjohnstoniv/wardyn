/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Go->TS wire parity for the approvals block of GET /governance/changes/{id} and of the list rows: the Go json
// tags of the server's read model (internal/api/governance_change_approvals.go and governance_change_pool.go)
// and of the SDK's (pkg/client/governance_review.go) against the TS interfaces in governance-change.ts. Same
// technique as governance-change-note.wire.test.ts (it reads both sources as text).
//
// Run: cd ui && pnpm vitest run src/app/lib/types/governance-approvals.wire.test.ts

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

// The property names of one exported TS interface.
function tsKeys(src: string, name: string): string[] {
  const m = new RegExp(`export\\s+interface\\s+${name}\\b[^{]*\\{([\\s\\S]*?)\\n\\}`).exec(stripComments(src));
  if (!m) throw new Error(`interface ${name} not found`);
  const keys: string[] = [];
  for (const line of m[1].split(/[;\n]/)) {
    const k = /^\s*(?:readonly\s+)?([A-Za-z_][A-Za-z0-9_]*)\??\s*:/.exec(line);
    if (k) keys.push(k[1]);
  }
  return keys.sort();
}

describe("Governance change approvals wire (the approvals block)", () => {
  const root = repoRoot();
  const goServer =
    readFileSync(join(root, "internal/api/governance_change_approvals.go"), "utf8") +
    readFileSync(join(root, "internal/api/governance_change_pool.go"), "utf8");
  const goClient = readFileSync(join(root, "pkg/client/governance_review.go"), "utf8");
  const ts = readFileSync(join(root, "ui/src/app/lib/types/governance-change.ts"), "utf8");
  const pairs: [server: string, client: string, ts: string][] = [
    ["approvalsRead", "GovernanceChangeApprovals", "GovernanceChangeApprovals"],
    ["approvalEntry", "GovernanceApprovalEntry", "ApprovalEntry"],
    ["poolPerson", "GovernanceApprovalEligible", "ApprovalEligible"],
    ["poolSource", "GovernanceApprovalUncountable", "ApprovalUncountable"],
    ["approvalViewer", "GovernanceApprovalViewer", "ApprovalViewer"],
  ];

  it.each(pairs)("the server's %s, the SDK's %s and the TypeScript %s carry the same keys", (server, client, tsName) => {
    const keys = goJSONTags(goServer, server);
    expect(keys.length).toBeGreaterThanOrEqual(2); // stale-regex guard
    expect(goJSONTags(goClient, client)).toEqual(keys);
    expect(tsKeys(ts, tsName)).toEqual(keys);
  });

  it("a list row carries only some of the keys of the read of one change", () => {
    const read = goJSONTags(goServer, "approvalsRead");
    const list = goJSONTags(goServer, "approvalsListRead");
    expect(list).toEqual(["have", "require_security", "required", "viewer"]);
    expect(list.every((k) => read.includes(k))).toBe(true);
  });
});
