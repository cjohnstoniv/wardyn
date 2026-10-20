/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Owns an application server's ports, inline_policy.ui_apps, and moves a draft between Automation / script and
// Application / server. Ports are kept as numbers; a half-typed one is 0 until it parses, and the issue says so.

import type { UIApp } from "../../../../lib/types/runs";
import type { RunIntent } from "../draft/run-intent";
import { withKeys } from "./lens-keys";
import { issue, type AppProblemCode, type StepIssue } from "./step-issues";

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

/** Mirrors the server's uiAppNameRE (internal/api/policy.go); wire-parity.test.ts keeps the two in step. */
export const APP_NAME_RE = /^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$/;

/** Mirrors the server's validateUIAppPath: empty means "/"; otherwise one leading slash, no "//" and no "..". */
const validPath = (path: string) => path === "" || (path.startsWith("/") && !path.startsWith("//") && !path.includes(".."));

export interface AppProblem {
  code: AppProblemCode;
  name?: string;
  port?: number;
}
export type RowProblems = Partial<Record<"name" | "port" | "path", AppProblem>>;

/** Each row's problems by field: a bad or repeated name, a port out of range or taken, a path the server refuses. */
export function portProblems(apps: readonly UIApp[]): RowProblems[] {
  const names = new Set<string>();
  const ports = new Set<number>();
  return apps.map((a) => {
    const row: RowProblems = {};
    if (!APP_NAME_RE.test(a.name)) row.name = { code: "app_name_invalid" };
    else if (names.has(a.name)) row.name = { code: "app_name_duplicate", name: a.name };
    names.add(a.name);
    if (!validPort(a.port)) row.port = { code: "app_port_invalid" };
    else if (ports.has(a.port)) row.port = { code: "app_port_duplicate", port: a.port };
    else ports.add(a.port);
    if (!validPath(a.path ?? "")) row.path = { code: "app_path_invalid" };
    return row;
  });
}

const FIELDS = ["name", "port", "path"] as const;

/** A server needs one port and every row sound; a failed Continue focuses the first bad row's first bad field. */
export function appIssues(intent: RunIntent): StepIssue[] {
  if (!isServer(intent)) return [];
  const apps = appsOf(intent);
  if (apps.length === 0) return [issue("run", "app_required")];
  const problems = portProblems(apps);
  for (const [i, row] of problems.entries()) {
    const field = FIELDS.find((f) => row[f]);
    const problem = field && row[field];
    if (field && problem) return [{ ...issue("run", problem.code), focus: `nr-app-${field}-${i}`, ...(problem.name !== undefined && { name: problem.name }), ...(problem.port !== undefined && { port: problem.port }) }];
  }
  return [];
}
