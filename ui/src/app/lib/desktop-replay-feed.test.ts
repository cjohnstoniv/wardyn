/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { decodeFrames, PlaybackSocket, ReplayFeed, type ReplaySink } from "./desktop-replay-feed";

const bytes = (b: Uint8Array) => [...b];

describe("desktop replay feed (#2968, M7-Replay)", () => {
  it("decodes the desktop's frames and skips what the browser sent", () => {
    const frames = decodeFrames(["{0{UkZC", "}5{AQ==", "{40{AQID", "garbage"]);
    expect(frames.map((f) => [f.ms, bytes(f.bytes)])).toEqual([[0, [0x52, 0x46, 0x42]], [40, [1, 2, 3]]]);
  });

  it("hands RFB each frame as an open socket's message, and closes once", () => {
    const sock = new PlaybackSocket();
    const got: number[][] = [];
    let closed = 0;
    sock.onmessage = (e) => got.push([...new Uint8Array(e.data)]);
    sock.onclose = () => closed++;
    expect(sock.readyState).toBe("open");
    sock.push(new Uint8Array([7, 8]));
    sock.close();
    sock.close();
    expect(got).toEqual([[7, 8]]);
    expect([sock.readyState, closed]).toEqual(["closed", 1]);
  });

  it("feeds the frames up to the playhead, and starts a fresh decoder on a seek back", () => {
    const sinks: { got: number[]; closed: boolean }[] = [];
    const open = (): ReplaySink => {
      const s = { got: [] as number[], closed: false };
      sinks.push(s);
      return { push: (b) => s.got.push(...b), close: () => (s.closed = true) };
    };
    const feed = new ReplayFeed(decodeFrames(["{0{AQ==", "{100{Ag==", "{300{Aw=="]), open);
    feed.seek(0);
    feed.seek(150);
    expect(sinks.map((s) => s.got)).toEqual([[1, 2]]);
    feed.seek(300);
    expect(sinks[0].got).toEqual([1, 2, 3]);
    feed.seek(120);
    expect(sinks.map((s) => [s.got, s.closed])).toEqual([[[1, 2, 3], true], [[1, 2], false]]);
    feed.close();
    expect(sinks[1].closed).toBe(true);
  });
});
