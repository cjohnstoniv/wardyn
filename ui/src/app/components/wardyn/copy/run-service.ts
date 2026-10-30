/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { AgentRun, RunService } from "../../../lib/types";

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
} as const;

/** The field errors under the form's inputs: the server's limits (internal/types/service.go) said before launch. Not in the mocks. */
export const SERVICE_FORM_ERRORS = {
  PORT_INVALID: "The service port must be from 1 to 65535.",
  PATH_INVALID: "The health check path must start with /.",
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
} as const;

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
 * Absent for any other run, and for a run that is not RUNNING.
 */
export function serviceBadge(run: Pick<AgentRun, "state" | "service" | "service_status">): ServiceBadge | undefined {
  if (run.state !== "RUNNING" || !run.service) return undefined;
  const st = run.service_status;
  const base = SERVICE_HEALTH[st?.health ?? "starting"] ?? SERVICE_HEALTH.unknown;
  const n = st?.restarts ?? 0;
  return n > 0 ? { ...base, word: `${base.word} · ${n} ${n === 1 ? "restart" : "restarts"}` } : base;
}
