/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Typed state + status→step mapping for the guided Workspace Import panel.
// Mirrors the wizard-types.ts pattern (a step id union + a steps table + pure
// helpers), so the panel's rail (StepIndicator<ImportStepId>) and resume logic
// share ONE source of truth for "which step is this workspace at".
import type {
  RecordResult,
  SetupCommand,
  VerifyResult,
  VerifyStep,
  Workspace,
  WorkspaceProfile,
  WorkspaceStatus,
} from "../../../lib/types";

// Source → Scan → Configure → Record → Verify → Finalize. Record is the OPEN
// recording step (recommended, skippable) that learns real usage before the
// confined Verify proves it. Build lives inside Verify (the verify run does build
// + verify together), so there's no separate Build step.
export type ImportStepId = "source" | "scan" | "configure" | "record" | "verify" | "finalize";

export const IMPORT_STEPS: { id: ImportStepId; label: string }[] = [
  { id: "source", label: "Source" },
  { id: "scan", label: "Scan" },
  { id: "configure", label: "Configure" },
  { id: "record", label: "Record" },
  { id: "verify", label: "Verify" },
  { id: "finalize", label: "Finalize" },
];

// Which import step a workspace's server-side status corresponds to — used to
// RESUME a mid-flight import at the right rail position when the panel opens.
export function activeStepForStatus(status: WorkspaceStatus): ImportStepId {
  switch (status) {
    case "pending_scan":
    case "scanning":
    case "error": // a scan error — stay on Scan so it can be retried
      return "scan";
    case "scanned":
      return "configure";
    case "building":
    case "build_error":
    case "verifying":
    case "verify_failed":
      return "verify";
    case "ready":
      return "finalize";
    default:
      return "source";
  }
}

// Statuses that are in-flight server-side — the panel polls while in one of these.
const TRANSIENT: WorkspaceStatus[] = ["scanning", "building", "verifying"];
export function isTransientStatus(status: WorkspaceStatus): boolean {
  return TRANSIENT.includes(status);
}

// The fixed verify vocabulary (Verifying / Success / Partial / Failed) + an
// idle state before a verify has run. Derived from the status AND the last
// verify_result: "partial" = it ran, failed overall, but some steps passed.
export type VerifyPhase = "idle" | "verifying" | "success" | "partial" | "failed";

export function verifyPhase(status: WorkspaceStatus, result?: VerifyResult): VerifyPhase {
  if (status === "building" || status === "verifying") return "verifying";
  if (result && result.ran) {
    if (result.ok) return "success";
    return result.steps.some((s) => s.exit_code === 0) ? "partial" : "failed";
  }
  if (status === "ready") return "success";
  if (status === "verify_failed" || status === "build_error") return "failed";
  return "idle";
}

export const VERIFY_PHASE_LABEL: Record<VerifyPhase, string> = {
  idle: "Not verified",
  verifying: "Verifying",
  success: "Success",
  partial: "Partial",
  failed: "Failed",
};

export const VERIFY_PHASE_TONE: Record<VerifyPhase, "neutral" | "info" | "success" | "warning" | "danger"> = {
  idle: "neutral",
  verifying: "info",
  success: "success",
  partial: "warning",
  failed: "danger",
};

// ------------------------------------------------------------
// Live verify progress — merge the operator-approved command list with a
// (possibly in-flight, streamed) verify_result into an ordered checklist so the
// panel can show pending / running / done rows instead of a bare spinner.
// ------------------------------------------------------------

// Present-progressive label for a running step, keyed by stage. Falls back to the
// bare command when the stage is unknown (older/newer scanner vocabulary).
const STAGE_GERUND: Record<string, string> = {
  install: "Installing dependencies…",
  build: "Building…",
  test: "Testing…",
  lint: "Linting…",
  run: "Running…",
};
export function runningLabel(step: { stage: string; command: string }): string {
  return STAGE_GERUND[step.stage] ?? `Running ${step.command}…`;
}

export type VerifyRowState = "pending" | "running" | "pass" | "fail";
export interface VerifyRow {
  stage: string;
  command: string;
  state: VerifyRowState;
  step?: VerifyStep; // present once the step has a result entry (running or done)
}

// Ordered rows for the verify checklist. The verify runs the approved commands
// sequentially, so match by ORDER: index i's result step (if present) wins, else
// the approved command shows as pending. Extra result steps beyond the approved
// list (a degraded/older shape with no matching command) are still rendered so
// nothing is dropped.
export function verifyRows(commands: SetupCommand[], result?: VerifyResult): VerifyRow[] {
  const steps = result?.steps ?? [];
  const n = Math.max(commands.length, steps.length);
  const rows: VerifyRow[] = [];
  for (let i = 0; i < n; i++) {
    const step = steps[i];
    if (step) {
      const state: VerifyRowState = step.running
        ? "running"
        : step.exit_code === 0 && !step.timed_out
          ? "pass"
          : "fail";
      rows.push({ stage: step.stage, command: step.command, state, step });
    } else {
      rows.push({ stage: commands[i].stage, command: commands[i].command, state: "pending" });
    }
  }
  return rows;
}

// "step N of total": N = how many steps have STARTED (running or finished =
// steps.length); total = verify_result.total ?? approved command count (never
// less than the steps we already have, for a degraded shape with no total).
export function verifyProgress(
  commands: SetupCommand[],
  result?: VerifyResult,
): { started: number; total: number } {
  const started = result?.steps?.length ?? 0;
  const total = Math.max(result?.total ?? commands.length, started);
  return { started, total };
}

// A verify step's wall time as a compact "12s" / "1m 5s" (blank under 1s or when
// duration is absent). Display-only; logs/exit codes carry the real detail.
export function fmtStepDuration(ms?: number): string {
  if (!ms || ms < 1000) return "";
  const s = Math.round(ms / 1000);
  return s < 60 ? `${s}s` : `${Math.floor(s / 60)}m ${s % 60}s`;
}

// ------------------------------------------------------------
// Record step — pure helpers. The task LIST is authored server-side
// (DeriveRecordTasks) and read verbatim off the workspace; the client NEVER
// derives tasks. These helpers only read the record fields the panel renders.
// ------------------------------------------------------------

// One recorded SESSION: its stable key (record_results map key) + the operator's
// display name. Sessions are user-named, not derived — the list is simply whatever
// sessions the operator has recorded (or is recording). Ordered by start time so a
// fresh session lands at the bottom, stable across polls.
export type RecordSession = { key: string; label: string };

// sessionKeyOf mirrors the server's recordSessionKey slug (lowercase, non-[a-z0-9]
// runs → single dash, trimmed, ≤48) so the UI can predict a session's record_results
// key for the busy indicator before the round-trip. The server slug is authoritative.
export function sessionKeyOf(name: string): string {
  let s = name.trim().toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "");
  if (s.length > 48) s = s.slice(0, 48).replace(/-+$/g, "");
  return s;
}

// Sessions of one kind: learning (open egress, Record step) or confined (verify,
// Verify step). The `confined` flag on each result is the discriminator; a legacy
// result without it is treated as a learning session.
// policyNameFor derives a "save as is" policy name from the workspace + recording
// names, e.g. ("slugify", "build & test") -> "slugify-build-test". Reuses the same
// slug as session keys so the name is stable and filesystem/URL-safe.
export function policyNameFor(workspaceName: string, recordingLabel: string): string {
  return [sessionKeyOf(workspaceName), sessionKeyOf(recordingLabel)].filter(Boolean).join("-");
}

// verifyKeyOf mirrors the server's confined-run key: a verify run REPLAYS a
// recording under least privilege and is stored under this derived key so it never
// clobbers the recording's open-mode capture (server: recordVerifyKeyPrefix).
export function verifyKeyOf(recordingKey: string): string {
  return "verify:" + recordingKey;
}

export function recordSessions(ws: Workspace, confined = false): RecordSession[] {
  const results = ws.record_results ?? {};
  return Object.keys(results)
    .filter((key) => !!results[key].confined === confined)
    .sort((a, b) => (results[a].started_at ?? "").localeCompare(results[b].started_at ?? ""))
    .map((key) => ({ key, label: results[key].label || key }));
}

// The per-session recording outcome, if a record run has been kicked for it.
export function recordResult(ws: Workspace, taskKey: string): RecordResult | undefined {
  return ws.record_results?.[taskKey];
}

// True while ANY session's record run is still in flight — drives the poll gate (a
// record adds no transient WorkspaceStatus, so isTransientStatus can't see it).
export function isRecording(ws: Workspace): boolean {
  return Object.values(ws.record_results ?? {}).some((r) => r.status === "recording");
}

// The "Approve N observed hosts" diff: hosts the open recording actually reached
// (allow_count > 0 — Synthesize's own promotion rule) that are NOT already
// auto-allowed by the scan profile or operator-approved. Dedup, order-preserving.
export function newEgressHosts(ws: Workspace, taskKey: string): string[] {
  const rr = recordResult(ws, taskKey);
  const observed = (rr?.observations?.domains ?? []).filter((d) => d.allow_count > 0).map((d) => d.host);
  const profile = (ws.profile ?? {}) as WorkspaceProfile;
  const already = new Set([...(ws.approved_egress ?? []), ...(profile.egress_domains ?? [])]);
  const out: string[] = [];
  for (const h of observed) {
    if (!already.has(h) && !out.includes(h)) out.push(h);
  }
  return out;
}

// True when a settled recording captured NO egress — the honest failure case the
// backend stamps `record_failed` + a reachability `failure_hint` for. The pane
// must render that hint (control-plane reachability), NEVER "the task needs no
// egress" (an open sandbox that reached nothing almost always means the proxy
// decision callback never landed, e.g. WSL2 NAT).
export function isEmptyCapture(rr?: RecordResult): boolean {
  if (!rr) return false;
  return (rr.observations?.domains?.length ?? 0) === 0;
}
