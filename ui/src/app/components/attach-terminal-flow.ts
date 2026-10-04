/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */
// Output flow control for one attach socket (ttyd's model). xterm's write
// callback fires once a chunk has been parsed, so the bytes written but not yet
// parsed are the backlog. Past HIGH the server is told to stop reading the
// sandbox; once the backlog drains below LOW it is told to go on. The server
// bounds a pause that is never resumed, and a 0.8.5 server ignores both frames.

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
  return {
    write(bytes) {
      pending += bytes.length;
      if (!paused && pending > FLOW_HIGH_WATERMARK) {
        paused = true;
        sendControl("pause");
      }
      term.write(bytes, () => {
        pending -= bytes.length;
        if (paused && pending < FLOW_LOW_WATERMARK) {
          paused = false;
          sendControl("resume");
        }
      });
    },
  };
}
