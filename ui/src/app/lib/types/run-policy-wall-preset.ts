/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// RunPolicyView.wall_preset (internal/api/run_policy_view.go runPolicyWallPreset): the wall preset
// the run was launched with and what it changed. A personal preset read by anyone but the run's owner
// carries its scope alone. The changes it made carry cause "wall_preset".
export interface RunPolicyWallPreset {
  id?: string;
  name?: string;
  scope: string;
  group_id?: string;
  version?: number;
  // The knobs it narrowed.
  narrowed?: string[];
  // The list entries it took out or added, keyed by field.
  removed?: Record<string, string[]>;
  added?: Record<string, string[]>;
}
