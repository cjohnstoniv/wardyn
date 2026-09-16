/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, ADMIN_TOKEN, gotoConsole, navTo } from "./fixtures";

// E2E coverage for "Make a policy from this run" (X2-F6) — run-detail.tsx's
// Audit tab button opens profile-review.tsx's ProfileReview sheet
// (POST /runs/{id}/profile), and its Save dialog persists the synthesized
// inline_policy via POST /policies (profile-review.tsx's SavePolicyDialog).
// Had zero e2e — this proves the real round trip: the saved policy is a REAL
// row the /policies screen lists, not just a client-side success toast.
const POLICY_NAME = `e2e-run-profile-${Date.now()}`;
const auth = { Authorization: `Bearer ${ADMIN_TOKEN}` };

test.describe("Run detail — Make a policy from this run (X2-F6)", () => {
  // Fix pass (review F4): this file's own backend is fresh per run-ui-e2e.sh
  // invocation, but policies.spec.ts's file-wide invariant (a clean policy
  // table, so its empty-state specs hold) only survives a plain
  // `pnpm playwright test` over ONE shared backend if every mutating spec
  // cleans up after itself — this one didn't. `request`, not `page`: an
  // afterAll hook runs at worker scope and cannot use the test-scoped `page`
  // fixture.
  test.afterAll(async ({ request }) => {
    const res = await request.get("/api/v1/policies", { headers: auth });
    const policies: Array<{ id: string; name: string }> = await res.json();
    const created = policies.find((p) => p.name === POLICY_NAME);
    // Review N2: the delete is asserted, not fire-and-forget — a 403/404 here
    // would otherwise be the silent leak this hook exists to prevent.
    if (created) expect((await request.delete(`/api/v1/policies/${created.id}`, { headers: auth })).ok()).toBe(true);
  });

  test("saves a real policy that appears on /policies", async ({ page }) => {
    await gotoConsole(page);
    await navTo(page, "Runs");
    await expect(page.getByText("e2e fixture 4")).toBeVisible();
    await page.getByText("e2e fixture 4").click();
    await expect(page).toHaveURL(/\/runs\/.+/);

    await page.getByRole("tab", { name: "Audit" }).click();
    await page.getByRole("button", { name: "Make a policy from this run" }).click();

    const sheet = page.getByRole("dialog").filter({ hasText: "Synthesized profile" });
    await expect(sheet).toBeVisible();
    await expect(sheet.getByText("Overall risk")).toBeVisible();

    await sheet.getByRole("button", { name: /^Save as policy$/ }).click();
    // Scoped by the dialog's DESCRIPTION, not its "Save as policy" title —
    // the sheet behind it stays mounted and its own trigger button carries
    // that exact same string, which would make a title-scoped locator
    // ambiguous (two dialogs open at once).
    const saveDialog = page.getByRole("dialog").filter({ hasText: "Persist the synthesized inline_policy" });
    await expect(saveDialog).toBeVisible();
    await saveDialog.getByLabel("Policy name").fill(POLICY_NAME);
    await saveDialog.getByRole("button", { name: "Save policy" }).click();

    // Both the name dialog and the sheet behind it close on a successful save
    // (profile-review.tsx's onSaved calls onClose too).
    await expect(saveDialog).toHaveCount(0);
    await expect(sheet).toHaveCount(0);

    await navTo(page, "Policies");
    await expect(page.getByRole("heading", { name: "Policies", level: 1 })).toBeVisible();
    await expect(
      page.getByRole("table").getByRole("row").filter({ hasText: POLICY_NAME }),
    ).toBeVisible();
  });
});
