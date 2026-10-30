/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Several macOS bases on a Mac (board J6, rc.9), byte for byte from the copy sheet; the id in each comment is the sheet's.
// B04 is the base's wire name too ("Standard"). The create and preflight refusals B08 and B09 are the
// server's sentences: they show as the server sent them, never from here.

export const MAC_BASES = {
  /** MAC-B01, New Run picker label */
  B01: "macOS base",
  /** MAC-B02, New Run picker help */
  B02: "Each Mac's operator sets up its bases. Your organisation decides which you can pick.",
  /** MAC-B03, an option's meta line; a part the wire did not send is left out with its dot */
  B03: (guestOS?: string, preparedOn?: string, harnesses?: string) =>
    [guestOS, preparedOn && `prepared ${preparedOn}`, harnesses].filter(Boolean).join(" · "),
  /** MAC-B04, the name of a Mac's first base */
  B04: "Standard",
  /** MAC-B05, option off: governance (the same form as NR_FILES.CREATE_DENIED) */
  B05: (profile: string) => `Not allowed by ${profile}.`,
  /** MAC-B06, option off: not on every Mac in the pool */
  B06: "Not on every Mac in this pool.",
  /** MAC-B07, option off: being rebuilt */
  B07: (percent: number) => `Being rebuilt from Standard (${percent}%).`,
  /** MAC-B10, the drawer's list label */
  B10: "Bases",
  /** MAC-B11, a derived base's line; `plus 1 item` for one, a NEW singular the sheet has no line for */
  B11: (items: number) => `From Standard, plus ${items} ${items === 1 ? "item" : "items"}`,
  /** MAC-B12, chip: a derived base being rebuilt */
  B12: "Rebuilding",
  /** MAC-B13, the hint under the list; `slots` is the Mac's VM allowance */
  B13: (slots: number) => `When Standard changes, each other base is rebuilt from it, one at a time, using one of the ${slots} VM slots.`,
  /** MAC-B14, the governance profile editor's field label */
  B14: "macOS bases",
  /** MAC-B15, the governance profile editor's field hint */
  B15: "Which macOS bases people under this profile may pick. Empty: any base a Mac offers.",
  /** MAC-B16, Review's Where it runs detail */
  B16: (base: string) => `macOS base · ${base}`,
} as const;
