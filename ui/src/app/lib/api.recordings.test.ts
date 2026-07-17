/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { api, HttpError } from "./api";

// Pins the getRecording error/response mapping against the recording wire
// contract (internal/api serves asciicast text; 404 = no recording).
describe("api.getRecording()", () => {
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
  });
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("returns undefined on a 404 (no recording for the run)", async () => {
    fetchMock.mockResolvedValueOnce(new Response("", { status: 404 }));
    await expect(api.getRecording("run_1")).resolves.toBeUndefined();
  });

  // U097: a non-404 failure must surface the control plane's `{"error":"…"}`
  // message, not a hardcoded "failed to load recording" string that discards
  // the server's actionable reason.
  it("surfaces the server error message on a 500", async () => {
    fetchMock.mockResolvedValueOnce(
      new Response(JSON.stringify({ error: "recording store offline" }), {
        status: 500,
        headers: { "Content-Type": "application/json" },
      }),
    );
    const err = await api.getRecording("run_1").catch((e) => e);
    expect(err).toBeInstanceOf(HttpError);
    expect((err as HttpError).status).toBe(500);
    expect((err as HttpError).message).toBe("recording store offline");
  });
});
