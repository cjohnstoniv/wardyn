/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// GET /api/v1/scim/status — mirrors internal/api scimStatus (scim_status.go). Names, steps and errors
// only; the read is securityOps and never carries a credential.
export interface SCIMDeactivated {
  person: string;
  deactivated_at: string;
  purge_after?: string; // absent when no purge is scheduled
}

export interface SCIMPending {
  person: string;
  step: string; // a ledger step key, e.g. "kill_run"
  last_error: string;
}

export interface SCIMDrive {
  person: string;
  drive: string;
  purged_at: string;
}

export interface SCIMStatus {
  configured: boolean; // false: every other field is empty
  last_token_slot: "primary" | "next" | "";
  purge_after_seconds: number; // 0: only a delete from the identity provider purges
  keep_workspaces: boolean;
  deactivated: SCIMDeactivated[];
  pending: SCIMPending[];
  drives: SCIMDrive[];
}
