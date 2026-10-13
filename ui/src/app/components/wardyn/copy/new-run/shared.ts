/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New Run canon strings for strings several screens share, byte for byte from docs/design/new-run-canon.md.
// Lazy-only: imported by the New Run flow and never re-exported through copy.ts, whose barrel is in the eager
// entry chunk. new-run-copy.test.ts keeps this file and the doc equal in both directions.

export const NR_SHARED = {
  COMING_SOON: "Coming soon",
  REQUIRED: "Required",
  OPTIONAL: "Optional",
  BACK: "Back",
  CONTINUE: "Continue",
  CANCEL: "Cancel",
  REMOVE: "Remove",
  CLOSE_DIALOG: "Close dialog",
  EYEBROW: (n: string | number) => `Guided setup · ${n} of 5`,
  PROGRESS_ARIA: "Setup progress",
  STEP_SHAPE: "Shape",
  STEP_RUN: "Run",
  STEP_HARNESSES: "Harnesses",
  STEP_FILES: "Files",
  STEP_TOOLS: "Tools and access",
  CONTINUE_TO: (step: string | number) => `Continue to ${String(step).toLowerCase()}`,
  REVIEW_RUN: "Review run",
  BARRIER_FENCE: "Fence",
  BARRIER_WALL: "Wall",
  BARRIER_VAULT: "Vault",
  GIB: (n: string | number) => `${n} GiB`,
  MIB: (n: string | number) => `${n} MiB`,
  CPU: (n: string | number) => `${n} CPU`,
  HOURS: (n: string | number) => `${n} hours`,
  HOUR_ONE: "1 hour",
  JOIN: (a: string | number, b: string | number) => `${a} · ${b}`,
} as const;
