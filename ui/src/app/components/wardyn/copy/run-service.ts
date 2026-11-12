/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { AgentRun, RunService, ServiceHealthState } from "../../../lib/types";

// The words of a service, a background command Wardyn keeps running: New Run's "Keep it running", the health chip
// on the run list and run page, the Service panel and the Review row. Byte for byte from the approved mocks (r15 T1-a,
// T1-b, T1-c). Lazy-only like the New Run copy: never re-exported through copy.ts.

export const SERVICE_FORM = {
  LABEL: "Keep it running",
  HELP: "Wardyn restarts the command when it exits with an error and checks it on a port inside the sandbox. It runs until it is stopped or reaches the longest life your limits allow.",
  RESTART_LABEL: "Restart",
  RESTART_ON_FAILURE: "Restart when it fails",
  RESTART_NEVER: "Don't restart",
  PORT_LABEL: "Port it listens on",
  PORT_HELP: "127.0.0.1 inside the sandbox.",
  PATH_LABEL: "Health check path",
  PATH_HELP: "Leave empty to check only that the port accepts connections.",
  EVERY_LABEL: "Check every",
  SECONDS: "seconds",
  FAILURES_LABEL: "Unhealthy after",
  FAILED_CHECKS: "failed checks in a row",
  PROBE_LABEL: "Also check it from Wardyn",
  PROBE_HELP: "Every minute Wardyn asks the port itself instead of only trusting the sandbox's report. It reaches the port through the sandbox, so it is not a check from the network outside. The lease is renewed only while both say the service is healthy or starting.",
  PROBE_NEEDS_PORT: "Name a port above to turn this on.",
} as const;

/** The field errors under the form's inputs: the server's limits (internal/types/service.go) said before launch. Not in the mocks. */
export const SERVICE_FORM_ERRORS = {
  PORT_INVALID: "The service port must be from 1 to 65535.",
  PATH_INVALID: "The health check path must start with /, use only URL characters, and be at most 256 characters.",
  EVERY_INVALID: "The health check runs every 5 to 300 seconds.",
  FAILURES_INVALID: "A service is unhealthy after 1 to 10 failed checks.",
} as const;

/** The server's sentence for a service under an idle pause (internal/api/run_service.go servicePauseWarning), byte for byte: New Run finds it in the checks' warnings. Mock r15 T1-a. */
export const SERVICE_PAUSE_WARNING = "Your admin pauses runs nobody is using, so this service will be paused while it waits for requests.";

export const DEFAULT_HEALTH_INTERVAL_SEC = 15;
export const DEFAULT_HEALTH_FAILURES = 3;

export const SERVICE_PANEL = {
  TITLE: "Service",
  RESTART: "Restart",
  RESTART_ON_FAILURE: "When it fails",
  RESTART_NEVER: "Never",
  CHECK: "Health check",
  CHECK_HTTP: (path: string, port: number, sec: number) => `GET ${path} on port ${port} every ${sec} s`,
  CHECK_TCP: (port: number, sec: number) => `Port ${port} accepts connections, every ${sec} s`,
  CHECK_NONE: "None",
  HEALTH: "Health",
  CHECKED: (ago: string) => `checked ${ago}`,
  WAITING_FIRST_CHECK: "waiting for the first check",
  RESTARTS: "Restarts",
  RESTARTS_WITH_EXIT: (n: number, code: number) => `${n} · last exit code ${code}`,
  LEASE: "Lease",
  LEASE_RENEWED: (ends: string) => `Renewed while healthy · ends ${ends} if it stays unhealthy`,
  LEASE_NONE: "No end",
  NO_CAP: "No lifetime limit applies to this run, so Wardyn will not keep renewing it: the service ends at its lease like any run. Ask your admin to set a lifetime limit on the pool to keep a service up.",
  REASON_RESTARTING: "The service exited with an error. It restarts after a pause that grows up to 5 minutes.",
  REASON_PROBE_RESTARTED: (failures: number) => `The health check failed ${failures} times in a row, so the service was restarted.`,
  REASON_PROBE_FAILING: "The health check is failing.",
  REASON_OLD_IMAGE: "This image's runner tools are older than services, so the command is not restarted or checked. Wrap the image again with this release.",
  REASON_UNREACHABLE: "Wardyn could not read the service's health. Its runner may be offline.",
  FOOTNOTE: "Health is reported from inside the sandbox.",
  // Wardyn's own check (service.health.probe: control_plane): its row, its findings, the notes and the lease.
  SANDBOX_SAYS: "Sandbox says",
  PROBE: "Wardyn's check",
  PROBE_HTTP: (path: string, status: number) => `GET ${path} answered ${status}.`,
  PROBE_TCP: (port: number) => `Port ${port} accepted a connection.`,
  PROBE_REFUSED: (port: number) => `Port ${port} refused the connection.`,
  PROBE_TIMEOUT: "No answer within 5 s.",
  PROBE_BAD_ANSWER: "The answer was not an HTTP status line Wardyn could read.",
  PROBE_FIRST_FAIL: (why: string, n: number) => `The last check failed: ${why} Unhealthy after ${n} in a row.`,
  PAUSED: "Not checked while the run is paused.",
  PROBE_NONE_YET: "No result of Wardyn's check has been received yet.",
  DISAGREE_PROBE_FAILS: (port: number) => `The sandbox says the service is healthy, but Wardyn's check of port ${port} failed. The lease is not renewed while the two disagree.`,
  DISAGREE_SANDBOX_FAILS: (port: number) => `The sandbox says the service is unhealthy, but port ${port} answers Wardyn's check. The lease is not renewed while the two disagree.`,
  LEASE_BOTH: (ends: string) => `Renewed while both checks say healthy or starting · ends ${ends} if renewal stops`,
  LEASE_NOT_NOW: (ends: string) => `Not renewed now · ends ${ends}`,
  FOOTNOTE_PROBE: "Wardyn's check reaches the port through the sandbox, not from the network outside it. A service that answers its health check passes, whatever else is wrong with it. The sandbox's health is its own report.",
} as const;

/** Whether the frozen spec opted in to Wardyn's own check of the port. */
export const probeOn = (s: RunService | undefined): boolean => s?.health?.probe === "control_plane";

/** Worst first, as Go's types.WorseServiceHealth: a word it does not know ranks as unknown. */
const HEALTH_RANK: Record<ServiceHealthState, number> = { unhealthy: 0, unknown: 1, starting: 2, healthy: 3 };
const rankOf = (h: string): number => HEALTH_RANK[h as ServiceHealthState] ?? HEALTH_RANK.unknown;

/** The worse of two readings: unhealthy, then unknown, starting, healthy. */
export function worseHealth(a: string, b: string | undefined): ServiceHealthState {
  const w = b !== undefined && rankOf(b) < rankOf(a) ? b : a;
  return w in HEALTH_RANK ? (w as ServiceHealthState) : "unknown";
}

// Go's serviceSupervisorGrace and serviceStatusStale (internal/api/run_service_health.go, run_service_lease.go).
export const SERVICE_SUPERVISOR_GRACE_MS = 10 * 60_000;
export const SERVICE_STATUS_STALE_MS = 15 * 60_000;

type ServiceRunTimes = Partial<Pick<AgentRun, "created_at" | "admitted_at">>;

/** A reading not yet made: starting inside the supervisor's grace from the run's start, unknown after it (Go's rule). */
function missingHealth(run: ServiceRunTimes, now: number): ServiceHealthState {
  const since = Date.parse(run.admitted_at ?? run.created_at ?? "");
  return now - since < SERVICE_SUPERVISOR_GRACE_MS ? "starting" : "unknown";
}

/**
 * Wardyn's check as it is shown, for a spec that opted in: its reading, or with none yet the grace rule renewal reads
 * (Go's probeRenewalHealth). Undefined when the spec did not opt in.
 */
export function shownProbeHealth(run: ServiceRunTimes & Pick<AgentRun, "service" | "service_status">, now: number): ServiceHealthState | undefined {
  if (!probeOn(run.service)) return undefined;
  const p = run.service_status?.probe;
  return p ? worseHealth(p.health, undefined) : missingHealth(run, now);
}

/**
 * Whether the service's health lets its lease renew now: Go's renewableServiceHealth over the readings the API serves
 * (a paused or stale one already reads unknown). Each reading must be starting or healthy; a missing one counts as
 * starting inside the supervisor's grace from the run's start and unknown after it; Wardyn's check counts only when
 * the spec opted in. The lease line's "Not renewed now" is this function's false, nothing else.
 */
export function serviceHealthRenews(run: ServiceRunTimes & Pick<AgentRun, "service" | "service_status">, now: number): boolean {
  const read = (r: { health: string; observed_at: string } | undefined): string => {
    if (!r) return missingHealth(run, now);
    return now - Date.parse(r.observed_at) > SERVICE_STATUS_STALE_MS ? "unknown" : r.health;
  };
  const renews = (h: string) => h === "starting" || h === "healthy";
  const st = run.service_status;
  return renews(read(st)) && (!probeOn(run.service) || renews(read(st?.probe)));
}

/** "40 s ago", "6 min ago", "2 h ago": the panel's own clock, finer than the board's "just now". */
export function agoText(iso: string, now: number): string {
  const sec = Math.max(0, Math.round((now - Date.parse(iso)) / 1000));
  if (sec < 60) return `${sec} s ago`;
  const min = Math.round(sec / 60);
  return min < 60 ? `${min} min ago` : `${Math.round(min / 60)} h ago`;
}

/** The Review row: Service · restarts when it fails · checks port 8080 /healthz every 15 s. */
export function serviceReviewLine(s: RunService): string {
  const parts = ["Service", s.restart === "never" ? "doesn't restart" : "restarts when it fails"];
  // No health block runs no probe, so the line says nothing about a check.
  if (s.port && s.health) {
    const h = s.health;
    parts.push(`checks port ${s.port}${h?.path ? ` ${h.path}` : ""} every ${h?.interval_sec || DEFAULT_HEALTH_INTERVAL_SEC} s`);
    if (probeOn(s)) parts.push("Wardyn also checks it every minute");
  }
  return parts.join(" · ");
}

type BadgeTone = "success" | "warning" | "info" | "neutral";
export interface ServiceBadge {
  word: string;
  tone: BadgeTone;
  /** A live word pulses; "Health unknown" does not. */
  pulse: boolean;
}

/** The health word a state is drawn with, without the restart suffix. */
export const SERVICE_HEALTH: Record<"starting" | "healthy" | "unhealthy" | "unknown", ServiceBadge> = {
  starting: { word: "Starting", tone: "info", pulse: true },
  healthy: { word: "Healthy", tone: "success", pulse: true },
  unhealthy: { word: "Unhealthy", tone: "warning", pulse: true },
  unknown: { word: "Health unknown", tone: "neutral", pulse: false },
};

/**
 * What a live service's chip says in place of "Running", on the run list and the run page: its health, then
 * "· 1 restart" or "· N restarts". A service with no observation yet reads as starting, the way wardynd counts it.
 * With Wardyn's own check opted in, the word is the worse of the two, a missing check reading counted as the panel
 * shows it (shownProbeHealth). Absent for any other run, and for a run that is not RUNNING.
 */
export function serviceBadge(run: ServiceRunTimes & Pick<AgentRun, "state" | "service" | "service_status">, now = Date.now()): ServiceBadge | undefined {
  if (run.state !== "RUNNING" || !run.service) return undefined;
  const st = run.service_status;
  const sandbox = st ? st.health : "starting";
  const base = SERVICE_HEALTH[worseHealth(sandbox, probeOn(run.service) ? shownProbeHealth(run, now) : st?.probe?.health)];
  const n = st?.restarts ?? 0;
  return n > 0 ? { ...base, word: `${base.word} · ${n} ${n === 1 ? "restart" : "restarts"}` } : base;
}
