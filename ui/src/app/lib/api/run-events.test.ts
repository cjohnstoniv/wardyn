/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { setToken } from "./core";
import { followRunEvents } from "./run-events";

const enc = new TextEncoder();
const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

/** A 200 event stream that sends each chunk in turn, and closes after the last unless `open` is set. */
function stream(chunks: string[], open = false, status = 200, type = "text/event-stream"): Response {
  return new Response(
    new ReadableStream<Uint8Array>({
      start(c) {
        for (const chunk of chunks) c.enqueue(enc.encode(chunk));
        if (!open) c.close();
      },
    }),
    { status, headers: { "Content-Type": type } },
  );
}

const fetchMock = vi.fn<(url: string, init: RequestInit) => Promise<Response>>();
const onEvent = vi.fn<(type: string) => void>();
const headerOf = (call: number, name: string) => new Headers(fetchMock.mock.calls[call][1].headers).get(name);

beforeEach(() => {
  fetchMock.mockReset();
  onEvent.mockReset();
  vi.stubGlobal("fetch", fetchMock);
});
afterEach(() => {
  vi.unstubAllGlobals();
  setToken(null);
});

describe("followRunEvents", () => {
  it("reads each event across chunk boundaries, skips the keepalive, and resumes from the last id", async () => {
    fetchMock
      .mockResolvedValueOnce(stream(["id: 1\nevent: provi", "sioning\ndata: {}\n\n: keepalive\n\n", "id: 2\nevent: pulling\ndata: {}\n\n"]))
      .mockResolvedValueOnce(stream(["id: 3\nevent: ended\ndata: {}\n\n"]));
    followRunEvents("r/1", onEvent, 0);
    await vi.waitFor(() => expect(onEvent).toHaveBeenCalledTimes(3));
    expect(onEvent.mock.calls.map((c) => c[0])).toEqual(["provisioning", "pulling", "ended"]);
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(String(fetchMock.mock.calls[0][0])).toMatch(/\/api\/v1\/runs\/r%2F1\/events$/);
    expect(headerOf(0, "Accept")).toBe("text/event-stream");
    expect(headerOf(1, "Accept")).toBe("text/event-stream");
    expect(headerOf(0, "Last-Event-ID")).toBeNull();
    expect(headerOf(1, "Last-Event-ID")).toBe("2");
  });

  it("stops after the run's stream says it ended", async () => {
    fetchMock.mockResolvedValue(stream(["id: 1\nevent: ended\ndata: {}\n\n"], true));
    followRunEvents("r1", onEvent, 0);
    await vi.waitFor(() => expect(onEvent).toHaveBeenCalledTimes(1));
    expect(onEvent).toHaveBeenCalledWith("ended");
    expect(fetchMock.mock.calls[0][1].signal?.aborted).toBe(true);
    await sleep(50);
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it("stops when the stream is refused", async () => {
    fetchMock.mockResolvedValue(
      new Response(JSON.stringify({ error: "too many streams" }), { status: 422, headers: { "Content-Type": "application/json" } }),
    );
    followRunEvents("r1", onEvent, 0);
    await sleep(50);
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(onEvent).not.toHaveBeenCalled();
  });

  it("gives up after two streams in a row end at once with no event", async () => {
    fetchMock.mockImplementation(async () => stream([]));
    followRunEvents("r1", onEvent, 0);
    await sleep(100);
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(onEvent).not.toHaveBeenCalled();
  });

  it("close aborts the open stream", async () => {
    fetchMock.mockResolvedValue(stream([], true));
    const close = followRunEvents("r1", onEvent, 0);
    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));
    close();
    expect(fetchMock.mock.calls[0][1].signal?.aborted).toBe(true);
    await sleep(50);
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(onEvent).not.toHaveBeenCalled();
  });

  it("carries the stored admin token", async () => {
    setToken("tok");
    fetchMock.mockResolvedValue(stream([], true));
    const close = followRunEvents("r1", onEvent, 0);
    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));
    expect(headerOf(0, "Authorization")).toBe("Bearer tok");
    close();
  });
});
