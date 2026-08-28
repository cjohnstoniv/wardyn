/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Run policies (admin-gated config) + the egress first-use mode helpers.
import type { ConfinementClass, UIApp } from "./runs";

// git_pat = a stored Personal Access Token brokered to git for a non-GitHub host
// (Azure DevOps / GitLab / ...). Unlike api_key (proxy-injected, value never
// returned) the PAT value reaches git via the credential helper as a password.
// ssh_key = a resident private key written to disk for git's SSH transport
// (WIRE-5; internal/types/types.go's GrantKind carries all six — this union
// was missing the one lane approvals.tsx already has a dedicated banner for).
// env_secret = a stored secret injected as a sandbox env var (admin-gated).
export type GrantKind = "github_token" | "cloud_sts" | "api_key" | "git_pat" | "ssh_key" | "env_secret";

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
  // Branch/tag/SHA to check out; omitted => the repo's default branch.
  ref?: string;
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

// firstUseLabel is a short human label for review/summary surfaces. always_deny
// is a real, restrictive choice ("Always deny") wherever the operator could
// have picked a review mode instead — labelling it "Off" there reads as LESS
// restrictive than it is (N4). Only under allow-all egress is the setting
// genuinely inert (buildSpec forces always_deny and the Network card hides the
// control entirely) — callers that know the run is allow-all pass `allowAll`
// and get the honest "Off (allow-all)" instead.
export function firstUseLabel(v: unknown, allowAll = false): string {
  switch (asFirstUseMode(v)) {
    case "wait_for_review":
      return "Ask & wait";
    case "deny_with_review":
      return "Ask";
    default:
      return allowAll ? "Off (allow-all)" : "Always deny";
  }
}

// Outbound content inspection on the brokered LLM routes (mirrors
// types.LLMInspectionSpec). A guardrail + visibility layer, NOT exfiltration
// prevention. Omitting the whole block — or mode "off" — is off.
export interface LLMInspectionSpec {
  // "off" (default) | "alert" (scan + audit, forward unchanged) | "block" (a
  // qualifying finding refuses the request). Left as `string` because Go's is:
  // validatePolicySpec is what rejects a garbage value, at write time.
  mode: string;
  // Operator-declared secret NAMES — the authoring field. Resolved to values
  // only at dispatch, in memory, on the copy handed to the proxy sidecar.
  workspace_secret_names?: string[];
  // NOT an authoring field: validatePolicySpec REFUSES a non-empty value on
  // every policy write. Mirrored only because the wire shape carries it.
  workspace_secret_values?: string[];
  detect_secrets?: boolean;
  detect_secret_patterns?: boolean;
  detect_entropy?: boolean;
  detect_pii?: boolean;
  detector_sidecar_url?: string;
  classified_markers?: string[];
  scan_attachments?: boolean;
  inspect_forward_egress?: boolean;
  max_scan_bytes?: number;
  // "pass" (default, fail-open) | "block" (fail-closed in block mode).
  on_scanner_error?: string;
  require_inspectable_llm?: boolean;
  intercept_tls?: boolean;
  // "low" (default) | "medium" | "high" | "critical".
  block_min_severity?: string;
}

// Sandbox resource caps (mirrors types.ResourceLimits). A zero or omitted field
// means "platform default" — dispatch fills conservative defaults so every run
// is capped even under a policy that sets nothing.
export interface ResourceLimits {
  cpu_millis?: number;
  memory_mib?: number;
  pids_limit?: number;
  disk_mib?: number;
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
  // Outbound LLM content inspection. Omitted => OFF (the safe default).
  llm_inspection?: LLMInspectionSpec;
  // Sandbox CPU/memory/PID/disk caps. Omitted => platform defaults.
  resources?: ResourceLimits;
  // Declared in-sandbox loopback HTTP apps the UI gateway may relay to the
  // browser (mirrors Go's RunPolicySpec.UIApps). Read-only in the console —
  // ui_apps is operator-authored via the API/YAML, no editor in 0.6.
  ui_apps?: UIApp[];
  // Turns off branch-namespace confinement for this run's brokered pushes
  // (mirrors Go's RunPolicySpec.GitPushAnyBranch). For a sandbox a human
  // drives through an external tool that names its own branches; the audit
  // stream marks each such push brokered:git:branch-ns-off. Read-only in the
  // console — operator-authored via the API/YAML.
  git_push_any_branch?: boolean;
}

export interface RunPolicy {
  id: string;
  name: string;
  created_at: string;
  updated_at: string;
  spec: RunPolicySpec;
}
