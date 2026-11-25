/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, asRealSecurityAdmin, gotoConsole, navTo } from "./fixtures";
import { CHANGES } from "../src/app/lib/governance-copy";
import { CHANGE_REVIEW } from "../src/app/lib/change-review-copy";
import { aheadByHours } from "../src/app/lib/test-clock";
import { PAGE_NOT_FOUND } from "../src/app/components/wardyn/copy/refusal";

// A governance change on its own page: opened from the Changes queue, read as a verdict and an impact, and approved.
// The queue, the change read by id, its impact and its approval are spliced, so the plain harness (no second-human
// switch, no approval minimum) shows what the page does with the server's answers; the held-write path against a real
// daemon is governance-changes.spec.ts. With no minimum in force the server applies a change on one approval, so
// the spliced answer to Approve is the applied change.
const ID = "00000000-0000-4000-8000-0000000000c1";
const NAME = "payments";

const held = {
  id: ID,
  target_kind: "governance_unit",
  op: "update",
  target_key: NAME,
  state: "pending",
  proposed_by: "alice",
  proposed_at: aheadByHours(-2),
  expires_at: aheadByHours(22),
  diff: {
    before: { name: NAME, ceiling: { allowed_domains: ["github.com"] } },
    after: { name: NAME, ceiling: { allowed_domains: ["github.com", "settle.partner-a.example"] } },
    changed: ["ceiling.allowed_domains"],
    review: {
      verdict: "widens",
      widens: [{ field: "allowed_domains", entries: ["settle.partner-a.example"] }],
      risk: { before: { overall: "low", items: [] }, after: { overall: "medium", items: [] } },
    },
  },
};

test.describe("governance — a change on its own page", () => {
  test("opens from the queue, shows the verdict and the impact, and approving returns to the queue", async ({ page }) => {
    await page.route("**/api/v1/governance/changes*", (route) => (route.request().method() === "GET" ? route.fulfill({ json: [held] }) : route.fallback()));
    await page.route("**/api/v1/governance/changes/*", (route) => (route.request().method() === "GET" ? route.fulfill({ json: held }) : route.fallback()));
    await page.route("**/api/v1/governance/changes/*/impact", (route) =>
      route.fulfill({ json: { computed: "tree", as_of: new Date().toISOString(), people: [], live_runs_total: 0 } }),
    );
    let approved = 0;
    await page.route("**/api/v1/governance/changes/*/approve", (route) => {
      approved += 1;
      return route.fulfill({ json: { ...held, state: "applied" } });
    });

    await asRealSecurityAdmin(page);
    await gotoConsole(page, "admin");
    await navTo(page, "Governance");
    await page.getByRole("tab", { name: CHANGES.TAB_COUNT(1) }).click();
    await page.getByRole("button", { name: `${CHANGES.KIND.governance_unit} · ${CHANGES.OP.update}`, exact: true }).click();

    // Its own page, not a dialog, and the verdict is the first thing on it.
    await expect(page).toHaveURL(new RegExp(`/admin/governance/changes/${ID}$`));
    await expect(page.getByRole("dialog")).toHaveCount(0);
    await expect(page.getByTestId("change-verdict").getByText(CHANGE_REVIEW.VERDICT.widens, { exact: true })).toBeVisible();

    await page.getByRole("tab", { name: CHANGE_REVIEW.IMPACT, exact: true }).click();
    await expect(page.getByTestId("change-impact").getByText(CHANGE_REVIEW.AS_OF_NOW, { exact: true })).toBeVisible();

    await page.getByRole("button", { name: CHANGES.APPROVE, exact: true }).click();
    await expect(page.getByText(CHANGES.TOAST_APPROVED, { exact: true })).toBeVisible();
    await expect(page).toHaveURL(/\/admin\/governance\?tab=changes$/);
    expect(approved).toBe(1);
  });

  test("an unknown change reads as a page that does not exist", async ({ page }) => {
    await page.route("**/api/v1/governance/changes/*", (route) =>
      route.request().method() === "GET" ? route.fulfill({ status: 404, json: { error: "no such change" } }) : route.fallback(),
    );
    await asRealSecurityAdmin(page);
    await gotoConsole(page, "admin");
    await page.goto("/admin/governance/changes/00000000-0000-4000-8000-0000000000ff");
    await expect(page.getByRole("heading", { name: PAGE_NOT_FOUND.TITLE })).toBeVisible();
  });
});
