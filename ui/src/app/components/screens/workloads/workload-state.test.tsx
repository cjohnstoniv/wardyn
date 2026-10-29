/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import type { Workload } from "../../../lib/types/workloads";
import { workloadPhase } from "./workload-state";

const base = (over: Partial<Workload>): Workload => ({
  name: "w",
  owner: "me",
  source: "api",
  intent: {},
  generation: 2,
  current_generation: 2,
  run_id: "7c41e0a9-0000-0000-0000-000000000000",
  state: "RUNNING",
  created_at: "t",
  updated_at: "t",
  ...over,
});

// The phases read the stored workload as workloadStatusOf (internal/api/runner_workloads.go) does.
describe("workloadPhase", () => {
  it.each([
    ["a running run", {}, "running"],
    ["a running run with a healthy service", { state: "RUNNING/healthy" }, "running"],
    ["a service whose health fails", { state: "RUNNING/unhealthy" }, "unhealthy"],
    ["a run waiting for a slot", { state: "QUEUED" }, "queued"],
    ["a run still starting", { state: "STARTING" }, "queued"],
    ["a spec with no run yet", { run_id: undefined, state: undefined, current_generation: 0 }, "queued"],
    ["a spec ahead of its run", { current_generation: 1 }, "queued"],
    ["a completed run", { state: "COMPLETED" }, "ended"],
    ["a killed run", { state: "KILLED" }, "ended"],
    [
      "a refused spec",
      { current_generation: 1, last_refusal: { generation: 2, reason: "runner_pool_full", at: "t" } },
      "refused",
    ],
    [
      "a refused spec whose binding lost its authority",
      { current_generation: 0, run_id: undefined, state: undefined, last_refusal: { generation: 2, reason: "workload_binding_inactive", at: "t" } },
      "binding_inactive",
    ],
    [
      "an older refusal under a spec that has since launched",
      { last_refusal: { generation: 1, reason: "runner_pool_full", at: "t" } },
      "running",
    ],
  ] as const)("%s", (_name, over, phase) => {
    expect(workloadPhase(base(over as Partial<Workload>))).toBe(phase);
  });
});
