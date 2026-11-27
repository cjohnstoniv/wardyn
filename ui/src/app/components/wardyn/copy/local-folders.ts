/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The run's walls for folders from a person's computer (local_folders, #2990): the Policy tab's summary lines and
// the folder rows' unreadable line. The server's own sentences (a walled pass back, a sync session ended at its size
// limit) are printed as sent and never defined here. Sizes come in the console's byte form (fmtBytes).
export const LOCAL_FOLDERS = {
  DENY_REMOTE: "Not allowed for a run on another machine",
  DENY_PULL: "Changes in the sandbox don't sync back to your computer",
  MAX_PUSH: (size: string) => `Sends up to ${size} per session`,
  MAX_PULL: (size: string) => `Brings back up to ${size} per session`,
  DENY: "Skipped by wardyn sync and the runner",
  // Inside RUN_COMPUTER_FOLDER.BACK_FAILED's frame.
  WALLS_UNREADABLE: "the run's folder walls could not be read",
};
