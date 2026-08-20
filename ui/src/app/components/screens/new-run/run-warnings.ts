/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { toast } from "sonner";
import type { CreateRunResult } from "../../../lib/types";

// surfaceRunWarnings — after a run is created, POST /runs may return an advisory
// `warnings[]` (e.g. the run's workspace directory collides with another active
// run's). The run STILL launched, so we never block on these — we raise one
// non-blocking sonner warning toast per message. Shared by every create path
// (New Run + the composer review) so the behaviour is identical.
export function surfaceRunWarnings(created: CreateRunResult): void {
  const warnings = created.warnings ?? [];
  for (const w of warnings) {
    toast.warning("Run launched with a warning", { description: w });
  }
}
