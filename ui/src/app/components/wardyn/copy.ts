/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Single-sourced console copy — the redesign's glossary in ONE place so screens
// can't drift (D3/D6/D9/D11/B6 + the honesty rules). Barrier metadata and the
// every-tier note live in cc-meta.ts (CC_META / CONFINEMENT_CONSTANT_NOTE) —
// import from there, never duplicate them here.

// The single residual-risk prefix (D11) — everywhere a tier is explained, never
// dropped or softened. The residual text itself is CC_META[*].doesntProtect.
export const RESIDUAL_PREFIX = "Doesn't stop:";

// Run mode pair (D3) — the only two mode names, used verbatim everywhere. Banned:
// Batch, Background, "Runs unattended"/"You drive it" AS mode names.
export type RunMode = "interactive" | "autonomous";
export const RUN_MODE: Record<RunMode, { label: string; blurb: string }> = {
  interactive: {
    label: "Interactive",
    // An interactive run execs NO agent at all (internal/types/types.go:
    // "no agent task is exec'd and no completion watcher is started — the
    // human drives via wardyn attach") — the blurb must not describe
    // per-action approval, an autonomous-shaped behavior this mode doesn't
    // have. Matches compose-form.tsx's own Field hint for the same toggle.
    blurb: "Comes up idle — you attach and drive it over a terminal.",
  },
  autonomous: {
    label: "Autonomous",
    // Honest: approvals are policy-conditional (GrantSpec.requires_approval /
    // first_use_approval), so we say the policy's gates apply — not that every
    // step is gated (a permissive policy may gate nothing).
    blurb: "Runs unattended — your policy's approval gates still apply.",
  },
};

// Unified status vocabulary (B6) — barriers, providers, CLIs, and keys all use
// these (plus "connected" for signed-in accounts). Two distinct negative states:
// "incompatible" = this HARDWARE/host can never run it (no install fixes it —
// always carries the concrete why); "unavailable" = not launchable right now,
// cause unstated (the wizard's launch-time truth). "Needs setup" = fixable here.
// "unverified" (compose setup-checklist items only): v1 doesn't live-probe a
// credential/workspace — it can only say "declared present" or "known absent".
// Neutral tone on purpose — it is neither a pass nor a fail.
export type StatusKind =
  | "ready"
  | "needs-setup"
  | "unavailable"
  | "incompatible"
  | "checking"
  | "connected"
  | "unverified";
export const STATUS_LABEL: Record<StatusKind, string> = {
  ready: "Ready",
  "needs-setup": "Needs setup",
  unavailable: "Unavailable here",
  incompatible: "Incompatible here",
  checking: "Checking…",
  connected: "Connected",
  unverified: "Unverified",
};

// Outcome-true button labels (D9) — the label predicts what the click does.
export const BTN = {
  showSetupCommand: "Show setup command",
  recheck: "Re-check",
  installGuide: "Install guide →",
  recheckLogin: "Re-check login",
} as const;

// Capability-grant wording (D2 + honesty). Grants render amber, never as a
// reassuring green check. allow_all_egress is ALWAYS the block-list phrasing,
// never "unrestricted".
export const CAPABILITY = {
  allowAllEgress: "Can reach almost any site (except a block-list).",
  // Fully true only for github_token — the broker mints a genuinely short-lived
  // installation token. api_key is different: the long-lived stored key is
  // injected proxy-side (it never enters the sandbox), so never pair this line
  // with mint/TTL claims there. cloud_sts can't be minted at all (needs SPIRE).
  brokerLine:
    "The run works through a short-lived, scoped credential — your stored key stays in Wardyn.",
  // #381: since 0.7 WARDYN_GIT_PAT_BROKER defaults ON, so a git_pat grant's
  // stored token is rewritten to a broker path and attached by the proxy on
  // the outbound leg — it never enters the sandbox, the same posture
  // brokerLine describes. It still isn't brokerLine's line, though: a PAT is
  // exactly as long-lived and unscoped as the value the operator stored, never
  // the "short-lived, scoped" credential brokerLine promises, so it keeps its
  // own honesty line rather than borrowing that one. This is the DEFAULT
  // (broker-on) shape; gitPatLineResident below is what remains true with the
  // switch off.
  gitPatLine:
    "A stored git access token is attached to the request by the proxy on the outbound leg — it never enters the sandbox.",
  // gitPatLineResident is gitPatLine's pre-0.7 shape: still exactly correct
  // for an operator who set WARDYN_GIT_PAT_BROKER=off, where a git_pat grant
  // reverts to being injected into git INSIDE the sandbox as the credential,
  // so whatever's running there can read it.
  gitPatLineResident:
    "A git access token is handed to git inside the sandbox — the process running there can read it.",
  // Honest exception (same shape as gitPatLine): an ssh_key grant writes a
  // RESIDENT private key file for the sandbox's git-over-SSH client to read —
  // git's SSH transport has no credential-helper seam, so the key can't be
  // proxy-injected or brokered like github_token.
  sshKeyLine:
    "A private SSH key is written to disk in the sandbox — the process running there can read it.",
} as const;

// Risk grades (D8) — deterministic, computed by Wardyn's rules (not the model).
// Only High gates an acknowledgment.
export const RISK_ATTRIBUTION = "Graded by Wardyn's rules, not the model.";

// Viewer/operator role gate (console-side UX only — internal/api/http.go's
// requireOperator/isOperator is the real enforcement and stays the backstop;
// this just stops a viewer from discovering the tier as a raw 403 toast).
// ONE reason string, reused at every disabled operator-only control so it
// never drifts between screens.
export const OPERATOR_ONLY_REASON = "Requires the admin role.";

// DRAFT (M2 canon pending) — X3-F6 residual. The SECURITY tier is a DIFFERENT
// gate: the server's isSecurityOperator admits an admin OR a security admin, so
// a control refused by it must not tell the reader "requires the admin role"
// when the role beside it would also do. Picked by the gate that fired, never
// by a role comparison of the reader's own: a surface gated on isOperator keeps
// OPERATOR_ONLY_REASON above. Used by ui-member-cluster's own sites AND by
// approvals.tsx's decide-gate chip (ui-workspaces-approvals, also keyed off
// useSecurityOperator) — one definition, canon owned by ui-member-cluster
// (local/v074/canon/ui-member-cluster.md).
export const SECURITY_ONLY_REASON = "Requires the admin or security admin role.";



// Per-domain modules under copy/ — each re-exported here so every existing
// `from "./copy"` import keeps working unchanged (copy/terminal.ts set the
// pattern; this finishes the split by seam started there).
export { EGRESS } from "./copy/egress";
export type { ApprovalKind } from "./copy/approvals";
export {
  VIEWER_APPROVAL_BLOCKS_NOTE,
  APPROVAL_BANNER_LABEL,
  APPROVAL_KIND_LABEL,
  APPROVAL,
  APPROVAL_SCOPE_ORDER,
  APPROVAL_SCOPE_LABEL,
  APPROVAL_SCOPE_HINT,
  DENY_SCOPE_LABEL,
  DENY_SCOPE_HINT,
  UNTIL_PRESETS,
  denyDialogCopy,
  egressBlastRadius,
  ALWAYS_NEEDS_WORKSPACE,
  approvalScopeBadge,
  TELEMETRY_TAG,
  WIRE_TO_COPY,
} from "./copy/approvals";
export type { CredentialKind } from "./copy/approvals";
export { credentialKind } from "./copy/approvals";
export { RUN_COCKPIT } from "./copy/run-cockpit";
export { UI_APPS_LANE, UI_APPS_LAUNCHER_MISSING_PREFIX, POLICY_UI_APPS } from "./copy/ui-apps";
export { YOUR_MODEL_KEY } from "./copy/model-key";
export { MEMBER_GETTING_STARTED, RUNS_MEMBER_EMPTY, RUNS_WAIT } from "./copy/getting-started";
export { EPISODES_COPY, FIRST_RUN_DEMOS_SUBTITLE } from "./copy/episodes";
export { PEOPLE_STEP, SETUP, SITE } from "./copy/setup-steps";
export { SHELL, UNSAVED_GUARD } from "./copy/shell";
export { RUN } from "./copy/run-clone";

// Moved to copy/terminal.ts (the target path for the later barrel split of
// this file) — re-exported here so every existing `from "./copy"` import
// keeps working unchanged.
export { TERMINAL } from "./copy/terminal";

export {
  RECORDING_DISABLED_TITLE,
  RECORDING_DISABLED_DESC,
  RAIL,
  RAIL_CREDENTIAL,
  RAIL_RECORDING_ON,
} from "./copy/new-run-rail";
