/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The host-call card (HC-1) on the standalone /approvals list. Like approvals-download-kube.spec.ts, it intercepts
// **/api/v1/approvals* with synthetic rows (the seeded backend has no held host request) and the approve route.
// HOSTED ONLY: the lane runs no browser locally.
import { test, expect, gotoConsole, navTo, sql } from "./fixtures";
import { HOST_CALL_CARD } from "../src/app/components/wardyn/copy/files-review";
import { APPROVAL_MCP_CALL } from "../src/app/components/wardyn/copy/approvals";
import type { Page } from "@playwright/test";

const HOST = "api.payments.example.com";
const LONG_PATH = "/v1/customers/cus_9f3b27e1a4c8/payment-methods/pm_1Q2w3e4r5t6y7u8i/refunds";

type Row = Record<string, unknown>;

const hostCall = (id: string, runId: string, path: string, over: Row = {}): Row => ({
  id,
  run_id: runId,
  kind: "tool_call",
  state: "PENDING",
  requested_at: new Date().toISOString(),
  decidable: true,
  held: true,
  requested_scope: { host: HOST, method: "POST", path },
  ...over,
});

async function serve(page: Page, rows: Row[]) {
  const approved: string[] = [];
  await page.route("**/api/v1/approvals*", async (route) => {
    const req = route.request();
    if (req.method() !== "GET") return route.continue();
    const state = new URL(req.url()).searchParams.get("state");
    const out = rows.filter((r) => (state === "" || state === null ? true : r.state === state) && !approved.includes(String(r.id)));
    await route.fulfill({ body: JSON.stringify(out), contentType: "application/json" });
  });
  await page.route("**/api/v1/approvals/*/approve", async (route) => {
    const id = route.request().url().split("/approvals/")[1].split("/")[0];
    approved.push(id);
    await route.fulfill({ body: JSON.stringify({ ...rows.find((r) => r.id === id), state: "APPROVED" }), contentType: "application/json" });
  });
  return approved;
}

async function open(page: Page) {
  await gotoConsole(page);
  await navTo(page, "Approvals");
}

test.describe("Approvals: a held host request", () => {
  test("the card names host, method and path and approves for the run", async ({ page }) => {
    const runId = sql("SELECT id FROM agent_runs ORDER BY created_at LIMIT 1");
    const approved = await serve(page, [hostCall("e2e-host-1", runId, "/v1/refunds")]);
    await open(page);
    const card = page.getByTestId("host-call-card");
    await expect(card).toBeVisible();
    await expect(card).toContainText(HOST_CALL_CARD.TITLE);
    await expect(card).toContainText(HOST_CALL_CARD.WHAT("POST", "/v1/refunds", HOST));
    await expect(card).toContainText(HOST_CALL_CARD.BLAST);
    await expect(card).toContainText(HOST_CALL_CARD.DENY_NOTE);
    await card.getByRole("button", { name: HOST_CALL_CARD.APPROVE }).click();
    await expect(card).toHaveCount(0);
    expect(approved).toEqual(["e2e-host-1"]);
  });

  test("the Decided tab titles the row with its path and wraps a long one", async ({ page }) => {
    const runId = sql("SELECT id FROM agent_runs ORDER BY created_at LIMIT 1");
    await serve(page, [
      hostCall("e2e-host-2", runId, LONG_PATH, { state: "APPROVED", held: false, decided_by: "ada", decided_at: new Date().toISOString(), decision_basis: "owner" }),
    ]);
    await open(page);
    await page.getByRole("tab", { name: /^Decided/ }).click();
    const title = page.getByText(HOST_CALL_CARD.LIST_TITLE("POST", LONG_PATH, HOST));
    await expect(title).toBeVisible();
    await expect(page.getByText("Allowed for this run")).toBeVisible();
    // The title wraps (break-all), so its text is never cut to an ellipsis.
    const clipped = await title.evaluate((el) => el.scrollWidth > el.clientWidth);
    expect(clipped).toBe(false);
  });

  test("Deny stays on the card pair with the shared Deny word", async ({ page }) => {
    const runId = sql("SELECT id FROM agent_runs ORDER BY created_at LIMIT 1");
    await serve(page, [hostCall("e2e-host-3", runId, "/v1/refunds")]);
    await open(page);
    await expect(page.getByTestId("host-call-card").getByRole("button", { name: APPROVAL_MCP_CALL.DENY })).toBeEnabled();
  });
});
