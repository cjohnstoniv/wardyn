/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

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
// constant exists to pin them against) — filtered out below.
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
