/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The user types canon (docs/design/user-types-canon.md) is parsed back here
// and every key rendered from its namespace byte for byte, in both
// directions: a reworded clause, a doc row with no key, or a key with no doc
// row fails this suite. A parameterized key is called with its own
// placeholder, so ONLY("{who}") must reproduce the doc's "only {who}".
//
// Two tables, two namespaces: "## Frozen strings" is EXPLAIN — "What this
// type gets", from packet A plus packet UT-G's eight approved gaps. "##
// Frozen strings — the rest of the screen" is USER_TYPES — the list, editor,
// delete and Ceiling lead lines packet A never drew, frozen as built by
// packet UT-G's G-8.
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { parseFrozenTables, splitKey } from "./copy-doc-parity";
import { EXPLAIN, USER_TYPES } from "./user-types-copy";

const DOC = resolve(process.cwd(), "../docs/design/user-types-canon.md");

// Every leaf key of a namespace, dotted, with a function's parameters as the doc spells them.
function moduleKeys(ns: Record<string, unknown>, prefix = ""): string[] {
  return Object.entries(ns).flatMap(([k, v]) =>
    typeof v === "object" && v !== null ? moduleKeys(v as Record<string, unknown>, `${prefix}${k}.`) : [`${prefix}${k}`],
  );
}

// checkParity binds one doc table (by its own heading) to one copy namespace,
// and runs the same three checks packet A's suite always has: the table is
// found, it covers exactly the namespace's own keys, and every row renders
// byte-exact against a call with its own placeholder args.
function checkParity(label: string, sectionHeading: RegExp, ns: Record<string, unknown>, size: number) {
  const doc = parseFrozenTables(DOC, sectionHeading);

  // "STATE.everyone" reads ns.STATE.everyone; "ONLY(who)" calls ONLY("{who}").
  function render(docKey: string): string {
    const [path, args] = splitKey(docKey);
    const value = path.split(".").reduce<unknown>((v, k) => (v as Record<string, unknown> | undefined)?.[k], ns);
    if (value === undefined) throw new Error(`${docKey}: no such key in ${label}`);
    return typeof value === "function" ? (value as (...a: string[]) => string)(...args.map((a) => `{${a}}`)) : String(value);
  }

  describe(label, () => {
    it("finds the frozen table", () => {
      expect(doc.size).toBe(size);
    });

    it("covers every doc key, and freezes no key the doc doesn't", () => {
      const docNames = [...doc.keys()].map((k) => splitKey(k)[0]).sort();
      expect(moduleKeys(ns).sort()).toEqual(docNames);
    });

    it.each([...doc.keys()])("%s is byte-exact", (key) => {
      expect(render(key)).toBe(doc.get(key));
    });
  });
}

checkParity("user-types canon — What this type gets (packet A + packet UT-G)", /^## Frozen strings$/, EXPLAIN, 27);
checkParity(
  "user-types canon — the rest of the screen (packet UT-G, G-8)",
  /^## Frozen strings — the rest of the screen$/,
  USER_TYPES,
  34,
);
