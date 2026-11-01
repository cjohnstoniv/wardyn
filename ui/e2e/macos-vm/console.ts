/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Shared inputs and results for the macOS VM check's console rows (playwright.macos-vm.config.ts). These run
// against a REAL stack and a real macOS VM run, never a stub. The admin token arrives as a FILE PATH and is
// never printed. Each spec writes one JSON file per check to WARDYN_MACVM_CONSOLE_OUT for the row summary:
//   { check, status: PASS | FAIL | NOT_RUN, reason, ms, finished_at }
// where `check` is the spec's file name without ".spec.ts". `reason` is set for NOT_RUN and FAIL; it is cut to
// one short line with the console address, run id and home paths replaced.

import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { test as base, expect, type APIRequestContext } from "@playwright/test";
import { TOKEN_KEY } from "../fixtures";

export { expect };

const REQUIRED = ["WARDYN_MACVM_CONSOLE_URL", "WARDYN_MACVM_CONSOLE_RUN_ID", "WARDYN_MACVM_CONSOLE_TOKEN_FILE"];

export const outDir = process.env.WARDYN_MACVM_CONSOLE_OUT || path.join(os.tmpdir(), "wardyn-macvm-console");

/** The optional marker a spec looks for, or "" when the check script set none. */
export const marker = (name: "WARDYN_MACVM_CONSOLE_FILE_NAME" | "WARDYN_MACVM_CONSOLE_OUTPUT_MARK"): string => process.env[name] || "";

interface Inputs {
  base: string;
  runId: string;
  token: string;
}

/** The inputs, or why there are none. The reason names variables, never values. */
function inputs(): { ok: Inputs } | { why: string } {
  const unset = REQUIRED.filter((n) => !process.env[n]);
  if (unset.length) return { why: `console rows need ${REQUIRED.join(", ")}; unset: ${unset.join(", ")}` };
  let token = "";
  try {
    token = fs.readFileSync(process.env.WARDYN_MACVM_CONSOLE_TOKEN_FILE as string, "utf8").trim();
  } catch {
    return { why: "WARDYN_MACVM_CONSOLE_TOKEN_FILE is not readable" };
  }
  if (!token) return { why: "WARDYN_MACVM_CONSOLE_TOKEN_FILE is empty" };
  return { ok: { base: (process.env.WARDYN_MACVM_CONSOLE_URL as string).replace(/\/+$/, "") + "/", runId: process.env.WARDYN_MACVM_CONSOLE_RUN_ID as string, token } };
}

/** One short line, with what could name this stack taken out. */
export function scrub(text: string): string {
  const { base, runId, token } = (inputs() as { ok?: Inputs }).ok ?? { base: "", runId: "", token: "" };
  let s = text.replace(/\u001b\[[0-9;]*m/g, "").split("\n").find((l) => l.trim()) ?? "";
  for (const [secret, label] of [[token, "<token>"], [base.replace(/\/$/, ""), "<console>"], [runId, "<run>"], [os.homedir(), "~"]] as const) {
    if (secret) s = s.split(secret).join(label);
  }
  return s.trim().slice(0, 200);
}

export interface Site {
  /** The console address of a path under the base, base path included. */
  url: (rel: string) => string;
  runId: string;
  /** GET an API path with the admin token; the body as JSON. */
  api: <T>(rel: string) => Promise<T>;
}

const STATUS = { passed: "PASS", skipped: "NOT_RUN" } as const;

export const test = base.extend<{ site: Site; record: void }>({
  // Signs the page in with the admin token before the first navigation, as ui/e2e/fixtures.ts does.
  site: async ({ page }, use, testInfo) => {
    const got = inputs();
    testInfo.skip("why" in got, "why" in got ? got.why : "");
    const { base: root, runId, token } = (got as { ok: Inputs }).ok;
    await page.addInitScript(([key, tok]) => window.localStorage.setItem(key, tok), [TOKEN_KEY, token]);
    const request: APIRequestContext = page.request;
    await use({
      url: (rel) => root + rel.replace(/^\/+/, ""),
      runId,
      api: async <T>(rel: string) => {
        const res = await request.get(root + rel.replace(/^\/+/, ""), { headers: { Authorization: `Bearer ${token}` } });
        expect(res.status(), `GET ${rel.split("?")[0].replace(runId, "<run>")}`).toBe(200);
        return (await res.json()) as T;
      },
    });
  },
  record: [
    async ({}, use, testInfo) => {
      const t0 = Date.now();
      await use();
      const status = testInfo.status === "passed" || testInfo.status === "skipped" ? STATUS[testInfo.status] : "FAIL";
      const skip = testInfo.annotations.find((a) => a.type === "skip");
      const reason = status === "NOT_RUN" ? (skip?.description ?? "skipped") : status === "FAIL" ? (testInfo.error?.message ?? "failed") : "";
      fs.mkdirSync(outDir, { recursive: true });
      const check = path.basename(testInfo.file).replace(/\.spec\.ts$/, "");
      fs.writeFileSync(
        path.join(outDir, `console-${check}.json`),
        JSON.stringify({ check, status, reason: scrub(reason), ms: Date.now() - t0, finished_at: new Date().toISOString() }) + "\n",
      );
    },
    { auto: true },
  ],
});
