/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */
import { describe, expect, it } from "vitest";
import { createOutputFlow, FLOW_HIGH_WATERMARK, FLOW_LOW_WATERMARK } from "./attach-terminal-flow";

function harness() {
  const cbs: Array<() => void> = [];
  const writes: Uint8Array[] = [];
  const sent: string[] = [];
  const flow = createOutputFlow(
    {
      write: (d, cb) => {
        writes.push(d);
        cbs.push(cb!);
      },
    },
    (t) => sent.push(t),
  );
  return { flow, cbs, writes, sent };
}

describe("createOutputFlow", () => {
  it("stays quiet at the high watermark", () => {
    const { flow, sent } = harness();
    flow.write(new Uint8Array(FLOW_HIGH_WATERMARK));
    expect(sent).toEqual([]);
  });

  it("pauses once past the high watermark and resumes below the low one", () => {
    const { flow, cbs, sent } = harness();
    flow.write(new Uint8Array(FLOW_HIGH_WATERMARK));
    flow.write(new Uint8Array(FLOW_LOW_WATERMARK + 1024));
    flow.write(new Uint8Array(1024));
    expect(sent).toEqual(["pause"]); // one frame, not one per chunk
    cbs[0]!(); // backlog is still above the low watermark
    expect(sent).toEqual(["pause"]);
    cbs[1]!(); // 1 KiB left
    expect(sent).toEqual(["pause", "resume"]);
    cbs[2]!();
    expect(sent).toEqual(["pause", "resume"]);
  });

  it("hands xterm a frame larger than the high watermark in slices no larger than it", () => {
    const { flow, cbs, writes, sent } = harness();
    const frame = Uint8Array.from({ length: 2 * FLOW_HIGH_WATERMARK + 5 }, (_, i) => i % 251);
    flow.write(frame);
    expect(writes.map((w) => w.length)).toEqual([FLOW_HIGH_WATERMARK, FLOW_HIGH_WATERMARK, 5]);
    expect(Uint8Array.from(writes.flatMap((w) => [...w]))).toEqual(frame); // in order, nothing lost
    expect(sent).toEqual(["pause"]);
    cbs[0]!();
    cbs[1]!();
    expect(sent).toEqual(["pause", "resume"]); // 5 bytes left
    cbs[2]!();
    expect(sent).toEqual(["pause", "resume"]);
  });
});
