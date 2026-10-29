/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { agoText, serviceBadge, serviceReviewLine, SERVICE_FORM, SERVICE_PANEL, SERVICE_PAUSE_WARNING } from "./run-service";

// The approved mocks (r15 T1-a, T1-b, T1-c): the strings here are the canon, spelled out so a reword fails.
describe("service canon strings", () => {
  it("New Run's Keep it running", () => {
    expect(SERVICE_FORM).toEqual({
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
    });
  });

  it("the pause sentence is byte-equal to the server's", () => {
    const go = readFileSync(resolve(process.cwd(), "../internal/api/run_service.go"), "utf8");
    expect(/const servicePauseWarning = "([^"]*)"/.exec(go)?.[1]).toBe(SERVICE_PAUSE_WARNING);
  });

  it("the Service panel, values in sentence case", () => {
    const P = SERVICE_PANEL;
    expect([P.TITLE, P.RESTART, P.RESTART_ON_FAILURE, P.RESTART_NEVER, P.CHECK, P.CHECK_NONE, P.HEALTH, P.RESTARTS, P.LEASE, P.LEASE_NONE]).toEqual([
      "Service", "Restart", "When it fails", "Never", "Health check", "None", "Health", "Restarts", "Lease", "No end",
    ]);
    expect(P.CHECK_HTTP("/healthz", 8080, 15)).toBe("GET /healthz on port 8080 every 15 s");
    expect(P.CHECK_TCP(8080, 15)).toBe("Port 8080 accepts connections, every 15 s");
    expect(P.CHECKED("40 s ago")).toBe("checked 40 s ago");
    expect(P.RESTARTS_WITH_EXIT(3, 1)).toBe("3 · last exit code 1");
    expect(P.LEASE_RENEWED("Tue 9:40 AM")).toBe("Renewed while healthy · ends Tue 9:40 AM if it stays unhealthy");
    expect(P.REASON_RESTARTING).toBe("The service exited with an error. It restarts after a pause that grows up to 5 minutes.");
    expect(P.REASON_PROBE_RESTARTED(3)).toBe("The health check failed 3 times in a row, so the service was restarted.");
    expect(P.REASON_PROBE_FAILING).toBe("The health check is failing.");
    expect(P.REASON_OLD_IMAGE).toBe("This image's runner tools are older than services, so the command is not restarted or checked. Wrap the image again with this release.");
    expect(P.REASON_UNREACHABLE).toBe("Wardyn could not read the service's health. Its runner may be offline.");
    expect(P.FOOTNOTE).toBe("Health is reported from inside the sandbox.");
  });

  it("the Review row", () => {
    const health = { path: "/healthz", interval_sec: 15, failure_threshold: 3 };
    expect(serviceReviewLine({ restart: "on_failure", port: 8080, health })).toBe("Service · restarts when it fails · checks port 8080 /healthz every 15 s");
    expect(serviceReviewLine({ restart: "never", port: 8080, health })).toBe("Service · doesn't restart · checks port 8080 /healthz every 15 s");
    expect(serviceReviewLine({ restart: "on_failure", port: 8080, health: { interval_sec: 15, failure_threshold: 3 } })).toBe("Service · restarts when it fails · checks port 8080 every 15 s");
    expect(serviceReviewLine({ restart: "on_failure", port: 8080 })).toBe("Service · restarts when it fails");
    expect(serviceReviewLine({ restart: "on_failure" })).toBe("Service · restarts when it fails");
  });
});

describe("serviceBadge", () => {
  const at = new Date().toISOString();
  const run = (service_status?: { health: "starting" | "healthy" | "unhealthy" | "unknown"; restarts: number }) =>
    ({ state: "RUNNING", service: { restart: "on_failure" }, service_status: service_status && { ...service_status, observed_at: at } }) as Parameters<typeof serviceBadge>[0];

  it("says each health word with its tone, and the restart count in the board's suffix", () => {
    expect(serviceBadge(run())).toEqual({ word: "Starting", tone: "info", pulse: true });
    expect(serviceBadge(run({ health: "healthy", restarts: 0 }))).toEqual({ word: "Healthy", tone: "success", pulse: true });
    expect(serviceBadge(run({ health: "healthy", restarts: 3 }))?.word).toBe("Healthy · 3 restarts");
    expect(serviceBadge(run({ health: "unhealthy", restarts: 1 }))).toEqual({ word: "Unhealthy · 1 restart", tone: "warning", pulse: true });
    expect(serviceBadge(run({ health: "unknown", restarts: 0 }))).toEqual({ word: "Health unknown", tone: "neutral", pulse: false });
  });

  it("is absent for a run that is not a live service", () => {
    expect(serviceBadge({ state: "RUNNING" })).toBeUndefined();
    expect(serviceBadge({ state: "FAILED", service: { restart: "never" } })).toBeUndefined();
  });

  it("reads a health word it does not know as unknown, never healthy", () => {
    expect(serviceBadge({ state: "RUNNING", service: { restart: "never" }, service_status: { health: "weird" as "unknown", restarts: 0, observed_at: at } })?.word).toBe("Health unknown");
  });
});

describe("agoText", () => {
  const now = Date.parse("2020-10-29T12:00:00Z");
  it.each([
    ["2020-10-29T11:59:20Z", "40 s ago"],
    ["2020-10-29T11:54:00Z", "6 min ago"],
    ["2020-10-29T09:00:00Z", "3 h ago"],
    ["2020-10-29T12:00:05Z", "0 s ago"],
  ])("%s reads %s", (iso, text) => expect(agoText(iso, now)).toBe(text));
});
