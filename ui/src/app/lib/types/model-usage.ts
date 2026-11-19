/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// GET /api/v1/runs/{id}/model-usage (Go: internal/api/run_model_usage.go,
// ModelUsageView). Token counts are exactly as each vendor reports them:
// Anthropic and Bedrock count Input without the cache tokens, OpenAI counts
// cached tokens inside Input. The fields are never added together here.
//
// `calls` counts EVERY call the proxy recorded: counted, partial (cut off, with
// the tokens reported so far) and uncounted. `uncounted_calls` and
// `partial_calls` are subsets of it.
export interface ModelUsageTokens {
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
  reasoning_tokens: number;
  calls: number;
  uncounted_calls: number;
  partial_calls: number;
}

export interface ModelUsageModel extends ModelUsageTokens {
  /** The run's provider row for the host, the host itself when none matches. */
  provider: string;
  /** The dialect the counts are in: anthropic | openai | bedrock | ollama. */
  vendor: string;
  host: string;
  model: string;
  /** Why the uncounted calls were not counted, by reason code. */
  uncounted_reasons: Record<string, number>;
}

export interface ModelUsage {
  totals: ModelUsageTokens;
  by_model: ModelUsageModel[];
  /** Tunnels to model hosts Wardyn could not open: tunnels, not calls. */
  opaque_tunnels: number;
  updated_at?: string;
}

// GET /governance/units/{id}/model-usage (internal/api/governance_unit_usage.go).
export interface UnitModelUsage extends ModelUsage {
  unit_id: string;
  unit_name: string;
}

export interface UnitModelUsageResponse {
  since: string;
  until: string;
  units: UnitModelUsage[];
}
