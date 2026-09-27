/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { PENDING_NO_DETAIL } from "../components/screens/run-status-detail";
import { RUN_DETAIL } from "../components/wardyn/copy/run-cockpit";
import { AGENTS } from "./workspace-providers-copy";
import { unmono } from "./copy-doc-parity";

// #493 (#726 remainder): docs/design/launch-navigates-canon.md's "## Frozen
// strings" table is a 4-column shape (Key | Status | Home | Value) the
// generic parseFrozenTables() can't fully read — it keeps only the first and
// last cell, with no notion of the Status column this doc needs: one row
// (AGENTS.OPEN_RUN_CTA) is marked "Removed" and documents a string this build
// no longer renders, not one still frozen. This suite parses the table
// directly so that row can be dropped before the byte-exact comparison,
// rather than pinned as though the app still showed it.
const DOC = resolve(process.cwd(), "../docs/design/launch-navigates-canon.md");

function frozenRows(): Map<string, string> {
  const rows = new Map<string, string>();
  let inSection = false;
  for (const line of readFileSync(DOC, "utf8").split("\n")) {
    if (line.startsWith("#")) {
      inSection = /^## Frozen strings/.test(line);
      continue;
    }
    if (!inSection || !line.startsWith("|")) continue;
    const cells = line.split("|").slice(1, -1).map((c) => c.trim());
    if (cells[0] === "Key") continue; // header
    if (/^:?-+:?$/.test(cells[0])) continue; // separator
    if (cells[1] === "Removed") continue; // history, not a live pin
    rows.set(unmono(cells[0]), unmono(cells[cells.length - 1]));
  }
  return rows;
}

const doc = frozenRows();

// RUN_DETAIL.LAUNCH_WARNING_TITLE is deliberately NOT its own module
// constant — the doc's own header note says the run page reuses
// AGENTS.LAUNCH_WARNING_TITLE verbatim so the two surfaces can't spell
// "launched with a warning" two different ways; rendered from there.
const rendered: Record<string, string> = {
  "RUN_STATUS.PENDING_NO_DETAIL": PENDING_NO_DETAIL,
  "RUN_DETAIL.LAUNCH_WARNING_TITLE": AGENTS.LAUNCH_WARNING_TITLE,
  "RUN_DETAIL.LAUNCH_WARNING_EPHEMERAL": RUN_DETAIL.LAUNCH_WARNING_EPHEMERAL,
  "RUN_DETAIL.LAUNCH_WARNING_DISMISS": RUN_DETAIL.LAUNCH_WARNING_DISMISS,
};

describe("launch-navigates-copy — launch-navigates-canon.md, #493", () => {
  it("finds all 4 still-frozen keys in the doc (the Removed row is history, not a pin)", () => {
    expect(doc.size).toBe(4);
  });

  it.each(Object.keys(rendered))("%s is byte-exact", (key) => {
    expect(rendered[key]).toBe(doc.get(key));
  });

  it("covers every doc key, and freezes no key the doc doesn't", () => {
    expect(Object.keys(rendered).sort()).toEqual([...doc.keys()].sort());
  });

  it("AGENTS.OPEN_RUN_CTA was actually removed, not just left out of this pin", () => {
    expect((AGENTS as Record<string, unknown>).OPEN_RUN_CTA).toBeUndefined();
  });
});
