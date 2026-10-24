/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { Locator, Page } from "@playwright/test";
import { test, expect, gotoConsole, ignoreMissingRunner, navToRoute, openRepositoryPicker, walkGuidedToFiles } from "./fixtures";
import { NR_SHARED } from "../src/app/components/wardyn/copy/new-run/shared";

// #2865: in dark a disabled primary button read as enabled (a 50% teal block). Filled variants now turn grey in
// dark when disabled; light is unchanged. Computed colours are compared with the same tokens drawn on a probe.
async function disabledContinue(page: Page) {
  // The seeded backend holds no repository, so the Add dialog's list is stubbed with one (the list GET only).
  await page.route("**/api/v1/workspaces?*", (route) =>
    route.request().method() === "GET"
      ? route.fulfill({
          json: [
            {
              id: "f1111111-1111-4111-8111-111111111111",
              name: "Acme",
              kind: "",
              source: "",
              available_to_you: true,
              sources: [{ type: "repo", source: "https://github.com/acme/web.git", ref: "main", clone_target: "/home/agent/work/web" }],
            },
          ],
        })
      : route.fallback(),
  );
  await gotoConsole(page);
  await ignoreMissingRunner(page);
  await navToRoute(page, "/runs/new");
  await walkGuidedToFiles(page);
  const dialog = await openRepositoryPicker(page);
  // Nothing chosen yet: the dialog's primary Continue cannot be pressed.
  const button = dialog.getByRole("button", { name: NR_SHARED.CONTINUE, exact: true });
  await expect(button).toBeDisabled();
  return button;
}

async function colours(button: Locator) {
  return button.evaluate((el) => {
    const probe = (cls: string, prop: "backgroundColor" | "color") => {
      const p = document.createElement("div");
      p.className = cls;
      document.body.appendChild(p);
      const v = getComputedStyle(p)[prop];
      p.remove();
      return v;
    };
    const cs = getComputedStyle(el);
    return {
      bg: cs.backgroundColor,
      fg: cs.color,
      muted: probe("bg-muted", "backgroundColor"),
      mutedFg: probe("text-muted-foreground", "color"),
      primary: probe("bg-primary", "backgroundColor"),
    };
  });
}

test.describe("dark", () => {
  test.use({ colorScheme: "dark" });
  test("a disabled primary Continue is the muted grey, not the teal fill", async ({ page }) => {
    const c = await colours(await disabledContinue(page));
    expect(c.bg).toBe(c.muted);
    expect(c.fg).toBe(c.mutedFg);
    expect(c.bg).not.toBe(c.primary);
  });
});

test.describe("light", () => {
  test.use({ colorScheme: "light" });
  test("a disabled primary Continue keeps the primary fill", async ({ page }) => {
    await page.addInitScript(() => localStorage.setItem("wardyn-theme", "light"));
    const c = await colours(await disabledContinue(page));
    expect(c.bg).toBe(c.primary);
  });
});
