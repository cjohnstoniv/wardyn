/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { toast } from "sonner";
import type { CreateRunResult } from "../../../lib/types";

// surfaceRunWarnings — after a run is created, POST /runs may return an advisory
// `warnings[]` (the run's workspace directory collides with another active
// run's; or, under an enforced capability, the hosts/secrets the member typed
// that were dropped before launch — internal/api/runs.go). The run STILL
// launched, so we never block on these — we raise one non-blocking sonner
// warning toast per message. The server composes the text, naming the kind and
// the exact value that went; this side never rewords it, or the console would
// tell a member a different story than the audit row does. Shared by every
// create path (New Run + the composer review) so the behaviour is identical.
export function surfaceRunWarnings(created: CreateRunResult): void {
  const warnings = created.warnings ?? [];
  for (const w of warnings) {
    toast.warning("Run launched with a warning", { description: w });
  }
}
