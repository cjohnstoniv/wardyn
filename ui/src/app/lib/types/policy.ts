/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Run policies (admin-gated config) + the egress first-use mode helpers.
import type { ConfinementClass } from "./runs";

// git_pat = a stored Personal Access Token brokered to git for a non-GitHub host
// (Azure DevOps / GitLab / ...). Unlike api_key (proxy-injected, value never
// returned) the PAT value reaches git via the credential helper as a password.
export type GrantKind = "github_token" | "cloud_sts" | "api_key" | "git_pat";

export interface GrantSpec {
  kind: GrantKind | (string & {});
  // Kind-specific scope object; free-form JSON on the wire.
  scope?: Record<string, unknown>;
  ttl_seconds?: number;
  requires_approval: boolean;
}

// A single operator/policy-controlled host bind mount. Mirrors the wire shape
// types.WorkspaceMount exactly: source = host path, target = in-container path,
// read_only optional with the SAFE DEFAULT being read-only (omitted => RO).
export interface WorkspaceMount {
  source: string;
  target: string;
  read_only?: boolean;
}

// One onboarded-repo attachment on a run (mirrors types.WorkspaceRepo). Repos
// are re-cloned fresh per run — this is just "which onboarded repo, and where"
// (target omitted => the server's convention default, ~/work/<repo-name>).
export interface WorkspaceRepo {
  repo: string;
  target?: string;
}

// SUBSCRIPTION_OAUTH_SECRET is the sentinel secret name (mirrors
// types.SubscriptionOAuthSecret in Go) that marks subscription LLM auth on a
// recorded profile — it is NOT a real stored secret.
export const SUBSCRIPTION_OAUTH_SECRET = "anthropic-subscription-oauth";

// FirstUseMode controls how an unknown (unlisted) egress domain is handled:
//  - always_deny: hard-deny, never surfaced for approval
//  - deny_with_review: raise an approval + deny now; a retry passes once approved
//  - wait_for_review: raise an approval and HOLD the connection until decided
// The wire accepts the legacy boolean too (true=deny_with_review, false=always_deny);
// asFirstUseMode() normalizes either form.
export type FirstUseMode = "always_deny" | "deny_with_review" | "wait_for_review";

export function asFirstUseMode(v: unknown): FirstUseMode {
  if (v === true) return "deny_with_review";
  if (v === false || v == null || v === "") return "always_deny";
  if (v === "deny_with_review" || v === "wait_for_review" || v === "always_deny") return v;
  return "always_deny"; // unknown => fail closed
}

// firstUseRaisesApproval reports whether the mode escalates to a human (either review mode).
export function firstUseRaisesApproval(v: unknown): boolean {
  const m = asFirstUseMode(v);
  return m === "deny_with_review" || m === "wait_for_review";
}

// firstUseLabel is a short human label for review/summary surfaces.
export function firstUseLabel(v: unknown): string {
  switch (asFirstUseMode(v)) {
    case "wait_for_review":
      return "Ask & wait";
    case "deny_with_review":
      return "Ask";
    default:
      return "Off";
  }
}

export interface RunPolicySpec {
  allowed_domains: string[];
  denied_domains?: string[];
  first_use_approval: FirstUseMode;
  allowed_methods?: string[];
  min_confinement_class: ConfinementClass;
  eligible_grants?: GrantSpec[];
  auto_stop_after_sec?: number;
  // Operator/policy-controlled host bind mounts injected into the sandbox.
  workspace_mounts?: WorkspaceMount[];
  // Onboarded repos cloned fresh into the sandbox for this run. Parallel list to
  // workspace_mounts (local dirs stay mounts; repos get their own list — see
  // types.Workspace / internal/workspacescan).
  workspace_repos?: WorkspaceRepo[];
  // When true the proxy allows ANY non-denied public host: denied_domains still
  // wins, allowed_domains may be empty. The SSRF/private-IP guard and the
  // exact-host allowlist required for credential injection are UNCHANGED.
  allow_all_egress?: boolean;
}

export interface RunPolicy {
  id: string;
  name: string;
  created_at: string;
  updated_at: string;
  spec: RunPolicySpec;
}
