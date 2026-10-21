/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A desktop's screen recording on the Recording tab (#2968, M7-Replay and States 6 and 7): every
// pause is named instead of hidden, with its start, end and reason from the tamper-evident log, which
// Wardyn wrote, not the desktop. Keystrokes are never recorded. The console has no screen player yet:
// the recording downloads as noVNC playback frames.
import * as React from "react";
import { Download, Loader2 } from "lucide-react";
import { recordings, type DesktopPlayback } from "../../../lib/api/recordings";
import { clockTime } from "../../../lib/format";
import type { AuditEvent } from "../../../lib/types";
import { Button } from "../../ui/button";
import { RECORDING_REPLAY as C, gapDuration } from "../../wardyn/copy/recording-level";

// The gap rows Wardyn wrote for this run (desktop.recording.resume carries the whole gap).
function gapRows(audit: AuditEvent[]) {
  return audit
    .filter((e) => e.action === "desktop.recording.resume" && e.outcome === "success")
    .map((e) => {
      const d = (e.data ?? {}) as { gap_start?: string; gap_end?: string; gap_ms?: number; reason?: string };
      return { id: e.id, start: d.gap_start, end: d.gap_end, ms: d.gap_ms ?? 0, reason: d.reason ?? "" };
    });
}

export function DesktopReplay({ runId, recKey, audit }: { runId: string; recKey: string; audit: AuditEvent[] }) {
  const [state, setState] = React.useState<"loading" | "error" | "ready">("loading");
  const [doc, setDoc] = React.useState<{ playback: DesktopPlayback; raw: string } | undefined>();
  const [attempt, setAttempt] = React.useState(0);
  React.useEffect(() => {
    let alive = true;
    setState("loading");
    recordings
      .getDesktopRecording(runId, recKey)
      .then((d) => {
        if (!alive) return;
        setDoc(d);
        setState("ready");
      })
      .catch(() => alive && setState("error"));
    return () => {
      alive = false;
    };
  }, [runId, recKey, attempt]);

  if (state === "loading") {
    return (
      <div className="flex h-[200px] items-center justify-center gap-2 rounded-xl border border-border bg-card text-body text-muted-foreground">
        <Loader2 className="size-4 animate-spin" aria-hidden />
        {C.LOADING}
      </div>
    );
  }
  if (state === "error" || !doc) {
    return (
      <div role="alert" className="space-y-2 rounded-xl border border-border bg-card p-4 text-body">
        <p className="font-medium">{C.LOAD_FAILED}</p>
        <Button size="sm" variant="outline" onClick={() => setAttempt((n) => n + 1)}>
          Try again
        </Button>
        <p className="text-meta text-muted-foreground">{C.LOAD_FAILED_NOTE}</p>
      </div>
    );
  }
  const download = () => {
    const url = URL.createObjectURL(new Blob([doc.raw], { type: "application/json" }));
    const a = document.createElement("a");
    a.href = url;
    a.download = `${recKey}.json`;
    a.click();
    setTimeout(() => URL.revokeObjectURL(url), 0);
  };
  const gaps = doc.playback.gaps ?? [];
  const rows = gapRows(audit);
  return (
    <div className="space-y-3" data-testid="desktop-replay">
      <div className="flex items-center gap-2">
        <span className="text-body font-medium">{C.SCREEN}</span>
        <Button size="sm" variant="outline" className="ml-auto" onClick={download}>
          <Download className="size-3.5" aria-hidden />
          {C.DOWNLOAD}
        </Button>
      </div>
      {gaps.map((g, i) => (
        <div key={i} role="status" className="rounded-lg border border-warning/25 bg-warning-subtle p-3 text-body" data-testid="desktop-replay-gap">
          <p className="font-semibold">{C.PAUSED_FOR(gapDuration((g.end_ms ?? g.start_ms) - g.start_ms))}</p>
          <p>{C.PAUSED_BODY(g.reason)}</p>
          <p className="text-meta">{C.GAP(gapDuration((g.end_ms ?? g.start_ms) - g.start_ms))}</p>
        </div>
      ))}
      <p className="text-meta text-muted-foreground">{C.PLAYER_PENDING}</p>
      <p className="text-meta text-muted-foreground">{C.KEYSTROKES}</p>
      {rows.length > 0 && (
        <section aria-labelledby="desktop-replay-log" className="rounded-lg border border-border p-3 text-body">
          <h3 id="desktop-replay-log" className="font-semibold">
            {C.LOG_TITLE}
          </h3>
          {rows.map((r) => (
            <dl key={r.id} className="mt-2 grid grid-cols-[8rem_1fr] gap-x-3 gap-y-1" data-testid="desktop-replay-log-row">
              <dt className="text-muted-foreground">{C.GAP_START}</dt>
              <dd>{r.start ? clockTime(r.start) : "—"}</dd>
              <dt className="text-muted-foreground">{C.GAP_END}</dt>
              <dd>{r.end ? clockTime(r.end) : "—"}</dd>
              <dt className="text-muted-foreground">{C.REASON}</dt>
              <dd>{r.reason.charAt(0).toUpperCase() + r.reason.slice(1)}</dd>
              <dt className="text-muted-foreground">{C.WRITTEN_BY}</dt>
              <dd>{C.WRITTEN_BY_VALUE}</dd>
            </dl>
          ))}
        </section>
      )}
    </div>
  );
}

// desktopSessions are the run's stored screen recordings, newest last.
export function desktopSessions(audit: AuditEvent[]): AuditEvent[] {
  return audit.filter((e) => e.action === "desktop.recording.write" && e.outcome === "success" && !!e.target);
}
