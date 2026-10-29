/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Workloads and their namespace bindings (pkg/client/workloads.go; handlers in internal/api/workloads.go and
// workload_bindings.go). Each interface has exactly the json tags of the Go struct it mirrors
// (workloads.wire.test.ts reads the Go source and diffs the two).
import type { TemplateIntent } from "./templates";

/** Which writer owns a workload: the API and CLI, or a custom resource. Go: client.WorkloadSource. */
export type WorkloadSource = "api" | "k8s";

/** Why the last launch attempt of a generation was refused. Go: client.WorkloadRefusal. */
export interface WorkloadRefusal {
  generation: number;
  reason: string;
  message?: string;
  at: string;
}

/** One stored workload. Go: client.Workload. */
export interface Workload {
  name: string;
  /** The person, or `service:<name>`, whose run it keeps running. */
  owner: string;
  source: WorkloadSource;
  /** The custom resource's namespace; only for source `k8s`. */
  namespace?: string;
  description?: string;
  run_in?: string;
  restart_key?: string;
  /** The door-normalised intent. */
  intent: TemplateIntent;
  /** The hex SHA-256 over the intent, run_in and restart_key; absent for a reader who sees the intent with its host axis withheld. */
  digest?: string;
  /** Counts spec changes; current_generation is the one the current run was launched from. */
  generation: number;
  current_generation: number;
  run_id?: string;
  /** The current run's state with its service health when it has one: `RUNNING` or `RUNNING/unhealthy`. */
  state?: string;
  last_refusal?: WorkloadRefusal;
  created_at: string;
  updated_at: string;
}

/** What an apply or restart did to the stored workload. Go: client.WorkloadOutcome. */
export type WorkloadOutcome = "created" | "updated" | "unchanged";

/** Whether an apply or restart started a run. Go: client.WorkloadLaunchState. */
export type WorkloadLaunchState = "launched" | "none" | "refused";

/** The launch half of an apply's or restart's answer. Go: client.WorkloadLaunch. */
export interface WorkloadLaunch {
  state: WorkloadLaunchState;
  run_id?: string;
  reason?: string;
  message?: string;
  warnings?: string[];
}

/** The answer of POST /workloads/{name}/restart. Go: client.WorkloadApplyResult. */
export interface WorkloadApplyResult {
  workload: Workload;
  outcome: WorkloadOutcome;
  launch: WorkloadLaunch;
}

/** The custom resources of one namespace may launch runs as a service identity on a pool. Go: client.WorkloadBinding. */
export interface WorkloadBinding {
  id: string;
  pool_id: string;
  namespace: string;
  service: string;
  /** The service identity's sponsor when the binding was made; a new sponsor ends it. */
  sponsor: string;
  created_by: string;
  created_at: string;
}

/** POST /workload-bindings. Go: client.CreateWorkloadBindingRequest. */
export interface CreateWorkloadBindingRequest {
  /** The runner pool's id or name. */
  pool: string;
  namespace: string;
  service: string;
}
