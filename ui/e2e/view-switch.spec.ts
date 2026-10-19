/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { BrowserContext, Page } from "@playwright/test";
import { test, expect, gotoConsole, mockMemberRole, navToRoute, sidebarLink, type NavLabel } from "./fixtures";
import { CONSOLE_VIEW, USER_PREVIEW, VIEW_DROPPED, VIEW_TO_ADMIN } from "../src/app/components/wardyn/copy/console-view";
import { NO_BARRIER, UNSAVED_GUARD } from "../src/app/components/wardyn/copy";
import { PROVIDERS, PROVIDERS_EXTRA } from "../src/app/lib/workspace-providers-copy";

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
  // The chosen type's id, persisted for as long as this mocked session lives
  // (across a reload too, since it's a JS closure the routes keep reading) —
  // #912's own "the choice persists" pin needs this remembered somewhere,
  // and the real backend's equivalent is the session cookie.
  viewType: string | null;
  posts: unknown[];
  failNext: boolean;
}

// user_view_types (#912, H2): /me carries the org's type list ONLY for a
// caller whose STAMPED role is admin or security_admin — never from GET
// /user-types, which a clamped in-view session cannot reach (pinned in Go,
// internal/api/user_view_test.go's TestMeUserViewTypes). Passed here as
// `extra.user_view_types` so a picker test can hand the mock a fixed roster
// with no real POST /user-types write needed.
//
// `stampedRole` is the role stamped on the signed cookie: while the view is on
// /me clamps every tier field but reports only whether that stamp is a super
// admin (user_view_super_admin, #1335) — a security admin reads false.
async function ssoAdminSession(
  context: BrowserContext,
  extra: Record<string, unknown> = {},
  stampedRole: "admin" | "security_admin" = "admin",
): Promise<Session> {
  const session: Session = { userView: false, viewType: null, posts: [], failNext: false };
  const types = (extra.user_view_types as { id: string; name: string }[] | undefined) ?? [];
  // CACHE-AND-SERVE the real fetch, not route.fetch()+refulfill per match — a
  // real round trip PER match races Playwright disposing an in-flight route's
  // response ("apiResponse.json: Response has been disposed"). One real
  // fetch caches the base body; every match still re-derives its own answer
  // from the mutable `session` above (the view can flip between calls), just
  // layered onto the cached base instead of a fresh network round trip.
  let cachedBase: Record<string, unknown> | null = null;
  await context.route("**/api/v1/me", async (route) => {
    if (!cachedBase) {
      cachedBase = (await (await route.fetch()).json()) as Record<string, unknown>;
    }
    const json: Record<string, unknown> = { ...cachedBase };
    Object.assign(json, { method: "sso", user_view: session.userView }, extra);
    if (session.userView) {
      Object.assign(json, {
        role: "user",
        operator: false,
        security_operator: false,
        user_view_super_admin: stampedRole === "admin",
      });
      const chosen = types.find((t) => t.id === session.viewType);
      if (chosen) json.user_type = chosen;
    }
    await route.fulfill({ json });
  });
  await context.route("**/api/v1/me/view", async (route) => {
    const body = route.request().postDataJSON() as { view: string; no_credential?: boolean; user_type?: string };
    session.posts.push(body);
    if (session.failNext) {
      session.failNext = false;
      await route.fulfill({ status: 500, contentType: "application/json", body: JSON.stringify({ error: "boom" }) });
      return;
    }
    session.userView = body.view === "user";
    if (session.userView && body.user_type) session.viewType = body.user_type;
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
    await expect(page.getByTestId("unsaved-marker")).toHaveText(PROVIDERS_EXTRA.UNSAVED_MARKER);

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

test.describe("the type picker (#912)", () => {
  test("with two or more types, the User segment opens a menu; entering names the picked type on the wire", async ({
    page,
    context,
  }) => {
    const session = await ssoAdminSession(context, {
      user_view_types: [
        { id: "e2e-vs-pm", name: "Portfolio manager" },
        { id: "e2e-vs-dev", name: "Developer" },
      ],
    });
    await gotoConsole(page, "admin");

    await segment(page, CONSOLE_VIEW.USER).click();
    await page.getByRole("menuitem", { name: "Developer" }).click();
    await expect(page).toHaveURL(/\/runs$/);
    // Waits for the chrome before reading posts: switchView's reload lands on
    // the SAME "/runs" this test started on, so a bare URL match can pass
    // before the round trip that populates it ever ran.
    await expectUserChrome(page);
    await expect.poll(() => session.posts).toEqual([{ view: "user", user_type: "e2e-vs-dev" }]);
  });

  test("the eyebrow reopens the picker; choosing another type re-enters without leaving the view", async ({ page, context }) => {
    const session = await ssoAdminSession(context, {
      user_view_types: [
        { id: "e2e-vs-pm2", name: "Portfolio manager 2" },
        { id: "e2e-vs-dev2", name: "Developer 2" },
      ],
    });
    session.userView = true;
    session.viewType = "e2e-vs-pm2";
    await gotoConsole(page);
    await expectUserChrome(page);

    const eyebrow = page.locator("aside").getByRole("button", { name: CONSOLE_VIEW.EYEBROW_USER("Portfolio manager 2") });
    await expect(eyebrow).toBeVisible();
    await eyebrow.click();
    await page.getByRole("menuitem", { name: "Developer 2" }).click();
    await expect(page).toHaveURL(/\/runs$/);
    await expectUserChrome(page);
    await expect.poll(() => session.posts).toEqual([{ view: "user", user_type: "e2e-vs-dev2" }]);
  });

  test("a dropped type shows its NAME, not its id; Choose another type re-enters as the picked one", async ({ page, context }) => {
    const session = await ssoAdminSession(context, {
      user_view_types: [{ id: "e2e-vs-analyst", name: "Analyst" }],
      user_view_dropped: { user_type: "e2e-vs-contractor", user_type_name: "Contractor", reason: "deleted" },
    });
    await gotoConsole(page, "admin");

    const notice = page.getByRole("status").filter({ hasText: VIEW_DROPPED.BODY("Contractor") });
    await expect(notice).toBeVisible();
    await notice.getByRole("button", { name: VIEW_DROPPED.CHOOSE_ANOTHER }).click();
    await page.getByRole("menuitem", { name: "Analyst" }).click();
    await expect(page).toHaveURL(/\/runs$/);
    await expectUserChrome(page);
    await expect.poll(() => session.posts).toEqual([{ view: "user", user_type: "e2e-vs-analyst" }]);
  });

  // M3: the admin's choice survives a reload (the server side of this is
  // pinned in Go — internal/api/user_view_test.go's
  // TestUserViewSwitchValidatesAndRemembersTheType, a fresh session of the
  // same principal preselecting the remembered choice). This proves the
  // CONSOLE reads it back correctly: the eyebrow still names the chosen type
  // after the page reloads, not just right after the click.
  test("the choice persists across a reload", async ({ page, context }) => {
    const session = await ssoAdminSession(context, {
      user_view_types: [
        { id: "e2e-vs-pm3", name: "Portfolio manager 3" },
        { id: "e2e-vs-dev3", name: "Developer 3" },
      ],
    });
    await gotoConsole(page, "admin");
    await segment(page, CONSOLE_VIEW.USER).click();
    await page.getByRole("menuitem", { name: "Developer 3" }).click();
    await expectUserChrome(page);
    await expect.poll(() => session.posts).toEqual([{ view: "user", user_type: "e2e-vs-dev3" }]);

    await page.reload();
    await expectUserChrome(page);
    await expect(
      page.locator("aside").getByRole("button", { name: CONSOLE_VIEW.EYEBROW_USER("Developer 3") }),
    ).toBeVisible();
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
    // Cache-and-serve (see ssoAdminSession): one real fetch, every match served from it.
    let healthz: Record<string, unknown> | null = null;
    await page.route("**/healthz", async (route) => {
      if (!healthz) {
        healthz = (await (await route.fetch()).json()) as Record<string, unknown>;
        healthz.sso = true;
      }
      await route.fulfill({ json: healthz });
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
    // Cache-and-serve the real fetch (see ssoAdminSession); each match still
    // re-derives its answer from the mutable session onto a copy of the base.
    let meBase: Record<string, unknown> | null = null;
    await context.route("**/api/v1/me", async (route) => {
      if (!meBase) meBase = (await (await route.fetch()).json()) as Record<string, unknown>;
      const json: Record<string, unknown> = { ...meBase };
      Object.assign(json, { method: "sso", user_view: session.userView, user_preview_available: !session.userView });
      if (session.userView) {
        Object.assign(json, { role: "user", operator: false, security_operator: false, user_view_no_credential: true });
      }
      await route.fulfill({ json });
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

// #1328 review round 2, R2-1 — a session-user (an SSO admin who switched to
// the User view) is clamped exactly like a plain member (role "user",
// operator false), so #214's no-barrier CTA cannot gate on meta.operator
// alone for them; it gates on access === "session-user" AND /me's
// user_view_super_admin (#1335: a security admin in the view is session-user
// too, and /admin/setup would refuse them) instead, routes to
// the SAME /admin/setup?step=environment an operator uses, and leaves
// entering admin authority to ViewGate's own "to-admin" click (never a
// silent redirect). The click's own target already carries the full
// pathname+search (console-view.tsx's ViewInterstitial), so `?step=
// environment` needs no extra plumbing to survive the switch.
// The same probe for both tiers, because the setup that makes the link render is
// identical: a settled, empty barrier probe (context-level, so it survives the
// full reload switchView triggers).
async function noBarrierProbe(context: BrowserContext) {
  // Cache-and-serve (see ssoAdminSession): the console re-reads this on every
  // poll, so a real round trip per match would race the response disposal.
  let setupStatus: Record<string, unknown> | null = null;
  await context.route("**/api/v1/setup/status*", async (route) => {
    if (!setupStatus) {
      const body = (await (await route.fetch()).json()) as Record<string, unknown>;
      body.runner = { ...(body.runner as object), driver: "docker", confinement_classes: [] };
      setupStatus = body;
    }
    await route.fulfill({ json: setupStatus });
  });
}

test.describe("#214 no-barrier CTA: the to-admin click keeps ?step=environment", () => {
  test("a session-user's CTA still reaches the Environment step, through the switch prompt", async ({
    page,
    context,
  }) => {
    const session = await ssoAdminSession(context);
    session.userView = true;
    // The Admin Setup funnel shows the welcome hero first until this is set
    // (confinement-posture.spec.ts's own precedent for reaching this step
    // directly).
    await page.addInitScript(() => {
      try {
        localStorage.setItem("wardyn-onboarding-seen", "1");
      } catch {
        /* private mode — ignore */
      }
    });
    // A settled, empty probe — the deployment genuinely has no barrier, so
    // the CTA this test clicks actually renders.
    await noBarrierProbe(context);
    await gotoConsole(page);

    // Scoped with .first(): the banner and the top bar both carry this CTA.
    await page.getByRole("link", { name: NO_BARRIER.CTA }).first().click();
    // The client-side navigation already lands the URL bar here — ViewGate
    // renders the interstitial INSTEAD of the Outlet, not a redirect away.
    await expect(page).toHaveURL(/\/admin\/setup\?step=environment$/);
    await expect(page.getByRole("heading", { name: VIEW_TO_ADMIN.TITLE })).toBeVisible();

    await page.getByRole("button", { name: VIEW_TO_ADMIN.GO }).click();
    await expect(page).toHaveURL(/\/admin\/setup\?step=environment$/);
    await expect(page.getByRole("heading", { name: "Pick your barrier", level: 2 })).toBeVisible();
  });

  // #1335 — a security admin in the User view is a session-user too, but
  // /admin/setup is super-admin-only: the reason shows with no link at either
  // site (shell banner, top bar), and nothing sends them to the member recap.
  test("a security admin in the User view reads the reason with no link", async ({ page, context }) => {
    const session = await ssoAdminSession(context, {}, "security_admin");
    session.userView = true;
    await noBarrierProbe(context);
    await gotoConsole(page);

    await expect(page.getByText(NO_BARRIER.BANNER_TITLE)).toBeVisible();
    await expect(page.getByRole("link", { name: NO_BARRIER.CTA })).toHaveCount(0);
  });
});
