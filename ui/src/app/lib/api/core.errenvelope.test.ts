/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { asJson, errEnvelope, errText, HttpError } from "./core";

// The envelope's second field. `{"error":"…"}` is what every non-2xx carries;
// a few refusals a console surface ACTS on add `"reason":"<class>"` —
// `model_credential` and `git_credential` (#386), the create-time refusals
// the New Run rail answers with a sign-in/connect dialog. The class has to
// survive asJson's HttpError, and a body without one has to read as ""
// (never undefined) so callers compare with === and nothing else changes for
// the ten errText callers. `org` (#386, review finding F1) is the same shape,
// carried only by the git_credential envelope.
describe("errEnvelope / asJson — the envelope's machine-readable reason", () => {
  it('{"error":"x","reason":"model_credential"} -> an HttpError carrying both', async () => {
    const res = new Response(JSON.stringify({ error: "x", reason: "model_credential" }), { status: 422 });
    const err = await asJson(res).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(HttpError);
    expect((err as HttpError).status).toBe(422);
    expect((err as HttpError).message).toBe("x");
    expect((err as HttpError).reason).toBe("model_credential");
  });

  it('{"error":"x"} -> reason "" (the envelope every other refusal sends)', async () => {
    const res = new Response(JSON.stringify({ error: "x" }), { status: 422 });
    const err = (await asJson(res).catch((e: unknown) => e)) as HttpError;
    expect(err.message).toBe("x");
    expect(err.reason).toBe("");
  });

  it("a non-string reason is ignored, never rendered", async () => {
    const res = new Response(JSON.stringify({ error: "x", reason: 42 }), { status: 422 });
    expect(await errEnvelope(res)).toEqual({ message: "x", reason: "", org: "", provider: "" });
  });

  it("a raw (non-envelope) body carries no reason", async () => {
    const res = new Response("plain", { status: 500 });
    expect(await errEnvelope(res)).toEqual({ message: "plain", reason: "", org: "", provider: "" });
  });

  it('{"error":"…","reason":"git_credential","org":"…"} -> an HttpError carrying the org', async () => {
    // ticket: F1
    const res = new Response(
      JSON.stringify({ error: "not connected", reason: "git_credential", org: "https://dev.azure.com/contoso" }),
      { status: 422 },
    );
    const err = (await asJson(res).catch((e: unknown) => e)) as HttpError;
    expect(err.reason).toBe("git_credential");
    expect(err.org).toBe("https://dev.azure.com/contoso");
  });

  it("a non-string org is ignored, never rendered", async () => {
    const res = new Response(JSON.stringify({ error: "x", org: 42 }), { status: 422 });
    expect(await errEnvelope(res)).toEqual({ message: "x", reason: "", org: "", provider: "" });
  });

  it('{"error":"…","provider":"corp"} -> an HttpError naming the refused provider (#532, #543)', async () => {
    const res = new Response(
      JSON.stringify({ error: "refused", reason: "model_credential", provider: "corp", kind: "custom_endpoint" }),
      { status: 422 },
    );
    const err = (await asJson(res).catch((e: unknown) => e)) as HttpError;
    expect(err.provider).toBe("corp");
    expect(new HttpError(422, "x").provider).toBe("");
  });

  it("errText is the envelope's message, byte for byte", async () => {
    const res = new Response(JSON.stringify({ error: "x", reason: "model_credential" }), { status: 422 });
    expect(await errText(res)).toBe("x");
  });

  it("an HttpError built without a reason reads \"\"", () => {
    expect(new HttpError(404, "gone").reason).toBe("");
  });
});
