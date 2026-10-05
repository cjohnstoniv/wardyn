/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */
// Output flow control for one attach socket (ttyd's model). xterm's write
// callback fires once a chunk has been parsed, so the bytes written but not yet
// parsed are the backlog. Past HIGH the server is told to stop reading the
// sandbox; once the backlog drains below LOW it is told to go on. The server
// bounds a pause that is never resumed, and a 0.8.5 server ignores both frames.
//
// xterm parses each write whole, between its own yields to the page, so a chunk
// reaches it in slices of at most HIGH bytes: however large a frame the server
// sends, one slice is one bounded piece of main-thread work.

export const FLOW_HIGH_WATERMARK = 128 * 1024;
export const FLOW_LOW_WATERMARK = 16 * 1024;

export interface OutputFlow {
  /** Writes one output chunk to the terminal and accounts for it. */
  write(bytes: Uint8Array): void;
}

export function createOutputFlow(
  term: { write(data: Uint8Array, cb?: () => void): void },
  sendControl: (type: "pause" | "resume") => void,
): OutputFlow {
  let pending = 0;
  let paused = false;
  const writeSlice = (slice: Uint8Array) => {
    pending += slice.length;
    if (!paused && pending > FLOW_HIGH_WATERMARK) {
      paused = true;
      sendControl("pause");
    }
    term.write(slice, () => {
      pending -= slice.length;
      if (paused && pending < FLOW_LOW_WATERMARK) {
        paused = false;
        sendControl("resume");
      }
    });
  };
  return {
    write(bytes) {
      for (let at = 0; at < bytes.length; at += FLOW_HIGH_WATERMARK) {
        writeSlice(bytes.subarray(at, at + FLOW_HIGH_WATERMARK));
      }
    },
  };
}
