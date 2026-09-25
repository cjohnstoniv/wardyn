/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// R4-F144, WCAG 2.1.2: the cockpit terminal takes Tab, Shift+Tab and Escape
// into the PTY, so a keyboard user needs an advertised way out and 2.1.2
// requires it be advised ON ENTRY.
//
// Chord: Ctrl+Shift+Backspace (#133) — the earlier Ctrl+] never fired on
// DE/FR/ES layouts, where `]` needs AltGr and AltGr arrives at the browser as
// ctrlKey && altKey (which the binding must ignore, or AltGr+9 could not type
// a bracket) — so those users had no way out at all. Backspace has no
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
  // Canon: docs/design/terminal-escape-canon.md (#133).
  ESCAPE_CHORD_HINT: `${ESCAPE_CHORD} leaves the terminal`,

  // #216 — connection state moves OUT of xterm's own scrollback (where `[closed]`,
  // `[reconnected]` and friends used to be written) into
  // attach-terminal-status.tsx's persistent strip below the grid, so a spent
  // reconnect budget always offers a way back in instead of scrolling away.
  RECONNECT: "Reconnect",
  RECONNECTING_LINE: (attempt: number, maxAttempts: number) =>
    `Reconnecting — attempt ${attempt} of ${maxAttempts}.`,
  // #510-F2 — no input buffer exists (attach-terminal.tsx's send() drops
  // anything typed while the socket isn't OPEN), so the hint must say that
  // rather than promise a queue that was never built.
  RECONNECTING_HINT: "Keystrokes typed now are not sent.",
  CLOSED_TITLE: "The terminal disconnected.",
  // #510-F2 — interpolates the live reconnect budget instead of hardcoding
  // it, so this stays true if MAX_RECONNECT_ATTEMPTS ever moves.
  CLOSED_BODY: (maxAttempts: number) =>
    `Wardyn stopped retrying after ${maxAttempts} attempts. This ends the terminal session only — the run itself is unaffected. Reconnect to watch it again.`,
} as const;
