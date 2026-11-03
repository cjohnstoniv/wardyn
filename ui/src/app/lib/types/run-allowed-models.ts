/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The hosted models a run's proxy holds every model request to, and the governance profile that set them, as
// written at dispatch (GET /runs/{id}): a later change to the profile does not reach the run. RunDetail carries
// it as `allowed_models`, absent when no limit binds the run.
export interface RunAllowedModels {
  models: string[];
  unit: { id: string; name: string };
}
