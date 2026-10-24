/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Feeds a desktop recording's playback frames (Go: recording.RFBPlayback, "{<ms>{<base64>") to an RFB
// decoder through a fake socket, so the replay draws the screen at the playhead (#2968, M7-Replay).
// The recording holds only the desktop's side of the stream, so what the decoder sends back goes
// nowhere. Seeking back starts a fresh decoder and replays the frames up to the playhead at once:
// RFB is a stream of updates, so a frame can only be drawn on top of the ones before it.
import type { RFBChannel } from "@novnc/novnc";

export interface ReplayFrame {
  ms: number;
  bytes: Uint8Array;
}

/** The server-to-browser frames, in order; anything else in the list is skipped. */
export function decodeFrames(frames: string[]): ReplayFrame[] {
  const out: ReplayFrame[] = [];
  for (const f of frames) {
    const end = f.indexOf("{", 1);
    const ms = Number(f.slice(1, end));
    if (f[0] !== "{" || end < 0 || !Number.isFinite(ms)) continue;
    const bin = atob(f.slice(end + 1));
    const bytes = new Uint8Array(bin.length);
    for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
    out.push({ ms, bytes });
  }
  return out;
}

/** The channel RFB reads the recording from: always open, and deaf to what RFB sends. */
export class PlaybackSocket implements RFBChannel {
  binaryType = "arraybuffer";
  protocol = "";
  readyState = "open";
  onopen: RFBChannel["onopen"] = null;
  onmessage: RFBChannel["onmessage"] = null;
  onerror: RFBChannel["onerror"] = null;
  onclose: RFBChannel["onclose"] = null;
  send(): void {}
  close(): void {
    if (this.readyState === "closed") return;
    this.readyState = "closed";
    this.onclose?.({ code: 1000, reason: "", wasClean: true });
  }
  push(bytes: Uint8Array): void {
    this.onmessage?.({ data: bytes.slice().buffer });
  }
}

/** One decoder: where frames go, and how to drop it. */
export interface ReplaySink {
  push(bytes: Uint8Array): void;
  close(): void;
}

/** Keeps a decoder at the playhead: forward feeds the frames since the last seek, back starts over. */
export class ReplayFeed {
  private sink?: ReplaySink;
  private next = 0;
  private at = -1;

  constructor(
    private readonly frames: ReplayFrame[],
    private readonly open: () => ReplaySink,
  ) {}

  seek(ms: number): void {
    if (!this.sink || ms < this.at) {
      this.sink?.close();
      this.sink = this.open();
      this.next = 0;
    }
    while (this.next < this.frames.length && this.frames[this.next].ms <= ms) {
      this.sink.push(this.frames[this.next].bytes);
      this.next++;
    }
    this.at = ms;
  }

  close(): void {
    this.sink?.close();
    this.sink = undefined;
  }
}
