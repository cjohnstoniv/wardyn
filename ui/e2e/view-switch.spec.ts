/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { BrowserContext, Page } from "@playwright/test";
import { test, expect, gotoConsole, mockMemberRole, navToRoute, sidebarLink, type NavLabel } from "./fixtures";
import { CONSOLE_VIEW, USER_PREVIEW } from "../src/app/components/wardyn/copy/console-view";
import { UNSAVED_GUARD } from "../src/app/components/wardyn/copy";
import { PROVIDERS, PROVIDERS_DRAFT } from "../src/app/lib/workspace-providers-copy";

// M-2 — the Console view switch and the per-view chrome
// (admin-member-modes-design.md §2.2, §2.4, §3; packet M-A).
//
// The seeded backend authenticates with a bare admin bearer and no identity
// provider, which is a single-operator install: the switch only navigates. An
// SSO admin is a stateful splice at the CONTEXT level, so every tab in it sees
// one session: POST /me/view flips the state and /me answers from it,
// clamped as the server clamps (role user, both tiers false). What the
// server itself refuses inside the User view is pinned in Go
// (membermode_test.go's TestMemberMode_DeniedOnEveryOperatorOnlyRoute) and on a
// real OIDC session in live/sso-member.spec.ts.

interface Session {
  userView: boolean;
  posts: unknown[];
  failNext: boolean;
}

async function ssoAdminSession(context: BrowserContext, extra: Record<string, unknown> = {}): Promise<Session> {
  const session: Session = { userView: false, posts: [], failNext: false };
  await context.route("**/api/v1/me", async (route) => {
    const response = await route.fetch();
    const json = await response.json();
    Object.assign(json, { method: "sso", user_view: session.userView }, extra);
    if (session.userView) Object.assign(json, { role: "user", operator: false, security_operator: false });
    await route.fulfill({ response, json });
  });
  await context.route("**/api/v1/me/view", async (route) => {
    const body = route.request().postDataJSON() as { view: string; no_credential?: boolean };
    session.posts.push(body);
    if (session.failNext) {
      session.failNext = false;
      await route.fulfill({ status: 500, contentType: "application/json", body: JSON.stringify({ error: "boom" }) });
      return;
    }
    session.userView = body.view === "user";
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ user_view: session.userView }) });
  });
  return session;
}

const views = (page: Page) => page.getByRole("banner").getByRole("group", { name: CONSOLE_VIEW.GROUP });
const segment = (page: Page, name: string) => views(page).getByRole("button", { name });

async function expectAdminChrome(page: Page) {
  await expect(segment(page, CONSOLE_VIEW.ADMIN)).toHaveAttribute("aria-pressed", "true");
  await expect(page.locator("aside").getByText(CONSOLE_VIEW.EYEBROW_ADMIN, { exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: "New run" })).toHaveCount(0);
  for (const label of ["Policies", "Permissions", "Audit", "Setup", "Settings"] as NavLabel[]) {
    await expect(sidebarLink(page, label)).toBeVisible();
  }
  await expect(page).toHaveTitle(CONSOLE_VIEW.TITLE_ADMIN);
}

async function expectUserChrome(page: Page) {
  await expect(segment(page, CONSOLE_VIEW.USER)).toHaveAttribute("aria-pressed", "true");
  await expect(page.locator("aside").getByText(CONSOLE_VIEW.EYEBROW_ADMIN, { exact: true })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "New run" })).toBeVisible();
  for (const label of ["Getting started", "Your account"] as NavLabel[]) {
    await expect(sidebarLink(page, label)).toBeVisible();
  }
  for (const label of ["Policies", "Permissions", "Audit", "Settings"] as NavLabel[]) {
    await expect(sidebarLink(page, label)).toHaveCount(0);
  }
  await expect(page).toHaveTitle(CONSOLE_VIEW.TITLE_USER);
}

test.describe("the view switch", () => {
  test("a round trip: Admin view → User view → Admin view, each a POST and a reload", async ({ page, context }) => {
    const session = await ssoAdminSession(context);
    await gotoConsole(page, "admin");
    await expectAdminChrome(page);

    await segment(page, CONSOLE_VIEW.USER).click();
    await expect(page).toHaveURL(/\/runs$/);
    await expect(page).not.toHaveURL(/\/admin\//);
    await expectUserChrome(page);

    await segment(page, CONSOLE_VIEW.ADMIN).click();
    await expect(page).toHaveURL(/\/admin\/runs$/);
    await expectAdminChrome(page);
    expect(session.posts).toEqual([{ view: "user" }, { view: "admin" }]);
  });

  test("a failed switch says so beside the switch and stays put", async ({ page, context }) => {
    const session = await ssoAdminSession(context);
    await gotoConsole(page, "admin");
    session.failNext = true;
    await segment(page, CONSOLE_VIEW.USER).click();
    await expect(page.getByRole("alert").getByText(CONSOLE_VIEW.SWITCH_FAILED)).toBeVisible();
    await expect(page).toHaveURL(/\/admin\/runs$/);
    await expect(segment(page, CONSOLE_VIEW.ADMIN)).toHaveAttribute("aria-pressed", "true");
  });

  test("the unsaved guard asks first, and Keep editing changes nothing", async ({ page, context }) => {
    const session = await ssoAdminSession(context);
    await gotoConsole(page, "admin");
    await navToRoute(page, "/admin/providers");
    // A fresh backend's registry is empty: adding the row starts the draft.
    await page.getByRole("button", { name: PROVIDERS.ADD_ROW_CTA }).first().click();
    const row = page.getByTestId("provider-row-github");
    await row.locator("textarea").fill("https://github.com/acme\nhttps://git.corp.example/team");
    await expect(page.getByTestId("unsaved-marker")).toHaveText(PROVIDERS_DRAFT.UNSAVED_MARKER);

    await segment(page, CONSOLE_VIEW.USER).click();
    const dialog = page.getByRole("alertdialog");
    await expect(dialog.getByText(UNSAVED_GUARD.TITLE)).toBeVisible();
    await dialog.getByRole("button", { name: UNSAVED_GUARD.STAY }).click();
    await expect(page).toHaveURL(/\/admin\/providers$/);
    expect(session.posts).toEqual([]);

    // Answered the other way it switches, with no second (browser) prompt.
    await segment(page, CONSOLE_VIEW.USER).click();
    await page.getByRole("alertdialog").getByRole("button", { name: UNSAVED_GUARD.LEAVE }).click();
    await expect(page).toHaveURL(/\/runs$/);
    expect(session.posts).toEqual([{ view: "user" }]);
  });

  test("cross-tab: switch in tab A, and tab B reloads into the same view", async ({ page, context }) => {
    await ssoAdminSession(context);
    // Tab A first: its init script stores the harness token, which tab B then
    // shares through the origin's storage.
    await gotoConsole(page, "admin");
    const other = await context.newPage();
    await gotoConsole(other, "admin");
    await navToRoute(other, "/admin/runs?keep=1#here");
    await expect(segment(other, CONSOLE_VIEW.ADMIN)).toHaveAttribute("aria-pressed", "true");

    await segment(page, CONSOLE_VIEW.USER).click();
    await expect(page).toHaveURL(/\/runs$/);
    // /admin/runs has a twin, so tab B lands on the same object in the User
    // view, search and hash kept as ViewGate's own twin redirect keeps them.
    await expect(other).toHaveURL(/\/runs\?keep=1#here$/);
    await expect(other).not.toHaveURL(/\/admin\//);
    await expectUserChrome(other);
  });

  test("a 403 in a tab left in the old view re-reads the session and follows it", async ({ page, context }) => {
    const session = await ssoAdminSession(context);
    await gotoConsole(page, "admin");
    // Another device switched the session; this tab heard nothing.
    session.userView = true;
    await page.route("**/api/v1/policies*", (route) =>
      route.fulfill({ status: 403, contentType: "application/json", body: JSON.stringify({ error: "forbidden" }) }),
    );
    await sidebarLink(page, "Policies").click();
    await expect(page).toHaveURL(/\/runs$/);
    await expectUserChrome(page);
  });

  test("a single-operator install switches by URL alone", async ({ page }) => {
    const posts: string[] = [];
    await page.route("**/api/v1/me/view", (route) => {
      posts.push(route.request().url());
      return route.continue();
    });
    await gotoConsole(page);
    await expectUserChrome(page);
    await segment(page, CONSOLE_VIEW.ADMIN).click();
    await expect(page).toHaveURL(/\/admin\/runs$/);
    await expectAdminChrome(page);
    await segment(page, CONSOLE_VIEW.USER).click();
    await expect(page).toHaveURL(/\/runs$/);
    expect(posts).toEqual([]);
  });

  test("below the small breakpoint the switch moves into the navigation sheet", async ({ page }) => {
    // After landing: gotoConsole waits on the desktop sidebar, hidden below md.
    await gotoConsole(page);
    await page.setViewportSize({ width: 390, height: 800 });
    await expect(views(page)).toBeHidden();
    await expect(page.getByRole("button", { name: "New run" })).toBeVisible();
    await page.getByRole("button", { name: "Open navigation menu" }).click();
    const sheet = page.getByRole("dialog");
    await expect(sheet.getByRole("group", { name: CONSOLE_VIEW.GROUP })).toBeVisible();
    // Single-operator: the switch only navigates, so the sheet closes itself
    // the way its links do.
    await sheet.getByRole("button", { name: CONSOLE_VIEW.ADMIN }).click();
    await expect(page).toHaveURL(/\/admin\/runs$/);
    await expect(sheet).toBeHidden();
  });
});

test.describe("who sees the switch", () => {
  test("a user has one view: no switch, no eyebrow, the User-view nav", async ({ page }) => {
    await mockMemberRole(page);
    await gotoConsole(page);
    await expect(page.getByRole("group", { name: CONSOLE_VIEW.GROUP })).toHaveCount(0);
    await expect(page.locator("aside").getByText(CONSOLE_VIEW.EYEBROW_ADMIN, { exact: true })).toHaveCount(0);
    await expect(sidebarLink(page, "Your account")).toBeVisible();
  });

  test("the admin token on an SSO install: no switch, and the eyebrow still names the view", async ({ page }) => {
    await page.route("**/healthz", async (route) => {
      const response = await route.fetch();
      const json = await response.json();
      json.sso = true;
      await route.fulfill({ response, json });
    });
    await gotoConsole(page, "admin");
    await expect(page.getByRole("group", { name: CONSOLE_VIEW.GROUP })).toHaveCount(0);
    await expect(page.locator("aside").getByText(CONSOLE_VIEW.EYEBROW_ADMIN, { exact: true })).toBeVisible();
    await expect(page.getByRole("button", { name: "New run" })).toHaveCount(0);
  });

  test("a security admin's Admin view: Drives joins; Secrets, Recordings, Setup and Settings do not", async ({ page, context }) => {
    await ssoAdminSession(context, { role: "security_admin", operator: false, security_operator: true });
    await gotoConsole(page, "admin");
    for (const label of ["Runs", "Approvals", "Workspaces", "Policies", "Governance", "Permissions", "Drives", "Audit"] as NavLabel[]) {
      await expect(sidebarLink(page, label)).toBeVisible();
    }
    for (const label of ["Secrets", "Recordings", "Setup", "Settings"] as NavLabel[]) {
      await expect(sidebarLink(page, label)).toHaveCount(0);
    }
  });
});

test.describe("the slimmed avatar menu and the preview", () => {
  test("the avatar menu holds identity and Sign out only", async ({ page }) => {
    await gotoConsole(page);
    await page.locator("header").getByRole("button").last().click();
    const items = page.getByRole("menu").getByRole("menuitem");
    await expect(items).toHaveCount(1);
    await expect(items).toHaveText(/Sign out/);
  });

  test("Preview as a new user sits on the Permissions header; its band is the way out", async ({ page, context }) => {
    const session = await ssoAdminSession(context, { user_preview_available: true });
    await context.route("**/api/v1/me", async (route) => {
      const response = await route.fetch();
      const json = await response.json();
      Object.assign(json, { method: "sso", user_view: session.userView, user_preview_available: !session.userView });
      if (session.userView) {
        Object.assign(json, { role: "user", operator: false, security_operator: false, user_view_no_credential: true });
      }
      await route.fulfill({ response, json });
    });
    await gotoConsole(page, "admin");
    await sidebarLink(page, "Permissions").click();
    await page.getByRole("button", { name: USER_PREVIEW.MENU_NEW }).click();
    await expect(page).toHaveURL(/\/runs$/);
    await expect(page.getByText(USER_PREVIEW.BANNER)).toBeVisible();

    await page.getByRole("button", { name: USER_PREVIEW.EXIT }).click();
    await expect(page).toHaveURL(/\/admin\/runs$/);
    await expect(page.getByText(USER_PREVIEW.BANNER)).toHaveCount(0);
    expect(session.posts).toEqual([{ view: "user", no_credential: true }, { view: "admin" }]);
  });

  test("the plain User view has no band", async ({ page, context }) => {
    const session = await ssoAdminSession(context);
    session.userView = true;
    await gotoConsole(page);
    await expect(segment(page, CONSOLE_VIEW.USER)).toHaveAttribute("aria-pressed", "true");
    await expect(page.getByText(/Viewing as/)).toHaveCount(0);
  });
});
