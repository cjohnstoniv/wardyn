/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Governance -> Baseline hosts against the real server: a security admin declares a host, it is stored (a
// reload reads it back), a wildcard is refused with the server's sentence, and the walk leaves the
// declaration empty again so no other spec grades against it.
import { test, expect, ADMIN_TOKEN, asRealSecurityAdmin, gotoConsole, navTo } from "./fixtures";
import { BASELINE } from "../src/app/lib/governance-copy";

const HOST = "models.e2e-baseline.example";

test.describe("Governance: the baseline hosts", () => {
  test.afterEach(async ({ page }) => {
    const res = await page.request.put("/api/v1/governance/egress-baseline", {
      headers: { Authorization: `Bearer ${ADMIN_TOKEN}` },
      data: { baseline_hosts: [], internal_host_suffixes: [] },
    });
    expect(res.status(), await res.text()).toBe(200);
  });

  test("a declared host is stored, and a wildcard is refused in the server's words", async ({ page }) => {
    await asRealSecurityAdmin(page);
    await gotoConsole(page, "admin");
    await navTo(page, "Governance");
    await page.getByRole("tab", { name: BASELINE.TAB }).click();
    await expect(page.getByText(BASELINE.LEAD)).toBeVisible();

    const hosts = page.getByLabel(BASELINE.HOSTS_LABEL, { exact: true });
    await hosts.fill(HOST);
    await page.getByRole("button", { name: BASELINE.SAVE, exact: true }).click();
    await expect(page.getByText(BASELINE.SAVED)).toBeVisible();

    await page.reload();
    await expect(page.getByLabel(BASELINE.HOSTS_LABEL, { exact: true })).toHaveValue(HOST);

    await page.getByLabel(BASELINE.HOSTS_LABEL, { exact: true }).fill(`${HOST}\n*.e2e-baseline.example`);
    await page.getByRole("button", { name: BASELINE.SAVE, exact: true }).click();
    await expect(page.getByText(BASELINE.REFUSED_TITLE)).toBeVisible();
    await expect(page.getByText(/must be an exact hostname — no wildcard/)).toBeVisible();
  });
});
