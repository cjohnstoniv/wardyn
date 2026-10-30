/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Drives on a Mac (board J4, rc.9), byte for byte from the copy sheet; the id in each comment is the sheet's.
// The refusals and the delete outcomes (MAC-D04, D05, D07, D10 to D13, D17, D18) are the server's sentences:
// lib/drive-refusals.ts pins the ones the console words itself, and the rest show as the server sent them.

export const MAC_DRIVES = {
  /** MAC-D01, the Stored on value of a disk drive */
  D01: (mac: string) => `Disk on ${mac}`,
  /** MAC-D02, the Create user drive intro on a macOS run */
  D02: (mac: string) => `The drive is a disk kept on ${mac}. Only macOS VM runs on ${mac} can use it, one run at a time.`,
  /** MAC-D03, a Docker or cluster volume in the picker of a macOS run */
  D03: "Not available in a macOS VM.",
  /** MAC-D06, what is kept on a macOS run */
  D06: (mac: string) => `Keeps the work folder, /Users/agent/work, as your own drive on ${mac}, named after this run. Your home folder, harness settings and sign-ins are not kept.`,
  /** MAC-D08, the note under the list on a macOS run */
  D08: "On a Mac, a drive keeps only its own folder. Apps you install onto the drive survive; keychain sign-ins don't.",
  /** MAC-D09, the admin list's Mac column */
  D09: "Mac",
  /** MAC-D19, the Main drive card's description on a macOS run */
  D19: "The VM's own disk: macOS, your tools and /Users/agent. Every run has one.",
  /** MAC-D14, the keep control's label on a macOS run */
  D14: "Keep the work folder",
  /** MAC-D15, the admin list's storage-type column */
  D15: "Storage",
  /** MAC-D16, the storage value of a disk kept on a Mac */
  D16: "Mac disk",
} as const;
