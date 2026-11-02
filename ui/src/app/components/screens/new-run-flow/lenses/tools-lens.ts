/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Owns the Tools and access carriers: tools[].model_provider, tools[].tool_rules,
// components and overrides. It never writes inline_policy: a host the person
// adds is overrides.hosts.add_allowed, which works beside policy_id.

import type { PersonalWallsSpec } from "../../../../lib/api/walls";
import type { AccessFacts, AccessHost, AccessRow } from "../../../../lib/types/new-run-access";
import type { SetupItem } from "../../../../lib/types/preflight";
import type { RunIntent } from "../draft/run-intent";
import { withKeys } from "./lens-keys";
import type { StepValidator } from "./step-issues";

export function addAllowedHost(intent: RunIntent, host: string): RunIntent {
  const hosts = intent.overrides?.hosts;
  if (hosts?.add_allowed?.includes(host)) return intent;
  return withKeys(intent, {
    overrides: { ...intent.overrides, hosts: { ...hosts, add_allowed: [...(hosts?.add_allowed ?? []), host] } },
  });
}

/** "Only the sites listed" is on: the run is sent with overrides.egress.allow_all_egress: false. */
export const narrowsEgress = (intent: RunIntent): boolean => intent.overrides?.egress?.allow_all_egress === false;

/** Turns the switch on, or off by leaving the key out (a run never asks for `true` here), pruning what it empties. */
export function setNarrowEgress(intent: RunIntent, on: boolean): RunIntent {
  const { allow_all_egress: _, ...rest } = intent.overrides?.egress ?? {};
  const egress = on ? { ...rest, allow_all_egress: false } : rest;
  const { egress: __, ...others } = intent.overrides ?? {};
  const overrides = Object.keys(egress).length > 0 ? { ...others, egress } : others;
  return withKeys(intent, { overrides: Object.keys(overrides).length > 0 ? overrides : undefined });
}

/**
 * A walls entry covers a host the way the proxy reads an entry: exactly, or a `*.` wildcard's subdomains, and not its bare
 * domain (domainmatch.MatchWild; internal/composer/meet_domains.go covers). Hosts are compared lower-cased on both sides.
 */
const covers = (entry: string, host: string): boolean => {
  const e = entry.toLowerCase();
  const h = host.toLowerCase();
  return e === h || (e.startsWith("*.") && h.endsWith(e.slice(1)));
};

/**
 * The hosts the server says are not allowed that the person's own walls take out of the run: one their
 * "never these sites" covers, or one outside their "only these sites". The server's answer does not name the
 * layer that dropped a host, so the walls the person set are read against it.
 */
export function wallsLeaveOut(walls: PersonalWallsSpec | undefined, access: AccessFacts | undefined, answered: boolean): string[] {
  return leftOutBy(walls, access, answered).map((l) => l.host);
}

/** How many of the answer's hosts the run reaches: all but those the person's walls take out. */
export const reachedHosts = (access: AccessFacts, leftOut: readonly string[]): number => access.hosts.length - leftOut.length;

/** The two host lists of the person's walls, by the name a warning gives them. */
export type WallsHostKnob = "allowed_domains" | "denied_domains";

/** Which of the walls' two host lists take hosts out of this run: "never these sites" for a covered host, "only these sites" for one outside. */
export function wallsLeaveOutKnobs(walls: PersonalWallsSpec | undefined, access: AccessFacts | undefined, answered: boolean): WallsHostKnob[] {
  return [...new Set(leftOutBy(walls, access, answered).flatMap((l) => l.knobs))];
}

function leftOutBy(walls: PersonalWallsSpec | undefined, access: AccessFacts | undefined, answered: boolean): { host: string; knobs: WallsHostKnob[] }[] {
  if (!walls || !access || !answered) return [];
  const denied = walls.denied_domains ?? [];
  return access.hosts
    .filter((h) => h.verdict === "not_allowed")
    .map(({ host }) => ({
      host,
      knobs: [
        ...(denied.some((e) => covers(e, host)) ? (["denied_domains"] as const) : []),
        ...(walls.allowed_domains !== undefined && !walls.allowed_domains.some((e) => covers(e, host)) ? (["allowed_domains"] as const) : []),
      ],
    }))
    .filter((l) => l.knobs.length > 0);
}

export function setModelProvider(intent: RunIntent, toolId: string, provider: string): RunIntent {
  const tools = (intent.tools ?? []).map((t) => (t.id === toolId ? { ...t, model_provider: provider } : t));
  return withKeys(intent, { tools });
}

/** Nothing here blocks locally: the server's setup items become Review blockers. */
export const toolsIssues: StepValidator = () => [];

export type HostBadge = "requested" | "allowed" | "needs_approval" | "not_allowed";
const KNOWN_VERDICTS: readonly string[] = ["allowed", "needs_approval", "not_allowed"];

/**
 * A verdict reads as one only after the server answered this exact body. Before
 * that, and for a verdict this console does not know, a host is only Requested.
 */
export function hostBadge(host: Pick<AccessHost, "verdict">, answered: boolean): HostBadge {
  if (!answered) return "requested";
  return KNOWN_VERDICTS.includes(host.verdict) ? (host.verdict as HostBadge) : "requested";
}

/**
 * The harness that starts with the run, as the server reads it (startsHarness): an interactive harness
 * startup's tool, or a background agent task's agent. No startup, or a command one, starts none.
 */
export const startsWithRun = (intent: RunIntent, harness: string): boolean =>
  intent.startup?.kind === "harness"
    ? intent.startup.tool === harness
    : intent.workload?.kind === "agent_task" && intent.workload.agent === harness;

export type SetupDoor = "sign_in" | "choose" | "add" | "connect";

/** The id Harness setup gives a harness's model-provider control: the Choose door focuses it. */
export const providerControlId = (tool: string): string => `nr-provider-${tool}`;

/**
 * The in-place door for a setup item, if this console has one. An add-secret
 * item has the same door wherever its secret came from: a component, an added
 * secret, or a template or Run again that names one. Signing in to Azure DevOps
 * is the existing connect popup; any other sign-in but a model provider's has
 * no door here yet.
 */
export function setupDoor(item: SetupItem): SetupDoor | undefined {
  const fix = item.fix;
  if (fix?.action === "choose_model_provider") return fix.tool ? "choose" : undefined;
  if (fix?.action === "sign_in") return fix.target === "model_provider" ? "sign_in" : fix.target === "azure_devops" ? "connect" : undefined;
  if (fix?.action === "add_secret") return fix.secret_name ? "add" : undefined;
  return undefined;
}

/** The provider a model-provider sign-in item is for: the one chosen for the harness that needs it. */
export const signInProvider = (intent: RunIntent, item: SetupItem): string | undefined =>
  (intent.tools ?? []).find((t) => t.id === item.required_by)?.model_provider;

export const setupRowId = (item: SetupItem): string => `nr-setup-${item.id}`;

/** Only an item the server reports missing needs setup, here and at Review; satisfied and unverified ones do not. */
export const needsSetup = (item: SetupItem): boolean => item.status === "missing";

/** harness id -> display name, from the shell's setup status. */
export type HarnessNames = ReadonlyMap<string, string>;

/** The server names a harness row by its id; the console says its display name. */
export const rowLabel = (row: Pick<AccessRow, "kind" | "name">, names?: HarnessNames): string =>
  (row.kind === "harness" ? names?.get(row.name) : undefined) ?? row.name;
