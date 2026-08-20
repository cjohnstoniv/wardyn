/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Single-sourced console copy — the redesign's glossary in ONE place so screens
// can't drift (D3/D6/D9/D11/B6 + the honesty rules). Barrier metadata and the
// every-tier note live in cc-meta.ts (CC_META / CONFINEMENT_CONSTANT_NOTE) —
// import from there, never duplicate them here.
import type { ApprovalKind as WireApprovalKind, ApprovalRequest, ApprovalScope } from "../../lib/types";
import { shortTime } from "../../lib/format";

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

// ============================================================
// Egress-approval decision scopes — egress_domain only. Canon strings from
// egress-scopes-PHASE0-COPY.md (the sign-off artifact; the mock/copy spec is
// UI source of truth in this repo — do not paraphrase these). Consumed by the
// three decision surfaces (live-approvals.tsx, reason-dialog.tsx) plus the
// blast-radius banner and decided-row badge (approvals.tsx, run-detail.tsx).
// ============================================================
export const APPROVAL_SCOPE_ORDER: ApprovalScope[] = ["once", "run", "until", "always"];

// Menu label, approve flavor.
export const APPROVAL_SCOPE_LABEL: Record<ApprovalScope, string> = {
  once: "Once",
  run: "This run",
  until: "Until…",
  always: "Always",
};
// The honest sub-label under each approve option — Phase 0 §1. "Once"
// deliberately says CONNECTION, not request: on HTTPS the proxy sees one
// CONNECT tunnel and everything inside it rides one decision; on plain HTTP it
// re-decides per request. "Connection" is true for the first and understates
// the second, which is the safe direction.
export const APPROVAL_SCOPE_HINT: Record<ApprovalScope, string> = {
  once: "This one connection. The next attempt asks again.",
  run: "Every attempt until this run ends. (default)",
  until: "Every attempt until a time you pick, or this run ends.",
  always: "Also saves it to the workspace — future runs start with it.",
};
// Menu label, deny flavor — Phase 0 §2.
export const DENY_SCOPE_LABEL: Record<ApprovalScope, string> = {
  once: "Deny once",
  run: "Deny for this run",
  until: "Deny until…",
  always: "Deny always",
};
export const DENY_SCOPE_HINT: Record<ApprovalScope, string> = {
  once: "Blocks this one connection. The next attempt asks again.",
  run: "Blocks it for the rest of this run.",
  until: "Blocks it until a time you pick.",
  always: "Also saves the block to the workspace — future runs start blocked.",
};

// Preset durations behind "Until…" (Phase 0's control shape). ms, not sec —
// callers do `new Date(Date.now() + ms)`.
export const UNTIL_PRESETS: { label: string; ms: number }[] = [
  { label: "15 minutes", ms: 15 * 60_000 },
  { label: "1 hour", ms: 60 * 60_000 },
  { label: "8 hours", ms: 8 * 60 * 60_000 },
  { label: "24 hours", ms: 24 * 60 * 60_000 },
];

// The deny confirm dialog's description (Phase 0 §3) — replaces a sentence
// that was flatly false for two of the four scopes: "Denying blocks this host
// for the rest of the session — there is no undo and no re-raise once it's
// denied" is wrong for `once` (which re-raises on the next attempt) and wrong
// for `always` (which outlives "the session" AND can be undone in the
// workspace's egress settings). `until` renders the honest reason it's
// missing a time rather than silently rendering "undefined".
export function denyDialogCopy(scope: ApprovalScope, opts: { until?: string } = {}): string {
  switch (scope) {
    case "once":
      return "Denying blocks this one connection. The agent can try again, and you'll be asked again.";
    case "until":
      return `Denying blocks this host until ${opts.until ? shortTime(opts.until) : "the time you pick"}. After that you'll be asked again.`;
    case "always":
      return "Denying blocks this host for the rest of this run and saves the block to the workspace, so future runs start blocked. You can undo it in the workspace's egress settings.";
    case "run":
    default:
      return "Denying blocks this host for the rest of this run. The agent won't be able to reach it, and you won't be asked again.";
  }
}

// The blast-radius banner's egress_domain case (Phase 0 §4) — consumed by
// approvals.tsx's deriveBanner ONLY, always called with the "run" scope: the
// PENDING card previews what a plain Approve does, before any scope is
// chosen, and that preview never sees the scope picker inside ReasonDialog
// (a separate component). reason-dialog.tsx does NOT read this function — its
// own live per-option comparison is APPROVAL_SCOPE_HINT/DENY_SCOPE_HINT below,
// a deliberately shorter, host-less copy family (all four scopes shown side
// by side, so brevity matters more than the fuller prose here). The two
// families are NOT single-sourced and can drift; keep them saying the same
// thing about each scope by hand when either changes.
//
// The Phase 0 spec interpolates a {workspace} name into the `always` case;
// this console has no workspace-name lookup in hand on either call site (only
// workspace_ids — see runs.ts), so it says "the workspace" rather than
// inventing or fetching one. Upgrade path: thread a name through once a
// caller actually has one resolved.
export function egressBlastRadius(scope: ApprovalScope, host: string, until?: string): { what: string; blast: string } {
  switch (scope) {
    case "once":
      return {
        what: `One outbound connection to ${host}.`,
        blast: "That one connection. The next attempt stops and asks you again.",
      };
    case "until":
      return {
        what: `Outbound access to ${host} until ${until ? shortTime(until) : "the time you choose"}.`,
        blast: `The run can reach ${host} until then; after that it asks again.`,
      };
    case "always":
      return {
        what: `Outbound access to ${host}, saved to the workspace.`,
        blast: `This run and every future run of the workspace can reach ${host} without asking. Undo it in the workspace's egress settings.`,
      };
    case "run":
    default:
      return {
        what: `Outbound access to ${host} from this run.`,
        blast: `The run can reach ${host} until it ends. No other new domain opens.`,
      };
  }
}

// Disabled-`Always` reason, no-workspace case (Phase 0 §5) — the OTHER gate,
// non-operator, reuses OPERATOR_ONLY_REASON below rather than a second string.
export const ALWAYS_NEEDS_WORKSPACE = "Always needs a workspace — this run isn't attached to one.";

// Decided-row scope badge (Phase 0 §6) — "once" / "this run" / "until 5:04 PM"
// / "always" / "expired". undefined for a non-egress_domain kind, a row with
// no recorded decision (PENDING, or decided before this feature shipped), or
// EXPIRED (ExpireStale writes the zero scope deliberately — see
// internal/approval/approval.go — an expiry is a sweep nobody decided, not a
// scope worth badging). Callers pair this with the existing
// ApprovalStateBadge, which already says Approved/Denied — this is just the
// "· once" half.
export function approvalScopeBadge(
  item: Pick<ApprovalRequest, "kind" | "state" | "decision_scope" | "decision_expires_at">,
): string | undefined {
  if (item.kind !== "egress_domain") return undefined;
  if (item.state !== "APPROVED" && item.state !== "DENIED") return undefined;
  const scope = item.decision_scope;
  if (!scope) return undefined;
  if (scope === "until") {
    const expired = !!item.decision_expires_at && new Date(item.decision_expires_at).getTime() <= Date.now();
    return expired ? "expired" : item.decision_expires_at ? `until ${shortTime(item.decision_expires_at)}` : "until";
  }
  return APPROVAL_SCOPE_LABEL[scope].toLowerCase();
}

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
  // session. Mirrors the confirm-required deny in live-approvals.tsx — NOT
  // "irreversible" (a deny can re-raise at `once` scope and can always be
  // undone in the workspace's egress settings at `always` scope), just a
  // consequential action that deserves a stop, same as this one.
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
  // The OTHER 409: a finished run whose sandbox was torn down. Two strings for
  // two opposite facts — "yet" on a run that ended days ago read as a promise.
  sandboxGone: "This run has finished — its sandbox is gone.",
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

// UI apps lane (docs/design/ui-sandboxes-prompt.md §7, FROZEN) — the run-detail
// "Attach from your terminal" card's third lane and the policies screen's
// read-only ui_apps row. Every byte here is canon; run-detail-ssh.tsx and
// run-detail-ssh.test.tsx must render/assert these verbatim, never a paraphrase.
export const UI_APPS_LANE = {
  title: "UI apps",
  intro:
    "Wardyn relays a port the sandbox is already listening on to your browser. The sandbox gets no network of its own — the relay rides the same exec lane the terminal does.",
  appSub: (port: number, path: string) => `localhost:${port}${path}`,
  cta: (app: string) => `Open ${app}`,
  ctaBusy: "Opening…",
  newTab:
    "Opens in a new tab, on a different address than this console. That separation is deliberate: the app is the sandbox's own code, and it must never be able to read your console session.",
  noRecording:
    "Session recording does not capture this: no keystrokes, no screen, no page content. Wardyn records that you opened and closed the app, never what you did in it.",
  off: "Off on this deployment. It relays a declared loopback port inside the sandbox — a code editor, a dev server — to your browser through Wardyn. An operator turns it on by setting WARDYN_UI_SANDBOX_LISTEN where wardynd starts.",
  noApps:
    "On for this deployment, but this run's policy declares no UI apps. The relay serves only ports named in the policy's ui_apps list — an app is a name, a loopback port and a path.",
  errorTitle: (app: string) => `Couldn't start ${app}`,
  errorLauncher: (app: string) =>
    `This image has no /usr/local/bin/wardyn-ui-${app}. Use an image that ships the launcher (deploy/images/vscode/), or add one to your own image.`,
  // Not from the frozen table (client-side condition, no server round trip) —
  // covers the mock's step-3 "failure in window.open" case (a blocked popup).
  errorPopupBlocked:
    "The browser blocked the new tab — allow pop-ups for this site and try again.",
} as const;

// Prefix of the server's verbatim missing-launcher body (docs/design/ui-
// sandboxes-prompt.md §7's "server-side counterpart"), used to decide whether
// to prepend the friendly lane.error.launcher guidance above the raw text.
export const UI_APPS_LAUNCHER_MISSING_PREFIX = "no UI launcher in this image:";

// Policies screen's read-only ui_apps detail row (same frozen table, §7).
export const POLICY_UI_APPS = {
  label: "UI apps",
  none: "None declared",
  value: (app: string, port: number, path: string) => `${app} → localhost:${port}${path}`,
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
