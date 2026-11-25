/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The assistant client: PUT sends the GET's ETag as If-Match and the block as JSON; a 200 is applied, a 202
// carrying a pending_change is held (never an applied save), and a stale save is an HttpError.
import { describe, it, expect, vi, afterEach } from "vitest";
import { assistant } from "./assistant";
import { HttpError } from "./core";

afterEach(() => vi.unstubAllGlobals());

const BLOCK = { compose: true, review: false, run_history_counts: false, never_sent_kinds: [], providers: [] };
const CHANGE = { id: "c1", target_kind: "assistant_settings", op: "replace", target_key: "assistant", state: "pending" };

function stubFetch(status: number, body: unknown, headers: Record<string, string> = {}) {
  const fetchMock = vi.fn(async () => new Response(JSON.stringify(body), { status, headers }));
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

const sent = (fetchMock: ReturnType<typeof stubFetch>) => {
  const [url, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit];
  return { url, init, headers: new Headers(init.headers) };
};

describe("assistant client", () => {
  it("getAssistantSettings returns the block with the ETag", async () => {
    const f = stubFetch(200, BLOCK, { ETag: '"a1"' });
    await expect(assistant.getAssistantSettings()).resolves.toEqual({ settings: BLOCK, etag: '"a1"' });
    expect(sent(f).url).toContain("/assistant/settings");
  });

  it("putAssistantSettings sends If-Match and the block, and maps a 200 to applied", async () => {
    const f = stubFetch(200, BLOCK, { ETag: '"a2"' });
    await expect(assistant.putAssistantSettings(BLOCK, '"a1"')).resolves.toEqual({ applied: { settings: BLOCK, etag: '"a2"' } });
    const { init, headers } = sent(f);
    expect(init.method).toBe("PUT");
    expect(headers.get("If-Match")).toBe('"a1"');
    expect(JSON.parse(init.body as string)).toEqual(BLOCK);
  });

  it("maps a 202 with a pending_change to held, carrying the change", async () => {
    stubFetch(202, { pending_change: CHANGE });
    const result = await assistant.putAssistantSettings(BLOCK, '"a1"');
    expect(result).toEqual({ held: CHANGE });
    expect("applied" in result).toBe(false);
  });

  it("a stale save is an HttpError, not a result", async () => {
    stubFetch(412, { error: "stale", reason: "assistant_settings_invalid" });
    await expect(assistant.putAssistantSettings(BLOCK, '"old"')).rejects.toBeInstanceOf(HttpError);
  });

  it("getMyAssistant asks for the purpose", async () => {
    const f = stubFetch(200, { state: "off" });
    await expect(assistant.getMyAssistant("review")).resolves.toEqual({ state: "off" });
    expect(sent(f).url).toContain("/me/assistant?purpose=review");
  });
});
