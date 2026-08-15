/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Pure per-session helpers for the Sessions card (record-pane.tsx). Ported from
// the retired import-workspace/import-types.ts — ONLY the pieces the Sessions
// card still needs survive here. Dropped (not ported): the ImportStepId rail,
// activeStepForStatus/isTransientStatus (the guided-import step rail is
// retired), and the verifyPhase/VERIFY_PHASE_*/verifyRows/verifyProgress/
// runningLabel/fmtStepDuration checklist helpers (they served the old
// automated build+verify pipeline, not the per-session record/replay model
// this card uses). Pure TS — no React, no fetch, no DOM.
import type { RecordResult, Workspace, WorkspaceProfile } from "../../../lib/types";

// One recorded SESSION: its stable key (record_results map key) + the
// operator's display name. Sessions are user-named, not derived — the list is
// simply whatever sessions the operator has recorded (or is recording).
export type RecordSession = { key: string; label: string };

// sessionKeyOf mirrors the server's recordSessionKey slug (lowercase,
// non-[a-z0-9] runs → single dash, trimmed, ≤48) so the UI can predict a
// session's record_results key for the busy indicator before the round-trip.
// The server slug is authoritative.
export function sessionKeyOf(name: string): string {
  let s = name.trim().toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "");
  if (s.length > 48) s = s.slice(0, 48).replace(/-+$/g, "");
  return s;
}

// policyNameFor derives a "save as is" policy name from the workspace +
// recording names, e.g. ("slugify", "build & test") -> "slugify-build-test".
export function policyNameFor(workspaceName: string, recordingLabel: string): string {
  return [sessionKeyOf(workspaceName), sessionKeyOf(recordingLabel)].filter(Boolean).join("-");
}

// verifyKeyOf mirrors the server's confined-run key: a confined replay of a
// recording is stored under this derived key so it never clobbers the
// recording's open-mode capture (server: recordVerifyKeyPrefix).
export function verifyKeyOf(recordingKey: string): string {
  return "verify:" + recordingKey;
}

// The OPEN (learning) sessions the operator has recorded/is recording — a
// session's identity. Confined replays live under verifyKeyOf(key) and are
// looked up per-session by the caller, not listed separately here.
export function recordSessions(ws: Workspace): RecordSession[] {
  const results = ws.record_results ?? {};
  return Object.keys(results)
    .filter((key) => !results[key].confined)
    .sort((a, b) => (results[a].started_at ?? "").localeCompare(results[b].started_at ?? ""))
    .map((key) => ({ key, label: results[key].label || key }));
}

// A confined result with no OPEN sibling — e.g. the workspace wizard's live
// Verify session (verify-session.tsx posts name="verify", confined=true;
// record.go stores it under key "verify:verify", never opening a plain
// "verify" entry first). recordSessions only lists OPEN sessions, so an
// orphan like this is otherwise invisible on the detail page even though
// isRecording (below) still counts it — the exact deadlock this closes: no
// card, no way to see or stop the run, "Start recording" stays disabled and
// every new record POST 409s until it's killed elsewhere.
export function orphanedVerifySessions(ws: Workspace): RecordSession[] {
  const results = ws.record_results ?? {};
  return Object.keys(results)
    .filter((key) => results[key].confined && !(key.replace(/^verify:/, "") in results))
    .sort((a, b) => (results[a].started_at ?? "").localeCompare(results[b].started_at ?? ""))
    .map((key) => ({ key, label: results[key].label || key }));
}

// The per-key recording outcome, if a record run has been kicked for it.
export function recordResult(ws: Workspace, key: string): RecordResult | undefined {
  return ws.record_results?.[key];
}

// True while ANY session's record run (open or confined replay) is still in
// flight — drives the poll gate (a record adds no transient WorkspaceStatus,
// so status-based transience checks can't see it).
export function isRecording(ws: Workspace): boolean {
  return Object.values(ws.record_results ?? {}).some((r) => r.status === "recording");
}

// The "Approve N observed hosts" diff: hosts the open recording actually
// reached (allow_count > 0 — Synthesize's own promotion rule) that are NOT
// already auto-allowed by the scan profile or operator-approved. Dedup,
// order-preserving.
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

// True when a settled recording captured NO egress — the honest failure case
// the backend stamps `record_failed` + a reachability `failure_hint` for. The
// pane must render that hint (control-plane reachability), NEVER "the task
// needs no egress" (an open sandbox that reached nothing almost always means
// the proxy decision callback never landed, e.g. WSL2 NAT).
export function isEmptyCapture(rr?: RecordResult): boolean {
  if (!rr) return false;
  return (rr.observations?.domains?.length ?? 0) === 0;
}

// One session's lifecycle stage, derived from its OPEN result plus its
// (optional) confined-replay result: record open -> recorded -> replaying
// confined -> replayed. A session always starts from its open result (that's
// what creates the key); the confined result only exists once the operator
// has clicked "Replay confined" at least once.
export type SessionStage = "recording" | "recorded" | "record_failed" | "replaying" | "replayed" | "replay_failed";

export function sessionStage(ws: Workspace, key: string): SessionStage {
  const open = recordResult(ws, key);
  const confined = recordResult(ws, verifyKeyOf(key));
  if (open?.status === "recording") return "recording";
  // A confined verdict only speaks for the CURRENT open capture if it was
  // started at/after that capture began. No cross-run id links them —
  // launchRecordRun (internal/api/workspace_run.go) stamps a confined entry
  // with its OWN run id, never the open run it replayed — so started_at is
  // the one real, server-stamped signal available to pair them (the same
  // field recordSessions above already orders by). A re-record stamps `open`
  // a fresh, later started_at while the stale verify:<key> entry from the
  // PRIOR capture keeps its older one — W20-capture-store-1: without this
  // gate that stale entry outranks the fresh, unverified open result below.
  const openStarted = open?.started_at;
  const confinedStarted = confined?.started_at;
  const confinedCurrent = !confined || !openStarted || !confinedStarted || confinedStarted >= openStarted;
  if (confinedCurrent && confined?.status === "recording") return "replaying";
  if (confinedCurrent && confined?.status === "recorded") return "replayed";
  if (confinedCurrent && confined?.status === "record_failed") return "replay_failed";
  if (open?.status === "record_failed") return "record_failed";
  return "recorded";
}
