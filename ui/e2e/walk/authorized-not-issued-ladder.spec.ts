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
// The demo's ttl_seconds is 60 and the proxy reuses the minted token until 30 s before it ends (git_broker.go:84),
// so a second ask 35 s after the mint reaches the broker, which refuses it as already_minted. The 5 s over the
// 30 s margin keeps a second or so of clock skew between this host and the cluster out of the reuse window.
const AFTER_TTL_MS = 35_000;
// The rule_source the proxy stamps on the deny it writes when the token cannot be minted (pat_broker.go:38, :200-203).
// The vet arm writes the same rule (pat_broker.go:211, :251-259), so a reused token that fails the vet is not told
// apart by it; a reused token that fails the dial writes another rule (builtin:dial-failed), which this excludes.
const MINT_DENIED_RULE = "brokered:git-pat:denied";

type AuditRow = { action: string; outcome: string; time: string; target?: string; data?: Record<string, unknown> };

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
  // One budget for the whole ladder: launching the sandbox (SANDBOX_UP), up to 120 s of git held for the person's
  // decision, the wait until 35 s after the mint, plus sign-in, the terminal and the audit polls. Their own timeouts
  // add up to about 515 s beyond SANDBOX_UP, so this sets 10 minutes (600 s) beyond it.
  test.setTimeout(SANDBOX_UP + 10 * 60_000);
  expect(ADMIN_TOKEN, "WARDYN_WALK_ADMIN_TOKEN is unset: it stores the demo secret").not.toBe("");

  await page.addInitScript(() => {
    try {
      localStorage.setItem("wardyn-onboarding-seen", "1");
    } catch {
      /* private mode: ignore */
    }
  });
  await walkSignIn(page, ADMIN_EMAIL);

  // 1. The secret the card needs. A secret already stored (a film take, a hand setup) is used as it is: not
  // written over, and left in place at the end. Any value works for one this walk stores; the sandbox never sees it.
  // An unreadable listing fails the test here, before any write or delete on a state that is not known.
  const listed = await page.request.get("/api/v1/secrets", { headers: { Authorization: `Bearer ${ADMIN_TOKEN}` } });
  expect(listed.ok(), `GET /secrets: ${listed.status()} ${await listed.text()}`).toBeTruthy();
  const secretExisted = ((await listed.json()) as { names?: string[] }).names?.includes(SECRET_NAME) === true;
  if (!secretExisted) {
    const put = await page.request.put(`/api/v1/secrets/${encodeURIComponent(SECRET_NAME)}`, {
      headers: { Authorization: `Bearer ${ADMIN_TOKEN}`, "Content-Type": "application/json" },
      data: { value: "walk-demo-key-value" },
    });
    expect(put.ok(), `PUT /secrets: ${await put.text()}`).toBeTruthy();
  }

  let runId = "";
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
    runId = (JSON.parse(stored ?? "{}") as Record<string, string>)[DEMO_ID] ?? "";
    expect(runId, "the card stores the run it started").toBeTruthy();

    // 3. Step 1: git is held, and the strip row appears.
    await typeLine(page, GIT_CMD);
    const row = card.getByTestId("live-approval-row").filter({ hasText: "Hand git a token for example.com" });
    await expect(row).toBeVisible({ timeout: 60_000 });

    // 4. Approve it.
    await row.getByRole("button", { name: "Approve" }).click();

    // 5. The proxy minted the token once, on its next 2 s poll after the decision. The token's age runs from that
    // row's own time, not from the click.
    const mints = async () => (await auditFor(page, runId, "credential.mint")).filter((e) => e.outcome === "success");
    await expect.poll(async () => (await mints()).length, { timeout: 120_000 }).toBe(1);
    const mintedAt = Date.parse((await mints())[0].time);
    expect(Number.isNaN(mintedAt), "the mint row carries a time").toBe(false);

    // 6. Step 3: ask the broker for the token directly. The card's own curl, with its status printed after the body.
    const mintLine = card.getByTestId("demo-steps").locator("li").nth(2);
    await expect(mintLine).not.toContainText("{grant_id}", { timeout: 30_000 });
    const curl = (await mintLine.innerText()).match(/curl [^\n]*credentials\/mint"/)?.[0];
    expect(curl, "step 3's pill carries the mint command").toBeTruthy();
    await typeLine(page, `${curl} -w ' http=%{http_code}'`);
    await expect.poll(async () => termText(screen).catch(() => ""), { timeout: 30_000 }).toContain("http=403");

    // 7. Step 4, once the minted token is past ttl_seconds - 30: refused, and no second mint.
    await page.waitForTimeout(Math.max(0, mintedAt + AFTER_TTL_MS - Date.now()));
    const denies = async () =>
      (await auditFor(page, runId, "egress.deny")).filter(
        (e) => e.data?.rule_source === MINT_DENIED_RULE && `${e.target ?? ""} ${JSON.stringify(e.data)}`.includes("example.com"),
      ).length;
    const deniedBefore = await denies();
    await typeLine(page, GIT_CMD);
    await expect.poll(denies, { timeout: 60_000 }).toBeGreaterThan(deniedBefore);
    expect((await auditFor(page, runId, "credential.mint")).filter((e) => e.outcome === "success")).toHaveLength(1);
  } finally {
    const auth = { Authorization: `Bearer ${ADMIN_TOKEN}` };
    // End the run so the sandbox does not idle out its lease, then drop the secret this walk stored. A failed
    // cleanup is logged, never thrown: it must not mask the result of the ladder itself.
    const cleanup = async (what: string, call: Promise<{ ok(): boolean; status(): number }>) => {
      const res = await call.catch((e: unknown) => String(e));
      if (typeof res === "string") console.warn(`cleanup: ${what} failed: ${res}`);
      else if (!res.ok()) console.warn(`cleanup: ${what} answered ${res.status()}`);
    };
    if (runId) await cleanup("kill the run", page.request.post(`/api/v1/runs/${encodeURIComponent(runId)}/kill`, { headers: auth }));
    if (!secretExisted) await cleanup("delete the secret", page.request.delete(`/api/v1/secrets/${encodeURIComponent(SECRET_NAME)}`, { headers: auth }));
  }
});
