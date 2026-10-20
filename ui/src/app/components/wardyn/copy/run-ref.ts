/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #3036 (R6RUN, approved 2026-10-20): a run is named by its title, with a short id after it that copies.
export const RUN_REF = {
  CLICK_TO_COPY: "Click to copy",
  COPIED: "Copied",
  /** The lead word when the page does not know the run's title: the id stands alone after it. */
  ID_ONLY: "Run",
  /** The kill question when no title is known; it names the short id, never the long one. */
  killRun: (shortId: string) => `Kill run ${shortId}?`,
} as const;
