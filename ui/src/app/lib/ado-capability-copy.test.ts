/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { ADO_CAPABILITY } from "./ado-capability-copy";

// This is the SAME parseFrozenTables() method ado-entra-copy.test.ts (PR
// #415) uses: parse docs/design/ado-entra-prompt.md's frozen §7 tables back
// out of the doc and compare every key this module carries, so a swapped
// hyphen or a reworded clause fails here instead of shipping. Unlike that
// suite, this one does NOT assert full §7.4/§7.6/§7.8 coverage — this module
// is a deliberate SUBSET (see its own doc comment for which keys and why),
// so the only claim this test makes is: every key ADO_CAPABILITY exports
// exists in the doc under §7.4, §7.6 or §7.8, and matches it byte-exact.
const DOC = resolve(process.cwd(), "../docs/design/ado-entra-prompt.md");

const unmono = (s: string) => s.replace(/`/g, "");

/** key -> frozen string, for every row of §7.4, §7.6 and §7.8's tables. */
function parseFrozenTables(): Map<string, string> {
  const rows = new Map<string, string>();
  let inSection = false;
  for (const line of readFileSync(DOC, "utf8").split("\n")) {
    if (line.startsWith("#")) {
      inSection = /^### 7\.(4|6|8)\b/.test(line);
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

const doc = parseFrozenTables();

/** `REQ_WAITING_OTHER(person)` -> ["REQ_WAITING_OTHER", ["person"]]. */
function splitKey(docKey: string): [string, string[]] {
  const m = /^([A-Z0-9_]+)\((.*)\)$/.exec(docKey);
  return m ? [m[1], m[2].split(",").map((a) => a.trim())] : [docKey, []];
}

// Every doc key whose bare name (no parens) matches one of this module's own
// exported names — i.e. the subset this module actually carries.
const CARRIED_DOC_KEYS = [...doc.keys()].filter((k) => splitKey(k)[0] in ADO_CAPABILITY);

function render(docKey: string): string {
  const [name, args] = splitKey(docKey);
  const value = (ADO_CAPABILITY as Record<string, unknown>)[name];
  if (value === undefined) throw new Error(`${docKey}: no such key in ADO_CAPABILITY`);
  return typeof value === "function" ? (value as (...a: string[]) => string)(...args.map((a) => `{${a}}`)) : String(value);
}

describe("ado-capability-copy — a subset of §7.4/§7.6/§7.8, parsed out of the prompt doc", () => {
  it("every module key is a real §7.4/§7.6/§7.8 row (no invented canon)", () => {
    const moduleNames = Object.keys(ADO_CAPABILITY).sort();
    const carriedNames = [...new Set(CARRIED_DOC_KEYS.map((k) => splitKey(k)[0]))].sort();
    expect(moduleNames).toEqual(carriedNames);
  });

  it.each(CARRIED_DOC_KEYS)("%s is byte-exact against the doc", (key) => {
    expect(render(key)).toBe(doc.get(key));
  });

  it("carries no copy the doc's freeze does not (defends against a hand-edit drifting from §7)", () => {
    // Every string value (and every function's rendering with placeholder
    // args) must appear verbatim somewhere in the doc's §7.4/§7.6/§7.8 cells.
    const docValues = new Set(doc.values());
    for (const key of Object.keys(ADO_CAPABILITY)) {
      const value = (ADO_CAPABILITY as Record<string, unknown>)[key];
      const rendered =
        typeof value === "function"
          ? (value as (...a: string[]) => string)("{a}", "{b}", "{c}")
          : String(value);
      // Functions are checked exactly by the byte-exact test above (which
      // renders with the DOC's own placeholder names) — this second pass
      // only re-confirms the plain string keys, which that test can't fully
      // distinguish from a coincidentally-matching substring.
      if (typeof value !== "function") {
        expect(docValues.has(rendered)).toBe(true);
      }
    }
  });
});
