/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The owner-approved canon (docs/design/available-to-mock/canon.html, #923)
// is parsed back here and every console row is rendered from the copy
// modules, byte for byte. A reworded clause, a swapped dash, or a canon row
// with no key here fails this suite rather than shipping.
//
// Two normalisations, both the canon's own rules: a backticked literal is
// plain text (the component renders it mono), and a {placeholder} is filled
// with its fixture text, so CHIP_GROUP("Data engineering") must render
// "Data engineering (group)".
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { AVAILABILITY, IMAGES } from "./availability-copy";
import { PERM } from "./permissions-copy";

const CANON = resolve(process.cwd(), "../docs/design/available-to-mock/canon.html");

const decode = (s: string) =>
  s
    .replace(/<[^>]+>/g, "")
    .replace(/&quot;/g, '"')
    .replace(/&#39;/g, "'")
    .replace(/&lt;/g, "<")
    .replace(/&gt;/g, ">")
    .replace(/&amp;/g, "&");

// id -> canon string, for the rows this console carries: the control's
// (AVAILABILITY.*, PERM.SUBJECT_*) and the Images tab's (IMAGES.*).
function canonRows(): Map<string, string> {
  const rows = new Map<string, string>();
  const html = readFileSync(CANON, "utf8");
  for (const m of html.matchAll(/<td class="id">([^<]+)<\/td><td class="str">([\s\S]*?)<\/td>/g)) {
    const id = decode(m[1]).trim();
    if (/^(AVAILABILITY|IMAGES)\.|^PERM\.SUBJECT_/.test(id)) rows.set(id, decode(m[2]).replace(/`/g, ""));
  }
  return rows;
}

const NAMESPACES: Record<string, Record<string, unknown>> = { AVAILABILITY, IMAGES, PERM };

function render(id: string, canon: string): { got: string; want: string } {
  const [, ns, key] = /^([A-Z]+)\.([A-Z0-9_]+)/.exec(id)!;
  const value = NAMESPACES[ns][key];
  if (value === undefined) throw new Error(`${id}: no such key in ${ns}`);
  const fixtures = [...canon.matchAll(/\{([^}]*)\}/g)].map((f) => f[1]);
  const got = typeof value === "function" ? (value as (...a: string[]) => string)(...fixtures) : String(value);
  return { got, want: canon.replace(/[{}]/g, "") };
}

describe("availability canon (#923 canon.html)", () => {
  const rows = canonRows();

  it("parses every console row the canon freezes", () => {
    // Table 1 (19 rows) + Table 2 (11) + Table 3's IMAGES.* (7).
    expect(rows.size).toBe(37);
  });

  for (const [id, canon] of rows) {
    it(`${id} renders the canon byte for byte`, () => {
      const { got, want } = render(id, canon);
      expect(got).toBe(want);
    });
  }
});
