/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Owns the Tools and access carriers: tools[].model_provider, tools[].tool_rules,
// components and overrides. It never writes inline_policy: a host the person
// adds is overrides.hosts.add_allowed, which works beside policy_id.

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
