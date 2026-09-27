/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The "What this type gets" canon (docs/design/user-types-canon.md, copied
// from the owner-approved user types packet A) is parsed back here and every
// key rendered from EXPLAIN byte for byte, in both directions: a reworded
// clause, a doc row with no key, or a key with no doc row fails this suite.
// A parameterized key is called with its own placeholder, so ONLY("{who}")
// must reproduce the doc's "only {who}".
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { parseFrozenTables, splitKey } from "./copy-doc-parity";
import { EXPLAIN } from "./user-types-copy";

const DOC = resolve(process.cwd(), "../docs/design/user-types-canon.md");
const doc = parseFrozenTables(DOC, /^## Frozen strings$/);

// "STATE.everyone" reads EXPLAIN.STATE.everyone; "ONLY(who)" calls ONLY("{who}").
function render(docKey: string): string {
  const [path, args] = splitKey(docKey);
  const value = path.split(".").reduce<unknown>((v, k) => (v as Record<string, unknown> | undefined)?.[k], EXPLAIN);
  if (value === undefined) throw new Error(`${docKey}: no such key in EXPLAIN`);
  return typeof value === "function" ? (value as (...a: string[]) => string)(...args.map((a) => `{${a}}`)) : String(value);
}

// Every leaf key of EXPLAIN, dotted, with a function's parameters as the doc spells them.
function moduleKeys(ns: Record<string, unknown>, prefix = ""): string[] {
  return Object.entries(ns).flatMap(([k, v]) =>
    typeof v === "object" && v !== null ? moduleKeys(v as Record<string, unknown>, `${prefix}${k}.`) : [`${prefix}${k}`],
  );
}

describe("user-types canon — What this type gets (packet A)", () => {
  it("finds the frozen table", () => {
    expect(doc.size).toBe(15);
  });

  it("covers every doc key, and freezes no key the doc doesn't", () => {
    const docNames = [...doc.keys()].map((k) => splitKey(k)[0]).sort();
    expect(moduleKeys(EXPLAIN).sort()).toEqual(docNames);
  });

  it.each([...doc.keys()])("%s is byte-exact", (key) => {
    expect(render(key)).toBe(doc.get(key));
  });
});
