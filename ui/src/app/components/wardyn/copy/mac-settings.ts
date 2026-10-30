/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The admin Settings field for the oldest macOS base (board J7, state "Admin · Settings field").
// P17 and P18 are byte for byte from the copy sheet. TITLE and UNIT are on the approved board but have no
// sheet id; SAVED_TOAST and INVALID are new and have neither (out for copy approval). The Save button is FORM_ACTIONS.SAVE.
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
  /** NEW, no sheet id */
  SAVED_TOAST: "Oldest macOS base saved.",
  /** NEW, no sheet id: the only values the server accepts that the field can hold are whole days, 1 or more */
  INVALID: "Enter a whole number of days, 1 or more, or leave it empty.",
} as const;
