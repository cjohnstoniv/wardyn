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
// (the manual wizard + the composer review) so the behaviour is identical.
export function surfaceRunWarnings(created: CreateRunResult): void {
  const warnings = created.warnings ?? [];
  for (const w of warnings) {
    toast.warning("Run launched with a warning", { description: w });
  }
}

// parseMissingSecret — extract the secret name from a create-run failure that
// named a not-yet-stored secret (inline_policy.go's validateInlineSecretRefs:
// `... unknown secret "name" ...`, now also hit by the stored/default policy
// path — H1). Shared by the composer review panel (compose-review.tsx) and the
// manual wizard (wizard.tsx) so both launch paths offer the same one-click
// "add it and retry" fix instead of each re-deriving the name from the string.
export function parseMissingSecret(launchError?: string | null): string | null {
  return launchError?.match(/unknown secret "([^"]+)"/)?.[1] ?? null;
}
