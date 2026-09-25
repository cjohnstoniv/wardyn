/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Test-only support for the *-copy.test.ts doc-parity suites (ado-entra,
// user-drives, workspace-providers — the drives precedent every one of their
// header comments cites). Each of those docs freezes a `### 7.N` table of
// `| Key | ... | Frozen string |` rows that a copy module's namespace(s) must
// render byte-exact; this factors the shared parse/split/render machinery out
// of the three near-identical copies so a fix to one (e.g. a new
// normalisation rule) lands once. governance-copy.test.ts shares only the doc
// parser — its `rendered` map is written out by hand, on purpose, so it stays
// here instead of moving to renderFromNamespaces().

import { readFileSync } from "node:fs";

/** Strip markdown backticks — §7's header note makes mono a DISPLAY concern the frozen string itself doesn't carry. */
export const unmono = (s: string): string => s.replace(/`/g, "");

/** key -> frozen string, for every table row under a heading `sectionHeading` matches. */
export function parseFrozenTables(docPath: string, sectionHeading: RegExp): Map<string, string> {
  const rows = new Map<string, string>();
  let inSection = false;
  for (const line of readFileSync(docPath, "utf8").split("\n")) {
    if (line.startsWith("#")) {
      inSection = sectionHeading.test(line);
      continue;
    }
    if (!inSection || !line.startsWith("|")) continue;
    const cells = line.split("|").slice(1, -1).map((c) => c.trim());
    if (cells[0] === "Key") continue; // header
    if (/^:?-+:?$/.test(cells[0])) continue; // separator
    rows.set(unmono(cells[0]), unmono(cells[cells.length - 1]));
  }
  return rows;
}

/** `EDITOR_TITLE_EDIT(name)` -> ["EDITOR_TITLE_EDIT", ["name"]]. */
export function splitKey(docKey: string): [string, string[]] {
  const m = /^([A-Z0-9_]+)\((.*)\)$/.exec(docKey);
  return m ? [m[1], m[2].split(",").map((a) => a.trim())] : [docKey, []];
}

/**
 * Render a doc key against the first of several frozen-string namespaces that
 * carries it, calling a function key with its own placeholder args (so
 * `EDITOR_TITLE_EDIT(name)` is called with `"{name}"` and must reproduce the
 * doc cell character for character).
 */
export function renderFromNamespaces(docKey: string, namespaces: Record<string, unknown>[]): string {
  const [name, args] = splitKey(docKey);
  const ns = namespaces.find((n) => name in n);
  if (!ns) throw new Error(`${docKey}: no such key in the given namespaces`);
  const value = ns[name];
  return typeof value === "function" ? (value as (...a: string[]) => string)(...args.map((a) => `{${a}}`)) : String(value);
}
