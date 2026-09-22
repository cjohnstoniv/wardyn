/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { REAUTH_BAR, REAUTH_DIALOG } from "./reauth-copy";

// The canon doc's table, parsed back and compared row for row: a reworded
// clause, a swapped dash or a row added on one side only fails here.
const DOC = resolve(process.cwd(), "../docs/design/reauth-in-place-canon.md");

function frozenRows(): Map<string, string> {
  const rows = new Map<string, string>();
  for (const line of readFileSync(DOC, "utf8").split("\n")) {
    const cells = line.split("|").slice(1, -1).map((c) => c.trim());
    if (cells.length === 3 && /^REAUTH_(DIALOG|BAR)\./.test(cells[0])) rows.set(cells[0], cells[1]);
  }
  return rows;
}

describe("reauth-copy.ts matches docs/design/reauth-in-place-canon.md", () => {
  it("every frozen row, byte for byte, and nothing extra on either side", () => {
    const module = new Map<string, string>([
      ...Object.entries(REAUTH_DIALOG).map(([k, v]) => [`REAUTH_DIALOG.${k}`, v] as [string, string]),
      ...Object.entries(REAUTH_BAR).map(([k, v]) => [`REAUTH_BAR.${k}`, v] as [string, string]),
    ]);
    expect(Object.fromEntries(module)).toEqual(Object.fromEntries(frozenRows()));
  });
});
