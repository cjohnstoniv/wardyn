/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { TERMINAL, TERMINAL_COPY, TERMINAL_RENDERER } from "../components/wardyn/copy/terminal";
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
const doc = new Map(
  [...rawDoc].filter(([key, value]) => !(key === "Constant" && value === "Value")).map(([key, value]) => [key, stripNote(value)]),
);

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

// M11 (term-t3b): the copy strings, parsed back out of the doc the same way.
const copyDoc = new Map(
  [...parseFrozenTables(DOC, /^## Copy strings/)].filter(([key]) => key !== "Constant"),
);

// `(pc)` / `(mac)` pick the platform; any other argument is a `{name}` placeholder.
const renderCopy = (docKey: string): string => {
  const [name, args] = splitKey(docKey);
  const fn = TERMINAL_COPY[name as keyof typeof TERMINAL_COPY];
  if (typeof fn !== "function") return String(fn);
  const call = fn as (...a: unknown[]) => string;
  return call(...args.map((a) => (a === "pc" ? false : a === "mac" ? true : `{${a}}`)));
};

describe("terminal-escape-copy — Copy strings, M11", () => {
  it("finds every TERMINAL_COPY row in the doc", () => {
    expect(copyDoc.size).toBe(15);
    expect(new Set([...copyDoc.keys()].map((k) => splitKey(k)[0]))).toEqual(new Set(Object.keys(TERMINAL_COPY)));
  });

  it.each([...copyDoc.keys()])("%s is byte-exact", (key) => {
    expect(renderCopy(key)).toBe(copyDoc.get(key));
  });

  it("counts take the singular at 1", () => {
    expect(TERMINAL_COPY.OFFER_SIZE(1)).toBe("1 character");
    expect(TERMINAL_COPY.OFFER_BREAKS(1)).toBe("1 line break");
    expect(TERMINAL_COPY.OFFER_INVISIBLE(1)).toBe("1 invisible character");
  });
});

// M11 (term-t10): the renderer strings.
const rendererDoc = new Map(
  [...parseFrozenTables(DOC, /^## Terminal renderer strings/)].filter(([key]) => key !== "Constant"),
);

describe("terminal-escape-copy — Terminal renderer strings, M11", () => {
  it("finds every TERMINAL_RENDERER row in the doc", () => {
    expect(new Set([...rendererDoc.keys()].map((k) => splitKey(k)[0]))).toEqual(new Set(Object.keys(TERMINAL_RENDERER)));
  });

  it.each([...rendererDoc.keys()])("%s is byte-exact", (key) => {
    const [name, args] = splitKey(key);
    const v = TERMINAL_RENDERER[name as keyof typeof TERMINAL_RENDERER];
    const text = typeof v === "function" ? (v as (a: string) => string)(`{${args[0]}}`) : v;
    expect(text).toBe(rendererDoc.get(key));
  });
});
