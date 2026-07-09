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
    blurb: "You drive; it asks before it acts.",
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

// Setup-checklist residency sub-line (compose_setup.go's Residency field) — the
// one-line honest answer to "where does this credential actually live at run
// time", shown muted under a checklist row. Keyed by the wire value; an
// unrecognized/absent residency renders no sub-line (see SetupItemResidency).
export const SETUP_RESIDENCY_NOTE: Record<string, string> = {
  proxy_injected: "held by the proxy — never inside the sandbox",
  resident_mount: "mounted into the sandbox",
  brokered_mint: "brokered at launch by the control plane — never stored in the sandbox",
};

// Outcome-true button labels (D9) — the label predicts what the click does.
export const BTN = {
  showSetupCommand: "Show setup command",
  recheck: "Re-check",
  installGuide: "Install guide →",
  recheckLogin: "Re-check login",
  reconnect: "Reconnect",
  finishLater: "Finish later",
  finishLaterHint: "Come back anytime from Getting started.",
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
    "The agent works through a short-lived, scoped credential — your stored key stays in Wardyn.",
  // Honest exception: a git PAT grant is injected into git INSIDE the sandbox as
  // the credential, so the agent's process can read it. Screens rendering a
  // git_pat grant must use THIS line, not brokerLine.
  gitPatLine:
    "A git access token is handed to git inside the sandbox — the agent's process can read it.",
  // Honest exception (same shape as gitPatLine): an ssh_key grant writes a
  // RESIDENT private key file for the sandbox's git-over-SSH client to read —
  // git's SSH transport has no credential-helper seam, so the key can't be
  // proxy-injected or brokered like github_token.
  sshKeyLine:
    "A private SSH key is written to disk in the sandbox — the agent's process can read it.",
} as const;

// Risk grades (D8) — deterministic, computed by Wardyn's rules (not the model).
// Only High gates an acknowledgment.
export const RISK_ATTRIBUTION = "Graded by Wardyn's rules, not the model.";

// Approval blast-radius banners (D1) — every approval kind gets two lines:
// what you're approving, and the worst realistic outcome. The scope-specific
// text is filled by the Approvals screen; these are the fixed labels + intents.
export type ApprovalKind = "tool" | "credential" | "egress";
export const APPROVAL_BANNER_LABEL = {
  what: "What you're approving:",
  blast: "Blast radius:",
} as const;
export const APPROVAL_KIND_LABEL: Record<ApprovalKind, string> = {
  tool: "Tool call",
  credential: "Credential",
  egress: "Network egress",
};

// One exit verb across the first-run funnel (B13).
export const EXIT_VERB = BTN.finishLater;
