/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { beforeEach, describe, expect, it, vi } from "vitest";

const wfetchMock = vi.fn();
vi.mock("../../../../../lib/api/core", async (orig) => ({ ...(await orig<typeof import("../../../../../lib/api/core")>()), wfetch: (...a: unknown[]) => wfetchMock(...a) }));

import { HttpError } from "../../../../../lib/api/core";
import { seedLoaders } from "./seed-loaders";

const json = (body: unknown) => new Response(JSON.stringify(body), { status: 200, headers: { "content-type": "application/json" } });
const signal = new AbortController().signal;
const T = "d1111111-1111-4111-8111-111111111111";
const R = "e1111111-1111-4111-8111-111111111111";
const document = { api_version: "wardyn/v1", kind: "RunTemplate", intent: { shape: "application", workload: { kind: "command", command: "make test" } } };

beforeEach(() => wfetchMock.mockReset());

describe("seed loaders", () => {
  it("reads a pinned template revision and seeds the draft with its document intent", async () => {
    wfetchMock.mockResolvedValue(json({ id: T, kind: "policy", name: "Nightly", revision: 4, document }));
    const seed = await seedLoaders.template({ id: T, revision: 4 }, signal);
    expect(wfetchMock.mock.calls[0][0]).toBe(`/templates/${T}/revisions/4`);
    expect(seed).toEqual({ origin: { kind: "template", id: T, revision: 4, name: "Nightly", templateKind: "policy" }, intent: document.intent });
  });

  it("reads the current revision when the link names none, and pins what it read", async () => {
    wfetchMock.mockResolvedValue(json({ id: T, kind: "template", name: "Nightly", revision: 7, document }));
    const seed = await seedLoaders.template({ id: T }, signal);
    expect(wfetchMock.mock.calls[0][0]).toBe(`/templates/${T}`);
    expect(seed.origin).toMatchObject({ kind: "template", revision: 7 });
  });

  it("reads a run's launch intent and seeds an again draft", async () => {
    wfetchMock.mockResolvedValue(json({ run_id: R, created_at: "2026-10-11T08:00:00Z", document }));
    const seed = await seedLoaders.again(R, signal);
    expect(wfetchMock.mock.calls[0][0]).toBe(`/runs/${R}/launch-intent`);
    expect(seed).toMatchObject({ origin: { kind: "again", runId: R }, intent: document.intent });
  });

  it("lets a refused read reach the seed-failed screen with the server's sentence", async () => {
    wfetchMock.mockResolvedValue(new Response(JSON.stringify({ error: "That template does not exist.", reason: "template_not_found" }), { status: 404, headers: { "content-type": "application/json" } }));
    await expect(seedLoaders.template({ id: T, revision: 1 }, signal)).rejects.toBeInstanceOf(HttpError);
  });
});
