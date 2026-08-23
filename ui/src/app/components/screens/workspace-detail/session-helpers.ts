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
import { effectiveWorkspaceRequirements, type RecordResult, type Workspace, type WorkspaceProfile } from "../../../lib/types";

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

// isModelProviderHost mirrors record.go's modelProviderEgress: the LLM
// harness's own egress (api.anthropic.com / any *.anthropic.com host,
// api.openai.com) — needed by every session regardless of the task, so never
// a task-specific "approve this" candidate. Static/cheap subset of
// promoteSkipHosts (record.go) — the two remaining categories there
// (a bound Bedrock integration's regional host, a required-integration's own
// egress) need server-side integration resolution this pure client-side
// helper has no view of, and stay a documented gap (see egressPromotionDiff).
function isModelProviderHost(host: string): boolean {
  const h = host.toLowerCase();
  return h.endsWith("anthropic.com") || h === "api.openai.com";
}

// EgressPromotionDiff buckets a session's observed+allowed hosts (Synthesize's
// own promotion rule: allow_count > 0) into exactly the three things the
// review card and its confirm dialog need to render honestly:
//   - approvable: new, real candidates — the "Approve N observed hosts" list.
//   - alreadyApproved: covered by ApprovedEgress, the scan profile's
//     egress_domains, or an effective egress:required requirement row.
//   - plumbing: excluded as platform plumbing (the control-plane host itself,
//     the model-provider harness hosts) — never approvable, and NOT the same
//     claim as alreadyApproved ("an operator already approved this").
// One function computing all three (rather than three separately-derived
// lists) so they can't drift apart — every observed+allowed host lands in
// EXACTLY one bucket.
export interface EgressPromotionDiff {
  approvable: string[];
  alreadyApproved: string[];
  plumbing: string[];
}

// approvedEgressSet — the ONE answer to "does this workspace already grant
// egress to <host>?", mirroring the union a confined replay actually launches
// with (the server's confinedEgressDomains — which is also a superset of
// promote's two-lane dedupe set): the legacy ApprovedEgress lane, the scan
// profile's auto-allowed egress_domains, and the effective egress:<host>
// requirement rows — the lane promote and the per-host approve now BOTH write. Shared on purpose: egressPromotionDiff's alreadyApproved
// bucket (below) and ConfinedReviewCard's caught bucket must subtract the SAME
// set, or a host approved through the requirements lane keeps rendering as
// off-policy under an Approve button that would fold in zero new rows.
export function approvedEgressSet(ws: Workspace): Set<string> {
  const profile = (ws.profile ?? {}) as WorkspaceProfile;
  const already = new Set([...(ws.approved_egress ?? []), ...(profile.egress_domains ?? [])]);
  for (const [key, req] of Object.entries(effectiveWorkspaceRequirements(ws))) {
    if (req.level === "required" && key.startsWith("egress:")) already.add(key.slice("egress:".length));
  }
  return already;
}

// W20-S1-1: this must mirror the server's OWN dedup in
// handlePromoteRecordEgress (internal/api/record.go) — which skips a host
// already covered by ApprovedEgress OR an effective egress:required
// requirement row — or the card offers a host that's already covered,
// the operator clicks Approve, the server folds in zero NEW rows (the
// dedup drops it), yet EgressPromoted still flips true unconditionally
// and the card renders "Promoted" for a click that promoted nothing.
// approved_egress + profile.egress_domains alone missed the requirements
// overlay (e.g. a host approved earlier via the workspace wizard, not this
// legacy lane) — approvedEgressSet above closes that gap.
//
// selfHost (the browser's own origin — the console is always same-origin
// with wardynd, see lib/api/core.ts's relative BASE) mirrors the server's
// controlPlaneHost exclusion; this file stays DOM-free (see the header
// comment), so callers (record-pane.tsx, workspace-detail.tsx) pass
// window.location.hostname — both MUST pass the same value, since
// workspace-detail.tsx's untrusted-content confirm dialog must never list
// more hosts than the button that opened it offered (UI-WS-14).
export function egressPromotionDiff(ws: Workspace, taskKey: string, selfHost?: string): EgressPromotionDiff {
  const rr = recordResult(ws, taskKey);
  const observed = (rr?.observations?.domains ?? []).filter((d) => d.allow_count > 0).map((d) => d.host);
  const already = approvedEgressSet(ws);
  const self = selfHost?.toLowerCase().trim();
  const diff: EgressPromotionDiff = { approvable: [], alreadyApproved: [], plumbing: [] };
  const seen = new Set<string>();
  for (const h of observed) {
    if (seen.has(h)) continue;
    seen.add(h);
    if (already.has(h)) {
      diff.alreadyApproved.push(h);
    } else if ((self && h === self) || isModelProviderHost(h)) {
      diff.plumbing.push(h);
    } else {
      diff.approvable.push(h);
    }
  }
  return diff;
}

// The "Approve N observed hosts" list — the approvable bucket of
// egressPromotionDiff. Kept as its own export: workspace-detail.tsx's
// untrusted-content confirm dialog only ever needs this one list.
export function newEgressHosts(ws: Workspace, taskKey: string, selfHost?: string): string[] {
  return egressPromotionDiff(ws, taskKey, selfHost).approvable;
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
