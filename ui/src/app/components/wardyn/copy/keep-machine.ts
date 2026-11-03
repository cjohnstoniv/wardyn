/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Kept macOS VM machines, character for character as the approved boards draw them; the id in each comment is the board's.

/** KM-6: the governance profile's deny_kept_machines. */
export const KEEP_POLICY = {
  /** K-80 */
  LABEL: "Deny kept machines",
  /** K-81 */
  HINT: "Stops people keeping a macOS VM between runs or resuming one. Machines already kept stay on their Macs until deleted or expired. Has no effect while kept machines are off for this install.",
  /** K-82 */
  CHIP: "Can't keep machines",
} as const;
