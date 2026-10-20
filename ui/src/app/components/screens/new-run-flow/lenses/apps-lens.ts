/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Owns an application server's ports, inline_policy.ui_apps, and moves a draft between Automation / script and
// Application / server. Ports are kept as numbers; a half-typed one is 0 until it parses, and the issue says so.

import type { UIApp } from "../../../../lib/types/runs";
import type { RunIntent } from "../draft/run-intent";
import { withKeys } from "./lens-keys";
import { issue, type StepIssue } from "./step-issues";

export const MAX_APPS = 8;
export type AppKind = "automation" | "server";

/** The first port a new server offers; the person sees and edits it before anything runs. */
const FIRST_APP: UIApp = { name: "web", port: 3000, path: "/" };

/** An Application / server: an interactive run that starts a command and declares the ports it opens (#1994). */
export const isServer = (intent: RunIntent): boolean => intent.shape === "application" && intent.experience === "interactive";

export const appsOf = (intent: RunIntent): UIApp[] => intent.inline_policy?.ui_apps ?? [];

/** The command the shape runs: the workload's for a script, the startup's for a server. */
export const commandOf = (intent: RunIntent): string | undefined =>
  isServer(intent) ? intent.startup?.command : intent.workload?.command;

export const setCommand = (intent: RunIntent, command: string): RunIntent =>
  isServer(intent)
    ? withKeys(intent, { startup: { kind: "command", command } })
    : withKeys(intent, { workload: { kind: "command", command } });

/** Replaces the declared ports. No ports removes the key, and an overlay left empty goes with it. */
export function setApps(intent: RunIntent, apps: UIApp[]): RunIntent {
  const { ui_apps: _dropped, ...rest } = intent.inline_policy ?? {};
  void _dropped;
  const policy = apps.length ? { ...rest, ui_apps: apps } : rest;
  return withKeys(intent, { inline_policy: Object.keys(policy).length ? policy : undefined });
}

/** Switches an Application between its two cards, carrying the command across and leaving no ports behind a script. */
export function chooseAppKind(intent: RunIntent, kind: AppKind): RunIntent {
  const command = commandOf(intent) ?? "";
  if (kind === "server") {
    if (isServer(intent)) return intent;
    const moved = withKeys(intent, { experience: "interactive", workload: undefined, startup: { kind: "command", command } });
    return setApps(moved, appsOf(intent).length ? appsOf(intent) : [FIRST_APP]);
  }
  if (!isServer(intent)) return intent;
  const moved = withKeys(intent, { experience: "background", workload: { kind: "command", command }, startup: undefined });
  return setApps(moved, []);
}

/** The first appN name no row has: the server refuses two apps with one name. */
function freeAppName(apps: readonly UIApp[]): string {
  const taken = new Set(apps.map((a) => a.name));
  let n = apps.length + 1;
  while (taken.has(`app${n}`)) n++;
  return `app${n}`;
}

export const addApp = (intent: RunIntent): RunIntent =>
  appsOf(intent).length >= MAX_APPS ? intent : setApps(intent, [...appsOf(intent), { name: freeAppName(appsOf(intent)), port: 0, path: "/" }]);

export const editApp = (intent: RunIntent, index: number, patch: Partial<UIApp>): RunIntent =>
  setApps(intent, appsOf(intent).map((a, i) => (i === index ? { ...a, ...patch } : a)));

export const removeApp = (intent: RunIntent, index: number): RunIntent =>
  setApps(intent, appsOf(intent).filter((_, i) => i !== index));

const validPort = (port: number) => Number.isInteger(port) && port >= 1 && port <= 65535;

/** Each port's problem: out of range first, then a port an earlier row already takes. */
export function portProblems(apps: readonly UIApp[]): ({ code: "app_port_invalid" } | { code: "app_port_duplicate"; port: number } | undefined)[] {
  const seen = new Set<number>();
  return apps.map((a) => {
    if (!validPort(a.port)) return { code: "app_port_invalid" };
    if (seen.has(a.port)) return { code: "app_port_duplicate", port: a.port };
    seen.add(a.port);
    return undefined;
  });
}

/** A server needs one port and every port sound; a failed Continue focuses the first bad row's port. */
export function appIssues(intent: RunIntent): StepIssue[] {
  if (!isServer(intent)) return [];
  const apps = appsOf(intent);
  if (apps.length === 0) return [issue("run", "app_required")];
  const problems = portProblems(apps);
  const at = problems.findIndex(Boolean);
  const problem = problems[at];
  if (!problem) return [];
  return [{ ...issue("run", problem.code), focus: `nr-app-port-${at}`, ...(problem.code === "app_port_duplicate" && { port: problem.port }) }];
}
