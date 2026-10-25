/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// internal/runnerwire/payload.go's MacVMCaps, as a Mac runner reports it. The organisation does not send
// it to the console yet (RunnerView has no field for it), so the macOS runner views take it as a prop.

/** Go: runnerwire.MacVMBase.State. */
export type MacVMBaseState = "none" | "preparing" | "ready" | "update_needed" | "failed";

/** Go: runnerwire.MacVMBase. */
export interface MacVMBase {
  state: MacVMBaseState;
  guest_os?: string;
  /** Percent done while it prepares. */
  progress?: number;
  error?: string;
}

/** Go: runnerwire.MacVMCaps.Desktop. */
export type MacVMDesktop = "control" | "observe" | "none";

/** Go: runnerwire.MacVMCaps. */
export interface MacVMCaps {
  /** The runner's allowance: its own live macOS VM runs plus the host's free headroom. */
  max: number;
  min_cpus?: number;
  min_memory_mib?: number;
  harnesses?: string[];
  desktop?: MacVMDesktop;
  sign_ins?: boolean;
  base: MacVMBase;
  /** In words, why the runner offers no macOS VM; absent when it offers them. */
  refusal?: string;
}
