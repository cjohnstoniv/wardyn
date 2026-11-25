/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The admin Settings card for the desktop watermark rule (`desktop_watermark.required` in site config), character for
// character as the approved frames draw it. The viewer's and the replay's own strings are DESKTOP.watermark* (desktop.ts);
// the load, retry, save-failed and changed-elsewhere sentences are the Custom components card's (COMPONENTS_ADMIN).

export const DESKTOP_WATERMARK = {
  TITLE: "Desktop watermark",
  LABEL: "Require a watermark on desktop views",
  HINT: "Desktop views in the console show the viewer's email, or their sign-in name when there is none, and the run id. When it isn't required, each person can turn it on for their own view. It discourages photos and screen captures of the live desktop and replay. Downloaded recordings aren't marked.",
  HONEST: "It helps trace a capture. It can't stop one.",
  SAVED_TOAST: "Desktop watermark setting saved.",
} as const;
