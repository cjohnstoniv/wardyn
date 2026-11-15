/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Go->TS wire parity for a governance change and its note: the Go json tags of types.GovernanceChange,
// types.ChangeNote and the SDK's GovernanceChange against the TS interfaces. Same technique as
// wire-parity.test.ts (it reads both sources as text), kept standalone so each file runs on its own.
//
// Run: cd ui && pnpm vitest run src/app/lib/types/governance-change-note.wire.test.ts

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
  return tags;
}

function stripComments(src: string): string {
  return src.replace(/\/\*[\s\S]*?\*\//g, "").replace(/\/\/.*$/gm, "");
}

// The top-level property names of one exported TS interface.
function tsInterfaceTopKeys(src: string, name: string): string[] {
  const m = new RegExp(`export\\s+interface\\s+${name}\\b[^{]*\\{([\\s\\S]*?)\\n\\}`).exec(stripComments(src));
  if (!m) throw new Error(`interface ${name} not found`);
  const keys: string[] = [];
  let depth = 0;
  let start = 0;
  const body = m[1];
  for (let i = 0; i <= body.length; i++) {
    const ch = body[i];
    if (ch === "{") depth++;
    else if (ch === "}") depth--;
    else if (depth === 0 && (i === body.length || ch === ";" || ch === "\n")) {
      const k = /^\s*(?:readonly\s+)?([A-Za-z_][A-Za-z0-9_]*)\??\s*:/.exec(body.slice(start, i));
      if (k) keys.push(k[1]);
      start = i + 1;
    }
  }
  return keys;
}

describe("Governance change wire (GET /governance/changes)", () => {
  it("the change and its note keep the Go and TypeScript keys in parity, in the server and the SDK", () => {
    const root = repoRoot();
    const goTypes = readFileSync(join(root, "internal/types/governance_change.go"), "utf8");
    const goNote = readFileSync(join(root, "internal/types/governance_change_note.go"), "utf8");
    const goClient = readFileSync(join(root, "pkg/client/governance.go"), "utf8");
    const ts = readFileSync(join(root, "ui/src/app/lib/types/governance-change.ts"), "utf8");
    // approvers rides only the read of one change (governanceChangeRead), and tokens_revocation_failed and detail
    // only an approval's answer, so the SDK struct carries them too.
    const tsKeys = tsInterfaceTopKeys(ts, "GovernanceChange").sort();
    expect(tsKeys.length).toBeGreaterThanOrEqual(12); // stale-regex guard
    const routeOnly = ["approvers", "detail", "tokens_revocation_failed"];
    expect(tsKeys.filter((k) => !routeOnly.includes(k))).toEqual(goJSONTags(goTypes, "GovernanceChange").sort());
    expect(tsKeys).toEqual(goJSONTags(goClient, "GovernanceChange").sort());
    expect(tsInterfaceTopKeys(ts, "GovernanceChangeNote").sort()).toEqual(goJSONTags(goNote, "ChangeNote").sort());
    expect(goJSONTags(goClient, "GovernanceChangeNote").sort()).toEqual(goJSONTags(goNote, "ChangeNote").sort());
  });
});
