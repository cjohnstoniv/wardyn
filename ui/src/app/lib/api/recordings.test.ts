/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { recordings } from "./recordings";

// parseRecording turns an UNTRUSTED asciicast document (fetched as text from the
// recording store) into the terminal-player shape. It must be robust to garbage:
// a single malformed line, a wrong event op, a truncated frame — none may throw
// or corrupt the render. It had zero coverage. Reached here through its
// only export path, recordings.getRecording, with a stubbed fetch.
describe("recordings.getRecording — asciicast v2 parsing", () => {
  let fetchMock: ReturnType<typeof vi.fn>;
  beforeEach(() => {
    fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
  });
  afterEach(() => vi.unstubAllGlobals());

  const textResponse = (body: string, status = 200) =>
    new Response(body, { status, headers: { "Content-Type": "text/plain" } });

  it("parses the header line and only the 'o' output events", async () => {
    const cast = [
      JSON.stringify({ version: 2, width: 80, height: 24, title: "demo" }),
      JSON.stringify([0.5, "o", "hello"]),
      JSON.stringify([1, "i", "keystroke ignored"]), // input event: dropped
      JSON.stringify([1.5, "o", "world"]),
    ].join("\n");
    fetchMock.mockResolvedValueOnce(textResponse(cast));

    const rec = await recordings.getRecording("run-1");
    expect(rec).toBeDefined();
    expect(rec!.header).toMatchObject({ version: 2, width: 80, height: 24, title: "demo" });
    expect(rec!.events).toEqual([
      [0.5, "o", "hello"],
      [1.5, "o", "world"],
    ]);
    expect(rec!.cast).toBe(cast); // raw text preserved for re-download
  });

  it("skips garbage/non-JSON lines instead of throwing", async () => {
    const cast = [
      JSON.stringify({ version: 2 }),
      "this is not json at all",
      JSON.stringify([0, "o", "ok"]),
      "{ truncated frame",
      JSON.stringify([2, "o"]), // too short (<3 elems): dropped
    ].join("\n");
    fetchMock.mockResolvedValueOnce(textResponse(cast));

    const rec = await recordings.getRecording("run-2");
    expect(rec!.events).toEqual([[0, "o", "ok"]]);
    // Defaults survive a header line that omits width/height.
    expect(rec!.header).toMatchObject({ version: 2, width: 96, height: 26 });
  });

  it("falls back to default header when the first line is already an event", async () => {
    const cast = JSON.stringify([0.1, "o", "no header here"]);
    fetchMock.mockResolvedValueOnce(textResponse(cast));
    const rec = await recordings.getRecording("run-3");
    expect(rec!.header).toMatchObject({ version: 2, width: 96, height: 26 });
    expect(rec!.events).toEqual([[0.1, "o", "no header here"]]);
  });

  it("returns undefined for a 404 (no recording captured)", async () => {
    fetchMock.mockResolvedValueOnce(new Response(null, { status: 404 }));
    expect(await recordings.getRecording("run-4")).toBeUndefined();
  });

  it("returns undefined for an empty/whitespace-only document", async () => {
    fetchMock.mockResolvedValueOnce(textResponse("   \n  \n"));
    expect(await recordings.getRecording("run-5")).toBeUndefined();
  });
});
