/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #133 — the escape chord's keydown decision, pulled out of attach-terminal.tsx
// so the layout matrix (US/DE/FR/ES) can be table-tested without mounting xterm.
//
// Advertised chord: Ctrl+Shift+Backspace (every physical key it uses sits on
// the base row of every layout this widget ships to — no AltGr involved).
//
// Ctrl+] keeps working, silently, but ONLY when altKey is false. On DE/FR/ES
// layouts `]` is a level-2 (AltGr) character, and AltGr arrives at the browser
// as ctrlKey && altKey — so Ctrl+]-with-altKey is a user typing a bracket, not
// invoking the chord, and must reach the PTY (#133).

export type KeyDecision = "escape" | "newline" | "paste" | "pty";

type KeyLike = Pick<KeyboardEvent, "key" | "code" | "ctrlKey" | "altKey" | "shiftKey" | "metaKey">;

export function decideKey(e: KeyLike): KeyDecision {
  if (e.ctrlKey && e.shiftKey && !e.altKey && e.key === "Backspace") {
    return "escape";
  }
  // Silent, US-only fallback — never advertised. altKey rules out AltGr typing
  // a bracket on DE/FR/ES layouts (#133).
  if (e.ctrlKey && !e.shiftKey && !e.altKey && !e.metaKey && e.key === "]") {
    return "escape";
  }
  if (e.key === "Enter" && (e.shiftKey || e.ctrlKey)) {
    return "newline";
  }
  // Ctrl+V / Cmd+V (NOT Ctrl+Shift+V): read the clipboard and paste RAW.
  if ((e.ctrlKey || e.metaKey) && !e.shiftKey && (e.key === "v" || e.key === "V")) {
    return "paste";
  }
  return "pty";
}
