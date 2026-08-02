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
export function TerminalPlayer({ recording }: { recording: Recording }) {
  const ref = React.useRef<HTMLDivElement>(null);

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
    });

    return () => {
      try {
        player.dispose();
      } catch {
        /* already torn down */
      }
    };
  }, [recording.run_id, recording.cast, recording.header.width, recording.header.height]);

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
      <div className="flex items-center gap-1.5 border-b border-border bg-card/60 px-3 py-2">
        <span className="size-3 rounded-full bg-[#ff5f56]" />
        <span className="size-3 rounded-full bg-[#ffbd2e]" />
        <span className="size-3 rounded-full bg-[#28c840]" />
        {recording.header.title ? (
          <span className="ml-3 font-mono text-xs text-muted-foreground">{recording.header.title}</span>
        ) : null}
        <span className="ml-auto font-mono text-[0.6875rem] text-muted-foreground">
          {recording.events.length} events
        </span>
        <button
          type="button"
          onClick={download}
          aria-label="Download recording (.cast)"
          className="inline-flex items-center text-muted-foreground hover:text-foreground"
        >
          <Download className="size-3.5" aria-hidden />
        </button>
      </div>
      <div ref={ref} />
    </div>
  );
}
