/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Owns `service`, the choice to keep a background command running. A service rides only an Automation / script run
// on a container sandbox; its port, health path, interval and threshold live in the draft as the server reads them,
// and the health check exists only while a port is named.

import type { RunService } from "../../../../lib/types/runs";
import { isVMKind } from "../../../../lib/types/machine-kind";
import { DEFAULT_HEALTH_FAILURES, DEFAULT_HEALTH_INTERVAL_SEC } from "../../../wardyn/copy/run-service";
import type { RunIntent } from "../draft/run-intent";
import { withKeys } from "./lens-keys";

/** A background run of a command on a container: the only run the server lets carry a service. */
export const mayKeepRunning = (intent: RunIntent): boolean =>
  intent.experience === "background" && intent.workload?.kind === "command" && !isVMKind(intent.machine_kind);

const set = (intent: RunIntent, service: RunService | undefined): RunIntent => withKeys(intent, { service });

/** Off by default: the run then ends when its command exits. On restarts it when it fails. */
export const keepRunning = (intent: RunIntent, on: boolean): RunIntent => set(intent, on ? { restart: "on_failure" } : undefined);

export const setRestart = (intent: RunIntent, restart: RunService["restart"]): RunIntent =>
  intent.service ? set(intent, { ...intent.service, restart }) : intent;

/** Naming a port adds the health check with its defaults; clearing it takes the check away with it. */
export function setPort(intent: RunIntent, port: number): RunIntent {
  const s = intent.service;
  if (!s) return intent;
  const { port: _port, health: _health, ...rest } = s;
  void _port;
  void _health;
  if (!port) return set(intent, rest);
  return set(intent, {
    ...rest,
    port,
    health: s.health ?? { interval_sec: DEFAULT_HEALTH_INTERVAL_SEC, failure_threshold: DEFAULT_HEALTH_FAILURES },
  });
}

/** One field of the health check. An empty path checks only that the port accepts connections. */
export function setHealth(intent: RunIntent, patch: Partial<NonNullable<RunService["health"]>>): RunIntent {
  const s = intent.service;
  if (!s?.health) return intent;
  const health = { ...s.health, ...patch };
  if (!health.path) delete health.path;
  return set(intent, { ...s, health });
}
