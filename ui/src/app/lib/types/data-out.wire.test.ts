/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// data_out: the Go json tags on the run policy, the ceiling overlay, the boundary and the restriction are the
// keys the console's TypeScript mirrors carry, and the four reasons the server answers with are in the Go
// reason list. The Go side is read from source.

import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { describe, expect, it } from "vitest";

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

const stripComments = (src: string) => src.replace(/\/\*[\s\S]*?\*\//g, "").replace(/\/\/.*$/gm, "");

function tsInterfaceBody(src: string, name: string): string {
  const m = new RegExp(`export\\s+interface\\s+${name}\\b[^{]*\\{([\\s\\S]*?)\\n\\}`).exec(stripComments(src));
  if (!m) throw new Error(`interface ${name} not found`);
  return m[1];
}

describe("data_out wire", () => {
  it("is a data_out field on the run policy, the ceiling overlay, the boundary and the restriction", () => {
    for (const [file, ptr] of [
      ["internal/types/policy.go", ""],
      ["internal/types/overlay.go", "\\*"],
      ["internal/types/boundary.go", "\\*"],
      ["internal/types/restriction.go", "\\*"],
    ] as const) {
      expect(read(file)).toMatch(new RegExp(`DataOut\\s+${ptr}TransferRule\\s+\`json:"data_out,omitempty"\``));
    }
  });

  it("is a data_out member on each TypeScript mirror", () => {
    for (const [file, name] of [
      ["ui/src/app/lib/types/policy.ts", "RunPolicySpec"],
      ["ui/src/app/lib/api/boundary.ts", "BoundarySpec"],
      ["ui/src/app/lib/api/restrictions.ts", "RestrictionSpec"],
    ] as const) {
      expect(tsInterfaceBody(read(file), name)).toMatch(/data_out\?: "allowed" \| "review" \| "blocked";/);
    }
  });

  it("has its four reasons in the Go reason list", () => {
    const go = read("internal/api/reasons_governance.go");
    for (const [name, value] of [
      ["reasonDataOutBlocked", "data_out_blocked"],
      ["reasonDataOutUnreadable", "data_out_unreadable"],
      ["reasonDataOutReviewUnavailable", "data_out_review_unavailable"],
      ["reasonComputerFolderDataOut", "computer_folder_data_out"],
    ] as const) {
      expect(go).toMatch(new RegExp(`${name}\\s+=\\s+"${value}"`));
    }
  });
});
