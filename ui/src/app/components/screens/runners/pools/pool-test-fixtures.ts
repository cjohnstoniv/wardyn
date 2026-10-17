/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Shared fixtures of the pool screens' tests: pools, their catalogue rows, and runners in them.
import type { RemoteRunnerView, RunnerPool, RunnerPoolCatalogueEntry, RunnerPoolLimits, RunnerPoolMemberView } from "../../../../lib/types/runner-pools";
import type { RunnerLimits, RunnerTally, RunnerView } from "../../../../lib/types/runners";

const NONE: RunnerTally = { runs: 0, cpu_millis: 0, memory_mib: 0, disk_mib: 0 };
export const LIMITS: RunnerLimits = {
  runs_at_once: 4,
  total: { cpu_millis: 8000, memory_mib: 16384, disk_mib: 204800 },
  per_run: { cpu_millis: 4000, memory_mib: 8192, disk_mib: 51200 },
};
const USAGE = (online: boolean) => ({
  online,
  in_use: { runs: 1, cpu_millis: 2000, memory_mib: 4096, disk_mib: 10240 },
  available: online ? { runs: 3, cpu_millis: 6000, memory_mib: 12288, disk_mib: 194560 } : NONE,
  disk_capped: true,
});

export function pool(over: Partial<RunnerPool> = {}): RunnerPool {
  return {
    id: "p-ci",
    name: "CI jobs",
    scope: "organization",
    state: "active",
    revision: 9,
    limits: { background: { memory_mib: { default: 4096, cap: 4096 } }, barriers: ["CC2"], max_concurrent_runs: 40 },
    created_at: "2026-10-01T10:00:00Z",
    updated_at: "2026-10-11T10:00:00Z",
    ...over,
  };
}

export function entry(p: RunnerPool, over: Partial<RunnerPoolCatalogueEntry> = {}): RunnerPoolCatalogueEntry {
  return {
    id: p.id,
    name: p.name,
    scope: p.scope,
    owner: p.owner,
    availability: "available",
    run_types: ["background", "interactive"],
    barrier_options: [],
    slots: { used: 6, waiting: 2, verdict: "starts_now" },
    ...over,
  };
}

export function remoteRunner(over: Partial<RemoteRunnerView> = {}): RemoteRunnerView {
  return {
    id: "rr-eu",
    kind: "remote_hosted",
    name: "eu-cluster",
    people_pools: "off",
    limits: LIMITS,
    revision: 1,
    created_at: "2026-10-01T10:00:00Z",
    updated_at: "2026-10-01T10:00:00Z",
    configured: true,
    online: true,
    pools: [{ id: "p-ci", name: "CI jobs", scope: "organization" }],
    usage: USAGE(true),
    ...over,
  };
}

export function selfRunner(over: Partial<RunnerView> = {}): RunnerView {
  return {
    id: "r-laptop",
    name: "laptop-7",
    state: "claimed",
    owner: "dana@example.com",
    owner_kind: "person",
    trust_class: "personal",
    online: true,
    key_fingerprint: "SHA256:abc",
    key_fingerprint_abbreviated: false,
    created_at: "2026-10-01T10:00:00Z",
    kind: "self_hosted",
    limits: LIMITS,
    usage: USAGE(true),
    machine_name: "dana-mbp-14.corp.example.com",
    pools: [],
    ...over,
  };
}

export function member(r: RemoteRunnerView | RunnerView): RunnerPoolMemberView {
  return r.kind === "remote_hosted"
    ? { kind: "remote_hosted", id: r.id, name: r.name, online: r.online, configured: r.configured, limits: r.limits, usage: r.usage }
    : { kind: "self_hosted", id: r.id, name: r.name, online: r.online, runner: r, limits: r.limits, usage: r.usage };
}

export const noLimits = (): RunnerPoolLimits => ({ barriers: [] });
