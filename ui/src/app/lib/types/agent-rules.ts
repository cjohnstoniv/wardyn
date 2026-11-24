/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The agent rules wire contract (types only): an organisation's narrow-only walls for one agent harness, with the
// harness's own sandbox and approvals. Mirrors internal/types/agent_rules.go.
import type { PersonalWallsSpec } from "../api/walls";
import type { HarnessInnerWalls } from "./policy";

export interface AgentRules {
  harness: string;
  spec: PersonalWallsSpec;
  inner_walls?: HarnessInnerWalls;
  /** The stored version: a replacement or a delete names the version it read. */
  version: number;
  updated_by?: string;
  updated_at: string;
}

/** The body of PUT /agent-rules/{harness}: version 0 creates, any other value must be the stored version. */
export interface AgentRulesWrite {
  spec: PersonalWallsSpec;
  inner_walls?: HarnessInnerWalls;
  version: number;
}

export interface AgentRulesList {
  rules: AgentRules[];
}
