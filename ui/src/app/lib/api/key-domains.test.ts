/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";

const wfetchMock = vi.fn();
vi.mock("./core", async (orig) => ({ ...(await orig<typeof import("./core")>()), wfetch: (...a: unknown[]) => wfetchMock(...a) }));

import { HttpError } from "./core";
import { keyDomains } from "./key-domains";

const reply = (status: number, body = "") => new Response(body || null, { status });

beforeEach(() => wfetchMock.mockReset());

describe("key-domain writes", () => {
  it("a 200 or 201 set is applied; a 202 is held, never reported as applied", async () => {
    wfetchMock.mockResolvedValueOnce(reply(201, "{}"));
    expect(await keyDomains.set("user", "ann@corp.example", "finance")).toBe("applied");
    wfetchMock.mockResolvedValueOnce(reply(202, "{}"));
    expect(await keyDomains.set("group", "eng", "finance")).toBe("pending");
  });

  it("a 204 delete is applied; a 202 is held", async () => {
    wfetchMock.mockResolvedValueOnce(reply(204));
    expect(await keyDomains.remove("user", "ann")).toBe("applied");
    wfetchMock.mockResolvedValueOnce(reply(202, "{}"));
    expect(await keyDomains.remove("user", "ann")).toBe("pending");
  });

  it("encodes the subject into the path, and names everyone as 'all'", async () => {
    wfetchMock.mockResolvedValue(reply(200, "{}"));
    await keyDomains.set("user", "ann@corp.example/x", "finance");
    expect(wfetchMock.mock.calls[0][0]).toBe("/key-domains/assignments/user/ann%40corp.example%2Fx");
    expect(JSON.parse(wfetchMock.mock.calls[0][1].body)).toEqual({ domain: "finance" });
    await keyDomains.set("all", "", "default");
    expect(wfetchMock.mock.calls[1][0]).toBe("/key-domains/assignments/all/all");
  });

  it("a refusal throws with the server's sentence", async () => {
    wfetchMock.mockResolvedValueOnce(new Response(JSON.stringify({ error: "not declared" }), { status: 422 }));
    await expect(keyDomains.set("user", "ann", "ghost")).rejects.toBeInstanceOf(HttpError);
  });
});
