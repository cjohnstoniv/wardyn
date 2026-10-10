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

import type { RunnerPoolReason } from "../runner-pool-refusals";

export type RunnerPoolHosting = "remote_provided" | "self_hosted";

export type RunnerPoolState = "active" | "disabled" | "deleted";

/** How a run's pool was chosen. */
export type RunnerPoolSelection = "explicit" | "personal_default" | "organisation_default";

/** Whether a permitted pool can take a run right now; `unknown` is never read as available. */
export type RunnerPoolAvailability = "available" | "unavailable" | "unknown";

/** The principal_prefs key of a person's own pool defaults (types.RunnerPoolDefaultsPrefKey). */
export const RUNNER_POOL_DEFAULTS_PREF_KEY = "runner_pool_defaults.v1";

/** Go: types.RunnerPool. */
export interface RunnerPool {
  id: string;
  name: string;
  hosting_type: RunnerPoolHosting;
  state: RunnerPoolState;
  revision: number;
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

/** Go: client.RunnerPoolChoice. Only pools the caller may use are ever sent. */
export interface RunnerPoolChoice {
  id: string;
  name: string;
  hosting_type: RunnerPoolHosting;
  availability: RunnerPoolAvailability;
  reason?: RunnerPoolReason;
}

/** Go: client.RunnerPoolList. */
export interface RunnerPoolList {
  pools: RunnerPoolChoice[];
}

/** Go: client.CreateRunnerPoolRequest. */
export interface CreateRunnerPoolRequest {
  name: string;
  hosting_type: RunnerPoolHosting;
}

/** Go: client.UpdateRunnerPoolRequest. `revision` is the one the caller read; a stale one is refused. */
export interface UpdateRunnerPoolRequest {
  revision: number;
  name?: string;
  state?: RunnerPoolState;
}
