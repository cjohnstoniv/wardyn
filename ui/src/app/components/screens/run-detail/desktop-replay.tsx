/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A desktop's screen recording on the Recording tab (#2968, M7-Replay and States 6 and 7): a timeline
// that marks every pause gap instead of hiding it, the action log beside it (a click jumps the
// playhead there), and the tamper-evident log's verdict on the recording's range. Keystrokes are
// never recorded. The screen is drawn by noVNC's own decoder (@novnc/novnc, unmodified, loaded on
// first use) fed the recording's frames through a fake socket (desktop-replay-feed.ts).
import * as React from "react";
import { Download, Loader2, Pause, Play } from "lucide-react";
import { recordings, type DesktopPlayback } from "../../../lib/api/recordings";
import { audit as auditApi } from "../../../lib/api/audit";
import { clockTime } from "../../../lib/format";
import type { AuditEvent } from "../../../lib/types";
import { advance, gapAt, pct, replayActions, replayDuration, replayGaps } from "../../../lib/desktop-replay-timeline";
import { decodeFrames, PlaybackSocket, ReplayFeed } from "../../../lib/desktop-replay-feed";
import { Button } from "../../ui/button";
import { RECORDING_REPLAY as C, gapDuration } from "../../wardyn/copy/recording-level";
import { ReplayActionLog, ReplayChain } from "./desktop-replay-panels";

const TICK_MS = 250;
const SPEEDS = [1, 2, 4] as const;

export function DesktopReplay({ runId, recKey, audit, onGoAudit }: { runId: string; recKey: string; audit: AuditEvent[]; onGoAudit?: () => void }) {
  const [state, setState] = React.useState<"loading" | "error" | "ready">("loading");
  const [doc, setDoc] = React.useState<{ playback: DesktopPlayback; raw: string } | undefined>();
  const [attempt, setAttempt] = React.useState(0);
  // The desktop's clipboard and file rows, fetched apart so the run's general trail cap can't crowd them out.
  const [moves, setMoves] = React.useState<AuditEvent[]>([]);
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
    void Promise.all([
      auditApi.listAudit(runId, { actionPrefix: "ui.clipboard" }).catch(() => []),
      auditApi.listAudit(runId, { actionPrefix: "desktop.file" }).catch(() => []),
    ]).then(([a, b]) => alive && setMoves([...a, ...b]));
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
  return <Player runId={runId} recKey={recKey} doc={doc} audit={[...audit, ...moves]} onGoAudit={onGoAudit} />;
}

function Player({ runId, recKey, doc, audit, onGoAudit }: {
  runId: string;
  recKey: string;
  doc: { playback: DesktopPlayback; raw: string };
  audit: AuditEvent[];
  onGoAudit?: () => void;
}) {
  const pb = doc.playback;
  const written = audit.find((e) => e.action === "desktop.recording.write" && e.target === recKey);
  const duration = React.useMemo(() => replayDuration(pb, written?.time), [pb, written?.time]);
  const gaps = React.useMemo(() => replayGaps(pb, duration, audit), [pb, duration, audit]);
  const actions = React.useMemo(() => replayActions(pb, duration, audit), [pb, duration, audit]);
  const [ms, setMs] = React.useState(0);
  const [playing, setPlaying] = React.useState(false);
  const [speed, setSpeed] = React.useState<number>(1);
  const [active, setActive] = React.useState<string | undefined>();
  const marks = React.useRef<Record<number, HTMLButtonElement | null>>({});
  const screen = React.useRef<HTMLDivElement>(null);
  const feed = React.useRef<ReplayFeed | undefined>(undefined);
  const msNow = React.useRef(ms);
  msNow.current = ms;
  const [screenFailed, setScreenFailed] = React.useState(false);
  const frames = React.useMemo(() => decodeFrames(pb.frames), [pb.frames]);
  // A decoder that throws (a stream or a browser noVNC refuses) drops the screen, never the replay:
  // the feed goes, so no later seek can throw again inside an effect.
  const seek = React.useCallback((at: number) => {
    try {
      feed.current?.seek(at);
    } catch {
      feed.current = undefined;
      setScreenFailed(true);
    }
  }, []);

  // The decoder loads once per recording; each seek back gives it a fresh RFB on the same element.
  React.useEffect(() => {
    let alive = true;
    import("@novnc/novnc")
      .then(({ default: RFB }) => {
        const el = screen.current;
        if (!alive || !el) return;
        feed.current = new ReplayFeed(frames, () => {
          el.replaceChildren();
          const sock = new PlaybackSocket();
          const rfb = new RFB(el, sock, { shared: true });
          rfb.viewOnly = true;
          rfb.scaleViewport = true;
          rfb.focusOnClick = false;
          return { push: (b) => sock.push(b), close: () => rfb.disconnect() };
        });
        seek(msNow.current);
      })
      .catch(() => {
        feed.current = undefined;
        if (alive) setScreenFailed(true);
      });
    return () => {
      alive = false;
      feed.current?.close();
      feed.current = undefined;
    };
  }, [frames, seek]);
  React.useEffect(() => {
    seek(ms);
  }, [ms, seek]);

  React.useEffect(() => {
    if (!playing) return;
    const t = setInterval(() => setMs((m) => advance(m, TICK_MS, speed, duration)), TICK_MS);
    return () => clearInterval(t);
  }, [playing, speed, duration]);
  React.useEffect(() => {
    if (ms >= duration) setPlaying(false);
  }, [ms, duration]);

  const started = Date.parse(pb.started_at);
  const at = (m: number) => clockTime(new Date(started + m).toISOString());
  const gap = gapAt(gaps, ms);
  const download = () => {
    const url = URL.createObjectURL(new Blob([doc.raw], { type: "application/json" }));
    const a = document.createElement("a");
    a.href = url;
    a.download = `${recKey}.json`;
    a.click();
    setTimeout(() => URL.revokeObjectURL(url), 0);
  };
  const seekTrack = (e: React.MouseEvent<HTMLDivElement>) => {
    const r = e.currentTarget.getBoundingClientRect();
    setMs(Math.round(((e.clientX - r.left) / r.width) * duration));
  };

  return (
    <div className="grid items-start gap-3.5 lg:grid-cols-[minmax(0,1fr)_420px]" data-testid="desktop-replay">
      <div className="overflow-hidden rounded-xl border border-border bg-card">
        <div className="flex items-center gap-2 border-b border-border px-3.5 py-2.5">
          <span className="text-body font-medium">{C.SCREEN}</span>
          <Button size="sm" variant="outline" className="ml-auto" onClick={download}>
            <Download className="size-3.5" aria-hidden />
            {C.DOWNLOAD}
          </Button>
        </div>
        <div className="relative p-3.5">
          <div ref={screen} className="h-[400px] overflow-hidden rounded-lg border border-border bg-background" data-testid="desktop-replay-screen" />
          {screenFailed && (
            <p role="alert" className="absolute inset-x-3.5 top-3.5 p-6 text-center text-meta text-muted-foreground">
              {C.SCREEN_FAILED}
            </p>
          )}
          {gap && (
            <div className="absolute inset-3.5 grid place-items-center">
              <div role="status" className="flex max-w-[440px] flex-col items-center gap-2.5 rounded-xl border border-warning/40 bg-warning-subtle px-7 py-5 text-center" data-testid="desktop-replay-gap">
                <Pause className="size-5 text-warning" aria-hidden />
                <span className="text-[15px] font-semibold">{C.PAUSED_FOR(gapDuration(gap.endMs - gap.startMs))}</span>
                <span className="text-body text-muted-foreground">{C.PAUSED_BODY(gap.reason)}</span>
                <div className="flex gap-2">
                  <Button size="sm" onClick={() => setMs(gap.endMs)}>
                    {C.SKIP_GAP}
                  </Button>
                  <Button size="sm" variant="ghost" onClick={() => marks.current[gap.startMs]?.focus()}>
                    {C.SHOW_GAP}
                  </Button>
                </div>
              </div>
            </div>
          )}
        </div>
        <div className="px-[18px] pb-3 pt-1">
          <div className="relative mt-1 h-[46px] cursor-pointer" onClick={seekTrack} data-testid="desktop-replay-timeline">
            <div className="absolute inset-x-0 top-[18px] h-2 rounded bg-muted" />
            <div className="absolute left-0 top-[18px] h-2 rounded-l bg-primary" style={{ width: pct(ms, duration) }} />
            {gaps.map((g) => (
              <button
                key={g.startMs}
                type="button"
                ref={(el) => {
                  marks.current[g.startMs] = el;
                }}
                className="absolute top-[14px] h-4 border border-warning bg-warning-subtle focus-visible:outline focus-visible:outline-2 focus-visible:outline-warning"
                style={{ left: pct(g.startMs, duration), width: pct(g.endMs - g.startMs, duration) }}
                onClick={(e) => {
                  e.stopPropagation();
                  setMs(g.startMs);
                }}
                data-testid="desktop-replay-gap-mark"
              >
                <span className="absolute left-0 top-[18px] whitespace-nowrap text-[11px] text-warning">{C.GAP(gapDuration(g.endMs - g.startMs))}</span>
              </button>
            ))}
            {actions.map((a) => (
              <i
                key={a.id}
                aria-hidden
                className={`absolute top-2 h-2.5 w-0.5 ${a.blocked ? "bg-warning" : "bg-primary"}`}
                style={{ left: pct(a.ms, duration) }}
              />
            ))}
            <span
              className="absolute top-[11px] -ml-1.5 h-[22px] w-3 rounded-[3px] bg-foreground"
              style={{ left: pct(ms, duration) }}
              data-testid="desktop-replay-playhead"
              data-ms={ms}
            />
          </div>
          <div className="flex items-center gap-3">
            <Button size="sm" variant="ghost" aria-label={playing ? C.PAUSE : C.PLAY} onClick={() => setPlaying((p) => !p)}>
              {playing ? <Pause className="size-4" aria-hidden /> : <Play className="size-4" aria-hidden />}
            </Button>
            <span className="font-mono text-meta" data-testid="desktop-replay-clock">
              {at(ms)} / {at(duration)}
            </span>
            <span className="flex-1" />
            <span className="text-meta text-muted-foreground">{C.SPEED}</span>
            {SPEEDS.map((s) => (
              <button
                key={s}
                type="button"
                aria-pressed={speed === s}
                onClick={() => setSpeed(s)}
                className={`rounded-[5px] px-[7px] py-0.5 font-mono text-meta ${speed === s ? "bg-foreground/15 text-foreground" : "text-muted-foreground"}`}
              >
                {s}×
              </button>
            ))}
          </div>
        </div>
        <div className="flex flex-wrap items-center gap-3 border-t border-border px-3.5 py-2 text-meta text-muted-foreground">
          <span><span className="text-primary">▏</span> {C.LEGEND_ACTION}</span>
          <span><span className="text-warning">▏</span> {C.LEGEND_BLOCKED}</span>
          <span><span className="text-warning">▒</span> {C.LEGEND_PAUSED}</span>
          <span className="flex-1" />
          <span>{C.KEYSTROKES}</span>
        </div>
      </div>
      <div className="flex flex-col gap-3.5">
        <ReplayActionLog
          actions={actions}
          gaps={gaps}
          active={active}
          duration={duration}
          onJump={(id, to) => {
            setActive(id);
            setMs(to);
          }}
          onGoAudit={onGoAudit}
        />
        <ReplayChain runId={runId} recKey={recKey} from={pb.started_at} gaps={gaps} audit={audit} />
      </div>
    </div>
  );
}

// desktopSessions are the run's stored screen recordings, newest last.
export function desktopSessions(audit: AuditEvent[]): AuditEvent[] {
  return audit.filter((e) => e.action === "desktop.recording.write" && e.outcome === "success" && !!e.target);
}
