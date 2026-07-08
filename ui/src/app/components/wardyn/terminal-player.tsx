/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
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

  return (
    <div className="overflow-hidden rounded-lg border border-border bg-[#0d1117]">
      <div className="flex items-center gap-1.5 border-b border-border bg-card/60 px-3 py-2">
        <span className="size-3 rounded-full bg-[#ff5f56]" />
        <span className="size-3 rounded-full bg-[#ffbd2e]" />
        <span className="size-3 rounded-full bg-[#28c840]" />
        {recording.header.title ? (
          <span className="ml-3 font-mono text-xs text-muted-foreground">{recording.header.title}</span>
        ) : null}
        <span className="ml-auto font-mono text-[11px] text-muted-foreground/70">
          {recording.events.length} events
        </span>
      </div>
      <div ref={ref} />
    </div>
  );
}
