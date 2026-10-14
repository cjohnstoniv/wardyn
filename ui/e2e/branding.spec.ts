/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { Browser, Page } from "@playwright/test";
import { consoleAPI, expandCard, expect, gotoConsole, test } from "./fixtures";
import { BRAND_HEADER, BRANDING } from "../src/app/lib/branding-copy";
import { CONSOLE_VIEW } from "../src/app/components/wardyn/copy/console-view";

// Console branding (#1125) — the issue's done-when: "an e2e spec pins a branded
// and an unbranded render" (sign-in + top bar).
//
// The branded half serves the brand through page.route rather than saving one
// to the harness: branding is ONE record for the whole deployment and this
// suite runs fullyParallel, so a real save would rebrand every other spec's
// console mid-run (navigation, auth and view-switch assert the Wardyn
// wordmark and titles). The server half — validation, the store, the logo's
// headers, the SVG rebuild — is internal/api/branding_test.go's and the PG
// store test's. The unbranded half is the real harness, untouched.

const LOGO_URL = "/api/v1/branding/logo?v=e2e";
const LOGO_SVG =
  '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32"><rect width="32" height="32" rx="7" fill="#7c3aed"></rect></svg>';
const BRAND = {
  org_name: "Example Corp",
  name_format: "prefix",
  primary: "#7c3aed",
  primary_text: "#ffffff",
  dark_primary: "#9058f0",
  dark_primary_text: "#0a0a0a",
  logo_url: LOGO_URL,
  icon_url: LOGO_URL,
};

type Routable = Pick<Page, "route">;

async function serveBrand(target: Routable, support?: string) {
  await target.route("**/api/v1/branding", (r) => r.fulfill({ json: BRAND }));
  await target.route("**/api/v1/branding/settings", (r) =>
    r.request().method() === "GET" ? r.fulfill({ json: { ...BRAND, support_url: support } }) : r.fallback(),
  );
  await target.route("**/api/v1/branding/logo*", (r) =>
    r.fulfill({ body: LOGO_SVG, headers: { "Content-Type": "image/svg+xml", "X-Content-Type-Options": "nosniff" } }),
  );
}

// A signed-out visitor: a fresh context carries no stored token.
async function signedOutPage(browser: Browser, baseURL: string | undefined, branded: boolean) {
  const ctx = await browser.newContext({ baseURL });
  if (branded) await serveBrand(ctx);
  const page = await ctx.newPage();
  await page.goto("/");
  await expect(page.getByRole("heading", { name: "Sign in", level: 2 })).toBeVisible();
  return page;
}

const css = (page: Page, prop: string) =>
  page.evaluate((p) => getComputedStyle(document.documentElement).getPropertyValue(p).trim(), prop);
const favicon = (page: Page) => page.locator('link[rel="icon"]').getAttribute("href");

test.describe("unbranded console (the harness as it stands)", () => {
  test("the sign-in page is Wardyn's own", async ({ browser }, testInfo) => {
    const page = await signedOutPage(browser, testInfo.project.use.baseURL, false);
    const read = await page.request.get("/api/v1/branding");
    expect(read.status()).toBe(200);
    expect(await read.json()).toEqual({});
    await expect(page.getByRole("heading", { level: 1 })).toHaveText("Wardyn");
    await expect(page).toHaveTitle("Wardyn");
    expect(await favicon(page)).toBe("/favicon.svg");
    await expect(page.locator("style[data-wardyn-brand]")).toHaveCount(0);
    await page.context().close();
  });

  test("the top bar is Wardyn's own, with no Support link", async ({ page }) => {
    await gotoConsole(page);
    await expect(page.locator("header").getByText("Wardyn", { exact: true })).toBeVisible();
    await expect(page.getByRole("link", { name: BRAND_HEADER.SUPPORT_CHIP })).toHaveCount(0);
    await expect(page).toHaveTitle(CONSOLE_VIEW.TITLE_USER);
  });

  test("a save that breaks a rule is refused with its named reason", async ({ page }) => {
    await gotoConsole(page);
    const res = await consoleAPI(page, "PUT", "/api/v1/branding/settings", {
      org_name: "Example Corp", name_format: "prefix", primary: "#7c3aed", primary_text: "#ffffff",
      support_url: "http://status.example.com",
    });
    expect(res.status).toBe(400);
    expect(JSON.parse(res.text)).toMatchObject({ reason: "link_not_https" });
  });
});

test.describe("branded console", () => {
  test("the sign-in page shows the org's logo, name, colours, tab title and icon", async ({ browser }, testInfo) => {
    const page = await signedOutPage(browser, testInfo.project.use.baseURL, true);
    const danger = await css(page, "--danger");
    await expect(page.getByRole("heading", { level: 1 })).toHaveText("Example Corp Wardyn");
    const logo = page.locator(`img[src="${LOGO_URL}"]`);
    await expect(logo).toBeVisible();
    expect(await logo.evaluate((img: HTMLImageElement) => img.naturalWidth)).toBeGreaterThan(0);
    await expect(page).toHaveTitle("Example Corp Wardyn");
    expect(await favicon(page)).toBe(LOGO_URL);
    // Dark is the default theme: the dark pair is in effect; light takes the light pair.
    expect(await css(page, "--primary")).toBe("#9058f0");
    await page.getByRole("button", { name: "Toggle theme" }).click();
    expect(await css(page, "--primary")).toBe("#7c3aed");
    expect(await css(page, "--primary-foreground")).toBe("#ffffff");
    // Only the primary pair is brandable (B-3).
    await page.getByRole("button", { name: "Toggle theme" }).click();
    expect(await css(page, "--danger")).toBe(danger);
    await page.context().close();
  });

  test("the top bar carries the name and one https Support link, in a new tab", async ({ page }) => {
    await serveBrand(page, "https://status.example.com");
    await gotoConsole(page);
    await expect(page.locator("header").getByText("Example Corp Wardyn", { exact: true })).toBeVisible();
    const support = page.getByRole("link", { name: BRAND_HEADER.SUPPORT_CHIP });
    await expect(support).toHaveAttribute("href", "https://status.example.com");
    await expect(support).toHaveAttribute("target", "_blank");
    await expect(support).toHaveAttribute("rel", "noopener noreferrer");
    await expect(page).toHaveTitle("Example Corp Wardyn");
  });

  test("the Admin view keeps its cue in the tab title, and Settings has the Branding card", async ({ page }) => {
    await serveBrand(page);
    await gotoConsole(page, "admin");
    await expect(page).toHaveTitle("Example Corp Wardyn admin");
    await page.goto("/admin/settings");
    await expect(page.getByRole("heading", { name: new RegExp(`^${BRANDING.TITLE}( |$)`), level: 3 })).toBeVisible();
    await expandCard(page, BRANDING.TITLE);
    await expect(page.getByLabel(BRANDING.ORG_NAME_LABEL)).toHaveValue("Example Corp");
  });
});

// #1215 — Remove logo and Remove branding. The two server doors (PUT with
// remove_logo, DELETE) are internal/api/branding_test.go's; like the branded
// half above, this routes them so the suite's one real branding record is
// never touched. What it pins is the console half: each removal asks first,
// sends the right request, and toasts.
test.describe("branding removal controls", () => {
  test("Remove logo and Remove branding each confirm, act and toast", async ({ page }) => {
    let put: Record<string, unknown> | null = null;
    let deleted = 0;
    await serveBrand(page, "https://status.example.com");
    await page.route("**/api/v1/branding/settings", (r) => {
      const method = r.request().method();
      if (method === "PUT") {
        put = r.request().postDataJSON();
        return r.fulfill({ json: { ...BRAND, logo_url: undefined, dark_custom: true } });
      }
      if (method === "DELETE") {
        deleted++;
        return r.fulfill({ status: 204 });
      }
      return r.fallback();
    });
    await gotoConsole(page, "admin");
    await page.goto("/admin/settings");
    await expandCard(page, BRANDING.TITLE);

    await page.getByRole("button", { name: BRANDING.REMOVE_LOGO }).click();
    const dialog = page.getByRole("alertdialog");
    await expect(dialog.getByText(BRANDING.REMOVE_LOGO_TITLE)).toBeVisible();
    await expect(dialog.getByText(BRANDING.REMOVE_LOGO_BODY("Example Corp"))).toBeVisible();
    expect(put).toBeNull();
    await dialog.getByRole("button", { name: BRANDING.REMOVE_LOGO_CONFIRM }).click();
    await expect(page.getByText(BRANDING.REMOVE_LOGO_TOAST)).toBeVisible();
    expect(put).toMatchObject({ org_name: "Example Corp", remove_logo: true });
    await expect(page.getByRole("button", { name: BRANDING.REMOVE_LOGO })).toHaveCount(0);
    // The first dialog is still animating out; the same locator would match it.
    await expect(dialog).toHaveCount(0);

    await page.getByRole("button", { name: BRANDING.REMOVE_BRANDING }).click();
    await expect(dialog.getByText(BRANDING.REMOVE_BRANDING_TITLE)).toBeVisible();
    await expect(dialog.getByText(BRANDING.REMOVE_BRANDING_BODY)).toBeVisible();
    expect(deleted).toBe(0);
    await dialog.getByRole("button", { name: BRANDING.REMOVE_BRANDING_CONFIRM }).click();
    await expect(page.getByText(BRANDING.REMOVE_BRANDING_TOAST)).toBeVisible();
    expect(deleted).toBe(1);
    await expect(page.getByLabel(BRANDING.ORG_NAME_LABEL)).toHaveValue("");
  });
});
