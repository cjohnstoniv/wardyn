/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The admin Settings fields of the macOS virtual machines card: the oldest macOS base (board J7, state "Admin ·
// Settings field"), the background-watch switch and the window switch (board WIN-2). P17 and P18 are byte for byte from the
// copy sheet. TITLE and UNIT are on the approved board but have no sheet id; INVALID is new and has neither (out for
// copy approval). The Save button is FORM_ACTIONS.SAVE.
// The load, save-failed, retry and changed-elsewhere sentences are the Custom components card's (COMPONENTS_ADMIN).

export const MAC_SETTINGS = {
  /** the card's title, board J7 (no sheet id) */
  TITLE: "macOS virtual machines",
  /** MAC-P17, field label */
  P17: "Oldest macOS base",
  /** MAC-P18, field hint */
  P18: "Runs are refused on a Mac whose base was prepared longer ago than this. Empty: no maximum.",
  /** the unit beside the number, board J7 (no sheet id) */
  UNIT: "days",
  /** the card's one Save covers every field on it */
  SAVED_TOAST: "macOS settings saved.",
  /** the site switch's label (absent reads allowed) */
  WATCH_LABEL: "Allow watching background desktops",
  WATCH_HINT:
    "On by default. The person who started a background macOS run can watch its desktop, view only, unless a governance profile denies it. A Mac that allows desktop control is never watched. Turn this off to stop all watching on this install. A watch that is open ends within a minute.",
  /** W2-09: the window switch's label (absent reads off) */
  WINDOW_LABEL: "Allow a window on the Mac for macOS VM runs",
  /** W2-10 */
  WINDOW_HINT:
    "Off by default. When on, the person who started a macOS VM run on a Mac they registered can open a window on that Mac and type in it, unless a governance profile or the Screen and actions recording level forbids it. A window is not recorded. A Mac that isn't theirs never gets one. Turn this off to stop new windows; a window that is open ends within a minute.",
  /** NEW, no sheet id: the only values the server accepts that the field can hold are whole days, 1 or more */
  INVALID: "Enter a whole number of days, 1 or more, or leave it empty.",
} as const;
