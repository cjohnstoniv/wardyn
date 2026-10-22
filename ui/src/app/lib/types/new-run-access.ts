/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Step 5's `access` block (pkg/client/runs_access.go; the server builds it in
// internal/api/run_access_facts.go). The policy preview and Review answer the
// same block for the same request. Every array is always sent.

import type { FirstUseMode, HarnessInnerWalls } from "./policy";
import type { ProvenanceSource } from "./policy-preview";
import type { SetupItem } from "./preflight";

/** Who put an access row or host there: provenance's kinds plus two of step 5's own. An unknown kind renders no chip. */
export interface AccessSource {
  kind: ProvenanceSource["kind"] | "harness" | "posture";
  id?: string;
  name?: string;
}

export type AccessRowKind = "harness" | "repository" | "drive" | "command" | "cli" | "mcp" | "api" | "secret" | "host" | "custom" | (string & {});
export type AccessRowStatus = "ready" | "needs_setup" | "unavailable" | "refused" | (string & {});
export type AccessVia = "egress" | "git_broker" | "model_lane" | "mcp_broker" | "injected" | (string & {});
export type AccessVerdict = "allowed" | "needs_approval" | "not_allowed" | (string & {});
export type AccessDelivery = "proxy" | "git_broker" | "git_helper" | "sandbox_env" | "sandbox_file" | "sandbox_short_lived" | (string & {});
export type AccessDecider = "you" | "admin" | "you_or_admin" | (string & {});

export interface AccessMCP {
  server_key: string;
  transport: string;
  harnesses: string[];
}

export interface AccessRow {
  id: string;
  kind: AccessRowKind;
  // The row's chip: harness, repository, drive, command or added.
  from: string;
  name: string;
  status: AccessRowStatus;
  // A closed code (access-reasons.golden.json).
  reason?: string;
  // Keys into AccessFacts.hosts and AccessFacts.credentials.
  hosts: string[];
  credentials: string[];
  // CLI rows: yes, no or unknown.
  in_image?: string;
  mcp?: AccessMCP;
  source: AccessSource;
}

export interface AccessHost {
  host: string;
  // The port the host is spelled with; absent when it names none.
  port?: number;
  via: AccessVia;
  // The rows that bring it, or ["policy"] when no row does.
  rows: string[];
  verdict: AccessVerdict;
  // not_allowed only: clamped, denied_list, capability or always_deny.
  reason?: string;
  source: AccessSource;
}

export interface AccessCredential {
  id: string;
  row: string;
  kind: string;
  owner: "own" | "shared" | "org_app" | (string & {});
  // The credential named by its kind and whose it is; absent for a model credential.
  label?: string;
  delivery: AccessDelivery;
  // A minted value's ceiling; absent when the run's lifetime bounds it or Wardyn does not set it.
  lifetime_sec?: number;
  renewed?: boolean;
  after_end: "gone" | "expires" | "stays" | (string & {});
  // A cluster account (kind kube_api): the namespaces it may reach and how.
  namespaces?: string[];
  access?: "read_only" | "read_write" | (string & {});
}

export interface AccessUnlisted {
  mode: FirstUseMode;
  allow_all: boolean;
  hold_sec?: number;
  decider: AccessDecider;
}

export interface EffectOption {
  effect: string;
  available: boolean;
  reason?: string;
}

export interface LockedToolRule {
  tool: string;
  effect: string;
  source: AccessSource;
}

export interface ToolDefaultFact {
  effective: string;
  source: AccessSource;
  options: EffectOption[];
}

export interface ToolPolicyFact {
  harness: string;
  lane: "gated" | "ungated" | "attended" | "unsupported" | (string & {});
  enforcer: "wardyn_gate" | "harness_settings" | "mcp_broker" | "none" | (string & {});
  default: ToolDefaultFact;
  policy_rules: LockedToolRule[];
  rule_effects: EffectOption[];
  rule_tools: "any" | "mcp_only" | (string & {});
  mcp_servers: string[];
  // The harness's own walls this run launches it with (#2822); absent for an exec run.
  inner_walls?: HarnessInnerWalls;
}

export interface AccessFacts {
  rows: AccessRow[];
  hosts: AccessHost[];
  credentials: AccessCredential[];
  unlisted: AccessUnlisted;
  harnesses: ToolPolicyFact[];
  setup: SetupItem[];
}
