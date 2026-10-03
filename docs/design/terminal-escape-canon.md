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

## Copy strings

M11, approved 2026-10-03 (term-t3b). Source:
`ui/src/app/components/wardyn/copy/terminal.ts`, `TERMINAL_COPY`. `(mac)` and
`(pc)` name the platform: macOS reads Option and Cmd, every other platform
Shift and Ctrl. Counts take the singular when `n` is 1 (`1 character`,
`1 line break`, `1 invisible character`).

| Constant | Value |
|---|---|
| `NATIVE_CHORD(pc)` | `Shift+drag` |
| `NATIVE_CHORD(mac)` | `Option+drag` |
| `SELECT_HINT(chord)` | `{chord} selects for right-click Copy` |
| `OFFER_TITLE` | `Copy selection` |
| `OFFER_SIZE(n)` | `{n} characters` |
| `OFFER_BREAKS(n)` | `{n} line breaks` |
| `OFFER_INVISIBLE(n)` | `{n} invisible characters` |
| `OFFER_EXPIRES(s)` | `Closes in {s}s` |
| `OFFER_KEYS(pc)` | `Ctrl+C copies` |
| `OFFER_KEYS(mac)` | `Cmd+C copies` |
| `COPY` | `Copy` |
| `DISMISS` | `Dismiss` |
| `COPIED` | `Copied` |
| `WRITE_FAILED` | `The browser refused the clipboard write. Nothing was copied.` |
| `BLOCKED` | `Copy blocked. The terminal sent different text than you selected, so nothing was copied.` |

Render sites, all in `ui/src/app/components/`: the selection hint in the
`attach-terminal.tsx` title bar (from `lg` up; always in the grid's
`aria-description`), and, in the blocked notice, `SELECT_HINT` with a full
stop as its second line; the offer card and the blocked notice in
`attach-terminal-copy-offer.tsx`. `COPIED` is the success toast after Copy.

Preview glyphs (D2) are literals: `↵` at a line end, `→` for a tab, and
`⟨U+XXXX⟩` in the warning colour for each control, format (Unicode Cf) or
U+FEFF code point. The text is never truncated.

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

## Terminal renderer strings

M11, approved 2026-10-03 (term-t10). Source:
`ui/src/app/components/wardyn/copy/terminal.ts`, `TERMINAL_RENDERER`. The
renderer menu (`MonitorCog`, before Redraw) and the fell-back strip render in
`attach-terminal-renderer-menu.tsx`. The choice is stored in this browser only.

| Constant | Value |
|---|---|
| `LABEL` | `Terminal renderer` |
| `AUTO` | `Auto` |
| `AUTO_HINT` | `GPU when this browser supports it, otherwise Compatible.` |
| `GPU` | `GPU` |
| `GPU_HINT` | `Faster with heavy output. Falls back to Compatible if the GPU stops.` |
| `GPU_UNAVAILABLE` | `Not available in this browser.` |
| `COMPATIBLE` | `Compatible` |
| `COMPATIBLE_HINT` | `Draws with the page. Use it if text looks wrong or the terminal goes blank.` |
| `FOOTER(active)` | `In use: {active}. Saved in this browser only.` |
| `FELL_BACK` | `The GPU renderer stopped, so this terminal switched to Compatible. The session is unaffected.` |
