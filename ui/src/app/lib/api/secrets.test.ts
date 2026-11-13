/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { secrets } from "./secrets";
import { setExpectedPrincipal } from "./core";

// A secret's name is resolved in the namespace of whoever the session names when the
// request arrives, so a delete names the person whose row was confirmed; the server
// refuses it (412) for anyone else.
describe("deleteSecret names a person", () => {
  let fetchMock: ReturnType<typeof vi.fn>;
  const sentAs = () => new Headers(fetchMock.mock.calls[0][1].headers).get("X-Wardyn-Expected-Principal");

  beforeEach(() => {
    fetchMock = vi.fn().mockResolvedValue(new Response(null, { status: 204 }));
    vi.stubGlobal("fetch", fetchMock);
  });
  afterEach(() => {
    vi.unstubAllGlobals();
    setExpectedPrincipal("");
  });

  it("names the person whose row was confirmed, not whoever this tab confirmed since", async () => {
    setExpectedPrincipal("bob@example.com");
    await secrets.deleteSecret("npm-token", "alice@example.com");
    expect(sentAs()).toBe(encodeURIComponent("alice@example.com"));
  });

  it("with no person given, names the person this tab confirmed, as setSecret does", async () => {
    setExpectedPrincipal("alice@example.com");
    await secrets.deleteSecret("npm-token");
    expect(sentAs()).toBe(encodeURIComponent("alice@example.com"));
  });

  it("a delete chosen before this tab knew who was signed in names the person it has confirmed by the time it is sent", async () => {
    setExpectedPrincipal("alice@example.com");
    await secrets.deleteSecret("npm-token", "");
    expect(sentAs()).toBe(encodeURIComponent("alice@example.com"));
  });

  it("a delete or write for a namespace goes to the route that names it, never the role-placed /secrets", async () => {
    const path = (i: number) => new URL(String(fetchMock.mock.calls[i][0]), "http://x").pathname;
    await secrets.deleteSecret("npm-token", "alice@example.com", "personal");
    await secrets.deleteSecret("npm-token", "alice@example.com", "shared");
    await secrets.setSecret("npm-token", "npm-value-123456", { scope: "personal" });
    fetchMock.mockResolvedValue(new Response(JSON.stringify({ names: ["npm-token"] }), { status: 200, headers: { "Content-Type": "application/json" } }));
    expect(await secrets.listScoped("shared")).toEqual(["npm-token"]);
    expect([0, 1, 2, 3].map(path)).toEqual([
      "/api/v1/me/secrets/npm-token",
      "/api/v1/shared-secrets/npm-token",
      "/api/v1/me/secrets/npm-token",
      "/api/v1/shared-secrets",
    ]);
  });

  it("a create-only write sends If-None-Match: *, and a name already stored rejects with its reason, sent once", async () => {
    fetchMock.mockResolvedValue(
      new Response(JSON.stringify({ error: "a secret with this name already exists", reason: "secret_exists" }), {
        status: 409,
        headers: { "Content-Type": "application/json" },
      }),
    );
    await expect(secrets.setSecret("npm-token", "npm-value-123456", { createOnly: true })).rejects.toMatchObject({ status: 409, reason: "secret_exists" });
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(new Headers(fetchMock.mock.calls[0][1].headers).get("If-None-Match")).toBe("*");
    fetchMock.mockResolvedValue(new Response(null, { status: 204 }));
    await secrets.setSecret("npm-token", "npm-value-123456");
    expect(new Headers(fetchMock.mock.calls[1][1].headers).get("If-None-Match")).toBeNull();
  });

  it("a 412 rejects with the server's sentence and is not sent again", async () => {
    const sentence = "This page was opened as a different person than the one now signed in — reload and retry";
    fetchMock.mockResolvedValue(
      new Response(JSON.stringify({ error: sentence, reason: "expected_principal_mismatch" }), {
        status: 412,
        headers: { "Content-Type": "application/json" },
      }),
    );
    await expect(secrets.deleteSecret("npm-token", "alice@example.com")).rejects.toMatchObject({ status: 412, message: sentence });
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });
});
