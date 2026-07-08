/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Client-side UI feature flags. These HIDE surfaces without deleting the code or
// the backend behind them — flip to re-enable.

// COMPOSER_UI_ENABLED gates the AI Run Composer surfaces: the "Describe your task"
// entry mode in New Run and the "Diagnose with AI" affordance in the import Verify
// panel. Off for now — the record → saved-profile → rerun flow covers the need; the
// composer (backend + code) stays intact for a later follow-up. Set true to restore.
export const COMPOSER_UI_ENABLED = false;
