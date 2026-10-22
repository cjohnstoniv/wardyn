/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// E2E for Your account > Signed-in CLIs (ATT-LOGIN-UI, issue #3275; boards MATT3-*). The list and the sign-out
// belong to the server lane (ATT-LOGIN-SRV), so they are answered here in its wire shape
// (internal/types/cli_sign_in.go); the spec proves what the card does with the answer: one row per computer, a
// per-device Sign out that asks first and ends only that session, the file-store note, the admin chip, the empty
// state with the command, and the card absent where the server does not serve CLI sign-in.
import type { Page } from "@playwright/test";
import { test, expect, asRealMember, expandCard, gotoConsole, navToRoute } from "./fixtures";
import { SIGNED_IN_CLIS as T } from "../src/app/components/wardyn/copy/cli-sign-in";

const H = 3_600_000;
const D = 24 * H;
const at = (ms: number) => new Date(Date.now() + ms).toISOString();

interface Session {
  id: string;
  machine: string;
  store: "keychain" | "file";
  created_at: string;
  last_refresh_at: string;
  expires_at: string;
  idle_expires_at: string;
  admin_expires_at?: string;
}

const mbp = (): Session => ({
  id: "s-mbp", machine: "mbp-dev", store: "keychain",
  created_at: at(-3 * D), last_refresh_at: at(-12 * 60_000), expires_at: at(27 * D), idle_expires_at: at(7 * D),
  admin_expires_at: at(41.5 * 60_000),
});
const wsl = (): Session => ({
  id: "s-wsl", machine: "wsl-dev", store: "file",
  created_at: at(-2 * D), last_refresh_at: at(-1 * D), expires_at: at(5 * D), idle_expires_at: at(6 * D),
});

// Serves the list from `live` and ends a session on DELETE, as the server does; returns the DELETEs it saw.
async function stubSessions(page: Page, live: Session[]): Promise<string[]> {
  const deleted: string[] = [];
  await page.route("**/api/v1/me/cli-sessions**", async (route) => {
    const req = route.request();
    if (req.method() === "DELETE") {
      const id = new URL(req.url()).pathname.split("/").pop()!;
      deleted.push(id);
      const i = live.findIndex((s) => s.id === id);
      if (i >= 0) live.splice(i, 1);
      return route.fulfill({ status: 204 });
    }
    return route.fulfill({ json: live });
  });
  return deleted;
}

async function openAccount(page: Page): Promise<void> {
  await asRealMember(page);
  await gotoConsole(page, "user");
  await navToRoute(page, "/account");
  await expect(page.getByRole("heading", { name: "Your account", level: 1 })).toBeVisible();
}

const cardOf = (page: Page) => page.getByTestId("signed-in-clis-card");
const toast = (page: Page, text: string) => page.locator("[data-sonner-toast]").filter({ hasText: text });

test.describe("Your account: Signed-in CLIs", () => {
  test("lists each computer with its store, times and admin chip, and a Sign out per device", async ({ page }) => {
    await stubSessions(page, [mbp(), wsl()]);
    await openAccount(page);

    const toggle = page.getByRole("button", { name: /^Signed-in CLIs\s*2 computers/ });
    await expect(toggle).toHaveAttribute("aria-expanded", "false");
    await expandCard(page, T.TITLE);
    const card = cardOf(page);
    await expect(card.getByText(T.LEDE, { exact: true })).toBeVisible();
    const table = card.getByRole("table", { name: T.TABLE_LABEL });
    for (const col of ["Computer", "Store", "Signed in", "Last used", "Ends", "Idle end"]) {
      await expect(table.getByRole("columnheader", { name: col })).toBeVisible();
    }

    const a = table.getByRole("row").filter({ hasText: "mbp-dev" });
    await expect(a).toContainText(T.CHIP);
    await expect(a).toContainText(T.STORE_KEYCHAIN);
    await expect(a).toContainText("3 days ago");
    await expect(a).toContainText("12 minutes ago");
    await expect(a).toContainText("in 27 days");
    await expect(a).toContainText("in 7 days");
    await expect(a).toContainText("Admin access, ends in 42 min");
    await expect(a).not.toContainText(T.FILE_NOTE);

    const b = table.getByRole("row").filter({ hasText: "wsl-dev" });
    await expect(b).toContainText(T.STORE_FILE);
    await expect(b).toContainText(T.FILE_NOTE);
    await expect(b).not.toContainText("Admin access");

    await expect(card.getByRole("button", { name: "Sign out mbp-dev" })).toBeVisible();
    await expect(card.getByRole("button", { name: "Sign out wsl-dev" })).toBeVisible();
    await expect(card.getByText(`${T.RUN_LEAD} ${T.RUN_COMMAND(new URL(page.url()).origin)}`, { exact: true })).toBeVisible();
  });

  test("Sign out asks first, ends only that computer's session, and drops its row", async ({ page }) => {
    const deleted = await stubSessions(page, [mbp(), wsl()]);
    await openAccount(page);
    await expandCard(page, T.TITLE);
    const card = cardOf(page);

    // Cancel ends nothing.
    await card.getByRole("button", { name: "Sign out mbp-dev" }).click();
    let dialog = page.getByRole("alertdialog");
    await expect(dialog.getByText(T.DIALOG_TITLE("mbp-dev"), { exact: true })).toBeVisible();
    await expect(dialog.getByText(T.DIALOG_BODY("mbp-dev"), { exact: true })).toBeVisible();
    await dialog.getByRole("button", { name: "Cancel" }).click();
    await expect(dialog).toHaveCount(0);
    expect(deleted).toEqual([]);

    await card.getByRole("button", { name: "Sign out mbp-dev" }).click();
    dialog = page.getByRole("alertdialog");
    await dialog.getByRole("button", { name: T.SIGN_OUT }).click();

    await expect(toast(page, T.TOAST("mbp-dev"))).toBeVisible();
    expect(deleted).toEqual(["s-mbp"]);
    await expect(card.getByRole("row").filter({ hasText: "mbp-dev" })).toHaveCount(0);
    await expect(card.getByRole("row").filter({ hasText: "wsl-dev" })).toHaveCount(1);
    await expect(page.getByRole("button", { name: /^Signed-in CLIs\s*1 computer/ })).toBeVisible();
  });

  test("stays when empty, says so and shows the command", async ({ page }) => {
    await stubSessions(page, []);
    await openAccount(page);
    await expect(page.getByRole("button", { name: /^Signed-in CLIs\s*No computers signed in/ })).toBeVisible();
    await expandCard(page, T.TITLE);
    const card = cardOf(page);
    await expect(card.getByText(T.EMPTY, { exact: true })).toBeVisible();
    await expect(card.getByRole("table")).toHaveCount(0);
    await expect(card.getByText(T.RUN_COMMAND(new URL(page.url()).origin), { exact: true })).toBeVisible();
  });

  test("is absent where the server does not serve CLI sign-in", async ({ page }) => {
    await page.route("**/api/v1/me/cli-sessions**", (route) => route.fulfill({ status: 404, json: { error: "not found" } }));
    // The card has asked and been told no before the spec looks for it.
    const asked = page.waitForResponse((r) => r.url().includes("/api/v1/me/cli-sessions"));
    await openAccount(page);
    expect((await asked).status()).toBe(404);
    await expect(cardOf(page)).toHaveCount(0);
  });

  test("at phone width each computer is a card with a 44 px Sign out", async ({ page }) => {
    await stubSessions(page, [wsl()]);
    await openAccount(page);
    // After the console has loaded: the sidebar the helper waits for is not drawn at this width.
    await page.setViewportSize({ width: 390, height: 844 });
    await expandCard(page, T.TITLE);
    const signOut = cardOf(page).getByRole("button", { name: "Sign out wsl-dev" });
    await expect(signOut).toBeVisible();
    expect((await signOut.boundingBox())!.height).toBeGreaterThanOrEqual(44);
    const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
    expect(overflow).toBeLessThanOrEqual(0);
  });
});
