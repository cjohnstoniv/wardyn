/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// KeyboardEvent -> X11 keysym, the value RFB's KeyEvent carries (RFC 6143 7.5.4, X11 keysym encoding).

const NAMED: Record<string, number> = {
  Backspace: 0xff08, Tab: 0xff09, Enter: 0xff0d, Escape: 0xff1b, Delete: 0xffff,
  Home: 0xff50, ArrowLeft: 0xff51, ArrowUp: 0xff52, ArrowRight: 0xff53, ArrowDown: 0xff54,
  PageUp: 0xff55, PageDown: 0xff56, End: 0xff57, Insert: 0xff63, ContextMenu: 0xff67,
  CapsLock: 0xffe5, NumLock: 0xff7f, ScrollLock: 0xff14, Pause: 0xff13, PrintScreen: 0xff61,
};

// Left and right modifiers are separate keysyms.
const MODIFIER: Record<string, [left: number, right: number]> = {
  Shift: [0xffe1, 0xffe2], Control: [0xffe3, 0xffe4], Alt: [0xffe9, 0xffea], AltGraph: [0xfe03, 0xfe03],
  Meta: [0xffeb, 0xffec], OS: [0xffeb, 0xffec],
};

/** The keysym for e, or undefined for a key with none (a dead key, an unidentified one). */
export function keysymOf(e: Pick<KeyboardEvent, "key" | "location">): number | undefined {
  const mod = MODIFIER[e.key];
  if (mod) return e.location === 2 ? mod[1] : mod[0];
  if (e.key in NAMED) return NAMED[e.key];
  const f = /^F([1-9]|1[0-2])$/.exec(e.key);
  if (f) return 0xffbe + Number(f[1]) - 1;
  const cp = e.key.codePointAt(0);
  if (cp === undefined || String.fromCodePoint(cp) !== e.key) return undefined;
  // Latin-1 keysyms are the code point; every other character is 0x01000000 plus it.
  return (cp >= 0x20 && cp <= 0x7e) || (cp >= 0xa0 && cp <= 0xff) ? cp : 0x01000000 | cp;
}
