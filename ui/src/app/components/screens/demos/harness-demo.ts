/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// D1 — the "agent in the box" Getting-Started step's binding picker: which AI
// integration a launch of that demo binds to, and which built-in agent (and so,
// server-side, which convention image — agentImage in
// internal/api/runs_policy.go) that implies. Reuses intro.tsx's
// defaultAgentRow — the SAME pick Readiness.llmReady is itself built from —
// instead of re-deriving "the connected model" here. Pure TS, no React.
import type { SetupStatus } from "../../../lib/types";
import type { IntegrationRow } from "../../../lib/api/integrations";
import { defaultAgentRow } from "../onboarding/intro";

export interface HarnessBinding {
  row: IntegrationRow;
  /** codex-cli only when the bound integration is an OpenAI key — Codex CLI
   *  speaks the OpenAI API only (T.X_KEY_CODEX/X_SUB_CODEX/X_BEDROCK_CODEX in
   *  lib/integrations.ts); every other AI type drives claude-code. */
  agent: "claude-code" | "codex-cli";
  /** Tier 1 of the server's model-access precedence (createRunRequest.
   *  IntegrationID / resolveRunIntegration, internal/api/llmcred.go). */
  integrationId: string;
}

// null when no agent-capable integration is connected (Readiness.llmReady is
// false) — the caller's honest signal to render the step's locked state
// instead of guessing a binding that doesn't exist. No client-authored
// api_key grant is produced here or anywhere downstream: the server authors
// the grant from the integration_id alone.
export function pickHarnessBinding(status: SetupStatus): HarnessBinding | null {
  const row = defaultAgentRow(status);
  if (!row) return null;
  const agent = row.aiType === "openai_api_key" ? "codex-cli" : "claude-code";
  return { row, agent, integrationId: row.id };
}
