/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Asking for elevation on Approvals, following the answer under "Your requests", and deciding another person's
// request under Waiting on you. The seeded tokens carry no groups and an elevation rule's eligible requesters
// are groups only, so, as approvals-waiting-on-you.spec.ts does for deciders, the request list and the create
// are spliced with synthetic rows and everything else hits the real backend.
import type { Page } from "@playwright/test";
import { test, expect, asRealMember, consoleAPI, gotoConsole, navTo } from "./fixtures";
import { ELEV } from "../src/app/components/wardyn/copy/elevation";

const ahead = (hours: number) => new Date(Date.now() + hours * 3_600_000).toISOString();
const payments = { unit_id: "u1", unit_name: "Payments", max_elevation: "4h" };

const requestRow = (id: string, extra: Record<string, unknown> = {}) => ({
  id,
  target_kind: "governance_elevation",
  op: "create",
  target_key: "u1",
  state: "pending",
  kind: "elevation",
  unit_id: "u1",
  unit_name: "Payments",
  duration: "4h",
  proposed_by: "e2e-member",
  proposed_at: ahead(-0.05),
  expires_at: ahead(24),
  diff: { before: null, after: null, changed: [] },
  mine: true,
  decidable: false,
  deciders: [{ kind: "unit_admin", unit_name: "Payments" }, { kind: "security" }],
  ...extra,
});

const listPath = (url: URL) => url.pathname.endsWith("/api/v1/governance/requests");

async function serveList(page: Page, body: { requests?: unknown[]; offered?: unknown[] }) {
  await page.route(listPath, (route) =>
    route.request().method() === "GET"
      ? route.fulfill({
          contentType: "application/json",
          body: JSON.stringify({ requests: [], offered: [], image_offered: false, ...body }),
        })
      : route.fallback(),
  );
}

async function openApprovals(page: Page) {
  await gotoConsole(page);
  await navTo(page, "Approvals");
}

const reasonField = (page: Page) => page.getByLabel(ELEV.REASON_LABEL);
const sendButton = (page: Page) => page.getByRole("dialog").getByRole("button", { name: ELEV.SEND });

async function openDialog(page: Page) {
  await page.getByRole("button", { name: ELEV.ACTION }).click();
  await expect(page.getByRole("dialog")).toBeVisible();
}

test.describe("Approvals: elevation requests", () => {
  test("a member whose units offer nothing sees no Your requests card", async ({ page }) => {
    await asRealMember(page);
    const read = await consoleAPI(page, "GET", "/api/v1/governance/requests");
    expect(read.status, read.text).toBe(200);
    expect(JSON.parse(read.text).offered ?? []).toEqual([]);
    await openApprovals(page);
    await expect(page.getByRole("tab", { name: /^Pending/ })).toBeVisible();
    await expect(page.getByText(ELEV.LIST_TITLE)).toHaveCount(0);
  });

  test("a unit that offers elevation shows the card, and the dialog disables the 8 h preset with its reason", async ({ page }) => {
    await asRealMember(page);
    await serveList(page, { offered: [payments] });
    await openApprovals(page);
    await expect(page.getByText(ELEV.LIST_TITLE)).toBeVisible();
    await expect(page.getByText(ELEV.LIST_LEAD)).toBeVisible();
    await openDialog(page);
    const eight = page.getByRole("radio", { name: "8 h" });
    await expect(eight).toBeDisabled();
    await expect(eight).toHaveAccessibleDescription(ELEV.PRESET_OVER("Payments", "4 hours"));
  });

  test("a sent request names who decides it", async ({ page }) => {
    await asRealMember(page);
    await serveList(page, { offered: [payments] });
    const created = requestRow("e2e-elev-1");
    await page.route(listPath, (route) =>
      route.request().method() === "POST"
        ? route.fulfill({ status: 202, contentType: "application/json", body: JSON.stringify({ pending_change: created, request: created }) })
        : route.fallback(),
    );
    await openApprovals(page);
    await openDialog(page);
    await reasonField(page).fill("Production incident INC-2210");
    await sendButton(page).click();
    await expect(page.getByText(ELEV.SENT_TITLE)).toBeVisible();
    await expect(page.getByRole("dialog")).toContainText("It is waiting for admins of Payments or a security admin.");
  });

  test("a request that is already waiting says so and keeps what was typed", async ({ page }) => {
    await asRealMember(page);
    await serveList(page, { offered: [payments] });
    await page.route(listPath, (route) =>
      route.request().method() === "POST"
        ? route.fulfill({
            status: 409,
            contentType: "application/json",
            body: JSON.stringify({ error: "a request for Payments is already waiting", reason: "governance_change_pending" }),
          })
        : route.fallback(),
    );
    await openApprovals(page);
    await openDialog(page);
    await reasonField(page).fill("Production incident INC-2210");
    await sendButton(page).click();
    await expect(page.getByRole("dialog")).toContainText(ELEV.ALREADY_WAITING("Payments"));
    await expect(reasonField(page)).toHaveValue("Production incident INC-2210");
  });

  test("each state of the person's own requests reads its chip", async ({ page }) => {
    await asRealMember(page);
    await serveList(page, {
      offered: [payments],
      requests: [
        requestRow("e2e-elev-wait", { proposed_at: ahead(-0.1) }),
        requestRow("e2e-elev-ok", { state: "applied", ends_at: ahead(3), proposed_at: ahead(-0.5), decided_by: "priya.nair", decided_at: ahead(-0.4) }),
        requestRow("e2e-elev-no", { state: "rejected", unit_name: "Billing", duration: "1h", proposed_at: ahead(-2), decided_by: "priya.nair", decided_at: ahead(-1.9), reason: "Use the runbook." }),
        requestRow("e2e-elev-late", { state: "expired", unit_name: "Support", duration: "2h", proposed_at: ahead(-26), expires_at: ahead(-2) }),
        requestRow("e2e-elev-over", { state: "applied", proposed_at: ahead(-7), ends_at: ahead(-2), decided_at: ahead(-6) }),
      ],
    });
    await openApprovals(page);
    const card = page.locator("div.rounded-xl", { has: page.getByText(ELEV.LIST_TITLE) }).first();
    for (const chip of [ELEV.STATE_WAITING, ELEV.STATE_APPROVED, ELEV.STATE_DENIED, ELEV.STATE_EXPIRED, ELEV.STATE_ENDED]) {
      await expect(card.getByText(chip, { exact: true })).toHaveCount(1);
    }
    await expect(card.getByRole("button", { name: ELEV.WITHDRAW })).toHaveCount(1);
  });

  test("a decider sees another person's request under Waiting on you and Approve posts to the approve route", async ({ page }) => {
    await asRealMember(page);
    const theirs = requestRow("e2e-elev-theirs", {
      mine: false,
      decidable: true,
      proposed_by: "dana.reyes@acme.example",
      note: { reason: "Production incident INC-2210", ticket: "INC-2210" },
      deciders: [{ kind: "unit_admin" }, { kind: "group", name: "support-leads" }, { kind: "security" }],
    });
    await serveList(page, { requests: [theirs] });
    await page.route("**/api/v1/governance/requests/e2e-elev-theirs/approve", (route) =>
      route.fulfill({ contentType: "application/json", body: JSON.stringify({ ...theirs, state: "applied" }) }),
    );
    await openApprovals(page);
    const tab = page.getByRole("tab", { name: /^Waiting on you/ });
    await expect(tab).toHaveText(/Waiting on you\s*1/);
    await tab.click();
    await expect(page.getByText(ELEV.F_TITLE("dana.reyes@acme.example", "Payments", "4 hours"))).toBeVisible();
    const posted = page.waitForRequest(
      (r) => r.method() === "POST" && new URL(r.url()).pathname.endsWith("/api/v1/governance/requests/e2e-elev-theirs/approve"),
    );
    await page.getByRole("button", { name: /Approve/ }).click();
    await posted;
  });
});
