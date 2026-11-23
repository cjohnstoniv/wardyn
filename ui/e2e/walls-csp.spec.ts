/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { Page, Request } from "@playwright/test";
import { expect, gotoConsole, test } from "./fixtures";

// The walls view's 3D stack under the console's own Content-Security-Policy. The e2e build exposes a probe that
// loads the stack in its own chunk and draws one frame off screen (webgl-probe.ts). The shared fixture fails a test
// on any CSP violation or uncaught page error, so a stack that needed eval, a blob worker or an image URL fails here.

type Spied = { __wallsFingerprint: string[] };

// Records any read of the GPU's identity: the debug renderer extension, or the unmasked vendor and renderer values.
// Installed before the page's own scripts, on both WebGL context types.
async function installFingerprintSpy(page: Page): Promise<void> {
  await page.addInitScript(() => {
    const seen: string[] = [];
    (window as unknown as Spied).__wallsFingerprint = seen;
    const protos = [WebGLRenderingContext.prototype, WebGL2RenderingContext.prototype] as unknown as Array<
      Record<"getExtension" | "getParameter", (...args: unknown[]) => unknown>
    >;
    for (const proto of protos) {
      const getExtension = proto.getExtension;
      const getParameter = proto.getParameter;
      proto.getExtension = function (this: unknown, ...args: unknown[]) {
        if (/debug_renderer_info/i.test(String(args[0]))) seen.push(`getExtension(${String(args[0])})`);
        return getExtension.apply(this, args);
      };
      proto.getParameter = function (this: unknown, ...args: unknown[]) {
        // 0x9245 and 0x9246 are UNMASKED_VENDOR_WEBGL and UNMASKED_RENDERER_WEBGL.
        if (args[0] === 0x9245 || args[0] === 0x9246) seen.push(`getParameter(${String(args[0])})`);
        return getParameter.apply(this, args);
      };
    }
  });
}

function directives(header: string): Map<string, string[]> {
  const out = new Map<string, string[]>();
  for (const part of header.split(";")) {
    const [name, ...tokens] = part.trim().split(/\s+/);
    if (name) out.set(name.toLowerCase(), tokens);
  }
  return out;
}

test.describe("walls: the 3D stack under the console's CSP", () => {
  test("loads lazily and draws with WebGL 2 under the console's policy, with no eval", async ({ page }) => {
    await installFingerprintSpy(page);
    const doc = page.waitForResponse((r) => r.request().resourceType() === "document");
    await gotoConsole(page, "admin");

    // The policy is the console's own: no JavaScript eval, and workers and images fall back to 'self'.
    const csp = directives((await doc).headers()["content-security-policy"] ?? "");
    expect(csp.get("script-src")).toEqual(["'self'", "'wasm-unsafe-eval'"]);
    expect(csp.get("default-src")).toEqual(["'self'"]);
    for (const name of ["worker-src", "child-src", "img-src"]) {
      expect(csp.has(name), `${name} would widen default-src 'self'`).toBe(false);
    }
    expect([...csp.values()].flat()).not.toContain("'unsafe-eval'");

    const hasProbe = await page.evaluate(
      () => typeof (window as unknown as { __wardynWallsProbe?: unknown }).__wardynWallsProbe === "function",
    );
    expect(hasProbe, "no probe on window: the backend is not serving the e2e build").toBe(true);

    const origin = new URL(page.url()).origin;
    const requests: string[] = [];
    const onRequest = (request: Request) => requests.push(request.url());
    let workers = 0;
    page.on("request", onRequest);
    page.on("worker", () => {
      workers += 1;
    });
    // Started from a page task: code that page.evaluate runs synchronously is a DevTools evaluation, which Chromium
    // exempts from the policy's eval block, so a probe started there would not be held to the policy.
    const result = await page.evaluate(
      () =>
        new Promise<unknown>((resolve) => {
          setTimeout(() => {
            try {
              (window as unknown as { __wardynWallsProbe: () => Promise<unknown> })
                .__wardynWallsProbe()
                .then(resolve, (err: unknown) => resolve(`error: ${String(err)}`));
            } catch (err) {
              resolve(`error: ${String(err)}`);
            }
          }, 0);
        }),
    );
    page.off("request", onRequest);

    expect(result).toEqual({ webgl2: true, drawn: true, steps: ["shader", "controls", "bloom", "fiber"] });
    const offOrigin = requests.filter((u) => /^(?:blob|data):/.test(u) || new URL(u).origin !== origin);
    expect(offOrigin, "requests that left the console's origin").toEqual([]);
    const chunks = requests.filter((u) => /\/assets\/[^/]+\.js$/.test(new URL(u).pathname));
    expect(chunks.length, "the probe loaded no chunk of its own, so the stack was already on the page").toBeGreaterThan(0);
    expect(workers, "the 3D stack started a worker").toBe(0);
    expect(await page.evaluate(() => (window as unknown as Spied).__wallsFingerprint)).toEqual([]);
  });

  test("the same policy refuses eval in this browser, and the spy sees a direct read", async ({ browser, baseURL }) => {
    // A context of its own: the refused eval below raises the CSP violation this check expects, and the shared
    // fixture would fail any test whose page reports one.
    const context = await browser.newContext({ baseURL });
    try {
      const page = await context.newPage();
      await installFingerprintSpy(page);
      await page.goto("/");
      await page.evaluate(() => {
        const log: Array<{ effectiveDirective: string; blockedURI: string }> = [];
        (window as unknown as { __wallsViolations: typeof log }).__wallsViolations = log;
        document.addEventListener("securitypolicyviolation", (e) => {
          log.push({ effectiveDirective: e.effectiveDirective, blockedURI: e.blockedURI });
        });
      });
      // Run from a page task: code that page.evaluate runs synchronously is a DevTools evaluation, which Chromium
      // exempts from the policy's eval block.
      const evaluated = await page.evaluate(
        () =>
          new Promise<string>((resolve) => {
            setTimeout(() => {
              try {
                resolve(String(new Function("return 'allowed'")()));
              } catch (err) {
                resolve(err instanceof EvalError ? "refused" : String(err));
              }
            }, 0);
          }),
      );
      expect(evaluated, "the console's CSP allowed eval, so the probe above proves nothing").toBe("refused");
      const violations = await page.evaluate(
        () =>
          (window as unknown as { __wallsViolations: Array<{ effectiveDirective: string; blockedURI: string }> })
            .__wallsViolations,
      );
      expect(violations, "the refusal was not a policy violation").toHaveLength(1);
      expect(violations[0].blockedURI).toBe("eval");
      const seen = await page.evaluate(() => {
        document.createElement("canvas").getContext("webgl2")?.getExtension("WEBGL_debug_renderer_info");
        return (window as unknown as Spied).__wallsFingerprint;
      });
      expect(seen.length, "the fingerprint spy missed a direct read").toBeGreaterThan(0);
    } finally {
      await context.close();
    }
  });
});
