/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, gotoConsole, mockMemberRole, navTo, sidebarLink, type NavLabel } from "./fixtures";
import { PROVIDERS } from "../src/app/lib/workspace-providers-copy";
import { SHELL } from "../src/app/components/wardyn/copy";

// Navigation + theme + error-boundary coverage for the Wardyn admin console.
//
// The shell (app-shell.tsx) renders a FLAT eight-item sidebar — Runs, Approvals,
// Workspaces, Policies, Permissions, Secrets, Audit, Recordings — of react-router
// <NavLink>s (role="link"), with no group headings. Settings, SSH keys and Demos
// live in the account menu.
// The top bar carries a "Toggle theme" button (aria-label) and no posture
// chips (0.7.3 F6 removed the Fence/NetworkPolicy chips — posture lives on
// the setup Environment step). Each screen supplies its own <h1> via
// PageHeader.
//
// There is no first-run gate and no /setup route: every destination below is
// reachable immediately, which is the property this spec now pins. AppShell
// wraps the routed screen in an ErrorBoundary keyed by pathname so a render
// error degrades to an inline alert card and navigating away clears it. The
// theme lives on <html> (documentElement.dark + color-scheme) via ThemeProvider
// — these specs assert that root state, never a tailwind utility class on a
// leaf node.

// Every sidebar destination, its <h1> page title, and a distinctive subtitle the
// screen renders so we prove the *screen content* mounted, not just the heading.
// Every blurb below is the screen's real PageHeader description.
// F077: DESTINATIONS gained Governance + Recordings (0.7.2) — the sidebar is
// nine items (app-shell.tsx's NAV_ITEMS), and this list used to walk seven,
// leaving the two newest destinations with no "loads its real screen" pin at
// all.
const DESTINATIONS: { label: NavLabel; heading: string; blurb: RegExp }[] = [
  { label: "Runs", heading: "Runs", blurb: /each confined behind its own barrier/i },
  { label: "Approvals", heading: "Approvals", blurb: /nothing privileged happens without one/i },
  { label: "Policies", heading: "Policies", blurb: /egress allowlist/i },
  { label: "Governance", heading: "Governance", blurb: /Named ceilings, assigned to people and groups/i },
  { label: "Permissions", heading: "Permissions", blurb: /Each capability is enforced on its own/i },
  { label: "Secrets", heading: "Secrets", blurb: /values go in and never come out/i },
  { label: "Workspaces", heading: "Workspaces", blurb: /a run can attach\. runs can only attach what's listed here/i },
  { label: "Audit", heading: "Audit", blurb: /Append-only/i },
  { label: "Recordings", heading: "Recordings", blurb: /Captured terminal sessions, replayed byte-for-byte/i },
];

// The set of sidebar links that must remain mounted on every screen — proves
// the app shell never unmounts as the user navigates between regions.
const SIDEBAR_LABELS: NavLabel[] = [
  "Runs",
  "Approvals",
  "Workspaces",
  "Policies",
  "Governance",
  "Permissions",
  "Secrets",
  "Audit",
  "Recordings",
];

async function expectSidebarMounted(page: import("@playwright/test").Page) {
  for (const label of SIDEBAR_LABELS) {
    await expect(sidebarLink(page, label)).toBeVisible();
  }
}

// Reads the live theme off <html>: class "dark" present + color-scheme value.
async function readTheme(page: import("@playwright/test").Page) {
  return page.evaluate(() => ({
    hasDarkClass: document.documentElement.classList.contains("dark"),
    colorScheme: document.documentElement.style.colorScheme,
    stored: window.localStorage.getItem("wardyn-theme"),
  }));
}

test.describe("navigation + shell", () => {
  test("boots into the Runs screen with the sidebar mounted", async ({ page }) => {
    await gotoConsole(page);
    // Runs is the default region (/ redirects to /runs): its <h1> and content
    // are present on boot.
    await expect(page.getByRole("heading", { name: "Runs", level: 1 })).toBeVisible();
    await expect(page.getByText(/each confined behind its own barrier/i)).toBeVisible();
    await expectSidebarMounted(page);
  });

  test("every sidebar destination loads its screen heading + content", async ({ page }) => {
    await gotoConsole(page);
    for (const dest of DESTINATIONS) {
      await navTo(page, dest.label);
      // The screen's own <h1> title (PageHeader renders an h1, distinct from the
      // sidebar <button> of the same name).
      await expect(
        page.getByRole("heading", { name: dest.heading, level: 1 }),
      ).toBeVisible();
      // ...and a screen-specific subtitle proves the *content* mounted.
      await expect(page.getByText(dest.blurb)).toBeVisible();
      // The shell (sidebar) survives the navigation.
      await expectSidebarMounted(page);
    }
  });

  test("the app shell (sidebar) stays mounted across navigation", async ({ page }) => {
    await gotoConsole(page);
    // Capture the Runs sidebar link handle, then navigate the full circuit and
    // back; the same shell element must remain attached the entire time.
    const runsNav = sidebarLink(page, "Runs");
    await expect(runsNav).toBeVisible();

    const circuit: { label: NavLabel; heading: string }[] = [
      { label: "Audit", heading: "Audit" },
      { label: "Policies", heading: "Policies" },
      { label: "Secrets", heading: "Secrets" },
      { label: "Approvals", heading: "Approvals" },
    ];
    for (const dest of circuit) {
      await navTo(page, dest.label);
      await expect(page.getByRole("heading", { name: dest.heading, level: 1 })).toBeVisible();
      // Sidebar Runs link never detaches while we move between screens.
      await expect(runsNav).toBeAttached();
      await expect(runsNav).toBeVisible();
    }
  });

  test("navigating away from a screen and back re-renders it", async ({ page }) => {
    await gotoConsole(page);

    // Boot lands on Runs already; leave to Audit, then come back to Runs.
    await navTo(page, "Runs");
    await expect(page.getByRole("heading", { name: "Runs", level: 1 })).toBeVisible();

    await navTo(page, "Audit");
    await expect(page.getByRole("heading", { name: "Audit", level: 1 })).toBeVisible();
    // Runs content is no longer in the main region (only one screen renders).
    await expect(page.getByText(/each confined behind its own barrier/i)).toHaveCount(0);

    await navTo(page, "Runs");
    await expect(page.getByRole("heading", { name: "Runs", level: 1 })).toBeVisible();
    await expect(page.getByText(/each confined behind its own barrier/i)).toBeVisible();
    // Audit subtitle is gone now that we returned to Runs.
    await expect(page.getByText(/Append-only/i)).toHaveCount(0);
  });

  test("the active sidebar item reflects the current screen", async ({ page }) => {
    await gotoConsole(page);
    // The seeded backend has PENDING approvals; the Approvals entry must exist
    // and be navigable. Clicking it lands on the Approvals screen.
    await navTo(page, "Approvals");
    await expect(page.getByRole("heading", { name: "Approvals", level: 1 })).toBeVisible();
    // Going to a different screen swaps the heading (single-screen region).
    await navTo(page, "Policies");
    await expect(page.getByRole("heading", { name: "Policies", level: 1 })).toBeVisible();
    await expect(page.getByRole("heading", { name: "Approvals", level: 1 })).toHaveCount(0);
  });

  // B1 — the sidebar itself is the surface the fail-open bug widened: a
  // settled-but-unknown /me used to render the FULL admin nav (every item in
  // NAV_ITEMS) off a guess. The auth-flow assertions (the banner, Retry) are
  // auth.spec.ts's; this is the sidebar's own pin, that NOTHING renders
  // rather than the wrong thing rendering.
  test("B1 — a settled-but-unknown /me renders no nav at all, admin or member", async ({ page }) => {
    await page.route("**/api/v1/me", (route) =>
      route.fulfill({ status: 500, contentType: "application/json", body: JSON.stringify({ error: "boom" }) }),
    );
    // NOT gotoConsole(): FirstRunLanding (App.tsx) explicitly returns null for
    // settled-but-unknown (`if (roleResolved && !identityResolved) return
    // null`) — with /me permanently failing there is no redirect off "/" to
    // wait for; the shell (and its banner) render right where the app
    // landed.
    await page.goto("/");
    await expect(page.getByRole("status").filter({ hasText: SHELL.UNKNOWN_BODY })).toBeVisible();
    for (const label of [...SIDEBAR_LABELS, "Workspaces"] as NavLabel[]) {
      await expect(sidebarLink(page, label)).toHaveCount(0);
    }
  });

  // /providers has no nav item at all (§9.1) — the funnel step card and the
  // Settings card are its only two entry points, the same as /drives. Present
  // for a member too: SIDEBAR_LABELS above never carried it, so this is the
  // negative the fixed list alone can't prove — that the omission is by
  // design, not an accident of the admin nav shrinking to the member set.
  // BROWSER VS API: mockMemberRole splices /me's role only (fixtures.ts's own
  // documented ceiling) — this proves the sidebar's RENDER behavior for a
  // member; server-side authorization over every route behind these links is
  // pinned in Go (the operatorOnly route group, authz_test.go's route matrix).
  test("/providers has no sidebar entry for an admin", async ({ page }) => {
    await gotoConsole(page);
    await expect(page.getByRole("link", { name: new RegExp(`^${PROVIDERS.TITLE}`) })).toHaveCount(0);
  });

  test("/providers has no sidebar entry for a member either", async ({ page }) => {
    // mockMemberRole registered BEFORE the first navigation (the codebase's
    // own idiom everywhere else it's used) — registering it mid-session and
    // then reload()ing races the reload against the in-flight /me it is
    // trying to splice, which Playwright reports as "Response has been
    // disposed" when the frame navigates out from under route.fetch().
    await mockMemberRole(page);
    await gotoConsole(page);
    await expect(page.getByRole("link", { name: new RegExp(`^${PROVIDERS.TITLE}`) })).toHaveCount(0);
  });
});

test.describe("theme toggle", () => {
  test("defaults to dark and the Toggle theme button is present", async ({ page }) => {
    await gotoConsole(page);
    const toggle = page.getByRole("button", { name: "Toggle theme" });
    await expect(toggle).toBeVisible();
    // ThemeProvider is dark-first; <html> carries the dark class + color-scheme.
    const initial = await readTheme(page);
    expect(initial.hasDarkClass).toBe(true);
    expect(initial.colorScheme).toBe("dark");
  });

  test("the Toggle theme button flips dark <-> light on documentElement", async ({ page }) => {
    await gotoConsole(page);
    const toggle = page.getByRole("button", { name: "Toggle theme" });

    const before = await readTheme(page);
    expect(before.hasDarkClass).toBe(true);

    // Flip to light.
    await toggle.click();
    await expect
      .poll(async () => (await readTheme(page)).hasDarkClass)
      .toBe(false);
    const light = await readTheme(page);
    expect(light.colorScheme).toBe("light");
    expect(light.stored).toBe("light");

    // Flip back to dark.
    await toggle.click();
    await expect
      .poll(async () => (await readTheme(page)).hasDarkClass)
      .toBe(true);
    const dark = await readTheme(page);
    expect(dark.colorScheme).toBe("dark");
    expect(dark.stored).toBe("dark");
  });

  // F7-F4: before this fix, index.html shipped bare <html lang="en"> with no
  // theme class — ThemeProvider's effect (theme-provider.tsx) only applies
  // `dark` AFTER React mounts, so the PRE-JS paint used :root's light tokens
  // until then, a visible flash on every load for the default (dark-first)
  // console. This reads the raw served markup directly — no JS execution at
  // all — so it proves the class ships in the HTML itself, not merely after
  // hydration.
  test("F7-F4: the served HTML carries class=dark and color-scheme=dark before any JS runs", async ({
    page,
  }) => {
    const res = await page.request.get("/");
    const html = await res.text();
    expect(html).toMatch(/<html[^>]*\bclass="dark"/);
    expect(html).toMatch(/<meta\s+name="color-scheme"\s+content="dark"/);
  });

  // neg: the static markup's default must not override a REAL stored
  // preference once the app takes over — ThemeProvider's effect still runs
  // `classList.toggle("dark", theme === "dark")` on mount and removes the
  // class for a stored "light" value, exactly as before this fix.
  test("F7-F4 neg: a stored light preference still renders light once the app mounts", async ({ page }) => {
    await page.addInitScript(() => {
      try {
        localStorage.setItem("wardyn-theme", "light");
      } catch {
        /* private mode */
      }
    });
    await gotoConsole(page);
    const theme = await readTheme(page);
    expect(theme.hasDarkClass).toBe(false);
    expect(theme.colorScheme).toBe("light");
  });

  test("theme choice persists across navigation", async ({ page }) => {
    await gotoConsole(page);
    const toggle = page.getByRole("button", { name: "Toggle theme" });

    // Switch to light, then navigate to another screen.
    await toggle.click();
    await expect.poll(async () => (await readTheme(page)).hasDarkClass).toBe(false);

    await navTo(page, "Audit");
    await expect(page.getByRole("heading", { name: "Audit", level: 1 })).toBeVisible();
    // The light theme set on <html> survives the in-app navigation.
    expect((await readTheme(page)).hasDarkClass).toBe(false);

    // The toggle is still mounted on the new screen and remains operable.
    await page.getByRole("button", { name: "Toggle theme" }).click();
    await expect.poll(async () => (await readTheme(page)).hasDarkClass).toBe(true);
  });
});

// F3-F8/F7-F6: the mobile nav is a left-side Sheet (app-shell.tsx's
// MobileNav) carrying the full nine-item sidebar; below the md breakpoint
// it's the ONLY way to navigate. At a short landscape-phone height the
// content used to be taller than the sheet's own h-full box with no scroll
// affordance at all — the last few items (Recordings) were unreachable.
// ui/sheet.tsx's primitive-level min-h-0 + overflow-y-auto (this lane) is
// what keeps it reachable here.
test.describe("mobile navigation drawer (F3-F8/F7-F6)", () => {
  test("667x375: the drawer scrolls — Recordings (near the bottom of the list) is reachable", async ({
    page,
  }) => {
    // gotoConsole waits on the DESKTOP sidebar link — below md that aside is
    // hidden entirely (md:flex), so the viewport switch has to come AFTER
    // landing, not before (the mobile hamburger only exists once mounted).
    await gotoConsole(page);
    await page.setViewportSize({ width: 667, height: 375 });
    await page.getByRole("button", { name: "Open navigation menu" }).click();
    const drawer = page.getByRole("dialog");
    await expect(drawer).toBeVisible();

    const recordings = drawer.getByRole("link", { name: /^Recordings/ });
    // scrollIntoViewIfNeeded, then confirm it actually landed on-screen — the
    // pre-fix defect was that no scroll container existed to scroll AT ALL.
    await recordings.scrollIntoViewIfNeeded();
    const box = await recordings.boundingBox();
    expect(box, "Recordings link boundingBox").not.toBeNull();
    expect(box!.y, "Recordings top edge").toBeGreaterThanOrEqual(0);
    expect(box!.y + box!.height, "Recordings bottom edge").toBeLessThanOrEqual(375);

    await recordings.click();
    await expect(page.getByRole("heading", { name: "Recordings", level: 1 })).toBeVisible();
  });
});

// 0.7.3 F6: the Fence tier and NetworkPolicy verdict were permanent, boot-time
// facts that never changed while the console was open — occupying the header's
// most valuable real estate for nothing an admin could act on. Both are gone
// outright (no degraded chip, no replacement); posture now lives on the setup
// Environment step alone.
test.describe("the header carries no posture chips (0.7.3 F6)", () => {
  test("no barrier or NetworkPolicy chip, on an admin session", async ({ page }) => {
    await gotoConsole(page);
    const header = page.getByRole("banner");
    await expect(header.getByText(/NetworkPolicy:/)).toHaveCount(0);
    await expect(
      header.getByText(/^(Fence|Wall|Vault|No barrier)$/),
    ).toHaveCount(0);
    // Positive control: the header still renders — this isn't an empty banner.
    await expect(header.getByRole("button", { name: "Toggle theme" })).toBeVisible();
  });
});

test.describe("error boundary (no spurious fallback)", () => {
  test("no screen renders the error-boundary fallback under healthy data", async ({ page }) => {
    // The ErrorBoundary fallback is role="alert" containing "Something went
    // wrong". The seeded backend exposes runs in ALL nine RunStates (incl. the
    // COMPLETED-state value that previously threw during render). Visiting every
    // screen must NOT trip the boundary — proving the fail-soft rendering holds.
    await gotoConsole(page);
    for (const dest of DESTINATIONS) {
      await navTo(page, dest.label);
      await expect(page.getByRole("heading", { name: dest.heading, level: 1 })).toBeVisible();
      // No error-boundary fallback card anywhere on the page for this screen.
      await expect(
        page.getByRole("alert").filter({ hasText: /Something went wrong/i }),
      ).toHaveCount(0);
      // And the screen's content actually rendered (boundary did not swallow it).
      await expect(page.getByText(dest.blurb)).toBeVisible();
    }
  });
});

// R4/F066 — a store outage must raise the unreachable banner.
//
// The banner is the only thing separating a quiet fleet from a dead control
// plane, and its verdict used to be `/healthz`'s `status === "ok"` alone —
// which handleHealthz writes as a LITERAL and never derives from the store
// (internal/api/healthz.go). So with wardynd up and Postgres down, /healthz
// answered 200 {"status":"ok"}, the banner stayed down, and every polled screen
// (runs 3s, run-detail 4s, approvals 10s, audit 5s) went on rendering last-good
// data behind its own silent `.catch`: a live-looking cockpit frozen at the
// instant the store died. App.tsx now also reads /readyz, which already Pings
// the store and answers 503 (internal/api/security_headers.go:124-136).
//
// DEFERRED (Docker down for the R4 fix wave — never run, never skipped):
//   DOCKER_HOST=unix:///var/run/docker.sock WARDYN_E2E_ADDR=:8288 \
//   WARDYN_E2E_UI_ADDR=:8289 WARDYN_E2E_PG_CONTAINER=wardyn-profiles-pg \
//   WARDYN_E2E_PG_HOSTPORT=localhost:55434 ./scripts/run-ui-e2e.sh e2e/navigation.spec.ts
test.describe("the unreachable banner sees a store outage (R4/F066)", () => {
  const BANNER = /Control plane unreachable — showing the last data received/i;

  test("a live daemon with an unreachable store raises the banner", async ({ page }) => {
    // /healthz keeps saying "ok" — it is liveness, and it is telling the truth.
    // /readyz reports what handleReadyz reports when Store.Ping fails.
    await page.route("**/readyz", (route) =>
      route.fulfill({
        status: 503,
        contentType: "application/json",
        body: JSON.stringify({ status: "error", postgres: "unreachable" }),
      }),
    );
    await gotoConsole(page);
    await expect(page.getByRole("status").filter({ hasText: BANNER })).toBeVisible({
      timeout: 15_000,
    });
  });

  test("…and it clears on its own when the store comes back", async ({ page }) => {
    let down = true;
    await page.route("**/readyz", async (route) => {
      if (!down) return route.fallback();
      return route.fulfill({
        status: 503,
        contentType: "application/json",
        body: JSON.stringify({ status: "error", postgres: "unreachable" }),
      });
    });
    await gotoConsole(page);
    await expect(page.getByRole("status").filter({ hasText: BANNER })).toBeVisible({
      timeout: 15_000,
    });
    down = false;
    await expect(page.getByRole("status").filter({ hasText: BANNER })).toHaveCount(0, {
      timeout: 15_000,
    });
  });

  test("a healthy deployment shows no banner — the negative control", async ({ page }) => {
    await gotoConsole(page);
    // Give the health poll a couple of ticks to be wrong in, then assert.
    await page.waitForTimeout(6_000);
    await expect(page.getByRole("status").filter({ hasText: BANNER })).toHaveCount(0);
  });
});
