// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Where a run's current proxy start's policy readback stands (Go: types.RunPolicyReadback,
// internal/types/policy_readback.go). enforced: the proxy reported the hash of the bytes its driver
// handed it; unconfirmed: an older runner or proxy image that cannot report; failed: the proxy was stopped.
export type ProxyStartState = "pending" | "enforced" | "unconfirmed" | "failed" | "abandoned" | "superseded";

export interface RunPolicyReadback {
  state: ProxyStartState;
  generation: number;
  confirmed_at?: string;
}
