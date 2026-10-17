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

/**
 * The draft's component an MCP row stands for: a stored one by its id, an inline one by the ordinal the
 * server numbered it with (`inline:<n>`, its place in `components`). An inline row must still name the
 * component now at that place, else it is stale and stands for none.
 */
export function mcpComponentIndex(intent: RunIntent, componentId: string, name: string): number {
  const refs = (intent.components ?? []) as McpComponentRef[];
  const inline = /^inline:(\d+)$/.exec(componentId);
  if (!inline) return refs.findIndex((c) => c.id === componentId);
  const at = Number(inline[1]);
  const ref = refs[at];
  return ref && !ref.id && !ref.builtin && ref.inline && ref.name === name ? at : -1;
}

/** Turns an MCP server on for exactly `harnesses`. A row that stands for no component of the draft changes nothing. */
export function setMcpHarnesses(intent: RunIntent, componentId: string, name: string, harnesses: string[]): RunIntent {
  const at = mcpComponentIndex(intent, componentId, name);
  if (at < 0) return intent;
  const components = ((intent.components ?? []) as McpComponentRef[]).map((c, i) => (i === at ? { ...c, mcp_harnesses: harnesses } : c));
  return withKeys(intent, { components });
}

export const personalRules = (intent: RunIntent, toolId: string): ToolRule[] =>
  (intent.tools ?? []).find((t) => t.id === toolId)?.tool_rules ?? [];
