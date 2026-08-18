/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { Download } from "lucide-react";
import * as AsciinemaPlayer from "asciinema-player";
import "asciinema-player/dist/bundle/asciinema-player.css";
import type { Recording } from "../../lib/types";

// TerminalPlayer renders a recorded PTY session through the real asciinema
// player, which embeds a terminal emulator. That is what makes escape
// sequences (cursor moves, color, screen clears, mouse / bracketed-paste mode
// toggles like the ones Claude Code emits on exit) render as a live terminal
// instead of literal garbage text — a plain <pre> dump cannot interpret them.
// The raw asciicast text is fed verbatim so playback is byte-faithful, and
// play / pause / seek / speed controls come for free.
// The replay speeds on offer. 1× is fidelity; 2×/4× exist because a replay is
// usually watched to find out WHAT happened, not to relive it in real time —
// a long agent session at 1× is minutes of watching text arrive.
const SPEEDS = [1, 2, 4] as const;

export function TerminalPlayer({ recording }: { recording: Recording }) {
  const ref = React.useRef<HTMLDivElement>(null);
  const [speed, setSpeed] = React.useState<(typeof SPEEDS)[number]>(1);

  React.useEffect(() => {
    const el = ref.current;
    if (!el || !recording.cast) return;

    // The capture may report 0x0 (no TTY size on the docker-exec PTY); fall
    // back to a sensible terminal size so the emulator has real dimensions.
    const cols = recording.header.width && recording.header.width > 0 ? recording.header.width : 100;
    const rows = recording.header.height && recording.header.height > 0 ? recording.header.height : 28;

    const player = AsciinemaPlayer.create({ data: recording.cast }, el, {
      cols,
      rows,
      fit: "width",
      terminalFontSize: "13px",
      theme: "asciinema",
      idleTimeLimit: 2, // compress long gaps of inactivity
      controls: true,
      autoPlay: false,
      speed,
    });

    return () => {
      try {
        player.dispose();
      } catch {
        /* already torn down */
      }
    };
    // speed is a creation-time option in asciinema-player v3, so changing it
    // rebuilds the player — acceptable: the rebuild is instant and seeking
    // back to where you were is what the progress bar is for.
  }, [recording.run_id, recording.cast, recording.header.width, recording.header.height, speed]);

  // Hand the operator the .cast itself — the console could only replay it, so
  // taking a session off-box meant curling the API by hand. Served from the
  // bytes already in memory: /runs/{id}/recording/{id} is bearer-authed and a
  // plain <a href> navigation can't carry the Authorization header (401).
  const download = () => {
    const url = URL.createObjectURL(
      new Blob([recording.cast], { type: "application/x-asciicast" }),
    );
    const a = document.createElement("a");
    a.href = url;
    a.download = `${recording.run_id}.cast`;
    a.click();
    // Next tick: Safari cancels the in-flight download if the object URL is
    // revoked synchronously after the click.
    setTimeout(() => URL.revokeObjectURL(url), 0);
  };

  return (
    <div className="overflow-hidden rounded-lg border border-border bg-[#0d1117]">
      <div className="flex items-center gap-1.5 border-b border-border bg-black/20 px-3 py-2">
        <span className="size-3 rounded-full bg-[#ff5f56]" />
        <span className="size-3 rounded-full bg-[#ffbd2e]" />
        <span className="size-3 rounded-full bg-[#28c840]" />
        {recording.header.title ? (
          <span className="ml-3 font-mono text-xs text-white/60">{recording.header.title}</span>
        ) : null}
        <span className="ml-auto font-mono text-[0.6875rem] text-white/60">
          {recording.events.length} events
        </span>
        <div role="radiogroup" aria-label="Playback speed" className="ml-2 flex items-center gap-0.5">
          {SPEEDS.map((x) => (
            <button
              key={x}
              type="button"
              role="radio"
              aria-checked={speed === x}
              aria-label={`${x}x speed`}
              onClick={() => setSpeed(x)}
              className={
                speed === x
                  ? "rounded px-1.5 py-0.5 font-mono text-[0.6875rem] bg-white/15 text-white"
                  : "rounded px-1.5 py-0.5 font-mono text-[0.6875rem] text-white/50 hover:text-white"
              }
            >
              {x}×
            </button>
          ))}
        </div>
        <button
          type="button"
          onClick={download}
          aria-label="Download recording (.cast)"
          className="inline-flex items-center text-white/60 hover:text-white"
        >
          <Download className="size-3.5" aria-hidden />
        </button>
      </div>
      <div ref={ref} />
    </div>
  );
}
