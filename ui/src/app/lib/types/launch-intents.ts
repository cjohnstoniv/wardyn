/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The launch-intent wire contract (types only), for Run again. Each interface
// mirrors one Go struct in internal/api/launch_intent.go; TestLaunchIntentWireTwin
// pins the keys. A launch intent is the setup a run was launched with, kept as a
// kind Run document, and only the run's creator reads it.

import type { RunShape } from "./runs";
import type { TemplateDocument } from "./templates";

/** GET /me/launch-intents: one run that kept its setup. */
export interface LaunchIntentSummary {
  run_id: string;
  title: string;
  shape?: RunShape;
  created_at: string;
}

/** GET /me/launch-intents?limit= (default 20, at most 50), newest first. */
export interface LaunchIntentList {
  intents: LaunchIntentSummary[];
}

/** GET /runs/{id}/launch-intent: the kept setup, a document of kind Run. */
export interface LaunchIntent {
  run_id: string;
  created_at: string;
  document: TemplateDocument;
}

/** The 404 for a run that kept no setup, or one the caller cannot see. */
export const LAUNCH_INTENT_NOT_FOUND = "launch_intent_not_found";
