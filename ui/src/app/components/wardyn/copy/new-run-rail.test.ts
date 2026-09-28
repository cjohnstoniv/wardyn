/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #542 rail-gap packet (owner-approved 2026-09-25) — the canon doc
// (docs/design/542-rail-gaps-mock/canon.md) parsed back out and compared
// whole against RAIL_PROVIDER's five new strings: a changed string, a new doc
// row or a dropped one all fail here. Same PARSE-THE-DOC-BACK-OUT method as
// model-providers-copy.test.ts's parseFrozenTables, adapted for this doc's own
// table shape (`| Id | String | Example | Why |` — the frozen string is the
// SECOND column, not the last, so parseFrozenTables' own last-column
// assumption doesn't fit and this file parses it directly instead).
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { unmono } from "../../../lib/copy-doc-parity";
import { RAIL_PROVIDER } from "./new-run-rail";

// process.cwd() is the vitest root (ui/) for every entry point that runs this
// suite — see user-drives-copy.test.ts's own comment on the same rule.
const DOC = resolve(process.cwd(), "../docs/design/542-rail-gaps-mock/canon.md");

/** `| Id | String | Example | Why |` -> Id -> String, for every row in the
 *  doc — canon.md has no OTHER table, so no section-heading filter is needed. */
function parseCanonTable(docPath: string): Map<string, string> {
  const rows = new Map<string, string>();
  for (const line of readFileSync(docPath, "utf8").split("\n")) {
    if (!line.startsWith("|")) continue;
    const cells = line.split("|").slice(1, -1).map((c) => c.trim());
    if (cells[0] === "Id") continue; // header
    if (/^:?-+:?$/.test(cells[0])) continue; // separator
    rows.set(unmono(cells[0]), unmono(cells[1]));
  }
  return rows;
}

const doc = parseCanonTable(DOC);

const rendered: Record<string, string> = {
  "RAIL_PROVIDER.NO_KEY(name)": RAIL_PROVIDER.NO_KEY("{name}"),
  "RAIL_PROVIDER.NOT_SIGNED_IN_CLAUDE(name)": RAIL_PROVIDER.NOT_SIGNED_IN_CLAUDE("{name}"),
  "RAIL_PROVIDER.NOT_GRANTED(harness)": RAIL_PROVIDER.NOT_GRANTED("{harness}"),
  "RAIL_PROVIDER.DEFAULT_OFF(name, harness)": RAIL_PROVIDER.DEFAULT_OFF("{name}", "{harness}"),
  "RAIL_PROVIDER.DEFAULT_OFF_ONLY(name, harness)": RAIL_PROVIDER.DEFAULT_OFF_ONLY("{name}", "{harness}"),
};

describe("RAIL_PROVIDER — docs/design/542-rail-gaps-mock/canon.md", () => {
  it("renders every frozen string byte for byte, and no key the doc lacks", () => {
    expect(Object.fromEntries(doc)).toEqual(rendered);
  });
});
