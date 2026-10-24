/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The desktop replay's timeline (#2968, board M7-Replay): where each pause gap and each action sits
// in a recording, in ms since it started, and where a click on the action log jumps to. Pure, so the
// rules are unit-tested without a player.
import type { DesktopGap, DesktopPlayback } from "./api/recordings";
import type { AuditEvent } from "./types";
import { fmtBytes } from "./format";
import { RECORDING_REPLAY as C, gapDuration } from "../components/wardyn/copy/recording-level";

/** One paused span, in ms since the recording started. */
export interface ReplayGap {
  startMs: number;
  endMs: number;
  reason: string;
  /** The desktop.recording.pause and .resume rows that mark it, when the log has them. */
  pauseId?: string;
  resumeId?: string;
}

export type ReplayActor = "AGENT" | "HUMAN" | "SYSTEM";

/** One action log row, placed on the timeline. */
export interface ReplayAction {
  id: string;
  ms: number;
  time: string;
  actor: ReplayActor;
  text: string;
  detail: string;
  outcome: AuditEvent["outcome"];
  /** A clipboard or file move the run's rules refused (the timeline's amber tick). */
  blocked: boolean;
}

/** The recording's length: to its last frame, gap or its write row, whichever is latest. */
export function replayDuration(pb: DesktopPlayback, writtenAt?: string): number {
  const started = Date.parse(pb.started_at);
  let ms = 0;
  for (const f of pb.frames) ms = Math.max(ms, frameMs(f));
  for (const g of pb.gaps ?? []) ms = Math.max(ms, g.end_ms ?? g.start_ms);
  if (writtenAt) ms = Math.max(ms, Date.parse(writtenAt) - started);
  return Math.max(ms, 1);
}

/** A playback frame's time: "{<ms>{<base64>". */
export function frameMs(frame: string): number {
  const end = frame.indexOf("{", 1);
  const ms = Number(frame.slice(1, end));
  return Number.isFinite(ms) ? ms : 0;
}

const clamp = (ms: number, duration: number) => Math.min(Math.max(ms, 0), duration);

type GapRow = { gap_start?: string; gap_end?: string; reason?: string };

/**
 * The paused spans, from the pause and resume rows Wardyn wrote (a resume carries the whole gap; a
 * pause with no resume yet runs to the recorder's own gap end, or the recording's end), plus any gap
 * the recorder kept that no row covers (a connection that opened during a pause). Clamped to the
 * recording and sorted.
 */
export function replayGaps(pb: DesktopPlayback, duration: number, audit: AuditEvent[]): ReplayGap[] {
  const started = Date.parse(pb.started_at);
  const doc = pb.gaps ?? [];
  const at = (iso: string | undefined) => (iso ? Date.parse(iso) - started : NaN);
  const resumes = audit.filter((e) => e.action === "desktop.recording.resume" && e.outcome === "success");
  const out: ReplayGap[] = [];
  for (const p of audit.filter((e) => e.action === "desktop.recording.pause" && e.outcome === "success")) {
    const d = (p.data ?? {}) as GapRow;
    const r = resumes.find((e) => ((e.data ?? {}) as GapRow).gap_start === d.gap_start);
    const start = at(d.gap_start);
    if (Number.isNaN(start)) continue;
    let end = at(r ? ((r.data ?? {}) as GapRow).gap_end : undefined);
    if (Number.isNaN(end)) end = docGapEnd(doc, start) ?? duration;
    out.push({ startMs: clamp(start, duration), endMs: clamp(end, duration), reason: d.reason ?? "", pauseId: p.id, resumeId: r?.id });
  }
  // A resume whose pause row is not in the list still names its gap.
  for (const r of resumes) {
    if (out.some((g) => g.resumeId === r.id)) continue;
    const d = (r.data ?? {}) as GapRow;
    const start = at(d.gap_start);
    const end = at(d.gap_end);
    if (Number.isNaN(start) || Number.isNaN(end)) continue;
    out.push({ startMs: clamp(start, duration), endMs: clamp(end, duration), reason: d.reason ?? "", resumeId: r.id });
  }
  for (const g of doc) {
    const end = g.end_ms ?? duration;
    if (!out.some((o) => o.startMs <= end && g.start_ms <= o.endMs)) {
      out.push({ startMs: clamp(g.start_ms, duration), endMs: clamp(end, duration), reason: g.reason });
    }
  }
  return out.filter((g) => g.endMs > g.startMs).sort((a, b) => a.startMs - b.startMs);
}

function docGapEnd(doc: DesktopGap[], start: number): number | undefined {
  const g = doc.find((d) => d.start_ms <= start + 1000 && start <= (d.end_ms ?? Infinity));
  return g?.end_ms;
}

/** The gap the playhead is in, if any. */
export function gapAt(gaps: ReplayGap[], ms: number): ReplayGap | undefined {
  return gaps.find((g) => g.startMs <= ms && ms < g.endMs);
}

type ActionData = { direction?: string; bytes?: number; sha256?: string; reason?: string; gap_ms?: number };

function shortHash(h?: string): string {
  return h && h.length > 8 ? `${h.slice(0, 4)}…${h.slice(-4)}` : (h ?? "");
}

function sized(d: ActionData): string {
  return [d.bytes != null ? fmtBytes(d.bytes) : "", d.sha256 ? `sha256 ${shortHash(d.sha256)}` : ""].filter(Boolean).join(" · ");
}

/** What a row says on the action log, or undefined for a row the log does not show. */
function describe(e: AuditEvent): { text: string; detail: string } | undefined {
  const d = (e.data ?? {}) as ActionData;
  const ok = e.outcome === "success";
  switch (e.action) {
    case "ui.clipboard.transfer": {
      const dir = d.direction ?? e.target;
      if (dir === "paste_in") return { text: ok ? C.ACT_PASTE : C.ACT_PASTE_TRIED, detail: sized(d) };
      if (dir === "copy_out") return { text: ok ? C.ACT_COPY : C.ACT_COPY_TRIED, detail: sized(d) };
      return undefined;
    }
    case "desktop.file.transfer":
      if (d.direction === "upload") return { text: ok ? C.ACT_UPLOAD : C.ACT_UPLOAD_TRIED, detail: sized(d) };
      if (d.direction === "download") return { text: ok ? C.ACT_DOWNLOAD : C.ACT_DOWNLOAD_TRIED, detail: sized(d) };
      return undefined;
    case "desktop.recording.pause":
      return { text: C.ACT_PAUSED, detail: C.ACT_PAUSED_DETAIL(d.reason ?? "") };
    case "desktop.recording.resume":
      return { text: C.ACT_RESUMED, detail: C.ACT_RESUMED_DETAIL(gapDuration(d.gap_ms ?? 0)) };
  }
  return undefined;
}

/** The action log: the rows the log shows that fall inside the recording, oldest first. */
export function replayActions(pb: DesktopPlayback, duration: number, audit: AuditEvent[]): ReplayAction[] {
  const started = Date.parse(pb.started_at);
  const seen = new Set<string>();
  const out: ReplayAction[] = [];
  for (const e of audit) {
    const ms = Date.parse(e.time) - started;
    if (seen.has(e.id) || !(ms >= 0 && ms <= duration)) continue;
    const said = describe(e);
    if (!said) continue;
    seen.add(e.id);
    out.push({
      id: e.id,
      ms,
      time: e.time,
      actor: e.actor_type === "human" ? "HUMAN" : e.actor_type === "agent" ? "AGENT" : "SYSTEM",
      ...said,
      outcome: e.outcome,
      blocked: e.outcome === "denied" && (e.action === "ui.clipboard.transfer" || e.action === "desktop.file.transfer"),
    });
  }
  return out.sort((a, b) => a.ms - b.ms);
}

/** Where a click on an action log row puts the playhead: the action's own moment. */
export function jumpTo(action: ReplayAction, duration: number): number {
  return clamp(action.ms, duration);
}

/** The playhead after one clock tick of tickMs at speed, stopping at the end. */
export function advance(ms: number, tickMs: number, speed: number, duration: number): number {
  return clamp(ms + tickMs * speed, duration);
}

/** A position as a share of the timeline, for CSS. */
export function pct(ms: number, duration: number): string {
  return `${(clamp(ms, duration) / duration) * 100}%`;
}
