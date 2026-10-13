/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { RunIntent } from "../draft/run-intent";

/**
 * Sets the lens's own keys on a copy of the intent, and removes those given as
 * undefined. Every other key, including document-only ones, is kept as it was.
 */
export function withKeys(intent: RunIntent, patch: Partial<RunIntent>): RunIntent {
  const next: Record<string, unknown> = { ...intent };
  for (const [key, value] of Object.entries(patch)) {
    if (value === undefined) delete next[key];
    else next[key] = value;
  }
  return next as RunIntent;
}

/** The ids of the run's harnesses, in order. */
export const harnessIds = (intent: RunIntent): string[] =>
  (intent.tools ?? []).filter((t) => t.kind === "harness").map((t) => t.id);
