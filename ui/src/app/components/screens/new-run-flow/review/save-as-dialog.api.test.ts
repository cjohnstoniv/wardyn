/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it, vi } from "vitest";
import { buildSaveRequest, ONLY_ME } from "./save-as-dialog.model";
import { saveTemplate, TemplateSaveError } from "./save-as-dialog.api";

afterEach(() => vi.unstubAllGlobals());

const request = buildSaveRequest({ kind: "template", audience: { value: ONLY_ME, scope: "person" }, name: "n", intent: {}, coverage: "partial" });
const reply = (status: number, body: string) => vi.fn().mockResolvedValue(new Response(body, { status, statusText: "Conflict" }));

describe("saveTemplate", () => {
  it("POSTs the request to /templates and returns the stored template", async () => {
    const fetchMock = reply(201, JSON.stringify({ id: "t1" }));
    vi.stubGlobal("fetch", fetchMock);
    await expect(saveTemplate(request)).resolves.toEqual({ id: "t1" });
    const [url, init] = fetchMock.mock.calls[0];
    expect(String(url)).toMatch(/\/templates$/);
    expect(init.method).toBe("POST");
    expect(JSON.parse(init.body as string)).toEqual(request);
  });

  it("keeps the refusal's reason and every diagnostic", async () => {
    const body = { error: "sentence", reason: "template_name_taken", diagnostics: [{ path: "name", reason: "template_name_taken", message: "sentence" }] };
    vi.stubGlobal("fetch", reply(409, JSON.stringify(body)));
    const err = await saveTemplate(request).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(TemplateSaveError);
    expect(err).toMatchObject({ status: 409, message: "sentence", reason: "template_name_taken", diagnostics: body.diagnostics });
  });

  it("falls back to the status text when the body is not the refusal envelope", async () => {
    vi.stubGlobal("fetch", reply(502, "<html>"));
    await expect(saveTemplate(request)).rejects.toMatchObject({ status: 502, message: "Conflict", reason: "", diagnostics: [] });
  });
});
