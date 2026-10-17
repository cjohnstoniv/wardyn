/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Harness setup's edits to the draft: each returns a new intent and touches only
// the harness's own tool entry (or one component's MCP binding).

import type { ComponentRef } from "../../../../../lib/types/components";
import type { ToolRule } from "../../../../../lib/types/policy";
import { withKeys } from "../../lenses/lens-keys";
import type { RunIntent } from "../../draft/run-intent";

type Effect = ToolRule["effect"];

/** Personal edits on one included tool; an emptied rule list is dropped, never sent as []. */
function editTool(intent: RunIntent, toolId: string, edit: (t: NonNullable<RunIntent["tools"]>[number]) => NonNullable<RunIntent["tools"]>[number]): RunIntent {
  return withKeys(intent, { tools: (intent.tools ?? []).map((t) => (t.id === toolId ? edit(t) : t)) });
}

/** "Run it" is no personal default: the key is removed, never sent as allow (DS-7 3.1). */
export function setDefaultEffect(intent: RunIntent, toolId: string, effect: Effect): RunIntent {
  return editTool(intent, toolId, (t) => {
    const { default_effect: _dropped, ...rest } = t;
    return effect === "allow" ? rest : { ...rest, default_effect: effect };
  });
}

export const addToolRule = (intent: RunIntent, toolId: string, rule: ToolRule): RunIntent =>
  editTool(intent, toolId, (t) => ({ ...t, tool_rules: [...(t.tool_rules ?? []), rule] }));

export function removeToolRule(intent: RunIntent, toolId: string, tool: string): RunIntent {
  return editTool(intent, toolId, (t) => {
    const { tool_rules, ...rest } = t;
    const left = (tool_rules ?? []).filter((r) => r.tool !== tool);
    return left.length ? { ...rest, tool_rules: left } : rest;
  });
}

/** A component entry as step 5 writes it: the harnesses its MCP server is turned on for. Absent leaves the server's own default. */
export type McpComponentRef = ComponentRef & { mcp_harnesses?: string[] };

export function setMcpHarnesses(intent: RunIntent, componentId: string, harnesses: string[]): RunIntent {
  const components = ((intent.components ?? []) as McpComponentRef[]).map((c) => (c.id === componentId ? { ...c, mcp_harnesses: harnesses } : c));
  return withKeys(intent, { components });
}

export const personalRules = (intent: RunIntent, toolId: string): ToolRule[] =>
  (intent.tools ?? []).find((t) => t.id === toolId)?.tool_rules ?? [];
