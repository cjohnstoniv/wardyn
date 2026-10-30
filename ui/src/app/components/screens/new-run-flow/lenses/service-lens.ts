/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Owns `service`, the choice to keep a background command running. A service rides only an Automation / script run
// on a container sandbox; its port, health path, interval and threshold live in the draft as the server reads them,
// and the health check counts only while a port is named.

import type { RunService } from "../../../../lib/types/runs";
import { isVMKind } from "../../../../lib/types/machine-kind";
import { DEFAULT_HEALTH_FAILURES, DEFAULT_HEALTH_INTERVAL_SEC } from "../../../wardyn/copy/run-service";
import type { RunIntent } from "../draft/run-intent";
import { setApps } from "./apps-lens";
import { withKeys } from "./lens-keys";
import { issue, type StepIssue } from "./step-issues";

/** A background run of a command on a container: the only run the server lets carry a service. */
export const mayKeepRunning = (intent: RunIntent): boolean =>
  intent.experience === "background" && intent.workload?.kind === "command" && !isVMKind(intent.machine_kind);

const set = (intent: RunIntent, service: RunService | undefined): RunIntent => withKeys(intent, { service });

/** Off by default: the run then ends when its command exits. On restarts it when it fails. */
export const keepRunning = (intent: RunIntent, on: boolean): RunIntent =>
  on ? set(intent, { restart: "on_failure" }) : setApps(set(intent, undefined), []);

export const setRestart = (intent: RunIntent, restart: RunService["restart"]): RunIntent =>
  intent.service ? set(intent, { ...intent.service, restart }) : intent;

/**
 * Naming a port adds the health check with its defaults. Emptying the field keeps the check, so retyping a port does not lose
 * the path, interval and threshold typed beside it; serviceWire leaves it out of the request while no port is named.
 */
export function setPort(intent: RunIntent, port: number): RunIntent {
  const s = intent.service;
  if (!s) return intent;
  const { port: _port, ...rest } = s;
  void _port;
  if (!port) return set(intent, rest);
  return set(intent, {
    ...rest,
    port,
    health: s.health ?? { interval_sec: DEFAULT_HEALTH_INTERVAL_SEC, failure_threshold: DEFAULT_HEALTH_FAILURES },
  });
}

/** The service as the request carries it: a health check with no port is the server's refusal, so it is left out. */
export function serviceWire(s: RunService): RunService {
  if (s.port || !s.health) return s;
  const { health: _health, ...rest } = s;
  void _health;
  return rest;
}

/** One field of the health check. An empty path checks only that the port accepts connections. */
export function setHealth(intent: RunIntent, patch: Partial<NonNullable<RunService["health"]>>): RunIntent {
  const s = intent.service;
  if (!s?.health) return intent;
  const health = { ...s.health, ...patch };
  if (!health.path) delete health.path;
  return set(intent, { ...s, health });
}

/** A field of the form that the server's limits (internal/types/service.go) would refuse. */
export type ServiceField = "port" | "path" | "interval" | "failures";

const inRange = (n: number | undefined, min: number, max: number): boolean => !n || (n >= min && n <= max);

/** Which fields are out of range. The health fields count only while a port is named; 0 or empty is the server's default. */
export function serviceProblems(s: RunService | undefined): Partial<Record<ServiceField, true>> {
  if (!s) return {};
  const out: Partial<Record<ServiceField, true>> = {};
  if (!inRange(s.port, 1, 65535)) out.port = true;
  const h = s.port ? s.health : undefined;
  if (h?.path && !h.path.startsWith("/")) out.path = true;
  if (!inRange(h?.interval_sec, 5, 300)) out.interval = true;
  if (!inRange(h?.failure_threshold, 1, 10)) out.failures = true;
  return out;
}

const SERVICE_CODES = { port: "service_port_invalid", path: "service_path_invalid", interval: "service_interval_invalid", failures: "service_failures_invalid" } as const;

export const serviceIssues = (intent: RunIntent): StepIssue[] =>
  mayKeepRunning(intent) ? (Object.keys(serviceProblems(intent.service)) as ServiceField[]).map((f) => issue("run", SERVICE_CODES[f])) : [];
