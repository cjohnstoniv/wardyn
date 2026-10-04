/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// M10 (approved 2026-10-03) — the shell band for the /setup/status
// substrate_health row. The body is the row's own `detail`, shown verbatim, so
// only the title (by the row's `cause`) and the action live here.
export const SUBSTRATE_BANNER = {
  TITLE_UNREACHABLE: "The sandbox runner isn't answering",
  TITLE_AUTH: "The sandbox runner is refusing Wardyn",
  TITLE_SWEEP: "A background sweep has stopped",
  ACTION: "Open setup checks",
} as const;

export const SUBSTRATE_BANNER_STEP = "/admin/setup?step=review";
