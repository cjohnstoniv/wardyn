/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { harnessAuth } from "./harness-auth";
import { HttpError } from "./core";

describe("harness credential client", () => {
  let fetchMock: ReturnType<typeof vi.fn>;
  beforeEach(() => {
    fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
  });
  afterEach(() => vi.unstubAllGlobals());

  it.each([
    [undefined, undefined, { provider: "anthropic", sso_start_url: "" }],
    ["aws", "https://example.awsapps.com/start", { provider: "aws", sso_start_url: "https://example.awsapps.com/start" }],
  ])("launches %s with the login payload and returns the pending run ID", async (provider, startURL, input) => {
    fetchMock.mockResolvedValueOnce(new Response(JSON.stringify({ run_id: "login-1", state: "PENDING" })));
    await expect(harnessAuth.harnessLogin(provider, startURL)).resolves.toBe("login-1");
    expect(fetchMock).toHaveBeenCalledWith("/api/v1/setup/harness-login", expect.objectContaining({
      method: "POST", body: JSON.stringify(input),
    }));
  });

  it("pastes the token in the body and encodes the provider as one path segment", async () => {
    fetchMock.mockResolvedValueOnce(new Response("{}"));
    await expect(harnessAuth.harnessCredentialPaste("provider/name ?", "synthetic-setup-token")).resolves.toBeUndefined();
    expect(fetchMock).toHaveBeenCalledWith("/api/v1/setup/harness-credential/provider%2Fname%20%3F", expect.objectContaining({
      method: "PUT", body: JSON.stringify({ token: "synthetic-setup-token" }),
    }));
  });

  it("disconnects the encoded provider", async () => {
    fetchMock.mockResolvedValueOnce(new Response("{}"));
    await expect(harnessAuth.harnessDisconnect("provider/name ?")).resolves.toBeUndefined();
    expect(fetchMock).toHaveBeenCalledWith("/api/v1/setup/harness-credential/provider%2Fname%20%3F", expect.objectContaining({
      method: "DELETE",
    }));
  });

  it.each([
    ["login", () => harnessAuth.harnessLogin()],
    ["paste", () => harnessAuth.harnessCredentialPaste("anthropic", "synthetic-setup-token")],
    ["disconnect", () => harnessAuth.harnessDisconnect("anthropic")],
  ] as const)("%s preserves a server refusal", async (_name, request) => {
    fetchMock.mockResolvedValueOnce(new Response(JSON.stringify({ error: "credential refused", reason: "model_credential" }), { status: 403 }));
    const error: unknown = await request().catch((e: unknown) => e);
    expect(error).toBeInstanceOf(HttpError);
    expect(error).toMatchObject({ status: 403, message: "credential refused", reason: "model_credential" });
  });

  it("does not treat a non-JSON disconnect failure as success", async () => {
    fetchMock.mockResolvedValueOnce(new Response("upstream unavailable", { status: 502 }));
    await expect(harnessAuth.harnessDisconnect("anthropic")).rejects.toMatchObject({ status: 502, message: "upstream unavailable" });
  });
});
