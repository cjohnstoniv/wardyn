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
