/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A run's service: its frozen spec (Go internal/types/service.go) and the health wardynd last read. Re-exported by
// ./runs, which AgentRun carries them in.

/** Go: types.RunService. A service is a background run of a command that wardynd keeps running. */
export interface RunService {
  restart: "on_failure" | "never";
  /** Listens on 127.0.0.1 inside the sandbox; absent = none declared. */
  port?: number;
  /** Needs `port`: an HTTP GET of `path` (2xx/3xx passes), or a TCP connect without one. */
  health?: ServiceHealth;
}

/** Go: types.ServiceHealth. */
export interface ServiceHealth {
  path?: string;
  interval_sec: number;
  failure_threshold: number;
  probe?: "control_plane"; // absent = off; wardynd also checks the port itself, through the sandbox
}

/** Go: types.ServiceHealthState. */
export type ServiceHealthState = "starting" | "healthy" | "unhealthy" | "unknown";

/** Go: types.RunServiceStatus. `reason` is closed: restarting | probe_failing | supervisor_missing | unreachable | paused. */
export interface RunServiceStatus {
  health: ServiceHealthState;
  reason?: string;
  restarts: number;
  last_exit_code?: number;
  /** wardynd's clock, never the sandbox's. */
  observed_at: string;
  probe?: RunServiceProbe; // Wardyn's own check, once it has run on a spec that opted in
}

/** Go: types.RunServiceProbe. `reason`: refused | timeout | bad_status | bad_answer | not_run, on the wire paused | unreachable. */
export interface RunServiceProbe {
  health: ServiceHealthState;
  reason?: string;
  http_status?: number;
  failures: number; // in a row
  observed_at: string;
}
