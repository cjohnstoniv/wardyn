/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// R4-F144, WCAG 2.1.2: the cockpit terminal takes Tab, Shift+Tab and Escape
// into the PTY, so a keyboard user needs an advertised way out and 2.1.2
// requires it be advised ON ENTRY.
//
// Chord: Ctrl+Shift+Backspace (#133) — the M2 sheet's Ctrl+] made `]`
// untypeable on DE/FR/ES layouts, where `]` is a level-2 (AltGr) character and
// AltGr arrives at the browser as ctrlKey && altKey, so the chord and the
// keystroke that types a bracket were indistinguishable. Backspace has no
// AltGr shape on any layout this widget ships to. Ctrl+Shift+Esc (the
// originally filed proposal) is still out: Windows intercepts it at OS level
// (Task Manager) before the browser ever sees it.
// Ctrl+] keeps working (decideKey in attach-terminal-keys.ts) as a silent,
// unadvertised US-only fallback — it never fires when altKey is held, which is
// exactly the AltGr-typing-a-bracket case.
// ONE spelling of the chord, composed into the sentence rather than typed
// twice — 2.1.2 is satisfied by an exit that WORKS, not by a sentence about
// one.
const ESCAPE_CHORD = "Ctrl+Shift+Backspace";
export const TERMINAL = {
  ESCAPE_CHORD,
  // DRAFT (M2) — DIVERGES from the §7.6 staging, which spells this
  // `TERMINAL_ESCAPE_HINT(chord)` = "{chord} moves focus out of the terminal."
  // This is the M2 sitting sheet's §2 wording; the chord is that sheet's ruled
  // alternative to the filed Ctrl+Shift+Esc.
  ESCAPE_CHORD_HINT: `${ESCAPE_CHORD} leaves the terminal`,

  // #216 — connection state moves OUT of xterm's own scrollback (where `[closed]`,
  // `[reconnected]` and friends used to be written) into
  // attach-terminal-status.tsx's persistent strip below the grid, so a spent
  // reconnect budget always offers a way back in instead of scrolling away.
  RECONNECT: "Reconnect",
  RECONNECTING_LINE: (attempt: number, maxAttempts: number) =>
    `Reconnecting — attempt ${attempt} of ${maxAttempts}.`,
  RECONNECTING_HINT: "Keystrokes are held until the terminal is back.",
  CLOSED_TITLE: "The terminal disconnected.",
  CLOSED_BODY:
    "Wardyn stopped retrying after 4 attempts. This ends the terminal session only — the run itself is unaffected. Reconnect to watch it again.",
} as const;
