/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The one state badge of a workload (mock r16 T3-a). It reads the stored workload as the server's own status
// push does (workloadStatusOf in internal/api/runner_workloads.go): a refusal of the current spec first, else the
// current run's state with its service health. "Not bound" is a custom resource's status in its cluster: a
// namespace nothing binds stores no workload, so the list never holds one.
import { CircleX, Square } from "lucide-react";
import { isTerminalRunState } from "../../../lib/types";
import type { RunState } from "../../../lib/types";
import type { Workload } from "../../../lib/types/workloads";
import { WORKLOADS as W } from "../../wardyn/copy/workloads";
import { Chip } from "../../wardyn/primitives";

export type WorkloadPhase = "running" | "unhealthy" | "queued" | "ended" | "refused" | "binding_inactive";

/** The reason a launch is refused with when the namespace binding no longer carries authority. */
export const BINDING_INACTIVE_REASON = "workload_binding_inactive";

/** Whether the latest spec's launch was refused: its refusal is of this generation and no run of it exists. */
export function refusedNow(w: Workload): boolean {
  return !!w.last_refusal && w.last_refusal.generation === w.generation && w.current_generation < w.generation;
}

export function workloadPhase(w: Workload): WorkloadPhase {
  if (refusedNow(w)) return w.last_refusal?.reason === BINDING_INACTIVE_REASON ? "binding_inactive" : "refused";
  const [state = "", health = ""] = (w.state ?? "").split("/");
  if (!state || w.current_generation < w.generation || state === "QUEUED" || state === "PENDING" || state === "STARTING") return "queued";
  if (isTerminalRunState(state as RunState)) return "ended";
  return health === "unhealthy" ? "unhealthy" : "running";
}

export function WorkloadBadge({ workload }: { workload: Workload }) {
  switch (workloadPhase(workload)) {
    case "running":
      return <Chip tone="success" dot pulse>{W.RUNNING}</Chip>;
    case "unhealthy":
      return <Chip tone="warning" dot pulse>{W.UNHEALTHY}</Chip>;
    case "queued":
      return <Chip tone="neutral" dot>{W.QUEUED}</Chip>;
    case "ended":
      return (
        <Chip tone="neutral">
          <Square className="size-3" aria-hidden />
          {W.ENDED}
        </Chip>
      );
    case "refused":
      return (
        <Chip tone="danger">
          <CircleX className="size-3" aria-hidden />
          {W.REFUSED}
        </Chip>
      );
    case "binding_inactive":
      return <Chip tone="warning">{W.BINDING_INACTIVE}</Chip>;
  }
}
