/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Single-sourced console copy — the redesign's glossary in ONE place so screens
// can't drift (D3/D6/D9/D11/B6 + the honesty rules). Barrier metadata and the
// every-tier note live in cc-meta.ts (CC_META / CONFINEMENT_CONSTANT_NOTE) —
// import from there, never duplicate them here.
import type { ApprovalKind as WireApprovalKind } from "../../lib/types";

// The single residual-risk prefix (D11) — everywhere a tier is explained, never
// dropped or softened. The residual text itself is CC_META[*].doesntProtect.
export const RESIDUAL_PREFIX = "Doesn't stop:";

// Run mode pair (D3) — the only two mode names, used verbatim everywhere. Banned:
// Batch, Background, "Runs unattended"/"You drive it" AS mode names.
export type RunMode = "interactive" | "autonomous";
export const RUN_MODE: Record<RunMode, { label: string; blurb: string }> = {
  interactive: {
    label: "Interactive",
    // UX-7: an interactive run execs NO agent at all (internal/types/types.go:
    // "no agent task is exec'd and no completion watcher is started — the
    // human drives via wardyn attach") — the old blurb described per-action
    // approval, an autonomous-shaped behavior this mode doesn't have. Matches
    // compose-form.tsx's own Field hint for the same toggle.
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
  // Honest exception: a git PAT grant is injected into git INSIDE the sandbox as
  // the credential, so whatever's running there can read it. Screens rendering a
  // git_pat grant must use THIS line, not brokerLine.
  gitPatLine:
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
export const OPERATOR_ONLY_REASON = "Requires the operator role.";

// Said once, where a viewer would actually feel the consequence (the run's
// own pending-approval banner) — the read/launch/kill viewer tier still means
// their run blocks on an approval exactly like an operator's does; only the
// deciding is out of reach.
export const VIEWER_APPROVAL_BLOCKS_NOTE =
  "This run is blocked until an operator decides it — you can see the requested scope below, but deciding needs the operator role.";

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

// Run cockpit (/runs/:id) — the terminal-first live-run screen. Every string
// the redesign introduces lands HERE first, so the four Terminal states and the
// evidence widgets can't drift into inventing their own vocabulary.
export const RUN_COCKPIT = {
  // --- Terminal widget, four states (design board 2d) ---
  // 1. You hold the PTY.
  driving: "you are driving",
  // NOTE: there is deliberately no "drivingHint" here. One existed, unreferenced,
  // advertising "⇧⌘F for real fullscreen" — nothing binds that chord; fullscreen
  // is the header button only. A hint for a shortcut that does not exist is
  // worse than no hint, and an unrendered string is a trap waiting for someone
  // to render it. Add it back the day the chord is actually wired.
  // 2. Someone else holds it. Attach is a SHARED tmux session, so this is a
  // real state the console used to have no words for — it just competed for
  // the same PTY silently.
  heldBy: (principal: string) => `held by ${principal}`,
  watchingReadOnly: "watching read-only — keystrokes go nowhere",
  // Does NOT assert a transport. The holder's source is on the wire
  // (AttachHolder.source, "web" | "ssh"), and "from a CLI" was wrong for the
  // very common case of a second browser tab or another operator's console —
  // it sent people hunting for a terminal window that did not exist.
  heldHint:
    "Someone else is driving this session. You can watch it live, or take it from them — they get told, and it lands in the audit trail.",
  // Where the transport IS known, say it — it is the one detail that tells an
  // operator where to go look.
  heldBySource: (source: string) =>
    source === "ssh" ? "attached over SSH" : source === "web" ? "attached from a browser" : "",
  // The DISPLACED case is not the same sentence. heldHint says "someone is
  // already driving" — true when you arrive second, false and confusing when
  // you were driving and got taken over. This one describes what actually
  // happened, and says the session is intact: tmux survives, so reclaiming it
  // costs nothing but a click.
  displacedHint: (principal: string) =>
    `${principal} took over this session. Nothing was lost — the terminal is still running, and you can take it back.`,
  // Same event, but a proxy ate the close reason so we cannot name who did it.
  // It must NOT fall through to heldHint, which offers to "watch it live" and
  // "take it from them" — the socket is closed and there is no named principal
  // to take it from, so both halves of that sentence would be false.
  displacedUnknownHint:
    "Your session was taken over. Nothing was lost — the terminal is still running; reconnect to take it back.",
  takeOver: "Take over",
  // Deliberately concrete about the consequence: take-over ENDS someone's
  // session. Mirrors the irreversible-deny confirm in live-approvals.tsx.
  takeOverConfirm: (principal: string) =>
    `${principal} is driving this session now. Taking over disconnects them and records you as the holder in the audit trail.`,
  // 3. No PTY to type into — the agent drives.
  autonomous: "autonomous — the agent drives",
  // 4. Terminal state: the pane becomes the replay surface in place.
  finishedReplay: "run finished · replay",
  // The in-place replay pane's two empty states. They exist HERE because the
  // same two facts are already stated by the Recording tab (states.tsx /
  // recording.tsx), and the inline copies had ALREADY drifted from those —
  // a trailing period on one, and a short form that dropped the explanation of
  // when a recording is produced at all.
  recordingLoading: "Loading the captured session…",
  recordingDisabled: "Session recording is disabled on this deployment",
  recordingMissing:
    "This run has no captured terminal session. A recording is produced once an agent process runs in the sandbox.",

  // --- Evidence widgets ---
  // Same eligibility framing as the full Credential grants card, tightened for
  // a 400px rail. Grants are what the run MAY request — never live credentials.
  credentialsEligibility:
    "Eligibility — what this run may request. The broker mints a short-lived, scoped token; the agent never sees your real keys.",
  // A metric the sandbox did not report. gVisor (Vault) presents a synthetic
  // procfs/sysfs and may withhold the cgroup files entirely — saying so is the
  // honest answer; rendering 0 would claim the sandbox is using no memory.
  metricUnavailable: "not available on this barrier",
  // The workspace has no git work tree, so there is no diff to state. NAMES the
  // directory that was inspected when the daemon reports one: the mount target
  // is configurable per workspace source, so a bare "not a git repository"
  // cannot be told apart from "we looked in the wrong place" — and that
  // mistake is otherwise completely silent.
  // git ran and failed for a reason that is not "no work tree" — most often it
  // is not installed in the sandbox image. Names the path but does NOT assert
  // anything about the workspace, because we genuinely do not know.
  vcsUnknown: (path?: string) =>
    path
      ? `Couldn't read a diff at ${path} — git didn't run there. Check the sandbox image.`
      : "Couldn't read a diff — git didn't run in this sandbox. Check the sandbox image.",
  noVcs: (path?: string) =>
    path
      ? `No git repository at ${path} — there's no diff to show.`
      : "This workspace isn't a git repository — there's no diff to show.",
  // The substrate has no exec channel, so neither evidence read can run.
  execUnsupported: "This runner can't be inspected while it's running.",
  // Empty/degraded states shared by the two POLLING evidence widgets. They live
  // here rather than as per-file consts because both widgets need the same two
  // sentences — a duplicated literal in each file is exactly the drift this
  // module exists to prevent.
  noFilesChanged: "No files changed yet.",
  noSandboxYet: "No sandbox for this run yet.",
  // A transient poll failure. Deliberately not an error banner: both widgets
  // keep their last-good data on a background poll blip (the run screen's own
  // rule — see the load() catch in run-detail.tsx), so this is a quiet note,
  // not an alarm.
  loadError: "Couldn't load right now.",
  filesTruncated: "Showing a partial list — more files changed than are shown here.",

  // --- Command bar ---
  // The pending-approval chip. Two forms, because they are two different facts:
  // an approval merely queued vs. one that is HOLDING the sandbox right now
  // (a wait_for_review first-use gate — the connection is parked until someone
  // decides). Only the second earns the urgency, and the command bar is the one
  // place that fact is visible without scrolling or clicking.
  waiting: (n: number) => `${n} waiting`,
  waitingHeld: (n: number) => `${n} waiting · sandbox held`,
  // Phase-2 layout controls (the canvas + its edit-mode toolbar).
  layoutPreset: (preset: string) => `layout: ${preset}`,
  addWidget: "Add widget",
  editLayout: "Edit layout",
  editing: "Editing layout",
  resetLayout: "Reset to default",
  saveLayoutDefault: "Save as my default",
  doneEditing: "Done",
  layoutSaved: "Saved as your default layout",
  // Honest, not an error: the deployment's store cannot persist layouts (the
  // endpoint 501s), so the arrangement is real but session-scoped. Said ONCE,
  // inline in the toolbar — never a toast per drag.
  // THIS attempt failed and the next may not — distinct from layoutNotPersisted,
  // which is a permanent fact about the deployment.
  layoutSaveFailed: "Couldn't save that layout — try again.",
  layoutNotPersisted:
    "This deployment can't store layouts — your arrangement lasts for this session.",
  removeWidget: (label: string) => `Remove ${label}`,

  // --- Focus mode (design board 2c) ---
  enterFocus: "Focus",
  exitFocus: "Exit focus",
  // The dock's rail is a widget switcher, so its accessible name says which
  // widget each button shows — see DOCK_BUTTON below.
  dock: "Widget dock",
  closeDock: "Close the widget dock",
  showWidget: (label: string) => `Show ${label}`,
  // Bottom strip. The three counts are the SAME facts the Egress and
  // Credentials widgets state, said as text you can read without opening
  // anything — which is the whole point of the strip on this board.
  egress: "egress",
  allow: (n: number) => `${n} allow`,
  held: (n: number) => `${n} held`,
  deny: (n: number) => `${n} deny`,
  credentials: "credentials",
  // Grants are ELIGIBILITY (what the run MAY request), never live credentials —
  // the same distinction credentials.tsx makes. Only a credential.mint audit
  // event with outcome "success" is a real issued credential.
  eligible: (n: number) => `${n} eligible`,
  brokered: (n: number) => `${n} brokered`,
  // Only the shortcuts this file actually binds. ⌘K (no command palette
  // exists), ⌘1..4 (the tabs belong to run-detail-command-bar.tsx) and ⇧⌘F
  // (AttachTerminal ships a fullscreen BUTTON, not a key binding) are on the
  // board's strip and deliberately absent here: a hint that lies is worse than
  // no hint.
  shortcuts: "⌘\\ dock · Esc exit focus",
} as const;

// ui-approvals-2: the wire kind (ApprovalRequest.kind, e.g. "egress_domain")
// -> this file's copy-vocabulary kind (ApprovalKind above). Hoisted from
// approvals.tsx (its own kindLabel() reads it via APPROVAL_KIND_LABEL) so
// ApprovalKindChip (primitives.tsx) renders the SAME human label as every
// title/banner derived from APPROVAL_KIND_LABEL, instead of keeping a
// second, wire-shaped label table of its own.
export const WIRE_TO_COPY: Record<WireApprovalKind, ApprovalKind> = {
  credential: "credential",
  egress_domain: "egress",
  tool_call: "tool",
};
