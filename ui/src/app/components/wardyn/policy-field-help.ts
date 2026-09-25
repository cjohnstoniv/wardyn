/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { RunPolicySpec } from "../../lib/types";

export interface FieldHelp {
  /** One line: what the key does. */
  what: string;
  /** Legal values / shape, in authoring terms. */
  values: string;
  /** docs/POLICIES.md heading anchor for depth. */
  doc: string;
  /** The value an "Insert" click writes under this key. */
  snippet: unknown;
}

// `satisfies Record<keyof RunPolicySpec, FieldHelp>` is the parity guard: add a
// field to RunPolicySpec (which mirrors types.RunPolicySpec) without documenting
// it here and this file stops compiling. No markdown-parsing test needed.
//
// Note what is not snippetable: llm_inspection.workspace_secret_values. It is
// refused on every policy write (validatePolicySpec) — dispatch resolves
// workspace_secret_names to values in memory, for the proxy sidecar only — so
// the llm_inspection snippet authors names and the help text says so.
export const FIELD_HELP = {
  allowed_domains: {
    what: "The egress allowlist — the hosts the sandbox may reach.",
    values:
      "Exact hosts or \"*.\"-prefixed wildcards. Empty under default-deny means the sandbox reaches nothing.",
    doc: "top-level",
    snippet: ["api.anthropic.com"],
  },
  denied_domains: {
    what: "Always wins over allowed_domains, in both egress modes.",
    values: "Same entry shapes as allowed_domains. A deny beats allow-all too.",
    doc: "top-level",
    snippet: ["example.com"],
  },
  first_use_approval: {
    what: "What happens when the sandbox reaches a host that is not listed.",
    // The three mode bodies are docs/design/ui-batch2-mock.md's canon strings
    // (D33 for deny_with_review, D8 for wait_for_review), verbatim — this panel
    // is their one source now, replacing the run wizard's old Confined card and
    // NetworkDialog's UNLISTED_RULES. A hold is bounded (first_use_hold_seconds,
    // 30s default), not "until you approve or deny it".
    values:
      "always_deny — refused outright, no prompt, no wait. " +
      "deny_with_review — Default-deny. A new host is refused and raised for your review — approve it once and a retry gets through. " +
      "wait_for_review — The connection waits, live, for the standard 30-second window. Decide in time and it goes through; miss it and it's refused — the approval itself stays open for you to decide. " +
      "Inert under allow_all_egress.",
    doc: "first_use_approval-modes",
    snippet: "deny_with_review",
  },
  first_use_hold_seconds: {
    what: "How long a wait_for_review connection is held open awaiting a decision before the proxy refuses it.",
    values: "Seconds. 0 or omitted keeps the built-in 30s default. Only wait_for_review holds — the other first_use_approval modes never wait.",
    doc: "top-level",
    snippet: 30,
  },
  max_holds: {
    what: "Caps concurrent wait_for_review holds (one held goroutine per held connection).",
    values: "0 or omitted keeps the built-in 16 default. The (N+1)th concurrent hold fails fast rather than consuming an unbounded goroutine.",
    doc: "top-level",
    snippet: 16,
  },
  allowed_methods: {
    what: "Optional HTTP method restriction.",
    values: "e.g. [\"GET\", \"POST\"]. Empty or omitted = every method.",
    doc: "top-level",
    snippet: ["GET", "POST"],
  },
  min_confinement_class: {
    what: "The barrier floor — the run refuses to launch below it.",
    values: "CC1 (Fence) | CC2 (Wall) | CC3 (Vault). Required.",
    doc: "top-level",
    snippet: "CC2",
  },
  eligible_grants: {
    what: "The ceiling of credential scopes this run may request — eligibility, not issuance.",
    values:
      "kind: github_token | cloud_sts | api_key | git_pat | ssh_key, each with its own scope, plus ttl_seconds (1h max) and requires_approval.",
    doc: "eligible_grants--grantspec",
    snippet: [
      {
        kind: "api_key",
        scope: {
          host: "api.anthropic.com",
          header: "x-api-key",
          format: "%s",
          secret_name: "anthropic-api-key",
        },
        ttl_seconds: 3600,
        requires_approval: false,
      },
    ],
  },
  auto_stop_after_sec: {
    what: "Idle auto-stop, in seconds of wall-clock idleness (an attach or an egress call resets it).",
    values:
      "> 0 stops after that many idle seconds, plus a 30s debounce. 0 or ABSENT = never reaped. -1 = never reaped, stated explicitly — identical behavior to leaving it out, written down as intent (what an interactive run means).",
    doc: "top-level",
    snippet: 3600,
  },
  workspace_mounts: {
    what: "Operator-authored host bind mounts. Never agent-chosen.",
    values:
      "source (absolute host path) + target (under /home/agent, /work or /workspace). read_only defaults to TRUE when omitted — read-write needs an explicit false.",
    doc: "workspace_mounts--workspacemount",
    snippet: [{ source: "/srv/data", target: "/work/data" }],
  },
  workspace_repos: {
    what: "Extra git repos cloned into the run — the clone counterpart of workspace_mounts.",
    values:
      "repo (slug or URL), optional target, optional ref (branch/tag/SHA; unset clones the remote's default branch).",
    doc: "workspace_repos--workspacerepo",
    snippet: [{ repo: "owner/repo" }],
  },
  allow_all_egress: {
    what: "Switches egress to deny-list only: any non-denied PUBLIC host is allowed.",
    values:
      "true | false (default). The private-IP/SSRF guard is unaffected, and credential injection still needs an exact allowed_domains entry — allow-all never widens where a secret may go.",
    doc: "top-level",
    snippet: true,
  },
  llm_inspection: {
    what: "Outbound content inspection on the brokered LLM routes — a guardrail and visibility layer, not exfiltration prevention.",
    values:
      "mode: off (default) | alert | block, with at least one detector when not off. Author workspace_secret_names — workspace_secret_values is REFUSED on every policy write.",
    doc: "llm_inspection--llminspectionspec",
    snippet: { mode: "alert", detect_secrets: true, workspace_secret_names: [] },
  },
  resources: {
    what: "Sandbox CPU / memory / PID / disk caps.",
    values:
      "cpu_millis (2000 = 2 vCPU), memory_mib, pids_limit, disk_mib. A zero or omitted field takes the platform default — every run is capped either way.",
    doc: "resources--resourcelimits",
    snippet: { cpu_millis: 2000, memory_mib: 4096, pids_limit: 512 },
  },
  ui_apps: {
    what: "In-sandbox loopback HTTP apps the UI gateway may relay to a browser.",
    values:
      "name (lower-case slug, also the /usr/local/bin/wardyn-ui-<name> launcher), port (inside the sandbox, on 127.0.0.1), optional path. Max 8. Declaring one grants nothing: the gateway is off unless WARDYN_UI_SANDBOX_LISTEN is set, and every session still needs a single-use ticket.",
    doc: "ui_apps--uiapp",
    snippet: [{ name: "editor", port: 8080 }],
  },
  tool_rules: {
    what: "Per-tool effects for a run whose tool approvals are held — the middle between approving everything and approving nothing.",
    values:
      "tool (matched exactly and case-sensitively, or the literal * default) + effect: allow | hold | deny. Duplicates are refused. Empty is today's behaviour: every gated call goes to a human.",
    doc: "tool_rules--toolrule",
    snippet: [
      { tool: "Read", effect: "allow" },
      { tool: "Bash", effect: "hold" },
    ],
  },
  git_push_any_branch: {
    what: "Turns OFF branch-namespace confinement (default ON) for this run's brokered GitHub pushes.",
    values:
      "true | false (default). For a sandbox a human drives through an external tool that names its own branches — every such push is marked brokered:git:branch-ns-off in audit, on or off.",
    doc: "git_push_any_branch-the-per-run-opt-out",
    snippet: true,
  },
  push_rules: {
    what: "Content rules for this run's brokered git pushes — WHAT a push may touch, alongside git_push_any_branch's WHERE.",
    values:
      "deny_paths (glob-shaped patterns, e.g. \".github/workflows/**\") + max_inspect_pack_mib (0-64). Stored and validated only — no matcher reads deny_paths yet. Unenforceable (a warning, not a refusal) when this run's only git grant is ssh_key.",
    doc: "push_rules--pushrulesspec",
    snippet: { deny_paths: [".github/workflows/**"] },
  },
} satisfies Record<keyof RunPolicySpec, FieldHelp>;
