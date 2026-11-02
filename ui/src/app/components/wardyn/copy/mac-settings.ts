/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The admin Settings fields of the macOS virtual machines card: the oldest macOS base (board J7, state "Admin ·
// Settings field") and the background-watch and direct mount switches (field-report board F10). P17 and P18 are byte for byte from the
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
  /** RF-17, board F10: the card's one Save covers every field on it */
  SAVED_TOAST: "macOS settings saved.",
  /** RF-12, board F10: the site switch's label (absent reads allowed) */
  WATCH_LABEL: "Allow watching background runs",
  /** RF-13 */
  WATCH_HINT:
    "On by default. A person can watch a background macOS run's desktop, view only, unless a governance profile denies it. Turn this off to stop all watching on this install. A watch that is open ends within a minute.",
  /** RF-14, board F10: the direct mount switch's label (off when absent) */
  DIRECT_LABEL: "Allow mounting a drive directly on the Mac",
  /** RF-15 */
  DIRECT_HINT:
    "Off by default. When on, a person can mount a run's disk drive on their own Mac with drive open --direct. The Mac's own system then reads a disk an agent wrote, which content review can't cover.",
  /** RF-16, shown whether the switch is on or off */
  DIRECT_LIMIT:
    "Mounting leaves no row in your organisation's audit log. This setting can't stop anyone who can run hdiutil on a Mac from mounting the image themselves.",
  /** NEW, no sheet id: the only values the server accepts that the field can hold are whole days, 1 or more */
  INVALID: "Enter a whole number of days, 1 or more, or leave it empty.",
} as const;
