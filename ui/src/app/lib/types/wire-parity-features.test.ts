/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Go->TS wire parity for the site config's features block (internal/types/site_features.go against
// ui/src/app/lib/types/site.ts). Same technique as wire-parity.test.ts, whose helpers this file copies so it stays a
// standalone probe runnable on its own, as the other parity files do.
//
// Run: cd ui && pnpm vitest run src/app/lib/types/wire-parity-features.test.ts

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

// The json tag names of one Go struct body (runs.wire.fields.test.ts's copy).
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

// The top-level members of one TS object-type body, each mapped to its inline
// object-type body when the member's type IS an inline `{ ... }` literal (null
// otherwise). Members split at `;` or a newline, but only at brace depth 0, so
// a nested key never reads as this body's own and a one-line
// `platform: { os: string; wsl: boolean }` stays one member.
function tsMembers(body: string): Map<string, string | null> {
  const members = new Map<string, string | null>();
  let depth = 0;
  let start = 0;
  for (let i = 0; i <= body.length; i++) {
    const ch = body[i];
    if (ch === "{") depth++;
    else if (ch === "}") depth--;
    else if (depth === 0 && (i === body.length || ch === ";" || ch === "\n")) {
      const seg = body.slice(start, i);
      const k = /^\s*(?:readonly\s+)?([A-Za-z_][A-Za-z0-9_]*)\??\s*:\s*(\{)?/.exec(seg);
      if (k) members.set(k[1], k[2] ? seg.slice(k[0].length, seg.lastIndexOf("}")) : null);
      start = i + 1;
    }
  }
  return members;
}

function tsInterfaceBody(src: string, name: string): string {
  const m = new RegExp(`export\\s+interface\\s+${name}\\b[^{]*\\{([\\s\\S]*?)\\n\\}`).exec(stripComments(src));
  if (!m) throw new Error(`interface ${name} not found`);
  return m[1];
}

// Like the other files' tsInterfaceKeys, but only TOP-LEVEL properties (see
// the file header for why SetupStatus needs this).
function tsInterfaceTopKeys(src: string, name: string): string[] {
  return [...tsMembers(tsInterfaceBody(src, name)).keys()];
}

describe("source parity — the site config features block vs its TS mirror", () => {
  const root = repoRoot();

  // The features block of the site config: one interface per key, each with its switch.
  it.each([
    ["SiteFeatureSwitches", ["org_sites", "windows_vm", "run_move", "machine_snapshots", "local_window", "preview_urls", "component_request_rules", "org_runner_drives"]],
    ["OrgSitesFeature", ["enabled"]],
    ["WindowsVMFeature", ["enabled"]],
    ["RunMoveFeature", ["enabled", "to_personal_runners", "max_carried_gib"]],
    ["MachineSnapshotsFeature", ["enabled", "warm_pool", "sleep_after_disconnect_sec", "max_per_person", "idle_days"]],
    ["WarmPoolTarget", ["image", "count"]],
    ["LocalWindowFeature", ["enabled"]],
    ["PreviewURLsFeature", ["enabled", "max_visibility"]],
    ["ComponentRequestRulesFeature", ["enabled"]],
    ["OrgRunnerDrivesFeature", ["enabled"]],
  ])("%s (`features` in site config): full parity with the TS mirror", (name, tags) => {
    const goTags = goJSONTags(readFileSync(join(root, "internal/types/site_features.go"), "utf8"), name);
    expect(goTags).toEqual(tags);
    const tsKeys = tsInterfaceTopKeys(readFileSync(join(root, "ui/src/app/lib/types/site.ts"), "utf8"), name);
    expect(new Set(tsKeys)).toEqual(new Set(goTags));
  });
});
