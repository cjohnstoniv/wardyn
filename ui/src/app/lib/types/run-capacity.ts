/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// internal/store.RunCapacitySums — one set of configured-reservation sums
// (GET /admin/runs/capacity). `unknown` counts holding runs whose reservation
// was never recorded; they add to no sum.
export interface RunCapacitySums {
  holding: number;
  unknown: number;
  agent_cpu_request_millis: number;
  agent_cpu_limit_millis: number;
  agent_memory_request_mib: number;
  agent_memory_limit_mib: number;
  proxy_cpu_millis: number;
  proxy_memory_mib: number;
  proxy_cpu_uncapped: number;
  held_cpu_millis: number;
  held_memory_mib: number;
}

// internal/store.RunCapacityRunner: `requests` (Kubernetes) or `caps` (Docker).
export interface RunCapacityRunner extends RunCapacitySums {
  basis: "requests" | "caps";
}

export interface RunCapacityOwner {
  owner: string;
  holding: number;
  by_runner: Record<string, RunCapacitySums>;
}

export interface RunCapacityAge {
  bucket: string;
  count: number;
}

// internal/store.RunCapacityUnschedulable; null columns are unrecorded.
export interface RunCapacityUnschedulable {
  id: string;
  owner: string;
  waited_seconds: number;
  reason: string;
  runner_kind: string | null;
  agent_cpu_request_millis: number | null;
  agent_memory_request_mib: number | null;
  proxy_cpu_millis: number | null;
  proxy_memory_mib: number | null;
}

// internal/store.RunCapacity.
export interface RunCapacity {
  states: Record<string, number>;
  paused: number;
  kept: number;
  totals: RunCapacityRunner;
  age_buckets: RunCapacityAge[];
  by_runner: Record<string, RunCapacityRunner>;
  by_owner: RunCapacityOwner[];
  by_owner_truncated: boolean;
  unschedulable: RunCapacityUnschedulable[];
  unschedulable_total: number;
  /** Age in seconds since created_at of the oldest holding run (0 when none). */
  oldest_active_seconds: number;
}

// GET /api/v1/admin/runs/capacity: RunCapacity plus generated_at and basis
// (the handler envelope in internal/api/runs_capacity.go).
export interface RunCapacityResponse extends RunCapacity {
  generated_at: string;
  basis: "configured_reservations";
}
