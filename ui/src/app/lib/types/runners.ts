/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The runners management wire (types only): the inventory the admin Runners page and the person's
// own runner card read, the unused registration tokens, and the runners switch. Every interface
// mirrors one Go struct and runners.wire.test.ts pins its json tags to that struct's source. Every
// posture fact is what the runner said about itself; none of it is verified.

import type { ConfinementClass } from "./runs";

/** Go: types.RunnerState. */
export type RunnerState = "unclaimed" | "claimed" | "revoked";

/** Go: types.RunnerOwnerKind. An org runner's `owner` is always empty. */
export type RunnerOwnerKind = "person" | "org";

/**
 * Go: types.RunnerTrustClass, ordered personal < managed_device < org_cluster. Written by the claim, then
 * only by a security operator (PUT /runners/{id}/trust); org_cluster if and only if the owner kind is org.
 */
export type RunnerTrustClass = "personal" | "managed_device" | "org_cluster";

/** Go: types.RunnerCapacity: the per-run ceiling a runner advertised. */
export interface RunnerCapacity {
  cpu_millis_max: number;
  memory_mib_max: number;
}

/** Go: types.RunnerPosture. */
export interface RunnerPosture {
  mdm_managed: boolean;
  disk_encrypted: boolean;
  os: string;
  os_version: string;
  /** "mdm_file" or "os_probe"; empty on a posture stored before it was recorded. */
  evidence: string;
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
  revoked_by?: string;
  /** `"not_mine"` when the owner revoked it as not theirs. */
  revoke_reason?: string;
  /** Set on a person's own list: the revoker's email where Wardyn holds one. */
  revoked_by_email?: string;
  posture?: RunnerPosture;
  posture_reported_at?: string;
  posture_source?: string;
  /**
   * The claimed runner's last caps event, all runner_asserted. Read from the replica that answers, so
   * like `online` it is absent until the runner has connected there ("Not reported yet"), never zero.
   */
  capacity?: RunnerCapacity;
  classes?: ConfinementClass[];
  allowed_roots?: string[];
  kind: "self_hosted";
  /**
   * What the owner declared; only they change it (PUT /me/runners/{id}). null while not all of it is
   * set, and then the runner takes no run.
   */
  limits: RunnerLimits | null;
  /** What its runs hold now: the figures placement counts. Its owner and operators only. */
  usage: RunnerUsage;
  /** The host name the runner last reported (registration, then each connect): runner-reported, never an identity. */
  machine_name?: string;
  /** The CPU architecture of the runner's Docker engine ("amd64" or "arm64") as last reported: runner-reported, absent until it reports one. */
  arch?: string;
  /** The owner's live user pools this runner is a member of; [] for none. */
  pools: RunnerPoolRef[];
  owner_kind: RunnerOwnerKind;
  trust_class: RunnerTrustClass;
  /** The administrators' routes only: who last wrote the trust class, and when. */
  trust_set_by?: string;
  trust_set_at?: string;
}

/** Go: types.RunnerAmounts: CPU in milli-CPU, memory and disk in MiB. */
export interface RunnerAmounts {
  cpu_millis: number;
  memory_mib: number;
  disk_mib: number;
}

/**
 * Go: types.RunnerLimits. A runner's declared limits, either kind: Runs at once (self-hosted 1 to 64,
 * remote-hosted 1 to 1024), the total all its runs at once may reserve and what one run may reserve.
 * At least 1 CPU; disk in whole GiB; per run never past the total.
 */
export interface RunnerLimits {
  runs_at_once: number;
  total: RunnerAmounts;
  per_run: RunnerAmounts;
}

/** Go: types.RunnerTally: a number of runs and the CPU, memory and disk they reserve. */
export interface RunnerTally {
  runs: number;
  cpu_millis: number;
  memory_mib: number;
  disk_mib: number;
}

/**
 * Go: types.RunnerUsage. `in_use` is every run holding a sandbox on the runner, from every pool;
 * `available` is the total less that, all zero while offline or with no limits. `disk_capped` false:
 * the runner cannot cap a run's disk, so its disk is counted, not capped. `reported` is what the
 * runner says it has, never a limit; `reported_why` is why some of it could not be measured.
 */
export interface RunnerUsage {
  online: boolean;
  in_use: RunnerTally;
  available: RunnerTally;
  disk_capped: boolean;
  reported?: RunnerAmounts;
  reported_why?: string;
}

/** Go: types.RunnerPoolRef: one pool a runner is a member of. */
export interface RunnerPoolRef {
  id: string;
  name: string;
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
  /** On the two mint answers only: the URL `wardyn-runnerd register --org` must match. */
  org_url?: string;
  /** On the two mint answers only, and only when the daemon has one configured: the egress-proxy image its own sandboxes run with. */
  proxy_image?: string;
  /** org on a token minted on POST /runners/org-tokens (owner is then empty). */
  owner_kind: RunnerOwnerKind;
  minted_by_kind: "human" | "admin_token";
  /** An org token the admin token minted while WARDYN_GOVERNANCE_SECOND_HUMAN was on. */
  break_glass: boolean;
}

/** Go: types.RunnerOrgTokenRequest. `name` labels the token in its audit row. */
export interface RunnerOrgTokenRequest {
  name: string;
}

/** Go: types.RunnerTrustRequest: a security operator's trust class change and its reason. */
export interface RunnerTrustRequest {
  trust_class: RunnerTrustClass;
  reason: string;
}

/** Go: types.RunnerTokenRequest. Names the person an administrator is minting for. */
export interface RunnerTokenRequest {
  owner: string;
}

/** Go: types.RunnerRegistration: the claim's answer (and the register answer a runner reads). */
export interface RunnerRegistration {
  runner_id: string;
  fingerprint: string;
  state: RunnerState;
  org_url_sha256: string;
  /** The limits the server stored; null on a runner registered before they existed. */
  limits: RunnerLimits | null;
  /** The register answer only, and only when the organisation names one: the proxy image for the architecture the runner reported. */
  proxy_image?: string;
}

/** The revoke answers' body (an inline map in internal/api/runner_revoke.go). */
export interface RunnerRevokeAnswer {
  id: string;
  state: RunnerState;
  runs_killed: number;
}

/** Go: types.RunnerClaimRequest. The fingerprint is the one the person read on their own computer. */
export interface RunnerClaimRequest {
  fingerprint: string;
}

/** Go: types.RunnerUpdateRequest: an owner's change to their own runner, the whole limit set. */
export interface RunnerUpdateRequest {
  limits: RunnerLimits;
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
  | "runners_enabled_required"
  | "runner_limits_invalid"
  | "runner_trust_invalid";
