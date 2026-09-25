/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { TERMINAL } from "../components/wardyn/copy/terminal";
import { parseFrozenTables, renderFromNamespaces, splitKey } from "./copy-doc-parity";

// The mock round's whole value is that it stays CHECKABLE (the sign-in/
// ado-entra precedent, copy-doc-parity.ts's parseFrozenTables(), T-66): this
// suite does not hand-retype a sample of the canon — it PARSES
// docs/design/terminal-escape-canon.md's "## Strings" table back out of the
// doc and compares every key.
//
// Two doc-specific quirks, both handled here rather than in the shared
// parser (which stays untouched):
//   - The table's header cell reads "Constant", not "Key" — the only literal
//     parseFrozenTables() skips as a header — so the raw parse carries one
//     spurious "Constant" -> "Value" row, filtered out below.
//   - Three of the four cells carry a parenthetical annotation glued onto the
//     frozen string in the SAME cell ("... (composed from ESCAPE_CHORD — one
//     spelling reaches every render site)", "(unchanged by #133)"), unlike
//     every other canon doc's table, which keeps commentary in its own
//     column. Stripped by a trailing " (...)" cut, same spirit as the shared
//     parser's own unmono() — a display/commentary concern, not the frozen
//     string itself. None of the four rows' actual strings contain a
//     parenthesis, so the cut is unambiguous.
const DOC = resolve(process.cwd(), "../docs/design/terminal-escape-canon.md");

const stripNote = (s: string): string => s.replace(/ \([^)]*\)$/, "");

const rawDoc = parseFrozenTables(DOC, /^## Strings/);
const doc = new Map([...rawDoc].filter(([key]) => key !== "Constant").map(([key, value]) => [key, stripNote(value)]));

// Owner ruling 2026-09-25 (#726): canon follows the app. RECONNECTING_HINT
// used to diverge — the doc still promised "Keystrokes are held until the
// terminal is back." after #510-F2 found there is no input buffer
// (attach-terminal.tsx's send() drops anything typed while the socket isn't
// OPEN) and changed terminal.ts's own string to "Keystrokes typed now are
// not sent." The doc has been updated to match; all four rows now pin
// byte-exact.
const render = (docKey: string) => renderFromNamespaces(docKey, [TERMINAL]);

describe("terminal-escape-copy — terminal-escape-canon.md, #486", () => {
  it("finds all 4 frozen keys in the doc", () => {
    expect(doc.size).toBe(4);
  });

  it("every doc key resolves to a TERMINAL symbol", () => {
    const docNames = [...doc.keys()].map((k) => splitKey(k)[0]).sort();
    expect(docNames).toEqual(["ESCAPE_CHORD", "ESCAPE_CHORD_HINT", "RECONNECTING_HINT", "RECONNECTING_LINE"]);
    for (const name of docNames) expect(TERMINAL).toHaveProperty(name);
  });

  it.each([...doc.keys()])("%s is byte-exact", (key) => {
    expect(render(key)).toBe(doc.get(key));
  });
});
