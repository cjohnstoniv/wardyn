/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import type { RunIntent } from "../draft/run-intent";
import { addApp, appIssues, appsOf, chooseAppKind, commandOf, editApp, isServer, MAX_APPS, removeApp, setApps, setCommand } from "./apps-lens";
import { stepIssues } from "./local-issues";
import { chooseShape } from "./shape-lens";

const SCRIPT: RunIntent = { shape: "application", experience: "background", workload: { kind: "command", command: "make nightly" }, runner_pool_id: "p1" };
const SERVER: RunIntent = {
  shape: "application", experience: "interactive", startup: { kind: "command", command: "npm run dev" },
  inline_policy: { ui_apps: [{ name: "web", port: 3000, path: "/" }] },
};

describe("Application / server carriers (#1994)", () => {
  it("choosing the server card makes an interactive run that starts the command and declares one port", () => {
    const next = chooseAppKind(SCRIPT, "server");
    expect(next).toMatchObject({
      shape: "application", experience: "interactive", startup: { kind: "command", command: "make nightly" },
      inline_policy: { ui_apps: [{ name: "web", port: 3000, path: "/" }] }, runner_pool_id: "p1",
    });
    expect(next).not.toHaveProperty("workload");
    expect(isServer(next)).toBe(true);
  });

  it("choosing the script card again gives back the background command and no ports, and no empty overlay", () => {
    const next = chooseAppKind(SERVER, "automation");
    expect(next).toMatchObject({ experience: "background", workload: { kind: "command", command: "npm run dev" } });
    expect(next).not.toHaveProperty("startup");
    expect(next).not.toHaveProperty("inline_policy");
    expect(chooseAppKind(SERVER, "server")).toBe(SERVER);
    expect(chooseAppKind(SCRIPT, "automation")).toBe(SCRIPT);
  });

  it("keeps the other policy fields when the ports come and go", () => {
    const withHosts: RunIntent = { ...SERVER, inline_policy: { ...SERVER.inline_policy, allowed_domains: ["a.example"] } };
    expect(chooseAppKind(withHosts, "automation").inline_policy).toEqual({ allowed_domains: ["a.example"] });
    expect(setApps(withHosts, [{ name: "api", port: 8080 }]).inline_policy).toEqual({ allowed_domains: ["a.example"], ui_apps: [{ name: "api", port: 8080 }] });
  });

  it("writes the command to the carrier the card uses", () => {
    expect(setCommand(SERVER, "pnpm dev").startup).toEqual({ kind: "command", command: "pnpm dev" });
    expect(setCommand(SCRIPT, "make").workload).toEqual({ kind: "command", command: "make" });
    expect(commandOf(SERVER)).toBe("npm run dev");
    expect(commandOf(SCRIPT)).toBe("make nightly");
  });

  it("takes the ports along when the shape changes away from the server", () => {
    const env = chooseShape(SERVER, "environment");
    expect(env).toMatchObject({ shape: "environment", experience: "interactive" });
    expect(env).not.toHaveProperty("startup");
    expect(env).not.toHaveProperty("inline_policy");
    expect(chooseShape(SCRIPT, "environment")).toEqual(expect.not.objectContaining({ inline_policy: expect.anything() }));
  });

  it("adds named blank rows up to eight, edits one row, and removes one", () => {
    let intent = SERVER;
    for (let n = 0; n < 10; n++) intent = addApp(intent);
    expect(appsOf(intent)).toHaveLength(MAX_APPS);
    expect(appsOf(intent)[1]).toEqual({ name: "app2", port: 0, path: "/" });
    expect(appsOf(editApp(SERVER, 0, { port: 8080 }))[0]).toEqual({ name: "web", port: 8080, path: "/" });
    expect(appsOf(removeApp(SERVER, 0))).toEqual([]);
  });

  it("names an added port with an appN no other row has", () => {
    let intent = chooseAppKind(SCRIPT, "server");
    intent = addApp(addApp(intent));
    intent = addApp(removeApp(intent, 1));
    const names = appsOf(intent).map((a) => a.name);
    expect(new Set(names).size).toBe(names.length);
  });
});

describe("Application / server issues", () => {
  const apps = (...ports: number[]): RunIntent => setApps(SERVER, ports.map((port, i) => ({ name: `a${i}`, port, path: "/" })));

  it("passes sound ports and ignores a script", () => {
    expect(appIssues(apps(3000, 8080))).toEqual([]);
    expect(appIssues(SCRIPT)).toEqual([]);
  });

  it("flags an empty, zero or out-of-range port and focuses that row", () => {
    expect(appIssues(apps(3000, 70000))).toEqual([{ home: "run", code: "app_port_invalid", focus: "nr-app-port-1" }]);
    expect(appIssues(apps(0))[0]).toMatchObject({ code: "app_port_invalid", focus: "nr-app-port-0" });
    expect(appIssues(apps(1, 65535))).toEqual([]);
    expect(appIssues(apps(65536))[0]).toMatchObject({ code: "app_port_invalid" });
  });

  it("flags the later of two rows that share a port, naming the port", () => {
    expect(appIssues(apps(3000, 3000))).toEqual([{ home: "run", code: "app_port_duplicate", focus: "nr-app-port-1", port: 3000 }]);
    expect(appIssues(apps(8080, 3000, 3000))[0]).toMatchObject({ focus: "nr-app-port-2", port: 3000 });
  });

  it("needs at least one port, and the Run step reports it with the command", () => {
    const none = removeApp(SERVER, 0);
    expect(appIssues(none).map((i) => i.code)).toEqual(["app_required"]);
    expect(stepIssues("run", setCommand(none, " ")).map((i) => i.code)).toEqual(["command_required", "app_required"]);
  });
});
