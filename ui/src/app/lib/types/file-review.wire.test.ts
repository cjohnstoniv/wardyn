/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Go->TS wire parity for a reviewed download (desktop.download review): the file_download approval kind, its
// requested_scope (FileDownloadScope) and the review transfer rule are read from the Go source as text and
// compared with the TS mirrors. Kept in its own file because wire-parity.test.ts is at the file-size cap.
//
// Run: cd ui && pnpm exec vitest run src/app/lib/types/file-review.wire.test.ts

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
  return [...m[1].matchAll(/json:"([^",]+)(?:,[^"]*)?"/g)].map((t) => t[1]).filter((t) => t !== "-");
}

function stripComments(src: string): string {
  return src.replace(/\/\*[\s\S]*?\*\//g, "").replace(/\/\/.*$/gm, "");
}

function tsInterfaceKeys(src: string, name: string): string[] {
  const m = new RegExp(`export\\s+interface\\s+${name}\\b[^{]*\\{([\\s\\S]*?)\\n\\}`).exec(stripComments(src));
  if (!m) throw new Error(`interface ${name} not found`);
  return [...m[1].matchAll(/^\s*([A-Za-z_][A-Za-z0-9_]*)\??\s*:/gm)].map((k) => k[1]);
}

// The string literals of one TS union type alias.
function tsUnion(src: string, name: string): string[] {
  const m = new RegExp(`export\\s+type\\s+${name}\\s*=([^;]*);`).exec(stripComments(src));
  if (!m) throw new Error(`type ${name} not found`);
  return [...m[1].matchAll(/"([^"]+)"/g)].map((v) => v[1]);
}

// The values of every Go constant of goType declared in src.
function goConsts(src: string, goType: string): string[] {
  return [...src.matchAll(new RegExp(`^\\s*\\w+\\s+${goType}\\s*=\\s*"([^"]+)"`, "gm"))].map((v) => v[1]);
}

describe("Reviewed download wire (file_download, desktop.download review)", () => {
  const root = repoRoot();
  const goTypes = readFileSync(join(root, "internal/types/types.go"), "utf8");
  const goDesktop = readFileSync(join(root, "internal/types/desktop.go"), "utf8");
  const goScope = readFileSync(join(root, "internal/types/file_download.go"), "utf8");
  const tsApprovals = readFileSync(join(root, "ui/src/app/lib/types/approvals.ts"), "utf8");
  const tsPolicy = readFileSync(join(root, "ui/src/app/lib/types/policy.ts"), "utf8");

  it("the TS ApprovalKind union is exactly Go's ApprovalKind constants", () => {
    expect(tsUnion(tsApprovals, "ApprovalKind").sort()).toEqual(goConsts(goTypes, "ApprovalKind").sort());
  });

  it("the TS TransferRule union is exactly Go's TransferRule constants", () => {
    expect(tsUnion(tsPolicy, "TransferRule").sort()).toEqual(goConsts(goDesktop, "TransferRule").sort());
  });

  it("FileDownloadScope's TS keys are exactly its Go json tags", () => {
    expect(tsInterfaceKeys(tsApprovals, "FileDownloadScope").sort()).toEqual(goJSONTags(goScope, "FileDownloadScope").sort());
  });
});
