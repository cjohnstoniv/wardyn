/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { beforeEach, describe, expect, it, vi } from "vitest";
import { templatesApi } from "./templates-api";

const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const fetchMock = vi.fn();

beforeEach(() => {
  fetchMock.mockReset();
  vi.stubGlobal("fetch", fetchMock);
});

const call = () => {
  const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
  return { url, method: init.method ?? "GET", body: init.body };
};

describe("templates api", () => {
  it("reads the list and one revision", async () => {
    fetchMock.mockResolvedValueOnce(json(200, { templates: [], publish: { org: false, groups: [], groups_unverified: false } }));
    await templatesApi.list();
    expect(call()).toMatchObject({ url: expect.stringMatching(/\/api\/v1\/templates$/), method: "GET" });

    fetchMock.mockReset();
    fetchMock.mockResolvedValueOnce(json(200, {}));
    await templatesApi.getRevision("a/b", 4);
    expect(call().url).toMatch(/\/api\/v1\/templates\/a%2Fb\/revisions\/4$/);
  });

  it("updates with PUT, deletes with DELETE and publishes by copying to the organisation", async () => {
    fetchMock.mockResolvedValueOnce(json(200, {}));
    await templatesApi.update("t1", { scope: "person", name: "n", document: { api_version: "wardyn/v1", kind: "RunTemplate", intent: {} }, expected_revision: 1 });
    expect(call()).toMatchObject({ url: expect.stringMatching(/\/templates\/t1$/), method: "PUT" });

    fetchMock.mockReset();
    fetchMock.mockResolvedValueOnce(new Response(null, { status: 204 }));
    await templatesApi.remove("t1");
    expect(call()).toMatchObject({ url: expect.stringMatching(/\/templates\/t1$/), method: "DELETE" });

    fetchMock.mockReset();
    fetchMock.mockResolvedValueOnce(json(201, {}));
    await templatesApi.publishToOrg("t1");
    expect(call()).toMatchObject({ url: expect.stringMatching(/\/templates\/t1\/copy$/), method: "POST" });
    expect(JSON.parse(call().body as string)).toEqual({ scope: "org" });
  });

  it("a refusal carries the server's reason and sentence", async () => {
    fetchMock.mockResolvedValueOnce(json(409, { error: "taken", reason: "template_name_taken" }));
    await expect(templatesApi.remove("t1")).rejects.toMatchObject({ status: 409, reason: "template_name_taken", message: "taken" });
  });
});
