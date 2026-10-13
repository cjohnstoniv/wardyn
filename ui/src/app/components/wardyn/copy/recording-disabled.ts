/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Recording, deduplicated: the same two facts were spelled three times
// (recording.tsx's local consts, run-detail.tsx's inline EmptyState literals)
// and had already drifted — "will ever produce one" vs "captures one". One
// spelling.
export const RECORDING_DISABLED_TITLE = "Session recording is disabled on this deployment";
// The switch is WARDYN_RECORDING_STORE, not WARDYN_RECORDING_DIR: the DIR only
// moves the `fs` store's path and turns nothing on (docs/ENV.md), while the
// chart renders STORE=off whenever persistence.enabled=false.
export const RECORDING_DISABLED_DESC =
  "No run on this server will ever produce one — set persistence.enabled (Helm) or WARDYN_RECORDING_STORE=pg to turn it on.";
