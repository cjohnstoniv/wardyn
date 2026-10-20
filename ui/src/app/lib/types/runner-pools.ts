/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The runner pool contract (types only). A pool is a set of runners: choosing one
// narrows where a run may go and grants nothing. Every interface mirrors one Go
// struct (internal/types/runner_pool.go, internal/types/remote_runner.go,
// pkg/client/runner_pools.go or pkg/client/remote_runners.go) and
// runner-pools.wire.test.ts pins its json tags to that struct's source.
//
// The UI words are "Organization pools" (`organization`) and "User pools" (`user`);
// a runner is a self-hosted runner (`self_hosted`) or a remote-hosted runner
// (`remote_hosted`); the run's transport value stays `placement` remote / local.

import type { BarrierReason, PoolAvailabilityReason, RunnerPoolReason } from "../runner-pool-refusals";
import type { RunnerAmounts, RunnerLimits, RunnerTally, RunnerUsage, RunnerView } from "./runners";
import type { ConfinementClass } from "./runs";

export type RunnerPoolScope = "organization" | "user";

/** The kind of a runner (Go: types.RunnerKind). */
export type RunnerKind = "self_hosted" | "remote_hosted";

/** Who may add a remote-hosted runner to their own user pools (Go: types.RemoteRunnerPeoplePools). */
export type RemoteRunnerPeoplePools = "off" | "everyone" | "listed";

export type RunnerPoolState = "active" | "disabled" | "deleted";

/** How a run's pool was chosen. */
export type RunnerPoolSelection = "explicit" | "personal_default" | "organisation_default";

/** Whether a permitted pool can take a run right now; `unknown` is never read as available. */
export type RunnerPoolAvailability = "available" | "unavailable" | "unknown";

/** The kind of run a pool serves (Go: types.RunnerPoolRunType); the run mode's wire values. */
export type RunnerPoolRunType = "background" | "interactive";

/** Which bound an effective limit came from (Go: types.LimitSource). */
export type LimitSource = "pool" | "governance" | "deployment" | "runner";

/** Why the platform ended a run (Go: types.RunEndReason). */
export type RunEndReason =
  | "max_lifetime_reached"
  | "end_reached"
  | "queue_timeout"
  | "queue_sign_in_timeout"
  | "scheduled_runs_off"
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

/**
 * Go: types.RunnerPoolBackgroundLimits. A Background task has no idle stop; a body that sends one is refused.
 * Every field is optional: one left out is set by the pool's runners, never "no limit".
 */
export interface RunnerPoolBackgroundLimits {
  cpu_millis?: RunnerPoolAmount;
  memory_mib?: RunnerPoolAmount;
  /** The main drive's default and cap in MiB, each a whole number of GiB. */
  disk_mib?: RunnerPoolAmount;
  lifetime?: RunnerPoolDuration;
}

/** Go: types.RunnerPoolInteractiveLimits. Every field is optional, as on the Background block. */
export interface RunnerPoolInteractiveLimits {
  cpu_millis?: RunnerPoolAmount;
  memory_mib?: RunnerPoolAmount;
  disk_mib?: RunnerPoolAmount;
  lifetime?: RunnerPoolDuration;
  idle?: RunnerPoolDuration;
}

/**
 * Go: types.RunnerPoolLimits. A run type with limits is a run type the pool allows; the others are refused.
 * An empty `barriers` allows every barrier the pool's runners offer.
 */
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
  scope: RunnerPoolScope;
  /** A user pool's owner (their subject); absent on an organization pool. */
  owner?: string;
  state: RunnerPoolState;
  revision: number;
  limits?: RunnerPoolLimits;
  /** Whether the pool requires its runs recorded; absent is preferred. */
  recording?: RecordingRequirement;
  created_at: string;
  updated_at: string;
}

/** Go: types.RecordingRequirement. The stricter of the organisation's and the pool's applies. */
export type RecordingRequirement = "preferred" | "required";

/** Go: types.RunnerPoolMember. Exactly one of runner_id (self_hosted) and remote_runner_id (remote_hosted) is set. */
export interface RunnerPoolMember {
  pool_id: string;
  kind: RunnerKind;
  runner_id?: string;
  remote_runner_id?: string;
  added_at: string;
}

/**
 * Go: types.RunnerPoolDefaults. The default pool: a person's own wins over the organization's, and it
 * never widens what the person may use. The organization's names an organization pool only.
 */
export interface RunnerPoolDefaults {
  pool_id?: string;
  /** The organisation's default barrier; a person's own document never carries one (the server refuses it). */
  barrier?: ConfinementClass;
  /** The organisation's alone, like barrier: whether every run must be recorded. */
  recording?: RecordingRequirement;
  /** The organisation's document alone. A write that names the revision it read is refused (409 `runner_pool_stale`) once it has moved on. */
  revision?: number;
}

/** Go: types.ResolvedRunnerPool. A label for the choice, never admission. */
export interface ResolvedRunnerPool {
  id: string;
  name: string;
  scope: RunnerPoolScope;
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
  scope: RunnerPoolScope;
  availability: RunnerPoolAvailability;
  // The server sends one of POOL_AVAILABILITY_REASONS; RunnerPoolReason stays for older fixtures.
  reason?: PoolAvailabilityReason | RunnerPoolReason;
  limits?: EffectiveRunLimits[];
  barriers?: ConfinementClass[];
  /** What this pool's runners do with the main drive's size; absent on a pool the caller may only manage. */
  main_drive?: MainDriveFacts;
  /** Every one of the caller's own runners in the pool, whether or not it can take a run now. On the list and both dry doors. */
  my_runners?: PoolRunner[];
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

/** Go: client.PoolRunner. One of the caller's OWN self-hosted runners in a user pool. */
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
  slots?: PoolSlots;
  capacity?: PoolCapacity;
  /** A user pool's owner: the caller, or the person on an administrator's oversight row. */
  owner?: string;
  /** The pool's own record, as GET /runner-pools/{id} reads it, on every row (including an oversight row). `pool_limits` is its raw limits, not the effective `limits`. */
  state?: RunnerPoolState;
  revision?: number;
  pool_limits?: RunnerPoolLimits;
}

/**
 * Go: client.PoolCapacity. The pool's runners added up, numbers only and never whose. `available` adds
 * only online members and is zero for a switched-off pool; `largest_per_run` is the biggest run a member
 * takes.
 */
export interface PoolCapacity {
  total: RunnerTally;
  in_use: RunnerTally;
  available: RunnerTally;
  largest_per_run: RunnerAmounts;
  runners: number;
  runners_online: number;
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

/** Go: client.CreateRunnerPoolRequest: an organization pool (admin). */
export interface CreateRunnerPoolRequest {
  name: string;
  limits?: RunnerPoolLimits;
  recording?: RecordingRequirement;
}

/** Go: client.CreateMyRunnerPoolRequest: a user pool of the caller's own. */
export interface CreateMyRunnerPoolRequest {
  name: string;
  limits?: RunnerPoolLimits;
  recording?: RecordingRequirement;
}

/** The state an update may put a pool in (Go: client.RunnerPoolSwitch); deleting is the DELETE route alone. */
export type RunnerPoolSwitch = "active" | "disabled";

/** Go: client.UpdateRunnerPoolRequest. `revision` is the one the caller read; a stale one is refused. */
export interface UpdateRunnerPoolRequest {
  revision: number;
  name?: string;
  state?: RunnerPoolSwitch;
  limits?: RunnerPoolLimits;
  recording?: RecordingRequirement;
}

/** Go: types.RemoteRunnerPoolRef. One live pool a remote-hosted runner is in; owner only on the operators' reads. */
export interface RemoteRunnerPoolRef {
  id: string;
  name: string;
  scope: RunnerPoolScope;
  owner?: string;
}

/** Go: types.RemoteRunner. The Docker host or Kubernetes cluster this server is set up with. */
export interface RemoteRunner {
  id: string;
  kind: "remote_hosted";
  name: string;
  people_pools: RemoteRunnerPeoplePools;
  /** Set when people_pools is listed; an emptied list admits nobody. Never on a person's own list. */
  people_subjects?: RunnerPoolSubject[];
  /** null while any of it is unset: the runner then takes no run. */
  limits: RunnerLimits | null;
  revision: number;
  updated_by?: string;
  created_at: string;
  updated_at: string;
}

/** Go: client.RemoteRunnerView. GET /remote-runners (operators) and /me/remote-runners (those the caller may add). */
export interface RemoteRunnerView extends RemoteRunner {
  /** This server runs it; one that is not configured stays listed and never takes a run. */
  configured: boolean;
  online: boolean;
  pools: RemoteRunnerPoolRef[];
  /** Every run on it, from every pool. */
  usage: RunnerUsage;
}

/** Go: client.UpdateRemoteRunnerRequest (admin). */
export interface UpdateRemoteRunnerRequest {
  revision: number;
  name?: string;
  limits?: RunnerLimits;
}

/** Go: client.RemoteRunnerPeoplePolicyRequest. Turning it off is the DELETE route. */
export interface RemoteRunnerPeoplePolicyRequest {
  revision?: number;
  people_pools: "everyone" | "listed";
  subjects?: RunnerPoolSubject[];
}

/** Go: client.RunnerPoolMemberView. One runner of a pool, either kind (GET /runner-pools/{id}/members). */
export interface RunnerPoolMemberView {
  kind: RunnerKind;
  /** The self-hosted runner's id or the remote-hosted runner's. */
  id: string;
  name: string;
  online: boolean;
  /** A remote-hosted member this server runs. */
  configured?: boolean;
  /** A self-hosted member's runner view; its owner is named on an operator's read. */
  runner?: RunnerView;
  limits: RunnerLimits | null;
  usage: RunnerUsage;
}
