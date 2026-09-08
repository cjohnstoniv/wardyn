/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { recordings } from "./recordings";
import { HttpError } from "./core";

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

  // An attach session's cast lives under the composite `<run-id>~<session-uuid>`
  // key; only the RUN segment is the run id. `~` is unreserved, so it must reach
  // the handler intact rather than percent-encoded into a different key.
  it("addresses the run's own cast by default and an attach session by its composite key", async () => {
    // A fresh Response per call — a Response body can only be read once.
    fetchMock.mockImplementation(async () => textResponse(JSON.stringify([0, "o", "x"])));

    await recordings.getRecording("run-7");
    expect(fetchMock.mock.calls[0][0]).toContain("/runs/run-7/recording/run-7");

    await recordings.getRecording("run-7", "run-7~9f2c");
    expect(fetchMock.mock.calls[1][0]).toContain("/runs/run-7/recording/run-7~9f2c");
  });

  it("returns undefined for a 404 (no recording captured)", async () => {
    fetchMock.mockResolvedValueOnce(new Response(null, { status: 404 }));
    expect(await recordings.getRecording("run-4")).toBeUndefined();
  });

  it("returns undefined for an empty/whitespace-only document", async () => {
    fetchMock.mockResolvedValueOnce(textResponse("   \n  \n"));
    expect(await recordings.getRecording("run-5")).toBeUndefined();
  });

  // a non-404 failure must surface the control plane's `{"error":"…"}`
  // message, not a hardcoded "failed to load recording" string that discards
  // the server's actionable reason.
  it("surfaces the server error message on a 500", async () => {
    fetchMock.mockResolvedValueOnce(
      new Response(JSON.stringify({ error: "recording store offline" }), {
        status: 500,
        headers: { "Content-Type": "application/json" },
      }),
    );
    const err = await recordings.getRecording("run-6").catch((e) => e);
    expect(err).toBeInstanceOf(HttpError);
    expect((err as HttpError).status).toBe(500);
    expect((err as HttpError).message).toBe("recording store offline");
  });
});

// F077: the Recordings library asks every run whether it has a cast, and a card
// renders two numbers. probeRecording answers those two numbers WITHOUT building
// an AsciicastEvent per output frame — so it must agree with getRecording on
// every one of them, including the 404/empty contract, or the library and the
// player would disagree about what exists.
describe("recordings.probeRecording — the same document, without the event stream", () => {
  let fetchMock: ReturnType<typeof vi.fn>;
  beforeEach(() => {
    fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
  });
  afterEach(() => vi.unstubAllGlobals());

  const textResponse = (body: string, status = 200) =>
    new Response(body, { status, headers: { "Content-Type": "text/plain" } });

  const cast = [
    JSON.stringify({ version: 2, width: 80, height: 24 }),
    JSON.stringify([0.5, "o", "hello"]),
    JSON.stringify([2, "i", "keystroke ignored"]), // input event: not the duration
    JSON.stringify([1.5, "o", "world"]),
    "", // trailing newline
  ].join("\n");

  it("reports the LAST output frame's timestamp and the payload's byte size", async () => {
    fetchMock.mockResolvedValueOnce(textResponse(cast));
    const probe = await recordings.probeRecording("run-1");
    expect(probe).toEqual({
      run_id: "run-1",
      durationSec: 1.5,
      bytes: new Blob([cast]).size,
      cast,
    });
  });

  it("agrees with getRecording's duration on the same document — the card and the player cannot disagree", async () => {
    fetchMock.mockImplementation(async () => textResponse(cast));
    const probe = await recordings.probeRecording("run-1");
    const rec = await recordings.getRecording("run-1");
    const fromEvents = rec!.events[rec!.events.length - 1][0];
    expect(probe!.durationSec).toBe(fromEvents);
    expect(new Blob([rec!.cast]).size).toBe(probe!.bytes);
  });

  it("leaves durationSec undefined when nothing was ever output (no fabricated 0:00)", async () => {
    const headerOnly = JSON.stringify({ version: 2, width: 80, height: 24 });
    fetchMock.mockResolvedValueOnce(textResponse(headerOnly));
    const probe = await recordings.probeRecording("run-2");
    expect(probe!.durationSec).toBeUndefined();
    expect(probe!.bytes).toBe(new Blob([headerOnly]).size);
  });

  it("skips garbage lines rather than throwing, like the parser it stands in for", async () => {
    const messy = ["{ truncated", JSON.stringify([3, "o", "ok"]), "not json"].join("\n");
    fetchMock.mockResolvedValueOnce(textResponse(messy));
    expect((await recordings.probeRecording("run-3"))!.durationSec).toBe(3);
  });

  it("keeps getRecording's contract: undefined on 404 and on an empty document, throw on a 500", async () => {
    fetchMock.mockResolvedValueOnce(new Response(null, { status: 404 }));
    expect(await recordings.probeRecording("run-4")).toBeUndefined();
    fetchMock.mockResolvedValueOnce(textResponse("   \n  \n"));
    expect(await recordings.probeRecording("run-5")).toBeUndefined();
    fetchMock.mockResolvedValueOnce(
      new Response(JSON.stringify({ error: "recording store offline" }), {
        status: 500,
        headers: { "Content-Type": "application/json" },
      }),
    );
    const err = await recordings.probeRecording("run-6").catch((e) => e);
    expect(err).toBeInstanceOf(HttpError);
    expect((err as HttpError).status).toBe(500);
  });

  it("addresses an attach session by its composite key, as getRecording does", async () => {
    fetchMock.mockImplementation(async () => textResponse(JSON.stringify([0, "o", "x"])));
    await recordings.probeRecording("run-7", "run-7~9f2c");
    expect(fetchMock.mock.calls[0][0]).toContain("/runs/run-7/recording/run-7~9f2c");
  });
});

// parseCast is the player's input, built on demand from a cast a probe already
// fetched — the same parser getRecording uses, exposed so the library screen
// does not have to hold one event stream per card.
describe("recordings.parseCast", () => {
  it("produces exactly what getRecording would have returned for that document", async () => {
    const doc = [JSON.stringify({ version: 2, width: 80, height: 24 }), JSON.stringify([1, "o", "hi"])].join("\n");
    const fetchMock = vi.fn().mockResolvedValue(new Response(doc, { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);
    const viaFetch = await recordings.getRecording("run-8");
    vi.unstubAllGlobals();
    expect(recordings.parseCast("run-8", doc)).toEqual(viaFetch);
  });
});
