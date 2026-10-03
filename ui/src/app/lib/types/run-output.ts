/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// GET /runs/{id}/output — mirrors runOutputResponse (internal/api/run_output.go).
export interface RunOutput {
  output: string;
  /** The output does not start at the run's first byte. */
  truncated: boolean;
  /** A final capture; false while the run's output is still being kept. */
  complete: boolean;
  /** "stdout" (command output) or "pane_snapshot" (an interactive run's last screen). */
  source: string;
  incomplete: boolean;
  capture_gap: boolean;
  /** "run": masked against the run's complete manifest; "globals_only": not. */
  mask_scope?: string;
  captured_at?: string;
}
