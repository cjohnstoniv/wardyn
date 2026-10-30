/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The admin Drives screen's list of people's own drives (board J4, "Admin view → User drives"). The column labels
// Storage (MAC-D15), Mac (MAC-D09) and the value Mac disk (MAC-D16) are the copy sheet's; Drive, Owner, Size and
// Docker/Cluster volume are read from the modules that already hold them. What is here has no sheet id: the section
// title, the two states and the dash are on the approved board; the paging lines are new.

export const MEMBER_DRIVES = {
  /** NEW, no sheet id: the section's title */
  TITLE: "People's drives",
  /** board J4 (no sheet id): a drive with nothing in flight */
  STATE_READY: "Ready",
  /** board J4 (no sheet id): a delete is in flight (a disk drive whose Mac is offline stays here until it returns) */
  STATE_DELETING: "Deleting",
  /** board J4 and the MAC-D15 note: where the runner reports no size, or the drive has no Mac */
  NONE: "—",
  /** NEW, no sheet id */
  SHOWING: (n: number, total: number) => `Showing ${n} of ${total}.`,
  /** NEW, no sheet id */
  SHOW_MORE: "Show more",
} as const;
