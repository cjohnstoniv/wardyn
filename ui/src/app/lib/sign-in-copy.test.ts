/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { SIGNIN } from "./sign-in-copy";
import { STATES } from "../components/wardyn/states";
import { parseFrozenTables } from "../../test/canon-doc-parser";

// The mock round's whole value is that it stays CHECKABLE (the ado-entra
// precedent, canon-doc-parser.ts's parseFrozenTables(), T-66): this suite
// does not hand-retype a sample of the canon — it PARSES
// docs/design/signin-first-contact-canon.md's "Frozen strings" table back out
// of the doc and compares every key. A swapped hyphen, a dropped ellipsis, a
// reworded clause, a new doc row or a deleted one all fail here rather than
// shipping. This is the "0 drift today by script only" row of the 0.8
// testing-gap audit's T-66 — a real vitest pin replaces the manual check.
//
// Unlike the ado-entra/governance/user-drives/workspace-providers docs (one
// module's flat namespace per doc), this doc's single table carries rows for
// TWO modules — SIGNIN (sign-in-copy.ts) and STATES (wardyn/states.tsx) —
// each key namespaced by its module ("SIGNIN.LEAD", "STATES.RETRY"). Five
// rows key "—": inline JSX literals the doc lists for completeness (#457)
// but that are not a copy-module key (no admin/SSO/Remember-checkbox
// constant exists to pin them against) — filtered out below, then pinned
// separately against sign-in.tsx's source text (#865).
const DOC = resolve(process.cwd(), "../docs/design/signin-first-contact-canon.md");

const rawDoc = parseFrozenTables(DOC, /^## Frozen strings/);
const doc = new Map([...rawDoc].filter(([key]) => key !== "—"));

const MODULES = { SIGNIN, STATES } as const;

function render(docKey: string): string {
  const [ns, name] = docKey.split(".");
  const mod = MODULES[ns as keyof typeof MODULES] as Record<string, unknown>;
  if (!mod || !(name in mod)) throw new Error(`${docKey}: no such key in ${ns}`);
  return String(mod[name]);
}

describe("sign-in-copy / states — signin-first-contact-canon.md, #457", () => {
  it("finds all 17 frozen keys in the doc", () => {
    expect(doc.size).toBe(17);
  });

  it.each([...doc.keys()])("%s is byte-exact", (key) => {
    expect(render(key)).toBe(doc.get(key));
  });

  it("covers every doc key, and freezes no key the doc doesn't", () => {
    // BOTH directions: every doc row resolves to a module symbol, and every
    // module symbol has a doc row.
    const docNames = [...doc.keys()].sort();
    const moduleNames = [
      ...Object.keys(SIGNIN).map((k) => `SIGNIN.${k}`),
      ...Object.keys(STATES).map((k) => `STATES.${k}`),
    ].sort();
    expect(moduleNames).toEqual(docNames);
  });
});

// #865: the five rows above key "—" are the doc's own escape hatch for
// strings that aren't a copy-module key at all — inline JSX text in
// sign-in.tsx. `parseFrozenTables` keys its Map by the doc's first cell, so
// five rows sharing key "—" collapse to one entry there; this block re-reads
// the same table's raw rows (keyed by "Where" instead, which IS unique) and
// checks each one against sign-in.tsx's source text directly — the same
// approach user-drives-copy.test.ts uses to pin mount.go's Go literals,
// scaled down for JSX that has no render function to call. A capturing
// regex, anchored on markup either side of the string that is not itself
// under test, extracts whatever text sign-in.tsx actually has; a mutation
// to either the doc cell or the JSX literal — without the same edit on the
// other side — fails a row here.
function rawFrozenStringsRows(): [string, string][] {
  const rows: [string, string][] = [];
  let inSection = false;
  for (const line of readFileSync(DOC, "utf8").split("\n")) {
    if (line.startsWith("#")) {
      inSection = /^## Frozen strings/.test(line);
      continue;
    }
    if (!inSection || !line.startsWith("|")) continue;
    const cells = line.split("|").slice(1, -1).map((c) => c.trim());
    if (cells[0] === "Key" || /^:?-+:?$/.test(cells[0])) continue;
    if (cells[0] === "—") rows.push([cells[2], cells[3]]);
  }
  return rows;
}

const SIGN_IN_TSX = readFileSync(
  resolve(process.cwd(), "src/app/components/screens/sign-in.tsx"),
  "utf8",
);

// "Where" (the doc's third cell) -> a regex whose one capture group is the
// JSX text node sign-in.tsx actually renders there, anchored on
// component/prop markup that isn't part of any of these five strings.
const INLINE_JSX_ANCHORS: Record<string, RegExp> = {
  "admin-token label": /<Label htmlFor="token"[^>]*>\s*\n\s*(.+?)\s*\n\s*<\/Label>/,
  "Remember checkbox label":
    /onCheckedChange=\{[^}]*\}\s*\n\s*aria-label="[^"]*"\s*\n\s*\/>\s*\n\s*(.+?)\s*\n\s*<span/,
  "Remember checkbox hint": /<span className="text-muted-foreground">\s*\n\s*(.+?)\s*\n\s*<\/span>/,
  "token form submit": /<>\s*\n\s*(.+?)\s*\n\s*<ArrowRight/,
  "SSO control": /<Building2[^>]*\/>\s*\n\s*(.+?)\s*\n\s*<\/a>/,
};

describe("sign-in-copy — the doc's inline-JSX rows, pinned against sign-in.tsx (#865)", () => {
  const rows = rawFrozenStringsRows();

  it("finds all 5 inline-JSX rows in the doc", () => {
    expect(rows.length).toBe(5);
  });

  it.each(rows)("%s: sign-in.tsx matches the doc cell", (where, expected) => {
    const anchor = INLINE_JSX_ANCHORS[where];
    if (!anchor) throw new Error(`${where}: no anchor regex for this doc row`);
    const match = anchor.exec(SIGN_IN_TSX);
    if (!match) throw new Error(`${where}: anchor regex found no match in sign-in.tsx`);
    expect(match[1]).toBe(expected);
  });
});
