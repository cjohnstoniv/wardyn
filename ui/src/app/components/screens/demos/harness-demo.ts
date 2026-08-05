/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The "agent in the box" Getting-Started step's three-state gate: is there an
// Anthropic-capable AI integration to run Claude Code against? Claude Code
// only — Codex CLI is NOT supported here: the catalog's task (`claude -p …`)
// and policy (api.anthropic.com-only egress) are Claude-specific, and
// deploy/images/codex-cli carries no claude binary. No integration_id
// override either: the server's own resolution precedence picks the
// credential, exactly like the four keyless demos — a client-synthesized id
// like "ai:anthropic_api_key" doesn't match the server's real integration ids
// and would 400 the launch (a review finding — see harness-demo-step.tsx).
// Pure TS, no React.
import type { SetupStatus } from "../../../lib/types";
import { defaultAgentRow } from "../onboarding/intro";

export type HarnessAvailability =
  // No agent-capable AI integration at all (Readiness.llmReady is false).
  | "none"
  // One resolves, but it can only drive Codex CLI, not Claude Code.
  | "openai_only"
  // A Claude-Code-capable row resolves: anthropic_api_key, anthropic_subscription,
  // or bedrock — any agent-capable row that isn't openai_api_key. (azure_openai
  // never reaches this pick at all — its Claude Code/Codex CLI capability is a
  // flat impossibility, lib/integrations.ts's CAPS.azure, so it's never in
  // agentCapableRows to begin with.)
  | "claude_ready";

// Reuses intro.tsx's defaultAgentRow — the SAME pick Readiness.llmReady is
// built from — rather than re-deriving "which integration" here. Push order
// in deriveAiRows (lib/api/integrations.ts) always lists a Claude-capable
// type before openai_api_key, so this pick is never openai_only unless it's
// the ONLY agent-capable row connected.
export function harnessAvailability(status: SetupStatus): HarnessAvailability {
  const row = defaultAgentRow(status);
  if (!row) return "none";
  return row.aiType === "openai_api_key" ? "openai_only" : "claude_ready";
}
