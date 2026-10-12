/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The runner pool contract (types only). A pool is an organisation-defined set of
// execution targets: choosing one narrows where a run may go and grants nothing.
// Every interface mirrors one Go struct (internal/types/runner_pool.go or
// pkg/client/runner_pools.go) and runner-pools.wire.test.ts pins its json tags to
// that struct's source. Nothing here is stored yet; the server refuses what it
// cannot honour with runner_pools_unavailable or request_field_unavailable.
//
// The UI words are "Remote Provided" (`remote_provided`) and "Self-Hosted"
// (`self_hosted`); the run's transport value stays `placement` remote / local.

import type { BarrierReason, PoolAvailabilityReason, RunnerPoolReason } from "../runner-pool-refusals";
import type { ConfinementClass } from "./runs";

export type RunnerPoolHosting = "remote_provided" | "self_hosted";

export type RunnerPoolState = "active" | "disabled" | "deleted";

/** How a run's pool was chosen. */
export type RunnerPoolSelection = "explicit" | "personal_default" | "organisation_default";

/** Whether a permitted pool can take a run right now; `unknown` is never read as available. */
export type RunnerPoolAvailability = "available" | "unavailable" | "unknown";

/** The kind of run a pool serves (Go: types.RunnerPoolRunType); the run mode's wire values. */
export type RunnerPoolRunType = "background" | "interactive";

/** Which bound an effective limit came from (Go: types.LimitSource). */
export type LimitSource = "pool" | "governance" | "deployment" | "runner_capacity";

/** Why the platform ended a run (Go: types.RunEndReason). */
export type RunEndReason =
  | "max_lifetime_reached"
  | "end_reached"
  | "queue_timeout"
  | "queue_profile_changed"
  | "queue_policy_changed"
  | "queue_portal_revoked"
  | "runner_pool_removed"
  | "identity_deactivated";

/** Go: types.RunnerPoolAmount. A CPU (milli-CPU) or memory (MiB) limit; `default` is at most `cap`. */
export interface RunnerPoolAmount {
  default: number;
  cap: number;
}

/**
 * Go: types.RunnerPoolDuration, in seconds. Either a finite `max_sec` with a default of 1 to `max_sec`, or
 * `unlimited` with no `max_sec` (for an idle stop, "never"). Never encoded by an absent number alone. An
 * Interactive `lifetime` that is unlimited still needs a `default_sec` of at least 1 (No end is chosen per
 * run); a Background `lifetime` may default to no end (`default_sec` 0).
 */
export interface RunnerPoolDuration {
  default_sec: number;
  max_sec?: number;
  unlimited?: boolean;
}

/** Go: types.RunnerPoolBackgroundLimits. A Background task has no idle stop; a body that sends one is refused. */
export interface RunnerPoolBackgroundLimits {
  cpu_millis: RunnerPoolAmount;
  memory_mib: RunnerPoolAmount;
  /** The main drive's default and cap in MiB, each a whole number of GiB. Absent: the pool sets no main-drive bound. */
  disk_mib?: RunnerPoolAmount;
  lifetime: RunnerPoolDuration;
}

/** Go: types.RunnerPoolInteractiveLimits. */
export interface RunnerPoolInteractiveLimits {
  cpu_millis: RunnerPoolAmount;
  memory_mib: RunnerPoolAmount;
  disk_mib?: RunnerPoolAmount;
  lifetime: RunnerPoolDuration;
  idle: RunnerPoolDuration;
}

/** Go: types.RunnerPoolLimits. A run type with limits is a run type the pool allows; the others are refused. */
export interface RunnerPoolLimits {
  background?: RunnerPoolBackgroundLimits;
  interactive?: RunnerPoolInteractiveLimits;
  barriers: ConfinementClass[];
  max_concurrent_runs?: number;
}

/** Go: types.LimitAmount. Unlimited is its own value, never a zero. */
export interface LimitAmount {
  value: number;
  unlimited?: boolean;
}

/** Go: types.EffectiveLimit. The source is absent when nothing bounded the value. */
export interface EffectiveLimit {
  default: LimitAmount;
  cap: LimitAmount;
  default_source?: LimitSource;
  cap_source?: LimitSource;
}

/** Go: types.ConcurrencyBound. Never folded into one number: each source counts a different set of runs. */
export interface ConcurrencyBound {
  source: LimitSource;
  max: number;
}

/**
 * Go: types.EffectiveRunLimits. `idle_sec` is absent for a Background run.
 * `lifetime_sec` caps a finite end (the lesser of the span and the lease);
 * `lifetime_span_sec` counts from the run's start (pool, deployment, runner),
 * `lifetime_lease_sec` from the moment an end is asked for (governance);
 * `no_end_allowed` is the separate yes for a run with no end.
 */
export interface EffectiveRunLimits {
  run_type: RunnerPoolRunType;
  cpu_millis: EffectiveLimit;
  memory_mib: EffectiveLimit;
  /** The main drive's size in MiB; an unlimited default means an untouched run gets no size. */
  disk_mib: EffectiveLimit;
  lifetime_sec: EffectiveLimit;
  lifetime_span_sec: EffectiveLimit;
  lifetime_lease_sec: EffectiveLimit;
  no_end_allowed: boolean;
  idle_sec?: EffectiveLimit;
  concurrency: ConcurrencyBound[];
}

/** Go: types.RunnerPool. */
export interface RunnerPool {
  id: string;
  name: string;
  hosting_type: RunnerPoolHosting;
  /** A Self-Hosted pool's owner (their subject); absent on a Remote Provided pool. */
  owner?: string;
  state: RunnerPoolState;
  revision: number;
  limits?: RunnerPoolLimits;
  created_at: string;
  updated_at: string;
}

/** Go: types.RunnerPoolMember. Exactly one of runner_id and executor_id is set. */
export interface RunnerPoolMember {
  pool_id: string;
  runner_id?: string;
  executor_id?: string;
  added_at: string;
}

/** Go: types.RunnerPoolDefaults. An absent field inherits; it never widens what the person may use. */
export interface RunnerPoolDefaults {
  preferred_hosting?: RunnerPoolHosting;
  remote_provided?: string;
  self_hosted?: string;
  /** The organisation's default barrier; a person's own document never carries one (the server refuses it). */
  barrier?: ConfinementClass;
}

/** Go: types.ResolvedRunnerPool. A label for the choice, never admission. */
export interface ResolvedRunnerPool {
  id: string;
  name: string;
  hosting_type: RunnerPoolHosting;
  revision: number;
  selection: RunnerPoolSelection;
}

/** Go: types.RunnerPoolSubject (a role is a user_type; an identity provider's roles arrive as groups). */
export interface RunnerPoolSubject {
  subject_type: "user" | "group" | "user_type";
  subject: string;
}

/** Go: types.RunnerPoolUsePolicy. With none, everyone who may launch remote runs may use the pool. */
export interface RunnerPoolUsePolicy {
  pool_id: string;
  subjects: RunnerPoolSubject[];
  revision: number;
  updated_at: string;
  updated_by?: string;
}

/**
 * Go: client.RunnerPoolChoice, as the preflight and the policy preview send it. Only pools the caller may
 * use are ever sent. `limits` and `barriers` are never filled on those doors.
 */
export interface RunnerPoolChoice {
  id: string;
  name: string;
  hosting_type: RunnerPoolHosting;
  availability: RunnerPoolAvailability;
  // The server sends one of POOL_AVAILABILITY_REASONS; RunnerPoolReason stays for older fixtures.
  reason?: PoolAvailabilityReason | RunnerPoolReason;
  limits?: EffectiveRunLimits[];
  barriers?: ConfinementClass[];
  /** What this pool's runners do with the main drive's size; absent on a pool the caller may only manage. */
  main_drive?: MainDriveFacts;
}

/** Go: client.MainDriveEnforcement. The weakest word across the pool's targets; never request or external. */
export type MainDriveEnforcement = "filesystem" | "eviction" | "none" | "unknown";

/** Go: client.MainDriveCoverage. Absent when enforcement is none or unknown. */
export type MainDriveCoverage = "all" | "scratch_paths";

/** Go: client.MainDriveKeep. `grace_sec` is set only when supported. */
export interface MainDriveKeep {
  supported: boolean;
  grace_sec?: number;
}

/** Go: client.MainDriveFacts. */
export interface MainDriveFacts {
  enforcement: MainDriveEnforcement;
  basis: "substrate" | "runner_asserted";
  coverage?: MainDriveCoverage;
  keep: MainDriveKeep;
  drive_mounts: boolean;
}

/** Go: runnerpool.BarrierOption. Always CC1, CC2, CC3, in that order. */
export interface BarrierOption {
  class: ConfinementClass;
  available: boolean;
  default?: boolean;
  /** "organisation" arrives with the organisation default barrier; Review already reads it. */
  default_source?: "server" | "organisation";
  reason_code?: BarrierReason;
  reason_detail?: "kvm_missing";
  floor?: ConfinementClass;
}

/** Go: client.PoolRunner. One of the caller's OWN runners in a self-hosted pool. */
export interface PoolRunner {
  id: string;
  name: string;
  online: boolean;
}

/**
 * Go: client.PoolSlots. A pool's slot counts, everyone's and never saying whose. The verdict is advisory:
 * the launch decides. `max_wait_sec` is the wait a run of the asked type gets (86400 Background, 3600
 * Interactive); on the pool list only with ?run_type=.
 */
export interface PoolSlots {
  used: number;
  waiting: number;
  /** Absent: the pool has no limit. */
  max?: number;
  verdict: "starts_now" | "waits";
  max_wait_sec?: number;
}

/**
 * Go: client.RunnerPoolCatalogueEntry, one row of GET /runner-pools: the dry doors' fields plus what the
 * pool offers. A row the caller may only manage (runner_pool_not_found) carries none of these.
 */
export interface RunnerPoolCatalogueEntry extends RunnerPoolChoice {
  run_types: RunnerPoolRunType[];
  run_types_source?: "pool" | "none";
  barrier_options: BarrierOption[];
  /** Every one of the caller's own runners in the pool, whether or not it can take a run now. */
  my_runners?: PoolRunner[];
  slots?: PoolSlots;
  /** A Self-Hosted pool's owner: the caller, or the person on an administrator's oversight row. */
  owner?: string;
}

/** Go: client.PersonAllowance. Counts only; absent for a caller outside governance. */
export interface PersonAllowance {
  active: number;
  max?: number;
  profile_name?: string;
}

/** Go: client.PoolDefault. The pool an untouched run resolves to, or the default that no longer applies. */
export interface PoolDefault {
  pool_id?: string;
  selection?: RunnerPoolSelection;
  reason?: RunnerPoolReason;
}

/** Go: client.RunnerPoolList. */
export interface RunnerPoolList {
  pools: RunnerPoolCatalogueEntry[];
  allowance?: PersonAllowance;
  default?: PoolDefault;
}

/** Go: client.CreateRunnerPoolRequest: a Remote Provided pool (admin); self_hosted is refused. */
export interface CreateRunnerPoolRequest {
  name: string;
  hosting_type: RunnerPoolHosting;
  limits?: RunnerPoolLimits;
}

/** Go: client.CreateMyRunnerPoolRequest: a Self-Hosted pool of the caller's own. */
export interface CreateMyRunnerPoolRequest {
  name: string;
  limits?: RunnerPoolLimits;
}

/** The state an update may put a pool in (Go: client.RunnerPoolSwitch); deleting is the DELETE route alone. */
export type RunnerPoolSwitch = "active" | "disabled";

/** Go: client.UpdateRunnerPoolRequest. `revision` is the one the caller read; a stale one is refused. */
export interface UpdateRunnerPoolRequest {
  revision: number;
  name?: string;
  state?: RunnerPoolSwitch;
  limits?: RunnerPoolLimits;
}
