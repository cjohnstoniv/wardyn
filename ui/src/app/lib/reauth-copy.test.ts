/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { REAUTH_BAR, REAUTH_DIALOG, REAUTH_RENEW } from "./reauth-copy";
import { SESSION_ENDED_REASON } from "./api/core";

// The canon doc's table, parsed back and compared row for row: a reworded
// clause, a swapped dash or a row added on one side only fails here.
//
// #488 (#726 remainder): widened past REAUTH_DIALOG/REAUTH_BAR to also pin
// SESSION_ENDED_REASON — the full sign-in screen's own notice, a bare key
// (no dot) rather than a REAUTH_* namespace member, but just as frozen a
// string as the two namespaces the regex already covered.
const DOC = resolve(process.cwd(), "../docs/design/reauth-in-place-canon.md");

function frozenRows(): Map<string, string> {
  const rows = new Map<string, string>();
  for (const line of readFileSync(DOC, "utf8").split("\n")) {
    const cells = line.split("|").slice(1, -1).map((c) => c.trim());
    if (cells.length === 3 && /^(REAUTH_(DIALOG|BAR|RENEW)\.|SESSION_ENDED_REASON$)/.test(cells[0])) {
      rows.set(cells[0], cells[1]);
    }
  }
  return rows;
}

describe("reauth-copy.ts matches docs/design/reauth-in-place-canon.md", () => {
  it("every frozen row, byte for byte, and nothing extra on either side", () => {
    const module = new Map<string, string>([
      ...Object.entries(REAUTH_DIALOG).map(([k, v]) => [`REAUTH_DIALOG.${k}`, v] as [string, string]),
      ...Object.entries(REAUTH_BAR).map(([k, v]) => [`REAUTH_BAR.${k}`, v] as [string, string]),
      // A sentence with a slot is a function; the canon row writes the slot as {time}.
      ...Object.entries(REAUTH_RENEW).map(
        ([k, v]) => [`REAUTH_RENEW.${k}`, typeof v === "function" ? v("{time}") : v] as [string, string],
      ),
      ["SESSION_ENDED_REASON", SESSION_ENDED_REASON],
    ]);
    expect(Object.fromEntries(module)).toEqual(Object.fromEntries(frozenRows()));
  });
});
