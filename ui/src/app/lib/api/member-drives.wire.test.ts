/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The super admin's list of people's drives (GET and DELETE /drives/member): each TS interface of api/drives.ts has
// exactly the json tags of the Go struct in pkg/client/drive.go, home_runner_id (the Mac a disk drive is kept on) among
// them. The Go side is read from source.

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

function goJSONTags(src: string, structName: string): string[] {
  const m = new RegExp(`type\\s+${structName}\\s+struct\\s*\\{([\\s\\S]*?)\\n\\}`).exec(src);
  if (!m) throw new Error(`struct ${structName} not found`);
  return [...m[1].matchAll(/json:"([^",]+)(?:,[^"]*)?"/g)].map((t) => t[1]).filter((t) => t !== "-");
}

function tsKeys(src: string, iface: string): string[] {
  const m = new RegExp(`export\\s+interface\\s+${iface}\\b[^{]*\\{([\\s\\S]*?)\\n\\}`).exec(stripComments(src));
  if (!m) throw new Error(`interface ${iface} not found`);
  return [...m[1].matchAll(/^\s*([a-z0-9_]+)\??\s*:/gm)].map((k) => k[1]);
}

describe("people's drives wire", () => {
  const go = read("pkg/client/drive.go");
  const ts = read("ui/src/app/lib/api/drives.ts");

  it.each(["MemberDriveAdminItem", "MemberDriveList", "MemberDriveDeleteResult"])("%s has the Go struct's json tags", (name) => {
    const tags = goJSONTags(go, name);
    expect(tags.length).toBeGreaterThanOrEqual(2); // stale-regex guard
    expect(new Set(tsKeys(ts, name))).toEqual(new Set(tags));
  });

  it("the Mac of a disk drive rides on the item", () => {
    expect(goJSONTags(go, "MemberDriveAdminItem")).toContain("home_runner_id");
    expect(tsKeys(ts, "MemberDriveAdminItem")).toContain("home_runner_id");
  });
});
