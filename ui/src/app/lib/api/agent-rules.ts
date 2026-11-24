/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Agent rules: an organisation's walls for one agent harness, at /agent-rules (internal/api/agent_rules.go), on the
// security tier. A write is held for a second security admin when the second-person switch is on: the PUT or DELETE
// answers 202 and asJson (or throwIfPending) throws a PendingChangeError, never a success.
import type { AgentRules, AgentRulesList, AgentRulesWrite } from "../types/agent-rules";
import { asJson, asNoContent, HttpError, throwIfPending, wfetch } from "./core";

/** The wire reasons (internal/api/reasons_agent_rules.go). The console matches on these, never on the text. */
export const AGENT_RULES_REASON = {
  INVALID: "agent_rules_invalid",
  VERSION_CONFLICT: "agent_rules_version_conflict",
  UNAVAILABLE: "agent_rules_unavailable",
} as const;

/** A store that keeps no agent rules (501): the section is absent. */
export const agentRulesAbsent = (e: unknown): boolean => e instanceof HttpError && e.reason === AGENT_RULES_REASON.UNAVAILABLE;

export const agentRules = {
  // GET /api/v1/agent-rules: {rules}, sorted by harness.
  async list(): Promise<AgentRules[]> {
    return (await asJson<AgentRulesList>(await wfetch("/agent-rules", { method: "GET" }))).rules;
  },

  // PUT /api/v1/agent-rules/{harness}: version 0 creates; the saved rules answer 200, a held write 202.
  async put(harness: string, write: AgentRulesWrite): Promise<AgentRules> {
    return asJson<AgentRules>(await wfetch(`/agent-rules/${encodeURIComponent(harness)}`, { method: "PUT", asPerson: true, body: JSON.stringify(write) }));
  },

  // DELETE /api/v1/agent-rules/{harness}?version=n -> 204, or 202 when held.
  async remove(harness: string, version: number): Promise<void> {
    const res = await wfetch(`/agent-rules/${encodeURIComponent(harness)}?version=${version}`, { method: "DELETE" });
    await throwIfPending(res);
    await asNoContent(res);
  },
};
