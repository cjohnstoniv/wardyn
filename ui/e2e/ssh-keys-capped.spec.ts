/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, ADMIN_TOKEN, gotoConsole, navToRoute } from "./fixtures";

// #584: a key added in the user view is capped at member rights for good, and
// the SSH keys screen says so with a chip. The bearer-token harness cannot sign
// in to the user view, so the key is added through the real door and the REAL
// GET /api/v1/me/ssh-keys response is marked capped on its way to the page —
// after checking that the real response already carries `capped` as a boolean,
// the field the chip reads. Frozen strings: docs/design/admin-access-canon.md.
// Its own key, not ssh-keys.spec.ts's: the two specs may run side by side.
const PUBLIC_KEY = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBvUZbH/FCPeFHqDUX5bD2AnpEfl06Qpg/DGH7HACslV e2e-capped";
const KEY_NAME = "e2e-capped-laptop";
const CHIP = "Member access";
const TOOLTIP = "Added while you were a member, so it keeps member rights. Add a new key to use admin access over SSH.";

const auth = { Authorization: `Bearer ${ADMIN_TOKEN}` };

test.describe("SSH keys — capped-key chip", () => {
  // ticket: #724 (#584)
  test("a capped key shows the Member access chip and its tooltip; an uncapped one does not", async ({ page }) => {
    const added = await page.request.post("/api/v1/me/ssh-keys", {
      headers: auth,
      data: { name: KEY_NAME, public_key: PUBLIC_KEY },
    });
    expect(added.ok()).toBe(true);
    const { fingerprint } = (await added.json()) as { fingerprint: string };

    try {
      let capped = false;
      await page.route("**/api/v1/me/ssh-keys", async (route) => {
        if (route.request().method() !== "GET") {
          await route.continue();
          return;
        }
        const res = await route.fetch();
        const keys = ((await res.json()) ?? []) as Array<{ fingerprint: string; capped?: unknown }>;
        for (const k of keys) expect(typeof k.capped).toBe("boolean");
        await route.fulfill({
          response: res,
          json: keys.map((k) => (k.fingerprint === fingerprint ? { ...k, capped } : k)),
        });
      });

      await gotoConsole(page);
      await navToRoute(page, "/ssh-keys");
      const row = page.getByRole("row").filter({ hasText: KEY_NAME });
      await expect(row).toBeVisible();
      await expect(row.getByText(CHIP)).toHaveCount(0);

      capped = true;
      await page.reload();
      const chip = page.getByRole("row").filter({ hasText: KEY_NAME }).getByText(CHIP);
      await expect(chip).toBeVisible();
      await expect(page.locator(`[title="${TOOLTIP}"]`)).toHaveText(CHIP);
    } finally {
      await page.request.delete(`/api/v1/me/ssh-keys/${encodeURIComponent(fingerprint)}`, { headers: auth });
    }
  });
});
