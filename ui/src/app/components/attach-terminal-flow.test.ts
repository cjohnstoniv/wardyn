/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */
import { describe, expect, it } from "vitest";
import { createOutputFlow, FLOW_HIGH_WATERMARK, FLOW_LOW_WATERMARK } from "./attach-terminal-flow";

function harness() {
  const cbs: Array<() => void> = [];
  const sent: string[] = [];
  const flow = createOutputFlow({ write: (_d, cb) => void cbs.push(cb!) }, (t) => sent.push(t));
  return { flow, cbs, sent };
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
});
