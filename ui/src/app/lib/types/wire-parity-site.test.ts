/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Go->TS wire parity for the site config's nested blocks (branding, macOS VM,
// open sessions, withheld SCM hosts). Split from wire-parity.test.ts, which
// holds the other DTOs and has the file header; the helpers below are the
// same copies.
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

describe("source parity — the site config's nested blocks vs their TS mirrors", () => {
  const root = repoRoot();

  // #1215: the site config's branding block — one tag today (logo_path), so it
  // cannot ride the it.each below, whose stale-regex guard wants two.
  it("SiteBranding (`branding` in site config): full parity with the TS mirror", () => {
    const goTags = goJSONTags(readFileSync(join(root, "internal/types/site_config.go"), "utf8"), "SiteBranding");
    expect(goTags).toEqual(["logo_path"]);
    const tsKeys = tsInterfaceTopKeys(readFileSync(join(root, "ui/src/app/lib/types/site.ts"), "utf8"), "SiteBranding");
    expect(new Set(tsKeys)).toEqual(new Set(goTags));
  });

  // The macOS VM block of the site config: max_base_age_days, the background-watch and window switches, and the three kept-machine keys.
  it("SiteMacOSVM (`macos_vm` in site config): full parity with the TS mirror", () => {
    const goTags = goJSONTags(readFileSync(join(root, "internal/types/site_config.go"), "utf8"), "SiteMacOSVM");
    expect(goTags).toEqual(["max_base_age_days", "allow_background_watch", "allow_attended_window", "allow_kept_machines", "kept_machine_idle_days", "max_kept_machines_per_person"]);
    const tsKeys = tsInterfaceTopKeys(readFileSync(join(root, "ui/src/app/lib/types/site.ts"), "utf8"), "SiteMacOSVM");
    expect(new Set(tsKeys)).toEqual(new Set(goTags));
  });

  // The open-session block of the site config: the one budget key.
  it("SiteSessions (`sessions` in site config): full parity with the TS mirror", () => {
    const goTags = goJSONTags(readFileSync(join(root, "internal/types/site_config.go"), "utf8"), "SiteSessions");
    expect(goTags).toEqual(["unconfirmed_budget_s"]);
    const tsKeys = tsInterfaceTopKeys(readFileSync(join(root, "ui/src/app/lib/types/site.ts"), "utf8"), "SiteSessions");
    expect(new Set(tsKeys)).toEqual(new Set(goTags));
  });

  // withheld_scm_hosts rows: three tags, no Go struct beyond types.WithheldScmHost.
  it("WithheldScmHost (`withheld_scm_hosts` in site config): full parity with the TS mirror", () => {
    const goTags = goJSONTags(readFileSync(join(root, "internal/types/site_config.go"), "utf8"), "WithheldScmHost");
    expect(goTags).toEqual(["host", "provider_id", "provider_kind"]);
    const tsKeys = tsInterfaceTopKeys(readFileSync(join(root, "ui/src/app/lib/types/site.ts"), "utf8"), "WithheldScmHost");
    expect(new Set(tsKeys)).toEqual(new Set(goTags));
  });
});
