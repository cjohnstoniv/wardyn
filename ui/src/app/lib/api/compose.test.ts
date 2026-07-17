/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { composer } from "./compose";
import { HttpError } from "./core";

// The compose() SSE-streaming branch (taken whenever an onStage callback is
// passed) hand-parses `data: <json>\n\n` frames off a ReadableStream. The
// existing composer tests pass no onStage, so the whole streaming path — frame
// buffering, stage dispatch, terminal result/error frames — was unexercised
// (U073). These drive it over a stubbed reader, including a frame split across
// two chunks (the buffering edge the \n\n scan exists for).
describe("composer.compose() — SSE streaming path", () => {
  let fetchMock: ReturnType<typeof vi.fn>;
  beforeEach(() => {
    fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
  });
  afterEach(() => vi.unstubAllGlobals());

  const enc = new TextEncoder();

  // A minimal Response-like object exposing exactly what the streaming branch
  // reads: status/ok (past wfetch's 401 guard), a text/event-stream content-type,
  // and a body.getReader() yielding the given raw chunks in order.
  function sseResponse(chunks: string[]) {
    let i = 0;
    return {
      status: 200,
      ok: true,
      headers: { get: (h: string) => (h === "Content-Type" ? "text/event-stream" : null) },
      body: {
        getReader() {
          return {
            read: async () =>
              i < chunks.length
                ? { value: enc.encode(chunks[i++]), done: false }
                : { value: undefined, done: true },
          };
        },
      },
    };
  }

  const result = {
    kind: "proposal",
    proposed: { run: {}, inline_policy: {} },
    overall_risk: "medium",
    summary: "ok",
  };

  it("dispatches every stage frame to onStage and returns the terminal result", async () => {
    // The propose frame is split across two chunks to exercise the \n\n buffer scan.
    fetchMock.mockResolvedValueOnce(
      sseResponse([
        'data: {"type":"stage","stage":"validate"}\n\n',
        'data: {"type":"stage","stage":"pro',
        'pose"}\n\n' + `data: ${JSON.stringify({ type: "result", result })}\n\n`,
      ]),
    );

    const stages: string[] = [];
    const out = await composer.compose({ prompt: "fix CI" }, [], (s) => stages.push(s));

    expect(stages).toEqual(["validate", "propose"]);
    expect(out).toEqual(result);
    // Streaming was requested via the Accept header.
    const init = fetchMock.mock.calls[0][1] as RequestInit;
    expect(new Headers(init.headers).get("Accept")).toBe("text/event-stream");
  });

  it("throws an HttpError when the stream carries an error frame", async () => {
    fetchMock.mockResolvedValueOnce(
      sseResponse([
        'data: {"type":"stage","stage":"detect"}\n\n',
        'data: {"type":"error","error":"backend exploded"}\n\n',
      ]),
    );
    await expect(composer.compose({ prompt: "x" }, [], () => {})).rejects.toMatchObject({
      status: 502,
      message: "backend exploded",
    });
  });

  it("throws when the stream ends without a result frame", async () => {
    fetchMock.mockResolvedValueOnce(sseResponse(['data: {"type":"stage","stage":"validate"}\n\n']));
    await expect(composer.compose({ prompt: "x" }, [], () => {})).rejects.toBeInstanceOf(HttpError);
  });

  it("falls back to the JSON path when the server does not stream", async () => {
    // A non-streaming server (or a pre-flush 4xx) returns plain JSON even though
    // onStage was passed — compose() must parse it via the synchronous path.
    fetchMock.mockResolvedValueOnce(
      new Response(JSON.stringify(result), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
    );
    const stages: string[] = [];
    const out = await composer.compose({ prompt: "x" }, [], (s) => stages.push(s));
    expect(stages).toEqual([]);
    expect(out).toEqual(result);
  });
});
