/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, afterEach, beforeEach } from "vitest";
import { access, AccessCollisionError, AccessPostureFlipRequiredError } from "./access";
import { HttpError } from "./core";

// R1-F112 — handleUpsertRoleMapping embeds TWO advisory counts beside the
// saved RoleMapping row (internal/api/access.go): tokens_revoked (already
// mirrored) and stale_token_snapshots (F6 type-mirror gap — silently dropped
// by the old client, so the People step could report tokens revoked without
// ever being able to say how many outstanding tokens were affected in the
// first place).
describe("access.upsertMapping — stale_token_snapshots wire mirror", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("surfaces stale_token_snapshots alongside tokensRevoked", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response(
          JSON.stringify({
            id: "m-1",
            value: "engineering",
            role: "user",
            stale_token_snapshots: 3,
            tokens_revoked: 2,
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
      ),
    );
    const res = await access.upsertMapping({ value: "engineering", role: "user" });
    expect(res.staleTokenSnapshots).toBe(3);
    expect(res.tokensRevoked).toBe(2);
    // The two counts, not the raw wire keys, ride on `mapping`.
    expect(res.mapping).toEqual({ id: "m-1", value: "engineering", role: "user" });
  });

  it("omits stale_token_snapshots when the write demoted no outstanding token", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response(JSON.stringify({ id: "m-1", value: "engineering", role: "user" }), {
          status: 201,
          headers: { "Content-Type": "application/json" },
        }),
      ),
    );
    const res = await access.upsertMapping({ value: "engineering", role: "user" });
    expect(res.staleTokenSnapshots).toBeUndefined();
    expect(res.created).toBe(true);
  });
});

describe("access request shapes and write refusals", () => {
  let fetchMock: ReturnType<typeof vi.fn>;
  beforeEach(() => {
    fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
  });
  afterEach(() => vi.unstubAllGlobals());

  it("reads access and preserves a preview error as an outcome", async () => {
    const state = { mappings: [], default_role: "user" };
    const preview = { error: "the supplied claims do not grant access" };
    fetchMock.mockResolvedValueOnce(new Response(JSON.stringify(state)));
    await expect(access.getAccess()).resolves.toEqual(state);
    expect(fetchMock).toHaveBeenLastCalledWith("/api/v1/access", expect.objectContaining({ method: "GET" }));
    const input = { groups: ["engineering"] };
    fetchMock.mockResolvedValueOnce(new Response(JSON.stringify(preview)));
    await expect(access.previewRole(input)).resolves.toEqual(preview);
    expect(fetchMock).toHaveBeenLastCalledWith("/api/v1/access/preview", expect.objectContaining({
      method: "POST", body: JSON.stringify(input),
    }));
  });

  it("writes the user type and posture acknowledgement in the mapping body", async () => {
    const input = { value: "engineering", role: "user", user_type: "developer", acknowledge_access_change: true };
    const mapping = { id: "mapping-1", ...input };
    fetchMock.mockResolvedValueOnce(new Response(JSON.stringify(mapping)));
    await expect(access.upsertMapping(input)).resolves.toMatchObject({ mapping, created: false });
    expect(fetchMock).toHaveBeenCalledWith("/api/v1/access/mappings", expect.objectContaining({
      method: "POST", body: JSON.stringify(input),
    }));
  });

  it.each([false, true])("deletes an encoded mapping with acknowledgement=%s in the query", async (acknowledge) => {
    fetchMock.mockResolvedValueOnce(new Response(null, { status: 204 }));
    await expect(access.deleteMapping("mapping/id ?", acknowledge)).resolves.toBeUndefined();
    const suffix = acknowledge ? "?acknowledge_access_change=true" : "";
    expect(fetchMock).toHaveBeenCalledWith(`/api/v1/access/mappings/mapping%2Fid%20%3F${suffix}`, expect.objectContaining({ method: "DELETE" }));
    expect(fetchMock.mock.calls[0][1].body).toBeUndefined();
  });

  describe.each([
    ["upsert", () => access.upsertMapping({ value: "engineering", role: "user" })],
    ["delete", () => access.deleteMapping("mapping-1")],
  ] as const)("%s refusal", (_name, request) => {
    it("preserves the typed posture change and both before/after values", async () => {
      const body = { error: "acknowledgement required", required_acknowledgement: true, before: "any person", after: "mapped people" };
      fetchMock.mockResolvedValueOnce(new Response(JSON.stringify(body), { status: 400 }));
      const error: unknown = await request().catch((e: unknown) => e);
      expect(error).toBeInstanceOf(AccessPostureFlipRequiredError);
      expect(error).toMatchObject({ status: 400, message: body.error, before: body.before, after: body.after });
    });

    it.each(["chart", "operator_allowlist"])("preserves the typed %s collision", async (cause) => {
      const body = { error: "mapping collision", cause, value: "engineering" };
      fetchMock.mockResolvedValueOnce(new Response(JSON.stringify(body), { status: 400 }));
      const error: unknown = await request().catch((e: unknown) => e);
      expect(error).toBeInstanceOf(AccessCollisionError);
      expect(error).toMatchObject({ status: 400, message: body.error, cause, value: body.value });
    });

    it.each([
      { error: "would lock out the last admin" },
      { error: "invalid posture body", required_acknowledgement: true, before: "any person", after: null },
      { error: "invalid collision body", cause: "unknown", value: "engineering" },
    ])("keeps an ordinary or incomplete envelope as HttpError: $error", async (body) => {
      fetchMock.mockResolvedValueOnce(new Response(JSON.stringify(body), { status: 400 }));
      const error: unknown = await request().catch((e: unknown) => e);
      expect(error).toBeInstanceOf(HttpError);
      expect(error).toMatchObject({ name: "HttpError", status: 400, message: body.error });
    });

    it("preserves a non-JSON error after attempting structured parsing", async () => {
      fetchMock.mockResolvedValueOnce(new Response("upstream unavailable", { status: 503 }));
      await expect(request()).rejects.toMatchObject({ name: "HttpError", status: 503, message: "upstream unavailable" });
    });
  });
});
