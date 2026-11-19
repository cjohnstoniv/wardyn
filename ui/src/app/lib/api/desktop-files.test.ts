/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Files client's two reviewed-download calls, against the real module with fetch stubbed.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { HttpError } from "./core";
import { desktopFiles } from "./desktop-files";

let fetchMock: ReturnType<typeof vi.fn>;
const answer = (status: number, body: unknown) => {
  fetchMock = vi.fn().mockImplementation(
    async () => new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } }),
  );
  vi.stubGlobal("fetch", fetchMock);
};
const sent = () => ({ url: String(fetchMock.mock.calls[0][0]), init: fetchMock.mock.calls[0][1] as RequestInit });

beforeEach(() => answer(202, { id: "ap_1", state: "PENDING" }));
afterEach(() => vi.unstubAllGlobals());

describe("desktopFiles.review", () => {
  it("POSTs to the file's /review and returns the pending row", async () => {
    const row = await desktopFiles.review("r 1", "build logs.tar.gz");
    const { url, init } = sent();
    expect(init.method).toBe("POST");
    expect(url).toContain("/runs/r%201/desktop/files/build%20logs.tar.gz/review");
    expect(row.id).toBe("ap_1");
  });

  it("throws the refusal with its reason", async () => {
    answer(409, { error: "a review of this file is already waiting", reason: "desktop_review_pending" });
    const err = await desktopFiles.review("r1", "a.txt").catch((e: unknown) => e);
    expect(err).toBeInstanceOf(HttpError);
    expect((err as HttpError).reason).toBe("desktop_review_pending");
  });
});

describe("desktopFiles.download", () => {
  it("adds ?approval= when an approval id is given", async () => {
    answer(200, "x");
    await desktopFiles.download("r1", "a b.csv", "ap/1");
    expect(sent().url).toContain("/runs/r1/desktop/files/a%20b.csv?approval=ap%2F1");
  });

  it("sends no query without one", async () => {
    answer(200, "x");
    await desktopFiles.download("r1", "a.csv");
    expect(sent().url).not.toContain("approval");
  });
});
