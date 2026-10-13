/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Harness setup's fixtures: the `access.harnesses` facts as the server builds
// them for each lane (internal/api toolPolicyFact), plus the model providers
// and one MCP server row they sit beside.

import type { AccessFacts, AccessRow, EffectOption, ToolPolicyFact } from "../../types/new-run-access";
import type { SetupModelProvider, SetupProviderAccess } from "../../types/setup";

const EFFECTS = ["allow", "hold", "deny"] as const;

/** Each effect available, except those named with their reason. */
const options = (cannot: Record<string, string> = {}): EffectOption[] =>
  EFFECTS.map((effect) => (cannot[effect] ? { effect, available: false, reason: cannot[effect] } : { effect, available: true }));

const fact = (harness: string, over: Partial<ToolPolicyFact> & { cannotDefault?: Record<string, string>; cannotRule?: Record<string, string> } = {}): ToolPolicyFact => {
  const { cannotDefault, cannotRule, ...rest } = over;
  return {
    harness, lane: "ungated", enforcer: "none",
    default: { effective: "allow", source: { kind: "harness" }, options: options(cannotDefault) },
    policy_rules: [], rule_effects: options(cannotRule), rule_tools: "any", mcp_servers: [],
    ...rest,
  };
};

/** Claude Code on the lane that runs every tool: nothing held, nothing locked. */
export const CLAUDE_UNGATED: ToolPolicyFact = fact("claude-code");

/** Claude Code on the gate: an unnamed tool is held, so Run it is stricter than the lane. */
export const CLAUDE_GATED: ToolPolicyFact = fact("claude-code", {
  lane: "gated", enforcer: "wardyn_gate",
  default: { effective: "hold", source: { kind: "harness" }, options: options({ allow: "policy_stricter_default" }) },
  cannotRule: { allow: "policy_stricter_tool" },
});

/** The gate, with the policy's rows: WebFetch blocked, Bash asks first. */
export const CLAUDE_GATED_LOCKED: ToolPolicyFact = {
  ...CLAUDE_GATED,
  policy_rules: [
    { tool: "WebFetch", effect: "deny", source: { kind: "policy", name: "Team policy" } },
    { tool: "Bash", effect: "hold", source: { kind: "policy", name: "Team policy" } },
  ],
};

/** Policy rows on a lane that never consults them: they must not read as enforced. */
export const CLAUDE_UNGATED_LOCKED: ToolPolicyFact = {
  ...CLAUDE_UNGATED,
  policy_rules: [{ tool: "WebFetch", effect: "deny", source: { kind: "policy", name: "Team policy" } }],
};

/** Claude Code in an interactive run: it asks the person, and may not skip asking. */
export const CLAUDE_ATTENDED: ToolPolicyFact = fact("claude-code", {
  lane: "attended", enforcer: "harness_settings",
  default: { effective: "hold", source: { kind: "harness" }, options: options({ allow: "attended_no_skip", deny: "attended_rules_unavailable" }) },
  cannotRule: { allow: "attended_no_skip", hold: "attended_rules_unavailable", deny: "attended_rules_unavailable" },
});

/** Codex CLI: no hold, no block of its own tools, rules for MCP tools only. */
export const CODEX: ToolPolicyFact = fact("codex-cli", {
  rule_tools: "mcp_only",
  cannotDefault: { hold: "harness_no_hold", deny: "harness_no_block_builtin" },
  cannotRule: { hold: "harness_no_hold" },
});

/** "Your own tools": Wardyn sets no rules for it. */
export const CUSTOM: ToolPolicyFact = fact("custom", {
  lane: "unsupported",
  default: { effective: "allow", source: { kind: "harness" }, options: options({ allow: "custom_harness_rules", hold: "custom_harness_rules", deny: "custom_harness_rules" }) },
  cannotRule: { allow: "custom_harness_rules", hold: "custom_harness_rules", deny: "custom_harness_rules" },
});

/** One remote MCP server on the run, turned on for Claude Code only. */
export const MCP_DOCS: AccessRow = {
  id: "component:mcp-docs", kind: "mcp", from: "added", name: "Docs", status: "ready", hosts: [], credentials: [],
  mcp: { server_key: "docs", transport: "streamable_http", harnesses: ["claude-code"] },
  source: { kind: "component", id: "mcp-docs", name: "Docs" },
};

export const withFacts = (base: AccessFacts, harnesses: ToolPolicyFact[], rows: AccessRow[] = []): AccessFacts => ({
  ...base, harnesses, rows: [...base.rows, ...rows],
});

/** GET /setup/status model_providers: records and whom they serve. */
export const MODEL_PROVIDERS: SetupModelProvider[] = [
  { id: "anthropic-key", name: "Anthropic API key", kind: "api_key", harnesses: ["claude-code"], host: "api.anthropic.com" },
  { id: "bedrock-sso", name: "Bedrock (SSO)", kind: "bedrock_sso", harnesses: ["claude-code"], host: "bedrock-runtime.us-east-1.amazonaws.com" },
  { id: "openai-key", name: "OpenAI API key", kind: "api_key", harnesses: ["codex-cli"], host: "api.openai.com" },
  { id: "gateway-off", name: "Switched-off gateway", kind: "gateway", disabled: true, harnesses: ["claude-code"], host: "gw.example" },
];

/** The caller's own credential per provider: signed in to one, never connected to the other. */
export const PROVIDER_ACCESS: SetupProviderAccess[] = [
  { provider: "anthropic-key", state: "live" },
  { provider: "bedrock-sso", state: "not_configured", cause: "never_connected" },
];
