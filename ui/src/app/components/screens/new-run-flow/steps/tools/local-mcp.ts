/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// MCP servers that run in the run's sandbox (features.mcp_local_servers): which access rows are
// one, and the one conflict the server refuses a run for (internal/api localMCPRulesRefusal).

import * as React from "react";
import type { AccessFacts, AccessRow, ToolPolicyFact } from "../../../../../lib/types/new-run-access";
import { useShellSetupStatus } from "../../../../wardyn/model-access-context";
import type { RunIntent } from "../../draft/run-intent";

/** The switch on the shell's /setup/status; absent is off. */
export const useLocalMcpOn = (): boolean => !!useShellSetupStatus().status?.features?.mcp_local_servers;

export const isLocalMcp = (row: AccessRow): boolean => row.kind === "mcp" && row.mcp?.transport === "stdio";

/** The one harness that cannot apply a local server's rules in an agent run (the server also refuses any harness in a command run). */
const CODEX = "codex-cli";

/**
 * True when `server` is turned on for Codex CLI and a rule holds or blocks one of its tools: the policy's
 * rules, the person's own and their default. Wardyn's proxy never sees a local server's calls, so the run
 * is refused rather than the rule dropped.
 */
export function localMcpRefused(fact: ToolPolicyFact, intent: RunIntent, server: AccessRow): boolean {
  if (fact.harness !== CODEX || !isLocalMcp(server) || !server.mcp) return false;
  const tool = intent.tools?.find((t) => t.id === fact.harness);
  const rules = [...fact.policy_rules, ...(tool?.tool_rules ?? []), ...(tool?.default_effect ? [{ tool: "*", effect: tool.default_effect }] : [])];
  const prefix = `mcp__${server.mcp.server_key}__`;
  return rules.some((r) => (r.effect === "hold" || r.effect === "deny") && (r.tool === "*" || r.tool.startsWith(prefix)));
}

export interface LocalMcpConflict {
  harness: string;
  server: string;
}

/** Every local server on for a harness whose rules that harness cannot apply, from the server's own facts. */
export function localMcpConflicts(access: AccessFacts | undefined, intent: RunIntent): LocalMcpConflict[] {
  return (access?.rows ?? []).filter(isLocalMcp).flatMap((row) =>
    (row.mcp?.harnesses ?? []).flatMap((harness) => {
      const fact = access?.harnesses.find((f) => f.harness === harness);
      return fact && localMcpRefused(fact, intent, row) ? [{ harness, server: row.name }] : [];
    }),
  );
}

/** The names of the secrets the draft's own components carry, by credential id: the access block never names one. */
export function secretNames(access: AccessFacts | undefined, intent: RunIntent): Map<string, string> {
  const out = new Map<string, string>();
  for (const row of access?.rows ?? []) {
    const at = /^inline:(\d+)$/.exec(row.source.id ?? "");
    const secrets = at ? intent.components?.[Number(at[1])]?.inline?.secrets : undefined;
    for (const c of access?.credentials ?? []) {
      const name = c.row === row.id ? secrets?.[Number(c.id.split(":secret:")[1])]?.secret_name : undefined;
      if (name) out.set(c.id, name);
    }
  }
  return out;
}

/** A secret a local server has in the sandbox: its name when the draft's own component says it, else the credential's label. */
export interface SandboxSecret {
  server: string;
  secret?: string;
}

export function sandboxSecrets(access: AccessFacts | undefined, intent: RunIntent): SandboxSecret[] {
  const names = secretNames(access, intent);
  return (access?.rows ?? []).filter(isLocalMcp).flatMap((row) =>
    (access?.credentials ?? [])
      .filter((c) => c.row === row.id && (c.delivery === "sandbox_env" || c.delivery === "sandbox_file"))
      .map((c) => ({ server: row.name, secret: names.get(c.id) || c.label || undefined })),
  );
}

/** harness id -> display name, from the shell's setup status. */
export function useHarnessNames(): ReadonlyMap<string, string> {
  const { status } = useShellSetupStatus();
  return React.useMemo(() => new Map((status?.harnesses ?? []).map((h) => [h.id, h.display])), [status]);
}
