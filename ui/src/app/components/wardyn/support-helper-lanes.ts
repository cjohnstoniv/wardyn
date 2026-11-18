/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Which doors of an open support grant each run-page lane needs (board M5c). The server admits a helper through
// the doors the owner ticked (internal/api/support_entry.go); the console draws a lane only for a door that is open
// and says in one line, with no button, where one is closed.
import type { SupportDoor } from "../../lib/api/support";
import { SUPPORT_DOORS } from "./copy/support";

/** The SSH lane's four doors: the connection opens for any of them and each channel needs its own. */
export const SSH_LANE_DOORS: readonly SupportDoor[] = ["terminal", "commands", "files", "forward"];

/** The Desktop tile's lanes: Open desktop = desktop, the Files panel = files. */
export const DESKTOP_TILE_DOORS: readonly SupportDoor[] = ["desktop", "files"];

/** The Attach tile's lanes: SSH (four doors), Sync (files), UI apps (web_apps). */
export const ATTACH_TILE_DOORS: readonly SupportDoor[] = [...SSH_LANE_DOORS, "web_apps"];

/** A helper's open doors, or null for anyone who is not a helper of an open grant (the viewer sees what they always did). */
export type HelperDoors = readonly SupportDoor[] | null | undefined;

export const hasAnyDoor = (doors: readonly SupportDoor[], wanted: readonly SupportDoor[]): boolean => wanted.some((d) => doors.includes(d));

/** The wanted doors the grant does not carry, in the banner's order. */
export const missingDoors = (doors: readonly SupportDoor[], wanted: readonly SupportDoor[]): SupportDoor[] =>
  SUPPORT_DOORS.filter((d) => wanted.includes(d) && !doors.includes(d));
