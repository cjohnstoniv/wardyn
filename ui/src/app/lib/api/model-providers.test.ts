/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { modelProviders } from "./model-providers";

// #970 — GET /model-providers carries connected_people beside the stored
// block. getModelProviders() splits it off: `connected` is what
// MODEL_PROVIDERS.CONNECTED(n) renders, and `providers` stays a document PUT
// can send back, whose strict decode would refuse the count.
describe("modelProviders.getModelProviders() — connected_people", () => {
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
  });
  afterEach(() => vi.unstubAllGlobals());

  function res(body: unknown) {
    return new Response(JSON.stringify(body), {
      status: 200,
      headers: { "Content-Type": "application/json", ETag: '"e1"' },
    });
  }

  it("splits the count off the document PUT sends back", async () => {
    const providers = [{ id: "anthropic", kind: "anthropic_api_key" }];
    fetchMock.mockResolvedValueOnce(res({ providers, connected_people: { anthropic: 3 } }));
    const snap = await modelProviders.getModelProviders();
    expect(snap.connected).toEqual({ anthropic: 3 });
    expect(snap.providers).toEqual({ providers });
    expect(snap.etag).toBe('"e1"');
  });

  it("reads a never-configured install ({}) as no counts", async () => {
    fetchMock.mockResolvedValueOnce(res({}));
    const snap = await modelProviders.getModelProviders();
    expect(snap.connected).toEqual({});
    expect(snap.providers).toEqual({});
  });
});
