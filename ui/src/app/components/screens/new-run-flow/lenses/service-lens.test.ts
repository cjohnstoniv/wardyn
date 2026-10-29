/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import type { RunService } from "../../../../lib/types/runs";
import type { RunIntent } from "../draft/run-intent";
import { keepRunning, serviceIssues, serviceProblems, serviceWire, setHealth, setPort } from "./service-lens";

const SCRIPT: RunIntent = { shape: "application", experience: "background", workload: { kind: "command", command: "./serve" } };
const health = { path: "/ready", interval_sec: 30, failure_threshold: 5 };
const svc = (over: Partial<RunService> = {}): RunService => ({ restart: "on_failure", port: 8080, health, ...over });
const withService = (service: RunService): RunIntent => ({ ...SCRIPT, service });

describe("setPort", () => {
  it("keeps the typed check while the port is emptied, and gives it back with the next port", () => {
    const emptied = setPort(withService(svc()), 0);
    expect(emptied.service).toEqual({ restart: "on_failure", health });
    expect(setPort(emptied, 9090).service).toEqual({ restart: "on_failure", port: 9090, health });
  });

  it("names a port with the check's defaults when there was none", () => {
    expect(setPort(keepRunning(SCRIPT, true), 8080).service).toEqual({ restart: "on_failure", port: 8080, health: { interval_sec: 15, failure_threshold: 3 } });
  });

  it("turning Keep it running off drops the check with the service", () => {
    expect(keepRunning(withService(svc()), false)).not.toHaveProperty("service");
  });
});

describe("serviceWire", () => {
  it("leaves the check out while no port is named, and sends it otherwise", () => {
    expect(serviceWire({ restart: "never", health })).toEqual({ restart: "never" });
    expect(serviceWire(svc())).toEqual(svc());
    expect(serviceWire({ restart: "never" })).toEqual({ restart: "never" });
  });
});

describe("serviceProblems: the limits of internal/types/service.go", () => {
  it("accepts the edges and the server's defaults (0)", () => {
    expect(serviceProblems(svc({ port: 1, health: { path: "/", interval_sec: 5, failure_threshold: 1 } }))).toEqual({});
    expect(serviceProblems(svc({ port: 65535, health: { interval_sec: 300, failure_threshold: 10 } }))).toEqual({});
    expect(serviceProblems(svc({ health: { interval_sec: 0, failure_threshold: 0 } }))).toEqual({});
    expect(serviceProblems({ restart: "never" })).toEqual({});
    expect(serviceProblems(undefined)).toEqual({});
  });

  it("flags each field outside its range", () => {
    expect(serviceProblems(svc({ port: 70000 }))).toEqual({ port: true });
    expect(serviceProblems(svc({ health: { ...health, path: "healthz" } }))).toEqual({ path: true });
    expect(serviceProblems(svc({ health: { ...health, interval_sec: 4 } }))).toEqual({ interval: true });
    expect(serviceProblems(svc({ health: { ...health, interval_sec: 301 } }))).toEqual({ interval: true });
    expect(serviceProblems(svc({ health: { ...health, failure_threshold: 11 } }))).toEqual({ failures: true });
  });

  it("does not hold the check to its limits while no port is named", () => {
    expect(serviceProblems({ restart: "on_failure", health: { path: "x", interval_sec: 1, failure_threshold: 99 } })).toEqual({});
  });
});

describe("serviceIssues", () => {
  it("is one issue per bad field on the Run step, and none for a service the run cannot carry", () => {
    const bad = withService(svc({ port: 70000, health: { ...health, path: "x" } }));
    expect(serviceIssues(bad).map((i) => [i.home, i.code])).toEqual([["run", "service_port_invalid"], ["run", "service_path_invalid"]]);
    expect(serviceIssues({ ...bad, experience: "interactive" })).toEqual([]);
    expect(serviceIssues(setHealth(withService(svc()), { path: "/ok" }))).toEqual([]);
  });
});
