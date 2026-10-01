/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it, vi } from "vitest";
import { adoPat } from "./ado-pat";

afterEach(() => vi.restoreAllMocks());

const answer = (status: number) => vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(status === 204 ? null : "no", { status }));

describe("adoPat.disconnect", () => {
  it("resolves on 204", async () => {
    answer(204);
    await expect(adoPat.disconnect()).resolves.toBeUndefined();
  });
  it("is an error on 404: a daemon without the route must not look like a disconnect that worked", async () => {
    answer(404);
    await expect(adoPat.disconnect()).rejects.toMatchObject({ status: 404 });
  });
  it("is an error on any other failure", async () => {
    answer(500);
    await expect(adoPat.disconnect()).rejects.toMatchObject({ status: 500 });
  });
});

// #1488: the person's own Remove. Method, path and encoding are the contract;
// the request carries no token and no subject (the caller is the subject).
describe("adoPat.removeOwnToken", () => {
  it("sends DELETE /me/scm/azure-devops/token?org=<encoded address>, with no body", async () => {
    const spy = answer(204);
    await expect(adoPat.removeOwnToken("https://dev.azure.com/my org&x=1")).resolves.toBeUndefined();
    expect(spy).toHaveBeenCalledTimes(1);
    const [url, init] = spy.mock.calls[0];
    expect(String(url)).toMatch(/\/api\/v1\/me\/scm\/azure-devops\/token\?org=https%3A%2F%2Fdev\.azure\.com%2Fmy%20org%26x%3D1$/);
    expect(init?.method).toBe("DELETE");
    expect(init?.body ?? undefined).toBeUndefined();
  });
  it("throws an HttpError on 404: an older daemon with no route must not look like a removal", async () => {
    answer(404);
    await expect(adoPat.removeOwnToken("https://dev.azure.com/o")).rejects.toMatchObject({ status: 404 });
  });
  it("throws on any other failure", async () => {
    answer(500);
    await expect(adoPat.removeOwnToken("https://dev.azure.com/o")).rejects.toMatchObject({ status: 500 });
  });
});
