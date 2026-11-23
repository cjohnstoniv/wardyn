/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it, vi } from "vitest";
import { cliSignIn } from "./cli-sign-in";

afterEach(() => vi.restoreAllMocks());

const approved = () =>
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    new Response(JSON.stringify({ code: "k", code_expires_in: 60 }), { status: 200, headers: { "Content-Type": "application/json" } }),
  );

describe("cliSignIn.approve", () => {
  it("sends no body and no Content-Type when the page did not ask for the code", async () => {
    const spy = approved();
    await cliSignIn.approve("id-1");
    const init = spy.mock.calls[0][1] as RequestInit;
    expect(String(spy.mock.calls[0][0])).toContain("/me/cli-sign-ins/id-1/approve");
    expect(init.method).toBe("POST");
    expect(init.body).toBeUndefined();
    expect(new Headers(init.headers).has("Content-Type")).toBe(false);
  });
  it("sends the typed code as user_code when the page asked for it", async () => {
    const spy = approved();
    await cliSignIn.approve("id-1", "kq7m2xjd");
    const init = spy.mock.calls[0][1] as RequestInit;
    expect(init.body).toBe(JSON.stringify({ user_code: "kq7m2xjd" }));
    expect(new Headers(init.headers).get("Content-Type")).toBe("application/json");
  });
});
