/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { escapeUnsafeRunes, quoteToASCII } from "./escape-runes";

// U+E0001 (a language tag) and U+1D173 (a musical begin-beam mark) are format runes above U+FFFF.
const ASTRAL = ["\u{E0001}", "\u{1D173}", "\u{E007F}"];

describe("escapeUnsafeRunes", () => {
  it.each(ASTRAL)("keeps JSON text meaning the same string when it holds astral format rune %j", (rune) => {
    const original = `{\n  "note": "a${rune}b",\n  "list": ["${rune}"]\n}`;
    const escaped = escapeUnsafeRunes(original, true);
    expect(escaped).not.toContain(rune);
    expect(JSON.parse(escaped)).toEqual(JSON.parse(original));
  });

  it("writes a BMP control or format rune as one four-digit escape and leaves newlines and plain text", () => {
    expect(escapeUnsafeRunes("a‮b\u001bc")).toBe("a\\u202eb\\u001bc");
    expect(escapeUnsafeRunes("x\ny", true)).toBe("x\ny");
    expect(escapeUnsafeRunes("café \u{1F600}")).toBe("café \u{1F600}");
  });
});

describe("quoteToASCII", () => {
  it("quotes as Go's QuoteToASCII does", () => {
    expect(quoteToASCII('a"b\\c\n\u001b‮é\u{1F600}')).toBe('"a\\"b\\\\c\\n\\x1b\\u202e\\u00e9\\U0001f600"');
  });
});
