/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The assistant client: PUT sends the GET's ETag as If-Match and the block as JSON; a 200 is applied, a 202
// carrying a pending_change is held (never an applied save), and a stale save is an HttpError.
import { describe, it, expect, vi, afterEach } from "vitest";
import { assistant, assistantSessionHeader } from "./assistant";
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

describe("assistant client: the editor's calls", () => {
  it("assistantSessionHeader names the session in the one header a save carries", () => {
    expect(assistantSessionHeader("s-1")).toEqual({ "X-Wardyn-Assistant-Session": "s-1" });
  });

  it("postEdits posts the draft and what changed to the session as the person", async () => {
    const f = stubFetch(202, { seq: 4 });
    await expect(assistant.postEdits("s 1", { draft: { allowed_domains: ["a.example"] }, changed: [{ field: "allowed_domains", value: '["a.example"]' }] })).resolves.toEqual({ seq: 4 });
    const { url, init } = sent(f);
    expect(url).toContain("/assistant/sessions/s%201/edits");
    expect(init.method).toBe("POST");
    expect(JSON.parse(init.body as string)).toEqual({ draft: { allowed_domains: ["a.example"] }, changed: [{ field: "allowed_domains", value: '["a.example"]' }] });
  });

  it("postChecks posts the target and the draft, and returns the server's answer as it is", async () => {
    const answer = { valid: true, errors: [], review: null, risk: null, approvals_needed: 2, dropped: [] };
    const f = stubFetch(200, answer);
    await expect(assistant.postChecks({ target_kind: "unit", target: "u-1", draft: { name: "Payments" } })).resolves.toEqual(answer);
    const { url, init } = sent(f);
    expect(url).toContain("/assistant/checks");
    expect(JSON.parse(init.body as string)).toEqual({ target_kind: "unit", target: "u-1", draft: { name: "Payments" } });
  });

  it("confirmBuildFiles posts the preview digest to the session's confirm route", async () => {
    const f = stubFetch(200, { id: "s-1", state: "starting", preview_digest: "d2" });
    await expect(assistant.confirmBuildFiles("s-1", "d1")).resolves.toMatchObject({ preview_digest: "d2" });
    const { url, init } = sent(f);
    expect(url).toContain("/assistant/sessions/s-1/build-files/confirm");
    expect(JSON.parse(init.body as string)).toEqual({ preview_digest: "d1" });
  });

  it("confirmBuildFiles keeps the stale preview's reason on a 409", async () => {
    stubFetch(409, { error: "changed", reason: "assistant_preview_stale" });
    await expect(assistant.confirmBuildFiles("s-1", "old")).rejects.toMatchObject({ status: 409, reason: "assistant_preview_stale" });
  });

  it("createSession sends the workspace and repository source and no address", async () => {
    const f = stubFetch(200, { id: "s-1", state: "fetching", provider: "p", model: "m", letters: {} });
    await assistant.createSession({ purpose: "compose", target: { kind: "template" }, preview_digest: "d1", workspace_id: "w-1", repo: "github.com/example-org/payments-ledger" });
    const body = JSON.parse(sent(f).init.body as string) as Record<string, unknown>;
    expect(body).toMatchObject({ workspace_id: "w-1", repo: "github.com/example-org/payments-ledger" });
    expect(Object.keys(body)).not.toContain("url");
  });
});
