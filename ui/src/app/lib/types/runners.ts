/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The runners management wire (types only): the inventory the admin Runners page and the person's
// own runner card read, the unused registration tokens, and the runners switch. Every interface
// mirrors one Go struct and runners.wire.test.ts pins its json tags to that struct's source. Every
// posture fact is what the runner said about itself; none of it is verified.

/** Go: types.RunnerState. */
export type RunnerState = "unclaimed" | "claimed" | "revoked";

/** Go: types.RunnerPosture. */
export interface RunnerPosture {
  mdm_managed: boolean;
  disk_encrypted: boolean;
  os: string;
  os_version: string;
}

/**
 * Go: types.RunnerView. `owner` is present on the administrators' routes only. On a person's own
 * routes an unclaimed runner's `key_fingerprint` is abbreviated (`key_fingerprint_abbreviated`), so
 * it is never a value to copy into the claim.
 */
export interface RunnerView {
  id: string;
  name: string;
  state: RunnerState;
  owner?: string;
  minted_by?: string;
  minted_by_email?: string;
  online: boolean;
  key_fingerprint: string;
  key_fingerprint_abbreviated: boolean;
  version?: string;
  created_at: string;
  claim_expires_at?: string;
  claimed_at?: string;
  last_seen_at?: string;
  revoked_at?: string;
  posture?: RunnerPosture;
  posture_reported_at?: string;
  posture_source?: string;
  runs_active: number;
}

/** Go: types.RunnerRegistrationToken. The raw `token` is returned once, at mint; a list never carries it. */
export interface RunnerRegistrationToken {
  id: string;
  owner: string;
  minted_by: string;
  token?: string;
  created_at: string;
  expires_at: string;
  consumed_at?: string;
}

/** Go: types.RunnerSettingsRequest. `enabled` is required; the answer is site.ts's RunnerSettings. */
export interface RunnerSettingsRequest {
  enabled: boolean;
}

/** `GET /runners?state=`; the default is active. */
export type RunnerFilter = "active" | "revoked" | "all";

/** The refusals the runners management routes add to the shared reason set (internal/api/reasons_routes.go). */
export type RunnerManagementReason =
  | "runner_not_found"
  | "runner_token_not_found"
  | "runner_filter_invalid"
  | "runner_session_required"
  | "runners_org_url_invalid"
  | "runners_enabled_required";
