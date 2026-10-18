/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Owns the Tools and access carriers: tools[].model_provider, tools[].tool_rules,
// components and overrides. It never writes inline_policy: a host the person
// adds is overrides.hosts.add_allowed, which works beside policy_id.

import type { AccessHost, AccessRow } from "../../../../lib/types/new-run-access";
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
