/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The admin Settings card for preview URLs (`features.preview_urls` in site config), character for character as the
// approved frames draw it. The load, retry and save-failed sentences are the Custom components card's
// (COMPONENTS_ADMIN); the server's refusal of a save is shown as it came.

export const PREVIEW_URLS = {
  TITLE: "Preview URLs",
  LABEL: "Allow preview URLs",
  HINT: "Let people share a port a run is listening on as a preview URL. Each one opens in its own tab, on a different address than this console.",
  CEILING_LABEL: "Widest a preview can be",
  CEILING_PRIVATE: "Only the run's owner can open a preview.",
  CEILING_ORG: "Anyone signed in to this console can open it. Needs per-run hosts.",
  HONEST: "Turning previews off revokes them all; lowering the ceiling revokes the previews above it. Open connections end within a minute.",
  SAVED_TOAST: "Preview URLs setting saved.",
} as const;
