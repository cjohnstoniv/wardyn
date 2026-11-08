/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/**
 * Control and format runes (bidi overrides, zero-width, ESC) written as \uXXXX. Names come from the remote server and
 * the arguments from the agent, so none of it is drawn with a rune that reorders or hides text. In the JSON text
 * such a rune sits inside a string, where the escape reads as the same character; `keepNewline` keeps its layout.
 */
export function escapeUnsafeRunes(s: string, keepNewline = false): string {
  return s.replace(/[\p{Cc}\p{Cf}]/gu, (c) => {
    if (keepNewline && c === "\n") return c;
    // JSON's \u takes four hex digits: a rune above U+FFFF is its two UTF-16 halves, as the CLI writes it.
    return Array.from({ length: c.length }, (_, i) => `\\u${c.charCodeAt(i).toString(16).padStart(4, "0")}`).join("");
  });
}

const GO_ESCAPES: Record<string, string> = { "\u0007": "\\a", "\b": "\\b", "\f": "\\f", "\n": "\\n", "\r": "\\r", "\t": "\\t", "\v": "\\v", '"': '\\"', "\\": "\\\\" };

/**
 * s in quotes with everything but printable ASCII escaped, as the CLI prints a name (Go's strconv.QuoteToASCII):
 * \xNN for other controls, \uXXXX and \UXXXXXXXX for the rest. The exact name stays readable and no rune in it can
 * reorder or hide text.
 */
export function quoteToASCII(s: string): string {
  let out = '"';
  for (const ch of s) {
    const cp = ch.codePointAt(0)!;
    if (GO_ESCAPES[ch]) out += GO_ESCAPES[ch];
    else if (cp < 0x20 || cp === 0x7f) out += `\\x${cp.toString(16).padStart(2, "0")}`;
    else if (cp < 0x7f) out += ch;
    else if (cp <= 0xffff) out += `\\u${cp.toString(16).padStart(4, "0")}`;
    else out += `\\U${cp.toString(16).padStart(8, "0")}`;
  }
  return out + '"';
}
