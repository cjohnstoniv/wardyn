/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { errText } from "./core";

// F6-F7 — errText is the ONE parser for a non-2xx body across every API
// module. Readability/DoS, not injection (React escapes the result either
// way): a server that answers with an oversized or HTML body (a proxy's
// error page, a misrouted request hitting a web server instead of wardynd)
// must not have that whole body land in a toast — fall back to the response's
// statusText instead. The JSON `{"error":"…"}` envelope is unaffected: a real
// API error message is always small and is extracted before this guard runs.
describe("errText — size/HTML guard on the raw-body fallback", () => {
  it('extracts the JSON envelope\'s error field: {"error":"x"} -> "x"', async () => {
    const res = new Response(JSON.stringify({ error: "x" }), { status: 400 });
    expect(await errText(res)).toBe("x");
  });

  it('a small plain-text body is returned verbatim: "plain" -> "plain"', async () => {
    const res = new Response("plain", { status: 500 });
    expect(await errText(res)).toBe("plain");
  });

  it("a 20 KB HTML body falls back to statusText, never rendered raw", async () => {
    const html = "<html><body>" + "x".repeat(20_000) + "</body></html>";
    const res = new Response(html, { status: 502, statusText: "Bad Gateway" });
    expect(await errText(res)).toBe("Bad Gateway");
  });

  it("a body over 500 chars (non-HTML) also falls back to statusText", async () => {
    const long = "e".repeat(501);
    const res = new Response(long, { status: 500, statusText: "Internal Server Error" });
    expect(await errText(res)).toBe("Internal Server Error");
  });

  it("a body at exactly 500 chars is still returned verbatim (boundary)", async () => {
    const body = "e".repeat(500);
    const res = new Response(body, { status: 500 });
    expect(await errText(res)).toBe(body);
  });

  it("a leading-whitespace HTML body is still caught by the </ guard", async () => {
    const res = new Response("  <!DOCTYPE html><title>oops</title>", { status: 404, statusText: "Not Found" });
    expect(await errText(res)).toBe("Not Found");
  });

  it("an empty body falls back to statusText (existing behaviour, untouched)", async () => {
    const res = new Response("", { status: 404, statusText: "Not Found" });
    expect(await errText(res)).toBe("Not Found");
  });
});
