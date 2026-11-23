/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The admin Drives screen's list of people's own drives (board J4, "Admin view → User drives"). The column labels
// Storage (MAC-D15), Mac (MAC-D09) and the value Mac disk (MAC-D16) are the copy sheet's; Drive, Owner, Size and
// Docker/Cluster volume are read from the modules that already hold them. What is here has no sheet id: the section
// title, the two states and the dash are on the approved board; the paging lines are new.

import { DRIVES } from "../../../lib/user-drives-copy";

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
  /** NEW, no sheet id: the delete confirm for a disk kept on a Mac, which may not remove the disk */
  DELETE_CONFIRM_MAC: (name: string) =>
    `Delete ${name}? Its disk is deleted on its Mac and can't be recovered. If the Mac is offline, the disk is deleted when it reconnects. If the Mac is no longer registered, the disk stays on it and only this record is removed.`,
  /** NEW, no sheet id: after a delete that removed only the record, when the runner list no longer names the Mac */
  RECORD_ONLY_UNNAMED:
    "This drive's Mac is no longer registered, so its disk stays on that Mac. Deleting removes only the record.",
  /** The per-row write switch's name: the column name, then the drive and its owner (names are unique per owner only). */
  TEAM_WRITE_SWITCH: (drive: string, owner: string) => `${DRIVES.FIELD_TEAM_WRITE}: ${drive} (${owner})`,
  TEAM_WRITE_ALLOW_TITLE: (drive: string) => `Let members write to ${drive}?`,
  TEAM_WRITE_ALLOW_BODY: (unit: string) =>
    `Once it's on, any run started by someone in ${unit}, or in a unit below it, can change what the others' runs read and run. Runs that were created before this keep the access they had.`,
  /** The allow body's unit when the list gives no name for it. */
  TEAM_WRITE_ITS_UNIT: "its unit",
  TEAM_WRITE_ALLOW: "Allow writing",
  TEAM_WRITE_STOP: "Make read-only",
  /** The word beside the write switch, as the board draws it. Hidden from assistive tech: the switch says it. */
  TEAM_WRITE_ON: "On",
  TEAM_WRITE_OFF: "Off",
  /** The write-bit confirm, turning members' writing off. The maker's own runs keep writing (#3459). */
  TEAM_WRITE_STOP_TITLE: (drive: string) => `Make ${drive} read-only for members?`,
  TEAM_WRITE_STOP_BODY: (owner: string) =>
    `Runs that others start after this can read the drive and never change it. ${owner}'s runs can still write. Runs that were created before this keep the access they had.`,
} as const;
