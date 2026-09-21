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

// DRAFT (M2 canon pending) — F5-F3. Removing an allowed host is PUT
// .../approved-egress plus, for an operator-authored requirements row, PUT
// .../requirements. A row that is in NEITHER — a host the workspace's own scan
// seeded — has no write to make: the pair fired, nothing changed, and the row
// came back with its old provenance.
export const EGRESS = {
  SCAN_SEEDED_REASON: "Detected by this workspace's scan, not approved here — the next scan puts it back.",
  // The other half of the same mismatch: an operator-authored requirement that
  // reaches this workspace through the EFFECTIVE fold rather than its own
  // overlay. The requirements PUT here replaces only the overlay, so clearing
  // it has to happen where it was written. Distinct from the scan sentence
  // above, which would contradict the row's own "required by this workspace".
  INHERITED_REASON: "Required by a source this workspace composes, not by an approval here — clear it where it was written.",
} as const;

// Said once, where a viewer would actually feel the consequence (the run's
// own pending-approval banner) — the read/launch/kill viewer tier still means
// their run blocks on an approval exactly like an admin's does; only the
// deciding is out of reach.
export const VIEWER_APPROVAL_BLOCKS_NOTE =
  "This run is blocked until an admin decides it — you can see the requested scope below, but deciding needs the admin role.";

// Approval blast-radius banners (D1) — every approval kind gets two lines:
// what you're approving, and the worst realistic outcome. The scope-specific
// text is filled by the Approvals screen; these are the fixed labels + intents.
export type ApprovalKind = "tool" | "credential" | "egress" | "reauth";
export const APPROVAL_BANNER_LABEL = {
  what: "What you're approving:",
  blast: "Blast radius:",
} as const;
export const APPROVAL_KIND_LABEL: Record<ApprovalKind, string> = {
  tool: "Tool call",
  credential: "Credential",
  egress: "Network egress",
  reauth: "AWS sign-in",
};

// The ONE console label for the CANCELLED approval state (the run ended before
// anyone decided it — internal/types/types.go ApprovalCancelled). Every other
// state is title-cased from the wire value by ApprovalStateBadge; this one is a
// key so the owner's wording lands in one place.
// DRAFT (M2) — not yet owner-frozen.
export const APPROVAL = {
  STATE_CANCELLED: "Cancelled",
  // B4, console half: what a human reads WHERE the Approve/Deny pair used to
  // be once the run has ended, and on the decided row the cascade wrote. One
  // string for both surfaces on purpose — they state the same fact, and two
  // copies would be two wordings of it.
  // DRAFT (M2 canon pending) — staged in workspace-providers-prompt.md §7.6.
  CANCELLED_BODY: "The run ended before anyone decided this. Nothing was approved and nothing was denied.",
  // P0.3 (R3-F001/F108/F145): an egress_domain approval is HOST-WIDE — the
  // server strips any port before it keys the decision (approvalHostKey,
  // internal/egress/proxy/approvals.go). 0.7.2 aligns the three surfaces to SAY
  // so rather than rely on it quietly; the port-scoped semantic is 0.8's.
  // DRAFT (M2 canon pending).
  HOST_WIDE_NOTE: "This covers the host, not one port — an approval here answers every port on it.",
} as const;

// Egress-approval decision scopes — egress_domain only. Canon strings from
// egress-scopes-PHASE0-COPY.md (the sign-off artifact; the mock/copy spec is
// UI source of truth in this repo — do not paraphrase these). Consumed by the
// three decision surfaces (live-approvals.tsx, reason-dialog.tsx) plus the
// blast-radius banner and decided-row badge (approvals.tsx, run-detail.tsx).
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

// The deny confirm dialog's description (Phase 0 §3) must not read "Denying
// blocks this host for the rest of the session — there is no undo and no
// re-raise once it's denied" — that is wrong for `once` (which re-raises on
// the next attempt) and wrong for `always` (which outlives "the session" AND
// can be undone in the workspace's egress settings). `until` must render the
// honest reason it's missing a time rather than silently rendering
// "undefined".
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

// D7 — the tag LiveApprovals shows on a pending row whose host matches the
// agent CLI's own known telemetry endpoint (WARDYN_ALLOW_AGENT_TELEMETRY
// opt-in, or a historical run from before that switch defaulted to
// suppressing it). Cockpit-adjacent, not a screen of its own — kept beside
// RUN_COCKPIT rather than folded into it.
export const TELEMETRY_TAG = {
  label: "Agent telemetry",
  title: "The agent CLI's usual diagnostics endpoint. Approve or deny it like any other host.",
};

// Run cockpit (/runs/:id) — the terminal-first live-run screen. Every string
// the redesign introduces lands HERE first, so the four Terminal states and the
// evidence widgets can't drift into inventing their own vocabulary.
export const RUN_COCKPIT = {
  // --- Terminal widget, four states (design board 2d) ---
  // 1. You hold the PTY.
  driving: "you are driving",
  // There is deliberately no "drivingHint" here. One existed, unreferenced,
  // advertising "⇧⌘F for real fullscreen" — nothing binds that chord; fullscreen
  // is the header button only. A hint for a shortcut that does not exist is
  // worse than no hint, and an unrendered string is a trap waiting for someone
  // to render it. Add it back the day the chord is actually wired.
  // 2. Someone else holds it. Attach is a SHARED tmux session, so this state
  // needs its own words — without them, a second attacher just competes for
  // the same PTY silently.
  heldBy: (principal: string) => `held by ${principal}`,
  watchingReadOnly: "watching read-only — keystrokes go nowhere",
  // Does NOT assert a transport. The holder's source is on the wire
  // (AttachHolder.source, "web" | "ssh") — "from a CLI" would be wrong for the
  // very common case of a second browser tab or another operator's console,
  // sending people hunting for a terminal window that does not exist.
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
  // DRAFT (M2 canon pending) — F1-F1: an interactive run canAttach may open
  // once it reaches RUNNING (PENDING/STARTING/WAITING_FOR_CONFIRMATION so
  // far) must not be told OPERATOR_ONLY_REASON, which is false for them —
  // that sentence is for the caller who can never attach, not the one who
  // merely has to wait. No link: there is no recording yet either.
  starting: "This run hasn't started yet — the terminal opens once it's running.",
  // 3. No PTY to type into — the agent drives.
  autonomous: "autonomous — the agent drives",
  // 3b. Same pane, exec task mode: a shell command ran with NO agent harness
  // (task_mode is audit-only, createRequestFromAudit) — "the agent drives" would be
  // false five ways on a run whose form said "No agent, no model".
  execNoHarness: "exec — shell command, no agent harness",
  // 4. Terminal state: the pane becomes the replay surface in place.
  finishedReplay: "run finished · replay",
  // The in-place replay pane's two empty states live HERE because the same
  // two facts are already stated by the Recording tab (states.tsx /
  // recording.tsx) — a second copy is exactly how a trailing period or a
  // dropped explanation of when a recording is produced would drift between
  // them.
  recordingLoading: "Loading the captured session…",
  recordingDisabled: "Session recording is disabled on this deployment",
  recordingMissing:
    "This run has no captured terminal session. A recording is produced once an agent process runs in the sandbox.",
  // The THIRD fact: a fetch that FAILED establishes nothing about the run, so
  // it must not be reported as "this run has no recording". Same sentence the
  // Recording tab already shows
  // for the same failure (run-detail.tsx's RecordingTab) — hoisted here so the
  // two cannot drift, exactly like the two above it.
  recordingError: "Couldn't load this run's recording.",

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
  //
  // M3: the same two hints, as two labelled key chips rather than one run of
  // text — the key itself is drawn by Kbd (kbd.tsx), which spells the modifier
  // for the platform, so only the LABEL is copy. Same words, split at the
  // separator they already had.
  shortcutDock: "dock",
  shortcutExitFocus: "exit focus",
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
  // The off-state's one affordance (mock M6): a pointer to the page that says
  // how to turn it on, next to the need rather than in a footer (§9). NOT an
  // <a href> — the console does not serve docs/, so a real link would 404;
  // the repo's pattern is to name the file, as policy-panel.tsx does for
  // docs/POLICIES.md. No button, either: a viewer cannot flip a server env var,
  // and offering one would be a lie about who can act.
  offDoc: "Read how to enable UI sandboxes",
  offDocPath: "docs/UI-SANDBOXES.md",
  noApps:
    "On for this deployment, but this run's policy declares no UI apps. The relay serves only ports named in the policy's ui_apps list — an app is a name, a loopback port and a path.",
  errorTitle: (app: string) => `Couldn't start ${app}`,
  errorLauncher: (app: string) =>
    `This image has no /usr/local/bin/wardyn-ui-${app}. Use an image that ships the launcher (deploy/images/vscode/), or add one to your own image.`,
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
  // Its OWN kind, not "credential" (UX round B3): mapping it there would make
  // /approvals title the row "Mint a scoped credential" and paint the
  // blast-radius banner over a request that mints nothing.
  credential_reauth: "reauth",
};

// Hoisted from approvals.tsx (kept out of live-approvals.tsx's own module,
// which is a static import — see live-approvals.tsx's H2 comment for why the
// screen module itself can't be the source instead) so both files read the
// SAME kind-sniffing heuristic rather than keeping two that could drift.
export type CredentialKind = "git_pat" | "github_token" | "api_key" | "ssh_key" | "generic";
// H7 fix: requested_scope never carries the grant KIND (ApprovalRequest.kind is
// only the wire-level "credential"/"egress_domain"/"tool_call"), so this stays a
// key-sniffing heuristic. Order matters:
//  - key_secret_ref is UNIQUE to ssh_key (broker.sshKeyScope) — must be checked
//    before the git_pat fallback, or every ssh_key approval (a resident,
//    agent-readable PRIVATE KEY, not a PAT) rendered the git_pat banner.
//  - api_key's scope ALSO carries secret_name (the broker requires it), so the
//    api_key discriminators (header/format) must be checked before the git_pat
//    fallback — otherwise every api_key approval renders the git_pat banner,
//    which claims the agent's process can read the key (the opposite of
//    api_key's proxy-side injection design).
// a MINIMAL api_key scope ({host,secret_name} only, header/format
// omitted since the broker defaults them) is indistinguishable from a minimal
// git_pat scope ({host,secret_name}, username omitted) by keys alone — genuine
// wire-format ambiguity, not fixable client-side. We bias the fallback toward
// git_pat: it's the safer direction (never under-claim exposure) and the only
// in-app path that produces a truly minimal scope today (git_pat with a blank
// username field). Upgrade path: expose the real GrantKind on ApprovalRequest.
export function credentialKind(scope: Record<string, unknown>): CredentialKind {
  if ("repos" in scope || "permissions" in scope) return "github_token";
  if ("key_secret_ref" in scope) return "ssh_key";
  if ("header" in scope || "format" in scope) return "api_key";
  if ("secret_name" in scope || "username" in scope) return "git_pat";
  return "generic";
}

// Member Getting Started's "Your model key" section (6c BYOK) — the
// provider-conventional secret name the member's own key is stored under, and
// the per-state copy. The name is a VALUE (not just a type) because the field
// shows it verbatim as a mono label; since X3-F3 it is per-provider, so
// BY_AGENT below is the only place it lives.
export const YOUR_MODEL_KEY = {
  // DRAFT (M2 canon pending) — X3-F3. The member's own key is stored under the
  // PROVIDER's conventional name, and which provider that is follows the org's
  // agent roster: a codex-only roster cannot use an anthropic key at all, so
  // offering one was a write nothing would ever read. MEMBERS.md already names
  // both. Keyed by the harness catalog id the roster row carries.
  BY_AGENT: {
    "claude-code": { secretName: "anthropic-api-key", placeholder: "sk-ant-…" },
    "codex-cli": { secretName: "openai-api-key", placeholder: "sk-…" },
  },
  EMPTY_BODY:
    "Bring your own key. It is stored write-only under the provider's conventional name; nothing ever reads it back to you.",
  SET_HINT: "Your runs can use this key — pick it under Model access when you launch.",
  PROVIDED_CHIP: "Provided by your admin",
  PROVIDED_BODY: "Model access is already configured for you.",
  USE_OWN_KEY: "Use my own key instead",
  REFUSED_SHORT: "Keys shorter than 8 characters are refused.",
  SAVE_ERROR: "Couldn't save this key.",
  REMOVE_ERROR: "Couldn't remove this key.",
  // DRAFT (M2 canon pending) — Appendix A finding 2. Under a per_user roster
  // row the card reads status.model_access instead of the deployment-wide
  // llm_ready (see model-key-state.ts's total truth table). EXPIRING reuses
  // SIGNED_IN_BODY (still true — the sign-in just needs renewing soon) and
  // SHARED_EXPIRED reuses AGENTS.MODEL_ACCESS_SHARED_EXPIRED for its chip
  // (workspace-providers-copy.ts).
  SIGNED_IN_CHIP: "Your AWS sign-in",
  SIGNED_IN_BODY: "You signed in to AWS. Your runs use your own session.",
  EXPIRING_CHIP: "Your AWS sign-in · Expiring",
  NOT_SIGNED_IN_CHIP: "Not signed in",
  NOT_SIGNED_IN_BODY: "Sign in to AWS to give your runs model access. Nothing is configured for you until you do.",
  SHARED_EXPIRED_BODY: "Ask your admin to sign in again.",
  // FIX PASS 1 (REVIEW-1.md rulings R1/R2) — the two "unknown" bodies a
  // member with no actionable state can land on: PER_PERSON_NA_BODY for a
  // per_user row whose model_access is a state the card takes no action on
  // (not_applicable — the admin-token principal on an enabled per_user row —
  // is the common real case, not a skew artifact); ADMIN_NOT_READY_BODY for
  // a shared row whose mechanism is Bedrock but llmReady is false.
  PER_PERSON_NA_BODY: "Model access on this deployment is per person. There is nothing to set up for this sign-in.",
  ADMIN_NOT_READY_BODY: "Model access is not set up on this deployment yet. Ask your admin.",
  // DRAFT (M2 canon pending) — U-10: `expired_signin` and `not_configured` both
  // grade `not_signed_in`, and NOT_SIGNED_IN_BODY's "Nothing is configured for
  // you until you do" is false for the first: a session IS stored for this
  // member and it stopped working — which is also what the server's own action
  // line on the same page says.
  EXPIRED_SIGNIN_BODY: "Your AWS sign-in is no longer valid. Sign in to AWS again to give your runs model access.",
  // DRAFT (M2 canon pending) — U-13's other half; see
  // MEMBER_GETTING_STARTED.SIGN_IN_AWS_ARIA_SUMMARY for why.
  SIGN_IN_AWS_ARIA_CARD: "Sign in to AWS — from Your model key",
} as const;

// Member Getting Started (Phase 5) — the six-SectionCard page a member lands
// on at /setup. Canon per the approved mock; member-getting-started.tsx is
// the sole reader.
export const MEMBER_GETTING_STARTED = {
  TITLE: "Getting started",
  SUBTITLE: "You're a member of this Wardyn. Your admin set the ceiling; you run inside it.",
  UNREACHABLE_TITLE: "Couldn't reach Wardyn.",
  UNREACHABLE_BODY:
    "Nothing below is marked done until it can be checked — a broken connection is not a finished step.",
  RETRY: "Retry",
  SETUP_SUMMARY_TITLE: "What's set up for you",
  SETUP_SUMMARY_HELPER: "Your admin configured the barrier, network and shared credentials. Your runs inherit them.",
  BARRIER_CHIP: (label: string) => `Barrier · ${label}`,
  MODEL_ACCESS_OWN_CHIP: "Model access · Your key",
  MODEL_ACCESS_PROVIDED_CHIP: "Model access · Provided by your admin",
  SIGNIN_SSO_CHIP: "Sign-in · SSO",
  WORKSPACE_TITLE: "Add your workspace",
  WORKSPACE_BODY: "A repo or directory a run can attach. Runs can only attach what is listed here.",
  WORKSPACE_ERROR: "Couldn't check your workspaces.",
  WORKSPACE_ACTION: "Add workspace",
  FIRST_RUN_TITLE: "Your first run",
  FIRST_RUN_BODY: "Launch a governed run against your workspace.",
  FIRST_RUN_HINT:
    "Your policy is clamped to your admin's ceiling. Preflight shows exactly what launch will do — read its warnings before you go.",
  FIRST_RUN_ACTION: "New run",
  APPROVALS_TITLE: "Approvals you can decide",
  APPROVALS_BODY: "When one of your runs reaches a host that isn't on the list, it holds at the door.",
  APPROVALS_HINT:
    "You decide — once, for this run, until, or always. Credential and tool-call approvals stay with your admin.",
  APPROVALS_ACTION: "Open approvals",
  CONNECT_TITLE: "Connect your tools",
  CONNECT_BODY: "Attach from your own terminal or editor over SSH.",
  CONNECT_HINT_PREFIX: "Register a key once: ",
  CONNECT_COMMAND: "wardyn ssh-key ensure",
  CONNECT_ACTION: "Add SSH key",
  // DRAFT (M2 canon pending) — Appendix A finding 2b. Under a per_user
  // roster row the credential is specifically NOT shared and NOT inherited
  // — that is the entire point of the lane, and the chip beside this
  // sentence already says so.
  SETUP_SUMMARY_HELPER_PER_USER:
    "Your admin configured the barrier, network and the model-access lane. Model access uses your own AWS sign-in; your runs inherit the rest.",
  // DRAFT (M2 canon pending) — U-13 (a11y). This page renders TWO buttons whose
  // visible text is "Sign in to AWS" (this card's and "Your model key"'s) plus a
  // plain-text action line saying the same words, so a screen reader's button
  // list carried the same name twice with nothing to choose by. The visible text
  // is unchanged; the accessible name adds the section. It STARTS with
  // AGENTS.SIGN_IN_AWS so a lookup by the visible name still finds it (pinned in
  // member-getting-started.test.tsx).
  SIGN_IN_AWS_ARIA_SUMMARY: "Sign in to AWS — from What's set up for you",
} as const;

// DRAFT (M2 canon pending) — X3-F4, the MEMBER's empty runs board. The operator
// first-run funnel it replaces is a host-barrier readout plus a setup
// checklist: redacted blank for a member, and pointing at routes their role
// cannot reach. These lines are what a member can actually do instead. Sited
// after MEMBER_GETTING_STARTED because GUIDE is that page's own title — the
// link names where it lands, and a second literal is how the two drift.
export const RUNS_MEMBER_EMPTY = {
  TITLE: "Runs you launch appear here",
  BODY: "Nothing is running yet. Start one against a workspace your admin has made available to you.",
  ACTION: "New run",
  GUIDE: MEMBER_GETTING_STARTED.TITLE,
} as const;

// #160 — TitleGroup's second chip row (runs/title-group.tsx): what a group's
// runs are waiting on, one counted chip per reason instead of a bare count.
// `n` is always the GROUP's count for that reason — CONSOLE-RULES §10's "say
// how many", never "some runs need attention".
export const RUNS_WAIT = {
  HELD: (n: number) => `${n} awaiting confirmation`,
  REAUTH: (n: number) => `${n} awaiting AWS sign-in`,
  STARTING: (n: number) => `${n} waiting to start`,
  // Absent while anything else waits; a single UNCOUNTED chip, and only once
  // the approvals fetch has resolved enough to know the group is really clean.
  NONE: "Nothing waiting",
  // The pre-fetch window: nothing derived from the approvals fetch may paint
  // before it resolves, so this stands alone rather than reading as "nothing
  // is held" (the empty-Map default's lie).
  CHECKING: "Checking…",
  // The card's own sentence once a hold isHeld no longer counts as live (the
  // 60-minute stale-hold ceiling, lib/types/approvals.ts) — replaces the
  // per-run reason line, not the group's chip below.
  STALE_CARD: "Was held — check the run",
  // The header's uncounted-elsewhere chip for the same fact, at group
  // granularity: a degraded claim, not silence about what happened here.
  STALE_GROUP: (n: number) => (n === 1 ? "1 was held" : `${n} were held`),
} as const;

// Demo episode rows (episode-card.tsx) — the funnel steps' "Watch" affordance
// and the welcome hero's full catalog. Canon per the approved mock.
export const EPISODES_COPY = {
  WATCH: "Watch",
  CLOSE: "Close",
  NOT_RECORDED: "Not recorded yet",
  STREAM_NOTE: "Streams from the Wardyn release on GitHub only after you press Watch. Nothing is prefetched.",
  LOAD_ERROR: "Couldn't load this episode from GitHub.",
  OPEN_RELEASE_PAGE: "Open the release page",
  ALL_EPISODES_TITLE: "All episodes",
  // Derived from EPISODES (recorded count, summed minutes) — never hand-typed,
  // so a re-shoot that ships/reserves an episode can't leave this stale.
  SUMMARY: (recorded: number, minutes: number) => `${recorded} recorded · about ${minutes} minutes · streamed from GitHub on click`,
  // Shape C grouping (approved mock round 2026-08-31): path-first groups, the
  // install's own deployment leading, the other path collapsed.
  GROUP_CORE: "Start here",
  GROUP_DEPLOYMENT_SINGLE: "Your deployment — single-user",
  GROUP_DEPLOYMENT_MULTI: "Your deployment — multi-user",
  GROUP_ANY: "Running work — any deployment",
  OTHER_PATH_SINGLE: (n: number) => `The single-user path — ${n} episodes`,
  OTHER_PATH_MULTI: (n: number) => `The multi-user path — ${n} episodes`,
  FOR_YOUR_MEMBERS: "For your members",
  MEMBER_YOUR_PATH: "Your path",
} as const;

// DRAFT (M2 canon pending) — X4-F3 (runs-first-run-demos.tsx's "See it work"
// grid subtitle): the prior sentence "No model, no key, no repo" was
// contradicted ten lines below by needsModel/needsSecret — some demo cards
// genuinely require a connected model or a stored secret. Canon row:
// local/v074/canon/docs.md, key runs-first-run-demos.subtitle.
export const FIRST_RUN_DEMOS_SUBTITLE =
  "No repo needed. Most need no model or key either — a few show what a connected model or a stored secret additionally protects. Each one runs a real governed sandbox in about a minute.";

// People step (step-bodies.tsx's DeploymentStep) — canon per the approved mock.
export const PEOPLE_STEP = {
  SINGLE_USER_CHIP: "Single-user",
  MULTI_USER_CHIP: "Multi-user",
  SINGLE_USER_LEDE_LOCAL:
    "One admin credential — no sign-in at all — anyone who reaches this console on this machine is the admin. No per-person identity.",
  SINGLE_USER_LEDE_TOKEN:
    "One admin credential — the token the installer printed. No per-person identity.",
  SINGLE_USER_BODY:
    "Just you. Whoever holds the admin token (or reaches a local-mode console) is the admin; runs, policies, secrets and approvals are all yours. There is no member role until people sign in as themselves.",
  SINGLE_USER_SSO_NOTE_PREFIX:
    "To add people, configure SSO: each person gets their own identity and an admin or member role, and members get their own Getting Started. The recipe is in ",
  SINGLE_USER_SSO_NOTE_DOC: "docs/OPERATIONS.md",
  SINGLE_USER_SSO_NOTE_SUFFIX: ', "Second user, same host".',
  MULTI_USER_LEDE: "People sign in with SSO; each is an admin or a member, per your role map.",
  // The PREFIX/SUFFIX pair must name all three sources a role can come from —
  // WARDYN_OIDC_ROLE_MAP, the operator allowlist, AND a console row (the
  // acting-surface table right below this lede) — not just the env var and
  // the allowlist, or the lede would contradict the table.
  // PREFIX ends with ONE trailing space (before MULTI_USER_ROLES_VAR is
  // concatenated in) — copy the literal string, do not trim it.
  MULTI_USER_ROLES_PREFIX: "Roles come from the mappings below — your chart's ",
  MULTI_USER_ROLES_VAR: "WARDYN_OIDC_ROLE_MAP",
  MULTI_USER_ROLES_SUFFIX: ", console rows added here, or the operator allowlist.",
  MULTI_USER_SSO_CHIP: "SSO",
  MULTI_USER_ADMINS_LABEL: "Admins",
  MULTI_USER_ADMINS_BODY: " set the ceiling — policies, secrets, workspaces, site configuration.",
  MULTI_USER_MEMBERS_LABEL: "Members",
  MULTI_USER_MEMBERS_BODY:
    " run inside it — their own workspaces, runs, approvals and SSH keys. They land on their own Getting Started the first time they sign in.",
  MULTI_USER_PERMISSIONS_ACTION: "Open Permissions",
  MULTI_USER_PERMISSIONS_HINT: "Capability grants, per person or group",
} as const;

// DRAFT (M2 canon pending) — staged in workspace-providers-prompt.md §7.6
// ("U1 → corp-network-step / wardyn/copy.ts (B2, F22)"), parsed by nothing
// today; each row moves into its lane's own frozen table at the M2 sitting.
// Rendered here ahead of that sitting because the states themselves (the save
// note, the trusted-CA count) already exist and shipping words for them beats
// a blank control.
// 0.7.3 F6: this block carries no CONFINEMENT_NETPOL_* rows (app-shell.tsx's
// header chip was their only consumer) — the netpol verdict lives on the
// setup Environment step alone; see docs/design/workspace-providers-prompt.md
// §7.6 for the retired rows.
export const SITE = {
  // B2: the site-config save path's own note — a change here does not reach a
  // run already going (the egress sidecar compiles its config once at sandbox
  // start).
  SAVE_NOTE: "Saved. This applies to runs started from now — a run already going keeps the network settings it started with.",
  // F22: the Network step's trusted-CA count, from /setup/status
  // (trusted_ca_certs) — the inline ternary (§5 #9), never a second helper.
  TRUSTED_CA_COUNT: (n: number) => `${n} trusted CA certificate${n === 1 ? "" : "s"}`,
} as const;

// DRAFT (M2 canon pending) — staged in workspace-providers-prompt.md §7.6
// ("B-γ → wardyn/copy.ts"), parsed by nothing today.
//
// The shell's identity states (B1, R4-F107). Both are about the ONE question
// the console cannot answer for itself: who is signed in. A failed /me must
// not render the admin nav off a fail-open guess — indistinguishable from an
// authz breach to the human reading it — and a failed sign-out must not
// silently console.error with nobody seeing it.
export const SHELL = {
  // B1: settled, but /me never answered. Rendered instead of a guessed nav, so
  // it has to say that the emptiness is ignorance and not a denial.
  UNKNOWN_BODY:
    "We couldn't confirm who you are. Nothing here is hidden from you on purpose — reload, or sign in again.",
  // M2 §1: no new word — the banner's action re-fires whoami(), which is
  // exactly what the member Getting Started page's Retry already means.
  UNKNOWN_ACTION: MEMBER_GETTING_STARTED.RETRY,
  // R4-F107: POST /auth/logout failed, so the HttpOnly OIDC session cookie may
  // still be live — the local token is gone either way, which is why the title
  // says "here".
  // DRAFT (M2) — DIVERGES from the §7.6 staging ("Couldn't sign you out" /
  // "Your session is still live…"): these two are the M2 sitting sheet's §2
  // texts, which do not overclaim — a failed POST does not PROVE the session
  // survived, only that nothing confirmed it died.
  SIGN_OUT_FAILED_TITLE: "Signed out here, but not on the server",
  SIGN_OUT_FAILED_BODY: "Your session may still be active on the server. Close the browser, or try signing out again.",
} as const;

// DRAFT (M2 canon pending) — B4b, the clone of a run that ended.
export const RUN = {
  // The affordance the killed panel's own advice ("Start a new run if the work
  // still needs doing") never had.
  CLONE_CTA: "Start a run like this one",
  // What carried over, and — the half that matters — what deliberately did not.
  CLONE_NOTE:
    "Prefilled from this run — task, agent, barrier, policy and what it attached. Credentials and approvals are minted fresh.",
  // The one thing a clone CANNOT carry: an inline policy is never persisted
  // (internal/api/inline_policy.go attaches it with a nil id), so the run row
  // records only that there was one. A named ceiling beats a silent default.
  CLONE_INLINE_POLICY_CEILING:
    "This run used an inline policy, which isn't stored — pick a saved policy or write one again.",
  // DRAFT (M2) — §7.6's staged RUN_CLONE_CEILING_NOTE, which shipped nowhere
  // until now. It is the console half of §7 B4b's "create re-clamps": a member
  // cloning an admin's run is narrowed AT LAUNCH, not flattered in this form,
  // and the banner has to say so before they press Launch rather than after.
  CLONE_CEILING_NOTE:
    "Your ceiling applies again at launch — anything this run had above it is narrowed, with the reason.",
  // DRAFT (M2 canon pending) — F2-F5: a saved-policy reference that no longer
  // resolves (deleted elsewhere) needs its own reason; "pick a saved policy,
  // or write a custom one" is false once one WAS picked.
  POLICY_GONE: "That saved policy no longer exists — pick another.",
  // DRAFT (M2 canon pending) — F2-F2: the original sentence claimed the saved
  // lane merges nothing; runs_create.go's create door prepends the attached
  // Workspace card's mounts even when launching by policy_id. Named, not
  // silently contradicted.
  SAVED_POLICY_GOVERNS: (barrier: string, egress: string) =>
    `The stored spec governs this run — barrier floor ${barrier}, ${egress}. Your attached workspace mounts into it; nothing else on this page is merged.`,
  // Exactly one installed class meets the floor: nothing to ask, so the Seg
  // collapses to this sentence instead (new-run-screen.tsx).
  BARRIER_ONLY_QUALIFIER: "— the only barrier this run can use.",
  // An inconclusive host probe never blocks launch and does not leave every
  // tier guessably selectable either: an untouched pick sends no
  // confinement_class at all, so the server's own read decides.
  BARRIER_UNKNOWN:
    "Couldn't check which barriers this host has — leave this alone and Wardyn will use the strongest one it can, or pick one yourself.",
} as const;

// Moved to copy/terminal.ts (the target path for the later barrel split of
// this file) — re-exported here so every existing `from "./copy"` import
// keeps working unchanged.
export { TERMINAL } from "./copy/terminal";

// DRAFT (M2 canon pending) — the New Run rail's TRUTH block
// Appendix A finding 1: the rail rendered two unconditional security claims —
// "Minted at launch, injected by the proxy. Never written into the sandbox." and
// "Every keystroke and every outbound connection." — neither of which consulted
// anything. The first is a FALSE ASSURANCE on a bedrock_sso deployment, read at
// the moment someone decides whether a per-user AWS credential may sit inside a
// CC1 container; the second promises recording that a stock Helm install
// (persistence.enabled=false) never captures.
//
// So every sentence below is SCOPED to the model credential and selected by what
// the SERVER resolved (SetupHarnessTool.credential_residency, overridden by
// preflight's model_credential). There is deliberately no default: an unresolved
// lane renders RESOLVED_AT_LAUNCH, never the proxy sentence.

// Recording, deduplicated: the same two facts were spelled three times
// (recording.tsx's local consts, run-detail.tsx's inline EmptyState literals)
// and had already drifted — "will ever produce one" vs "captures one". One
// spelling, and the rail reads it too.
export const RECORDING_DISABLED_TITLE = "Session recording is disabled on this deployment";
// The switch is WARDYN_RECORDING_STORE, not WARDYN_RECORDING_DIR: the DIR only
// moves the `fs` store's path and turns nothing on (docs/ENV.md), while the
// chart renders STORE=off whenever persistence.enabled=false.
export const RECORDING_DISABLED_DESC =
  "No run on this server will ever produce one — set persistence.enabled (Helm) or WARDYN_RECORDING_STORE=pg to turn it on.";

export const RAIL_CREDENTIAL = {
  // residency "proxy": late-bound, swapped onto the wire, never resident.
  // U-15: "minted" was true of the Bedrock exchange this lane is NOT — a static
  // API key or a stored bearer is injected as it stands, nothing is minted for
  // it. What every proxy lane shares is where the credential goes and where it
  // does not.
  PROXY: "Model credential — injected by the proxy at launch; never written into the sandbox.",
  // residency "proxy" + staged_placeholder: the ~/.claude mount with injection
  // ON. "Proxy" is the deployment's STATED mode, not something Wardyn verified —
  // the sentinel is written by an operator-run script (scripts/stage-claude-creds.sh)
  // the daemon never reads back — so the mount is named rather than denied.
  PROXY_STAGED:
    "Model credential — this deployment injects it at the proxy; the sign-in mounted into the sandbox is staged as a placeholder.",
  // residency "sandbox", Bedrock family. The operator's own sentence.
  SANDBOX_BEDROCK:
    "Model credential — AWS credentials sign inside the sandbox, so this run holds them for its lifetime.",
  // Whose credential that is — the per_user/shared distinction, in the Barrier
  // chip's shape because it is the same kind of fact: a bound, stated up front.
  // OWNERSHIP, not status. This chip must be painted from the ROSTER ROW
  // alone — the rail never reads model_access — so it must not reuse "Your
  // AWS sign-in" (byte-identical to YOUR_MODEL_KEY.SIGNED_IN_CHIP, which on
  // Getting Started is the SIGNED-IN success chip), which would tell a
  // member who had not signed in that they had. The row's fact is whose
  // credential the lane uses, and that is what it says.
  SANDBOX_BEDROCK_CHIP_PER_USER: "Per-person AWS sign-in",
  SANDBOX_BEDROCK_CHIP_SHARED: "Admin's credential",
  // residency "sandbox", subscription: WARDYN_SUBSCRIPTION_INJECT=off, which is
  // the COMPOSE stack's own default (threatmodel/THREAT-MODEL.md).
  SANDBOX_SUBSCRIPTION:
    "Model credential — this deployment mounts the Claude sign-in into the sandbox, so this run holds it for its lifetime.",
  // residency "image" (a `none` roster row, BYOA). The server's own
  // llmMechanismWords wording for that lane, said once in both places.
  IMAGE: "Wardyn wires no model credential — the image brings its own, and Wardyn cannot say where it lives.",
  // Nothing resolved, and the absent-row doctrine in one line: the rail states
  // no residency it was not given. This is the COMMON case, not an error — a
  // roster cannot settle residency, so only a dry run of this exact body can.
  RESOLVED_AT_LAUNCH: "Resolved at launch.",
  // …and therefore the way to find out, said where the absence is. Without it
  // "Resolved at launch." reads as "nothing to see", when the precise answer is
  // one click away on the panel directly to the left.
  RUN_PREFLIGHT_HINT: "Run Preflight to see where this run's model credential will live.",
} as const;

// DRAFT (M2 canon pending) — U-15: the New Run rail's "recording is on"
// sentence. It was an inline literal in the rail and re-typed in its vitest and
// in ui/e2e/new-run.spec.ts, while its DISABLED twin
// (RECORDING_DISABLED_TITLE, right above) was already a shared constant — so a
// reworded promise would have moved on screen while three copies of the old one
// went on passing. Beside RAIL_CREDENTIAL rather than inside it: the rail's
// Recording section is not a credential fact.
export const RAIL_RECORDING_ON = "Every keystroke and every outbound connection.";
