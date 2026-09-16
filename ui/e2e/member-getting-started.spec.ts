/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, gotoConsole, mockMemberRole, mockSecurityAdminRole, navToRoute } from "./fixtures";

// Member Getting Started (Phase 5) — same mockMemberRole splice
// member-console.spec.ts uses (the seeded backend always authenticates as
// admin server-side; only the CLIENT believes it is a member). This spec
// proves the render: the six member sections replace the operator funnel at
// /setup, the account menu drops Demos, and the video player streams nothing
// until Watch is pressed. Server-side ownership scoping (creator-scoped
// runs/secrets) is proven in Go, not here — the daemon-backed part of the
// plan's invariant.

const MEMBER_SECTION_TITLES = [
  "What's set up for you",
  "Add your workspace",
  "Your model key",
  "Your first run",
  "Approvals you can decide",
  "Connect your tools",
] as const;

test.describe("member Getting Started (mocked /me role)", () => {
  test.beforeEach(async ({ page }) => {
    await mockMemberRole(page);
  });

  test("shows the six member sections, never the admin funnel", async ({ page }) => {
    await gotoConsole(page);
    await navToRoute(page, "/setup");

    for (const title of MEMBER_SECTION_TITLES) {
      await expect(page.getByRole("heading", { name: title })).toBeVisible();
    }
    await expect(page.getByText("Setup is managed by your workspace admin")).toHaveCount(0);
    await expect(page.getByText("Pick your barrier")).toHaveCount(0);
  });

  test("the episode rail leads with the member's own path, then core (Shape C)", async ({
    page,
  }) => {
    await gotoConsole(page);
    await navToRoute(page, "/setup");
    // Group labels, in order: "Your path" (every member-audience episode —
    // including 13, whose lesson is the member terminal) then "Start here".
    const labels = page.locator(".label-eyebrow", { hasText: /Your path|Start here/ });
    await expect(labels).toHaveCount(2);
    await expect(labels.nth(0)).toHaveText("Your path");
    await expect(labels.nth(1)).toHaveText("Start here");
    await expect(page.getByText("Your terminal, our cluster")).toBeVisible();
    // The flipped core episode is in the member's Start-here set.
    await expect(page.getByText("Your first policy")).toBeVisible();
  });

  test("the account menu has no Demos entry", async ({ page }) => {
    await gotoConsole(page);
    await page.locator("header").getByRole("button").last().click();
    const menu = page.getByRole("menu");
    await expect(menu).toBeVisible();
    await expect(menu.getByText("Demos")).toHaveCount(0);
  });

  test("an episode streams nothing until Watch is pressed, then at least one request", async ({ page }) => {
    let mp4Requests = 0;
    await page.route("**/*.mp4", async (route) => {
      mp4Requests++;
      await route.fulfill({ status: 200, contentType: "video/mp4", body: Buffer.alloc(16) });
    });

    await gotoConsole(page);
    await navToRoute(page, "/setup");
    await expect(page.getByRole("heading", { name: "What's set up for you" })).toBeVisible();

    // Rows finish rendering before any network activity — no prefetch.
    const watch = page.getByRole("button", { name: "Watch" }).first();
    await expect(watch).toBeVisible();
    expect(mp4Requests).toBe(0);

    await watch.click();
    await expect.poll(() => mp4Requests, { timeout: 5000 }).toBeGreaterThanOrEqual(1);
  });

  // Appendix A finding 5: not_applicable is the admin-token principal's own
  // answer, not a member's — it must never dangle a "Sign in to AWS" button
  // in front of a caller with no person to sign in as, whatever the
  // deployment-wide llm_ready fallback renders instead (this e2e daemon
  // declares a Bedrock lane via scripts/e2e-backend.sh's WARDYN_BEDROCK_*
  // env, so llm_ready is deterministically true here, on any host — the
  // fallback chip legitimately shows — the CTA is the thing that must never
  // appear).
  test("a member under not_applicable is not offered a sign-in they cannot complete", async ({ page }) => {
    let cached: Record<string, unknown> | null = null;
    await page.route("**/api/v1/setup/status*", async (route) => {
      if (!cached) {
        const response = await route.fetch();
        const body = await response.json();
        body.model_access = { state: "not_applicable" };
        cached = body;
      }
      // TS can't narrow a `let` captured by this closure across the `await`
      // above — the `if` guarantees it non-null by here.
      await route.fulfill({ json: cached! });
    });
    await gotoConsole(page);
    await navToRoute(page, "/setup");
    await expect(page.getByRole("heading", { name: "What's set up for you" })).toBeVisible();
    await expect(page.getByRole("button", { name: "Sign in to AWS" })).toHaveCount(0);
  });

  // P1 (0.7.3 field report), the defect itself: a member's "Sign in to AWS"
  // never reached its terminal. The pane mounts AttachTerminal on a run the
  // member created one round trip earlier and passes no createdBy — there is no
  // run object to read one from — so the client gate read that absence as "not
  // yours" and refused before any POST. Unknown ownership now takes the ticket
  // lane, which is owner-or-admin SERVER-side (mintAttachTicket ->
  // getRunAuthorizedBy) and is the enforcement point.
  //
  // The launch itself is spliced: this daemon runs `-runner none`
  // (scripts/e2e-backend.sh), so a real POST /setup/harness-login has no runner
  // to answer with. What is REAL here is the console's own decision — whether it
  // asks the server for a ticket or refuses on its own authority.
  test("a member's own sign-in reaches the terminal by asking the server for a ticket", async ({ page }) => {
    const loginRunId = "3f1b7c26-0000-4000-8000-00000000f001";
    await page.route("**/api/v1/setup/status*", async (route) => {
      const response = await route.fetch();
      const body = await response.json();
      body.model_access = { state: "not_configured", mechanism: "bedrock_sso", action: "Sign in to AWS" };
      await route.fulfill({ response, json: body });
    });
    await page.route("**/api/v1/setup/harness-login", async (route) =>
      route.fulfill({ json: { run_id: loginRunId, state: "PENDING" } }),
    );
    await page.route(`**/api/v1/runs/${loginRunId}`, async (route) =>
      route.fulfill({ json: { id: loginRunId, task: "harness login", state: "RUNNING", interactive: true } }),
    );
    // Registered LAST so it wins over the run read above (Playwright matches the
    // most recently registered route first).
    let ticketPosts = 0;
    await page.route(`**/api/v1/runs/${loginRunId}/attach-ticket`, async (route) => {
      ticketPosts++;
      await route.fulfill({ json: { ticket: "e2e-ticket" } });
    });

    await gotoConsole(page);
    await navToRoute(page, "/setup");
    await page.getByRole("button", { name: "Sign in to AWS" }).click();
    await expect(page.getByTestId("harness-login-pane")).toBeVisible();
    await page.getByRole("button", { name: /start login/i }).click();

    // THE assertion: a ticket POST happened. Before the fix there was none —
    // no POST, no socket, no audit row, just the admin-role sentence.
    await expect.poll(() => ticketPosts, { timeout: 10_000 }).toBeGreaterThanOrEqual(1);
    await expect(page.getByText(/requires the admin role/i)).toHaveCount(0);
  });

  // member-cold-load lane (plan P3, absorbs F3-F7): a COLD document load of
  // /setup (bookmark, reload, the SSO callback's return) used to mount
  // SetupScreen — the ADMIN orchestrator — under the role context's fail-open
  // "admin" default before /me answered, firing reloadSiteConfig/loadSecrets/
  // loadProviderCount against admin-only endpoints. A DIRECT page.goto (not
  // navToRoute's client-side pushState, which never re-triggers the race) is
  // required to reproduce the cold-load window at all.
  //
  // GET /api/v1/site-config and GET /api/v1/workspace-providers are
  // unambiguous — no member surface ever calls either. GET /api/v1/secrets is
  // NOT: secrets.ts's listSecrets() (the admin orchestrator's operator-wide
  // read) and listSecretsMine() (MemberGettingStarted's own "Your model key"
  // read) hit the IDENTICAL URL — the server tells the two apart by caller
  // identity, not the request. So instead of a zero-count on that path (which
  // would false-fail on the member's OWN legitimate read), this pins the
  // request COUNT at exactly one: the leaked admin-orchestrator read this fix
  // removes would have shown up as a second, earlier GET before role resolved.
  test("a direct cold page.goto(\"/setup\") fires no admin-only reads", async ({ page }) => {
    const requests: string[] = [];
    page.on("request", (req) => requests.push(req.url()));

    await page.goto("/setup");
    await expect(page.getByRole("heading", { name: "What's set up for you" })).toBeVisible();
    // Nothing else is still in flight from the mount race this pins — the
    // settled member section heading above is itself proof the role resolved
    // and GettingStarted committed to the member branch, but give any stray
    // admin-only read a beat to land before counting.
    await page.waitForLoadState("networkidle");

    const isGet = (u: string, path: string) => {
      try {
        return new URL(u).pathname === path;
      } catch {
        return false;
      }
    };
    expect(requests.filter((u) => isGet(u, "/api/v1/site-config"))).toEqual([]);
    expect(requests.filter((u) => isGet(u, "/api/v1/workspace-providers"))).toEqual([]);
    expect(requests.filter((u) => isGet(u, "/api/v1/secrets"))).toHaveLength(1);

    // The admin welcome hero and the funnel's barrier-step heading — first
    // paint never shows either, whichever of the two an admin cold load would
    // have landed on.
    await expect(page.getByText("Sandboxed. Governed. Self-hosted. Free.")).toHaveCount(0);
    await expect(page.getByText("Pick your barrier")).toHaveCount(0);
  });
});

// Sibling negative control: the SAME route, unspliced (the harness's real
// admin session) — the operator funnel, the Demos entry, and none of the
// member-only section titles.
test.describe("admin session at /setup (unmocked — negative control)", () => {
  test.beforeEach(async ({ page }) => {
    // Past the welcome hero, straight to the funnel's own step heading — same
    // seed demos.spec.ts uses for a deep link into /setup.
    await page.addInitScript(() => {
      try {
        localStorage.setItem("wardyn-onboarding-seen", "1");
      } catch {
        /* private mode — ignore */
      }
    });
  });

  test("the admin still sees the barrier step, Demos, and no member sections", async ({ page }) => {
    await gotoConsole(page);
    await navToRoute(page, "/setup");

    await expect(page.getByRole("heading", { name: "Pick your barrier" })).toBeVisible();

    // /setup renders the funnel's own <header> inside the shell, so scope to the
    // shell's top bar (the first header in DOM order) before taking the last
    // button — the account-menu trigger. An unscoped .last() lands on a funnel
    // button and no menu ever opens.
    await page.locator("header").first().getByRole("button").last().click();
    const menu = page.getByRole("menu");
    await expect(menu.getByText("Demos")).toBeVisible();

    for (const title of MEMBER_SECTION_TITLES) {
      await expect(page.getByRole("heading", { name: title })).toHaveCount(0);
    }
  });

  // Negative control for the member-cold-load fix above: an admin's cold
  // /setup load is UNCHANGED — it still fires the admin-only site-config read
  // (this suite's beforeEach seeds wardyn-onboarding-seen so the load lands
  // straight on the funnel's barrier step, same as the sibling test above).
  test("a plain admin cold page.goto(\"/setup\") still requests /api/v1/site-config", async ({ page }) => {
    const requests: string[] = [];
    page.on("request", (req) => requests.push(req.url()));

    await page.goto("/setup");
    await expect(page.getByRole("heading", { name: "Pick your barrier" })).toBeVisible();

    expect(requests.some((u) => new URL(u).pathname === "/api/v1/site-config")).toBe(true);
  });
});

// R4/F034: the guard in GettingStarted was `role === "member"` after role went
// three-valued, so a SECURITY ADMIN fell through to the deployer funnel — built
// from a SetupStatus the server redacts for every non-operator
// (redactSetupStatusForMember zeroes Checks/Providers/Secrets,
// internal/api/setup.go), over mutations that are super-admin-only. It reads
// `role !== "admin"` now, the way setupGateActive already did.
//
// Browser-only: this is a ROUTE decision made from /me, so only a real
// navigation with a security-admin /me proves it.
test.describe("security admin at /setup (mocked /me role) — R4/F034", () => {
  test.beforeEach(async ({ page }) => {
    await mockSecurityAdminRole(page);
  });

  test("lands on the member Getting Started, never the deployer funnel", async ({ page }) => {
    await gotoConsole(page);
    await navToRoute(page, "/setup");

    for (const title of MEMBER_SECTION_TITLES) {
      await expect(page.getByRole("heading", { name: title })).toBeVisible();
    }
    // The operator welcome hero and the funnel's barrier picker — the two
    // surfaces built from the redacted status.
    await expect(page.getByText("Sandboxed. Governed. Self-hosted. Free.")).toHaveCount(0);
    await expect(page.getByText("Pick your barrier")).toHaveCount(0);
  });
});
