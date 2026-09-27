/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Run cockpit (/runs/:id) — the terminal-first live-run screen. Every string
// the redesign introduces lands HERE first, so the four Terminal states and the
// evidence widgets can't drift into inventing their own vocabulary.
import { RUN_WAIT } from "./run-wait";

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
  // Split into run-wait.ts (#498) — see that file's comment. Spread rather
  // than retyped so this stays the one definition.
  ...RUN_WAIT,
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

// #125 — the run page's own launch-advisory block. A launch that answers 2xx
// always navigates here in the same tick (use-launch.ts), carrying any
// advisory `warnings[]` as router state; this is where they land, in the New
// Run rail's own advisory-block shape. LAUNCH_WARNING_TITLE is deliberately
// NOT re-declared here — it is AGENTS.LAUNCH_WARNING_TITLE
// (workspace-providers-copy.ts), reused verbatim so the two surfaces can never
// spell "launched with a warning" two different ways.
export const RUN_DETAIL = {
  // Router state dies on reload — this says so, rather than letting the note
  // simply vanish with no explanation. The durable record stays the
  // run.create audit row's own clamp warnings (see the Audit tab).
  LAUNCH_WARNING_EPHEMERAL: "This note goes when you reload. The run's audit trail keeps it.",
  LAUNCH_WARNING_DISMISS: "Dismiss",
} as const;

