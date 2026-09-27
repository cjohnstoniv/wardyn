<!--
Copyright 2025 The Wardyn Authors
SPDX-License-Identifier: Apache-2.0
-->

# Terminal escape chord — canon (#133)

R4-F144 (WCAG 2.1.2, No Keyboard Trap): the cockpit terminal takes Tab,
Shift+Tab and Escape into the PTY, so a keyboard user needs an advertised way
out of the panel, and 2.1.2 requires that exit be advised on entry.

## Strings

Source: `ui/src/app/components/wardyn/copy/terminal.ts`.

| Constant | Value |
|---|---|
| `ESCAPE_CHORD` | `Ctrl+Shift+Backspace` |
| `ESCAPE_CHORD_HINT` | `Ctrl+Shift+Backspace leaves the terminal` (composed from `ESCAPE_CHORD` — one spelling reaches every render site) |
| `RECONNECTING_LINE(n, max)` | `Reconnecting — attempt {n} of {max}.` (unchanged by #133) |
| `RECONNECTING_HINT` | `Keystrokes typed now are not sent.` (owner ruling 2026-09-25, #726: canon follows the app — #510-F2 found no input buffer exists, so the earlier "held" promise was false) |

Render sites: the title-bar strip and the grid's `aria-description`, both in
`ui/src/app/components/attach-terminal.tsx`.

## Decisions

**Q133-1 — the advertised chord is Ctrl+Shift+Backspace.**
The prior chord, Ctrl+], never fired on DE/FR/ES keyboard layouts: `]` is a
level-2 (AltGr) character there, AltGr arrives at the browser as
`ctrlKey && altKey`, and the binding (correctly) ignores a keystroke with
`altKey` held — so those users had no working exit from the terminal, and the
WCAG 2.1.2 keyboard trap stood for them. Ctrl+Shift+Esc (the originally filed proposal) was ruled
out earlier because Windows intercepts it at OS level (Task Manager) before
the browser ever sees it. Backspace has no AltGr shape on any layout this
widget ships to, so Ctrl+Shift+Backspace is typeable everywhere and collides
with nothing the terminal itself binds.

**Q133-2 — Ctrl+] is kept, silently, and never advertised.**
Existing US-layout muscle memory keeps working: Ctrl+] still escapes the
terminal. It is not mentioned in any hint, title bar, doc, or aria text — the
one advertised chord is Ctrl+Shift+Backspace, so there is only ever one
sentence to get right (2.1.2 is satisfied by an exit that works, not by a
sentence about one). Ctrl+] must NOT fire when `altKey` is also held: that
combination is AltGr typing a bracket on DE/FR/ES, not a user invoking the
chord, and has to reach the PTY.

## Implementation

The keydown decision is a pure function, `decideKey` in
`ui/src/app/components/attach-terminal-keys.ts`, table-tested per keyboard
layout "shape" in `attach-terminal-keys.test.ts` (US/DE/FR/ES). It returns
one of `"escape" | "newline" | "paste" | "pty"`; `attach-terminal.tsx` only
wires the result into xterm's `attachCustomKeyEventHandler`.
