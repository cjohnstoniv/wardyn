/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * THE "AUTHORIZED, NOT ISSUED" LADDER, AGAINST A REAL SANDBOX.
 *
 * The card's grant is an approval-gated git_pat on the PAT broker. The whole ladder is one run:
 *   1. git is held, and the approval strip under the terminal shows "Hand git a token for example.com";
 *   2. Approve: the proxy mints once (Audit: credential.mint);
 *   3. a direct ask for the token from inside the sandbox is refused with a 403;
 *   4. after the minted token has aged past ttl_seconds - 30, git asks again and is refused: the one mint is
 *      spent (Audit: egress.deny for example.com, and still one credential.mint).
 *
 * hosted ui-e2e runs `-runner none` (secrets-demos.spec.ts says why), so none of this can run there; this file
 * needs a real runner and the PAT broker on (its default). Assertions read Audit rows and the 403 only, never
 * git's own error wording, which is not pinned anywhere.
 *
 * Self-skips without WARDYN_TEST_K8S=1, same as every other file here. Needs WARDYN_WALK_ADMIN_TOKEN to store the
 * demo secret, and a Getting Started barrier that is ready (the card's Start stays disabled otherwise).
 */

import { expect, test, type Page } from "@playwright/test";
import { ADMIN_EMAIL, ADMIN_TOKEN, SANDBOX_UP, walkSignIn } from "./helpers";
import { termText, waitForShell } from "../terminal-text";

test.skip(process.env.WARDYN_TEST_K8S !== "1", "live cluster walk: set WARDYN_TEST_K8S=1");

const DEMO_ID = "authorized-not-issued";
const SECRET_NAME = "wardyn-demo-key";
const GIT_CMD = "git ls-remote https://example.com/wardyn-demo.git";
// The demo's ttl_seconds is 60 and the proxy re-mints only inside the last 30 s of it (git_broker.go:84), so a
// second ask 31 s after the approval reaches the broker, which refuses it as already_minted.
const AFTER_TTL_MS = 31_000;

type AuditRow = { action: string; outcome: string; target?: string; data?: Record<string, unknown> };

/** The run's own audit trail, filtered by the server on action (internal/api/audit.go). */
async function auditFor(page: Page, runId: string, action: string): Promise<AuditRow[]> {
  return page.evaluate(
    async ([rid, act]: [string, string]) => {
      const r = await fetch(`/api/v1/audit?run_id=${encodeURIComponent(rid)}&action=${encodeURIComponent(act)}&limit=200`, {
        credentials: "include",
      });
      if (!r.ok) throw new Error(`GET /audit: ${r.status}`);
      const body = (await r.json()) as AuditRow[] | { items?: AuditRow[] };
      return Array.isArray(body) ? body : (body.items ?? []);
    },
    [runId, action] as [string, string],
  );
}

async function typeLine(page: Page, line: string): Promise<void> {
  await page.locator(".xterm-screen").first().click();
  await page.keyboard.type(line);
  await page.keyboard.press("Enter");
}

test("an approved git token is minted once at the proxy, never handed to the sandbox, and refused a second time", async ({ page }) => {
  // The budget: launching the sandbox (SANDBOX_UP), up to 120 s of git held for the person's decision, the 31 s
  // wait for the minted token to age past ttl_seconds - 30, and the audit polls around them. The 30 s default
  // would fail this test before the sandbox even came up.
  test.setTimeout(SANDBOX_UP + 120_000 + AFTER_TTL_MS + 120_000);
  expect(ADMIN_TOKEN, "WARDYN_WALK_ADMIN_TOKEN is unset: it stores the demo secret").not.toBe("");

  await page.addInitScript(() => {
    try {
      localStorage.setItem("wardyn-onboarding-seen", "1");
    } catch {
      /* private mode: ignore */
    }
  });
  await walkSignIn(page, ADMIN_EMAIL);

  // 1. Store the secret the card needs. Any value works; the sandbox never sees it.
  const put = await page.request.put(`/api/v1/secrets/${encodeURIComponent(SECRET_NAME)}`, {
    headers: { Authorization: `Bearer ${ADMIN_TOKEN}`, "Content-Type": "application/json" },
    data: { value: "walk-demo-key-value" },
  });
  expect(put.ok(), `PUT /secrets: ${await put.text()}`).toBeTruthy();

  try {
    // 2. Launch the card as an operator and attach.
    await page.goto(`/setup?step=${DEMO_ID}`);
    const card = page.getByTestId(`demo-card-${DEMO_ID}`);
    await expect(card).toBeVisible({ timeout: 30_000 });
    await page.getByTestId(`demo-start-${DEMO_ID}`).click();
    const screen = card.locator(".xterm-screen").first();
    await expect(screen).toBeVisible({ timeout: SANDBOX_UP });
    await waitForShell(page, screen);

    const stored = await page.evaluate(() => localStorage.getItem("wardyn-demo-runs"));
    const runId = (JSON.parse(stored ?? "{}") as Record<string, string>)[DEMO_ID];
    expect(runId, "the card stores the run it started").toBeTruthy();

    // 3. Step 1: git is held, and the strip row appears.
    await typeLine(page, GIT_CMD);
    const row = card.getByTestId("live-approval-row").filter({ hasText: "Hand git a token for example.com" });
    await expect(row).toBeVisible({ timeout: 60_000 });

    // 4. Approve it. The clock for the token's age starts here.
    await row.getByRole("button", { name: "Approve" }).click();
    const approvedAt = Date.now();

    // 5. The proxy minted the token once.
    await expect
      .poll(async () => (await auditFor(page, runId, "credential.mint")).filter((e) => e.outcome === "success").length, {
        timeout: 120_000,
      })
      .toBe(1);

    // 6. Step 3: ask the broker for the token directly. The card's own curl, with its status printed after the body.
    const mintLine = card.getByTestId("demo-steps").locator("li").nth(2);
    await expect(mintLine).not.toContainText("{grant_id}", { timeout: 30_000 });
    const curl = (await mintLine.innerText()).match(/curl [^\n]*credentials\/mint"/)?.[0];
    expect(curl, "step 3's pill carries the mint command").toBeTruthy();
    await typeLine(page, `${curl} -w ' http=%{http_code}'`);
    await expect.poll(async () => termText(screen).catch(() => ""), { timeout: 30_000 }).toContain("http=403");

    // 7. Step 4, once the minted token is past ttl_seconds - 30: refused, and no second mint.
    await page.waitForTimeout(Math.max(0, approvedAt + AFTER_TTL_MS - Date.now()));
    const denies = async () =>
      (await auditFor(page, runId, "egress.deny")).filter((e) => `${e.target ?? ""} ${JSON.stringify(e.data ?? {})}`.includes("example.com")).length;
    const deniedBefore = await denies();
    await typeLine(page, GIT_CMD);
    await expect.poll(denies, { timeout: 60_000 }).toBeGreaterThan(deniedBefore);
    expect((await auditFor(page, runId, "credential.mint")).filter((e) => e.outcome === "success")).toHaveLength(1);
  } finally {
    await page.request
      .delete(`/api/v1/secrets/${encodeURIComponent(SECRET_NAME)}`, { headers: { Authorization: `Bearer ${ADMIN_TOKEN}` } })
      .catch(() => {});
  }
});
