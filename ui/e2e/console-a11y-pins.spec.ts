/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { createHash, randomBytes } from "node:crypto";
import type { APIRequestContext } from "@playwright/test";
import { test, expect, ADMIN_TOKEN, gotoConsole, navTo, sql } from "./fixtures";
import { TIER } from "../src/app/components/wardyn/copy/tier";

// Keyboard and screen-reader pins for console primitives: a state-opened drawer hands focus back to its opener,
// a destructive button's focus ring is full alpha, and a chip that carries a reason exposes it as a name.

const auth = { Authorization: `Bearer ${ADMIN_TOKEN}` };

async function seedPerson(request: APIRequestContext) {
  const raw = `wdn_${randomBytes(32).toString("hex")}`;
  const hash = createHash("sha256").update(raw).digest("hex");
  const subject = `e2e-a11y-${hash.slice(0, 8)}`;
  const email = `a11y-${hash.slice(0, 8)}@e2e.wardyn.invalid`;
  sql(
    `INSERT INTO api_tokens (id, principal, email, role, user_type, groups, groups_truncated, name, token_sha256, created_at)
     VALUES (gen_random_uuid(), '${subject}', '${email}', 'user', 'standard', '[]'::jsonb, false, 'e2e', '${hash}', now())`,
  );
  const created = await request.post("/api/v1/people", { headers: auth, data: { principal: subject, email } });
  expect(created.status(), await created.text()).toBeLessThan(300);
  return { email };
}

async function openPersonDrawerByKeyboard(page: import("@playwright/test").Page, email: string) {
  await gotoConsole(page, "admin");
  await navTo(page, "People");
  await page.getByRole("textbox", { name: "Search people" }).fill(email.slice(0, 14));
  const opener = page.getByRole("row").filter({ hasText: email }).getByRole("button", { name: email });
  await expect(opener).toBeVisible();
  await opener.focus();
  await page.keyboard.press("Enter");
  await expect(page.getByRole("dialog")).toBeVisible();
  return opener;
}

test.describe("Console accessibility pins", () => {
  test("closing a state-opened drawer with Escape puts focus back on the row that opened it", async ({ page }) => {
    const person = await seedPerson(page.request);
    const opener = await openPersonDrawerByKeyboard(page, person.email);
    await page.keyboard.press("Escape");
    await expect(page.getByRole("dialog")).toHaveCount(0);
    await expect(opener).toBeFocused();
  });

  test("a destructive button's focus ring is the same full ring as Cancel's, not a faint red halo", async ({ page }) => {
    const person = await seedPerson(page.request);
    await openPersonDrawerByKeyboard(page, person.email);
    // The ring eases in (transition-all) and a mid-flight shadow ("2.90663px") still contains "3px", so no poll on
    // the string can tell eased from settled. This pin is about which ring, not how it animates: switch easing off.
    await page.addStyleTag({ content: "*, *::before, *::after { transition: none !important; }" });
    await page.getByRole("dialog").getByRole("button", { name: "Sign out everywhere" }).click();
    const confirm = page.getByRole("alertdialog");
    const action = confirm.getByRole("button", { name: "Sign out everywhere" });
    // Tab from the focused Cancel so the browser treats the focus as keyboard focus (:focus-visible).
    await expect(confirm.getByRole("button", { name: "Cancel" })).toBeFocused();
    await page.keyboard.press("Tab");
    await expect(action).toBeFocused();
    // The destructive action's ring must be the very ring Cancel gets: the faint red halo was a different,
    // translucent one. With easing off the focused button's shadow is already its final one, at a whole 3px.
    const ring = async (button: typeof action) => {
      await expect.poll(() => button.evaluate((el) => getComputedStyle(el).boxShadow)).toMatch(/(^|[^\d.])3px/);
      return button.evaluate((el) => getComputedStyle(el).boxShadow);
    };
    const destructive = await ring(action);
    await page.keyboard.press("Shift+Tab");
    const cancel = confirm.getByRole("button", { name: "Cancel" });
    await expect(cancel).toBeFocused();
    expect(destructive).toBe(await ring(cancel));
  });

  test("a chip with a reason is a named image, so a screen reader announces the reason", async ({ page }) => {
    await page.route("**/api/v1/setup/status*", async (route) => {
      const response = await route.fetch();
      const json = await response.json();
      json.tier = "local-only";
      await route.fulfill({ response, json });
    });
    await gotoConsole(page);
    await expect(page.getByRole("img", { name: TIER.LABEL["local-only"] })).toHaveText(TIER.CHIP);
  });
});
