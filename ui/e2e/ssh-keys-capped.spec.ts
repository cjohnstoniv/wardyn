/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { generateKeyPairSync, randomBytes } from "node:crypto";
import { test, expect, ADMIN_TOKEN, expandCard, gotoConsole, navToRoute } from "./fixtures";

// #584: a key added in the user view is capped at member rights for good, and
// the SSH keys screen says so with a chip. The bearer-token harness cannot sign
// in to the user view, so the key is added through the real door and the REAL
// GET /api/v1/me/ssh-keys response is marked capped on its way to the page —
// after checking that the real response already carries `capped` as a boolean,
// the field the chip reads. Frozen strings: docs/design/admin-access-canon.md.
// Its own key, not ssh-keys.spec.ts's: the two specs may run side by side.
//
// M-5 (#636, S-2, approved 2026-09-27): /ssh-keys is gone (no alias) —
// SshKeysPane is mounted at /account now. The chip and tooltip were rewritten
// too: "Member access" -> "User access", pointing at Admin Settings' new
// Admin SSH keys card instead of a door M-5 removed.
// A fresh ed25519 public key (public half only) and key name per test run. The fingerprint is the primary key
// across all users, so a fixed key would get a 409 whenever two runs of this file overlap (parallel workers,
// --repeat-each); a fixed name would also make two rows match one filter.
function freshKey(label: string): { name: string; publicKey: string } {
  const raw = generateKeyPairSync("ed25519").publicKey.export({ format: "der", type: "spki" }).subarray(-32);
  const blob = Buffer.concat([Buffer.from([0, 0, 0, 11]), Buffer.from("ssh-ed25519"), Buffer.from([0, 0, 0, 32]), raw]);
  return { name: `${label}-${randomBytes(4).toString("hex")}`, publicKey: `ssh-ed25519 ${blob.toString("base64")} ${label}` };
}
const CHIP = "User access";
const TOOLTIP =
  "Added in the user view, so it keeps user rights. To reach other people's runs over SSH, add a key in Settings in the admin view.";

const auth = { Authorization: `Bearer ${ADMIN_TOKEN}` };

test.describe("SSH keys — capped-key chip", () => {
  // ticket: #724 (#584)
  test("a capped key shows the User access chip and its note; an uncapped one does not", async ({ page }) => {
    const KEY = freshKey("e2e-capped-laptop");
    const added = await page.request.post("/api/v1/me/ssh-keys", {
      headers: auth,
      data: { name: KEY.name, public_key: KEY.publicKey },
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
      await navToRoute(page, "/account");
      await expandCard(page, "Your SSH keys");
      const row = page.getByRole("row").filter({ hasText: KEY.name });
      await expect(row).toBeVisible();
      await expect(row.getByText(CHIP)).toHaveCount(0);

      capped = true;
      await page.reload();
      await expandCard(page, "Your SSH keys");
      const chip = page.getByRole("row").filter({ hasText: KEY.name }).getByText(CHIP);
      await expect(chip).toBeVisible();
      // r11 #2244: the meaning is text under the table, not a hover-only title.
      await expect(page.getByTestId("ssh-key-chip-notes").getByText(TOOLTIP)).toBeVisible();
      await expect(page.locator(`[title="${TOOLTIP}"]`)).toHaveCount(0);
      await expect(chip).not.toHaveAttribute("title");
    } finally {
      await page.request.delete(`/api/v1/me/ssh-keys/${encodeURIComponent(fingerprint)}`, { headers: auth });
    }
  });
});

// r11 #2244: the Admin override chip's meaning is text under the table too, and the chip carries no title.
test.describe("SSH keys — admin-override chip", () => {
  test("an admin-stamped key shows its meaning as text, not a title", async ({ page }) => {
    const KEY = freshKey("e2e-override-laptop");
    const added = await page.request.post("/api/v1/me/ssh-keys", {
      headers: auth,
      data: { name: KEY.name, public_key: KEY.publicKey },
    });
    expect(added.ok()).toBe(true);
    const { fingerprint } = (await added.json()) as { fingerprint: string };

    try {
      await page.route("**/api/v1/me/ssh-keys", async (route) => {
        if (route.request().method() !== "GET") {
          await route.continue();
          return;
        }
        const res = await route.fetch();
        const keys = ((await res.json()) ?? []) as Array<{ fingerprint: string }>;
        await route.fulfill({
          response: res,
          json: keys.map((k) => (k.fingerprint === fingerprint ? { ...k, role: "admin" } : k)),
        });
      });

      await gotoConsole(page);
      await navToRoute(page, "/account");
      await expandCard(page, "Your SSH keys");
      const chip = page.getByRole("row").filter({ hasText: KEY.name }).getByText("Admin override");
      await expect(chip).toBeVisible();
      await expect(chip).not.toHaveAttribute("title");
      await expect(page.getByTestId("ssh-key-chip-notes").getByText(/^Registered while you were an admin, so this key reaches runs you do not own\./)).toBeVisible();
    } finally {
      await page.request.delete(`/api/v1/me/ssh-keys/${encodeURIComponent(fingerprint)}`, { headers: auth });
    }
  });
});
