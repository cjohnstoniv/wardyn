/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A group decider who is not a unit admin cannot open Governance, so an image request they may decide is a card
// under Approvals › Waiting on you. The seeded tokens carry no groups, so, as elevation-request.spec.ts does,
// the request list and the decide routes are spliced with synthetic rows and everything else hits the real backend.
import type { Page } from "@playwright/test";
import { test, expect, asRealMember, gotoConsole, navTo, navToRoute } from "./fixtures";
import { APPROVALS } from "../src/app/lib/approvals-copy";
import { VIEW_REFUSAL } from "../src/app/components/wardyn/copy/console-view";

const ahead = (hours: number) => new Date(Date.now() + hours * 3_600_000).toISOString();
const ID = "e2e-image-card-1";
const PERSON = "dana.reyes@acme.example";
const NAME = "ghcr.io/acme/build-tools";
const DIGEST = "sha256:4f2a91c37be0d5a8e61c29f04b7d3a5c8e9f1a2b3c4d5e6f708192a3b4c5d6e7";

const imageRow = {
  id: ID,
  target_kind: "governance_image_request",
  op: "create",
  target_key: "u1",
  state: "pending",
  kind: "image",
  unit_id: "u1",
  unit_name: "Payments",
  image: `${NAME}@${DIGEST}`,
  proposed_by: PERSON,
  proposed_at: ahead(-0.05),
  expires_at: ahead(24),
  diff: { before: null, after: null, changed: [] },
  note: { reason: "The release job needs the pinned build-tools image.", ticket: "INC-2210" },
  mine: false,
  decidable: true,
  deciders: [{ kind: "security" }, { kind: "group", name: "platform-leads" }],
};

async function serveRow(page: Page) {
  await page.route(
    (url) => url.pathname.endsWith("/api/v1/governance/requests"),
    (route) =>
      route.request().method() === "GET"
        ? route.fulfill({ contentType: "application/json", body: JSON.stringify({ requests: [imageRow], offered: [], image_offered: false }) })
        : route.fallback(),
  );
}

async function openWaiting(page: Page) {
  await gotoConsole(page);
  await navTo(page, "Approvals");
  const tab = page.getByRole("tab", { name: /^Waiting on you/ });
  await expect(tab).toHaveText(/Waiting on you\s*1/);
  await tab.click();
}

const decideRoute = (verb: string) => `**/api/v1/governance/requests/${ID}/${verb}`;
const isDecide = (verb: string) => (r: { method(): string; url(): string }) =>
  r.method() === "POST" && new URL(r.url()).pathname.endsWith(`/api/v1/governance/requests/${ID}/${verb}`);

test.describe("Approvals: image requests under Waiting on you", () => {
  test("a group decider who is not a unit admin approves an image request from Waiting on you", async ({ page }) => {
    await asRealMember(page);
    await gotoConsole(page);
    await navToRoute(page, "/admin/governance");
    await expect(page.getByRole("heading", { name: VIEW_REFUSAL.TITLE })).toBeVisible();

    await serveRow(page);
    await page.route(decideRoute("approve"), (route) =>
      route.fulfill({ contentType: "application/json", body: JSON.stringify({ ...imageRow, state: "applied" }) }),
    );
    await openWaiting(page);
    await expect(page.getByText(`Grant ${PERSON} one image, by digest`)).toBeVisible();
    await expect(page.getByText(NAME, { exact: true })).toBeVisible();
    await expect(page.getByText(DIGEST, { exact: true })).toBeVisible();
    const posted = page.waitForRequest(isDecide("approve"));
    await page.getByRole("button", { name: /Approve/ }).click();
    await posted;
    await expect(page.getByText(APPROVALS.TOAST_APPROVED)).toBeVisible();
  });

  test("Deny sends no reason, and a refusal names the button and the server's sentence", async ({ page }) => {
    await asRealMember(page);
    await serveRow(page);
    await page.route(decideRoute("reject"), (route) =>
      route.fulfill({
        status: 409,
        contentType: "application/json",
        body: JSON.stringify({ error: "this change is no longer pending (applied)", reason: "governance_change_not_pending" }),
      }),
    );
    await openWaiting(page);
    const posted = page.waitForRequest(isDecide("reject"));
    await page.getByRole("button", { name: /Deny/ }).click();
    expect((await posted).postDataJSON()).toEqual({});
    await expect(page.getByText(APPROVALS.TOAST_DENY_FAILED)).toBeVisible();
    await expect(page.getByText("this change is no longer pending (applied)")).toBeVisible();
  });

  test("Approve refused as stale", async ({ page }) => {
    const stale =
      "the target, or the deployment default, has changed since this change was proposed: it was not applied and is now stale; propose it again";
    await asRealMember(page);
    await serveRow(page);
    await page.route(decideRoute("approve"), (route) =>
      route.fulfill({ status: 409, contentType: "application/json", body: JSON.stringify({ error: stale, reason: "governance_change_stale" }) }),
    );
    await openWaiting(page);
    await page.getByRole("button", { name: /Approve/ }).click();
    await expect(page.getByText(APPROVALS.TOAST_APPROVE_FAILED)).toBeVisible();
    await expect(page.getByText(stale)).toBeVisible();
  });
});
