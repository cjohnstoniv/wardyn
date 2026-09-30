/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, ADMIN_TOKEN, asRealMember, asRealSecurityAdmin, consoleAPI, gotoConsole, navToRoute } from "./fixtures";
import { MEMBER_GETTING_STARTED } from "../src/app/components/wardyn/copy";
import { CONNECTIONS } from "../src/app/components/wardyn/copy/door";

// Real per-person identities; individual setup splices below exercise rendering
// states the none runner cannot produce, without claiming backend acceptance.

const BEDROCK = {
  id: "bedrock-prod",
  name: "Bedrock (prod)",
  kind: "bedrock_sso",
  harnesses: ["claude-code"],
  default_for: ["claude-code"],
  host: "bedrock-runtime.us-east-1.amazonaws.com",
};

const MEMBER_SECTION_TITLES = [
  "What's set up for you",
  "Add your workspace",
  "Your first run",
  "Approvals you can decide",
  "Connect your tools",
] as const;

test.describe("member Getting Started (real per-person token)", () => {
  test.beforeEach(async ({ page }) => {
    await asRealMember(page);
  });

  test("real member sees redacted setup facts", async ({ page }) => {
    const admin = await page.request.get("/api/v1/setup/status", {
      headers: { Authorization: `Bearer ${ADMIN_TOKEN}` },
    });
    expect(admin.status()).toBe(200);
    const operatorStatus = await admin.json();
    expect(operatorStatus.checks.length).toBeGreaterThan(0);
    expect(operatorStatus.secrets.present).toContain("e2e-test-secret");
    expect(operatorStatus.runner.driver).toBe("none");

    const response = await consoleAPI(page, "GET", "/api/v1/setup/status");
    expect(response.status, response.text).toBe(200);
    const status = JSON.parse(response.text);
    expect(status).toMatchObject({ checks_redacted: true, checks: [], providers: [], secrets: { present: [] } });
    expect(status.runner.driver ?? "").toBe("");
    expect(status.runner.confinement_classes).toEqual(operatorStatus.runner.confinement_classes);
    expect(status.bedrock).toBeUndefined();
    expect(response.text).not.toContain("e2e-test-secret");

    await page.goto("/setup");
    await expect(page.getByRole("heading", { name: "What's set up for you" })).toBeVisible();
    await expect(page.getByText("Image builder", { exact: true })).toHaveCount(0);
    await expect(page.getByText("Pick your barrier")).toHaveCount(0);
  });

  test("shows the five member sections, never the admin funnel", async ({ page }) => {
    await gotoConsole(page);
    await navToRoute(page, "/setup");

    for (const title of MEMBER_SECTION_TITLES) {
      await expect(page.getByRole("heading", { name: title })).toBeVisible();
    }
    await expect(page.getByText("Setup is managed by your workspace admin")).toHaveCount(0);
    await expect(page.getByText("Pick your barrier")).toHaveCount(0);
  });

  // Packet M-B (modes-b.html): the model-connection row links to Your account.
  test("the model connections row opens Your account", async ({ page }) => {
    await gotoConsole(page);
    await navToRoute(page, "/setup");

    const link = page.getByRole("link", { name: MEMBER_GETTING_STARTED.MODEL_CONNECTIONS });
    await expect(link).toHaveAttribute("href", "/account");
    await link.click();
    await expect(page).toHaveURL(/\/account$/);
  });

  // M-6 (D5): the demos the admin funnel used to walk now live here. This
  // pins the section exists and offers a keyless demo for a real member
  // role — the walkable/gated set itself (needsModel/needsSecret) is unit
  // coverage in member-getting-started.test.tsx, not re-proven per backend
  // state here.
  test("the Egress demos section renders and offers a keyless demo", async ({ page }) => {
    await gotoConsole(page);
    await navToRoute(page, "/setup");

    await expect(page.getByText(MEMBER_GETTING_STARTED.DEMOS_EGRESS_TITLE)).toBeVisible();
    await expect(page.getByText("The sealed box")).toBeVisible();
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
    // X2-F14: a bare toBe(0) here is a point-in-time check — it passes just
    // as well when a prefetch is merely still in flight as when one never
    // fires at all. Give the page a real beat to go idle first (the cold
    // `/setup` load test above already relies on this same wait to settle
    // its own network-count pin), so the zero below is an actual claim about
    // "never", not "not yet".
    await page.waitForLoadState("networkidle");
    expect(mp4Requests).toBe(0);

    await watch.click();
    await expect.poll(() => mp4Requests, { timeout: 5000 }).toBeGreaterThanOrEqual(1);
  });

  // Appendix A finding 5: not_applicable is the admin-token principal's own
  // answer, not a member's — it must never dangle a "Sign in to AWS" button in
  // front of a caller with no person to sign in as. Getting Started offers no
  // sign-in of its own at all (Your account is the door), and the shell strip
  // draws no line for not_applicable.
  test("a caller under not_applicable is not offered a sign-in they cannot complete", async ({ page }) => {
    let cached: Record<string, unknown> | null = null;
    await page.route("**/api/v1/setup/status*", async (route) => {
      if (!cached) {
        const response = await route.fetch();
        const body = await response.json();
        body.model_providers = [BEDROCK];
        body.provider_access = [{ provider: BEDROCK.id, state: "not_applicable" }];
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
  // (scripts/e2e-backend.sh), so a real provider sign-in has no runner to
  // answer with. What is REAL here is the console's own decision — whether it
  // asks the server for a ticket or refuses on its own authority.
  test("a member's own sign-in reaches the terminal by asking the server for a ticket", async ({ page }) => {
    const loginRunId = "3f1b7c26-0000-4000-8000-00000000f001";
    await page.route("**/api/v1/setup/status*", async (route) => {
      const response = await route.fetch();
      const body = await response.json();
      body.model_providers = [BEDROCK];
      body.provider_access = [{ provider: BEDROCK.id, state: "not_configured", action: "Sign in to AWS" }];
      await route.fulfill({ response, json: body });
    });
    await page.route(`**/api/v1/model-providers/${BEDROCK.id}/sign-in`, async (route) =>
      route.fulfill({ json: { run_id: loginRunId, state: "PENDING" } }),
    );
    await page.route(`**/api/v1/runs/${loginRunId}`, async (route) =>
      route.fulfill({ json: { id: loginRunId, task: "harness login", state: "RUNNING", interactive: true } }),
    );
    // Registered LAST so it wins over the run read above (Playwright matches the
    // most recently registered route first).
    let ticketPosts = 0;
    await page.route(`**/api/v1/runs/${loginRunId}/attach/ticket`, async (route) => {
      ticketPosts++;
      await route.fulfill({ json: { ticket: "e2e-ticket" } });
    });

    await gotoConsole(page);
    await navToRoute(page, "/account");
    await page.getByRole("button", { name: "Sign in to AWS" }).first().click();
    await expect(page.getByTestId("harness-login-pane")).toBeVisible();

    // THE assertion: a ticket POST happened. Before the fix there was none —
    // no POST, no socket, no audit row, just the admin-role sentence.
    await expect.poll(() => ticketPosts, { timeout: 10_000 }).toBeGreaterThanOrEqual(1);
    await expect(page.getByText(/requires the admin role/i)).toHaveCount(0);
  });

  // The provider-mode chip: Ready when the granted harness's default provider
  // is connected — computed by the SAME predicate Your account's own header
  // chip reads (lib/model-connections.ts's connectionsSummary). Your account
  // is the only door (MP-D's own drawing).
  test("the summary chip reads Ready with a connected default provider, and offers no sign-in", async ({
    page,
  }) => {
    await page.route("**/api/v1/setup/status*", async (route) => {
      const response = await route.fetch();
      const body = await response.json();
      body.model_providers = [
        {
          id: "bedrock-prod",
          name: "Bedrock (prod)",
          kind: "bedrock_sso",
          harnesses: ["claude-code"],
          default_for: ["claude-code"],
          host: "bedrock-runtime.us-east-1.amazonaws.com",
        },
      ];
      body.provider_access = [{ provider: "bedrock-prod", state: "live" }];
      await route.fulfill({ response, json: body });
    });
    await gotoConsole(page);
    await navToRoute(page, "/setup");
    await expect(page.getByText(CONNECTIONS.SUMMARY_READY)).toBeVisible();
    await expect(page.getByRole("button", { name: "Sign in to AWS" })).toHaveCount(0);
  });

  // Fix review (MED, the empty-grant shape): a REAL provider block that
  // grants this caller nothing (`model_providers: []`, no `omitempty` on the
  // wire any more): Not set up by your admin.
  test("model_providers: [] is a real block granting nothing — Not set up", async ({ page }) => {
    await page.route("**/api/v1/setup/status*", async (route) => {
      const response = await route.fetch();
      const body = await response.json();
      body.model_providers = [];
      body.provider_access = [];
      await route.fulfill({ response, json: body });
    });
    await gotoConsole(page);
    await navToRoute(page, "/setup");
    await expect(page.getByText(CONNECTIONS.SUMMARY_NOT_SET_UP)).toBeVisible();
    await expect(page.getByRole("button", { name: "Sign in to AWS" })).toHaveCount(0);
  });

  // member-cold-load lane (plan P3, absorbs F3-F7): a COLD document load of
  // /setup (bookmark, reload, the SSO callback's return) used to mount
  // SetupScreen — the ADMIN orchestrator — under the role context's fail-open
  // "admin" default before /me answered, firing reloadSiteConfig/loadSecrets/
  // loadProviderCount against admin-only endpoints. A DIRECT page.goto (not
  // navToRoute's client-side pushState, which never re-triggers the race) is
  // required to reproduce the cold-load window at all.
  //
  // GET /api/v1/site-config, GET /api/v1/workspace-providers and GET
  // /api/v1/secrets are unambiguous — since #548 retired "Your model key" (the
  // one member read of /secrets) no member Getting Started surface calls any
  // of them, so each must be absent.
  test("a direct cold page.goto(\"/setup\") fires no admin-only reads", async ({ page }) => {
    const requests: { method: string; url: string }[] = [];
    page.on("request", (req) => requests.push({ method: req.method(), url: req.url() }));

    await page.goto("/setup");
    await expect(page.getByRole("heading", { name: "What's set up for you" })).toBeVisible();
    // Nothing else is still in flight from the mount race this pins — the
    // settled member section heading above is itself proof the role resolved
    // and GettingStarted committed to the member branch, but give any stray
    // admin-only read a beat to land before counting.
    await page.waitForLoadState("networkidle");

    // Named "isGet": every read this pins is a GET, and the method is part of
    // the match — a future member flow that PUTs one of these same paths (a
    // save, not a read) must not silently count against this pin.
    const isGet = (r: { method: string; url: string }, path: string) => {
      if (r.method !== "GET") return false;
      try {
        return new URL(r.url).pathname === path;
      } catch {
        return false;
      }
    };
    expect(requests.filter((r) => isGet(r, "/api/v1/site-config"))).toEqual([]);
    expect(requests.filter((r) => isGet(r, "/api/v1/workspace-providers"))).toEqual([]);
    expect(requests.filter((r) => isGet(r, "/api/v1/secrets"))).toEqual([]);

    // The admin welcome hero and the funnel's barrier-step heading — first
    // paint never shows either, whichever of the two an admin cold load would
    // have landed on.
    await expect(page.getByText("Sandboxed. Governed. Self-hosted. Free.")).toHaveCount(0);
    await expect(page.getByText("Pick your barrier")).toHaveCount(0);
  });
});

// Sibling negative control, updated for M-6 (admin-member-modes-design.md
// §4.8/§6, D1): the SAME route, unspliced (the harness's real admin
// session) — since M-6 the mode is the URL, not the caller's role
// (onboarding-screen.tsx's own header comment), and D1 (single-operator: an
// admin bearer, no SSO) says the URL is the ONLY thing that ever decided it
// here. So the sole admin at plain /setup now gets the SAME User Getting
// Started page a member would, never the operator funnel — which still
// renders, unchanged, at /admin/setup.
test.describe("admin session at /setup and /admin/setup (unmocked — D1: the URL decides)", () => {
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

  test("plain /setup shows the same User Getting Started a member gets, never the operator funnel", async ({
    page,
  }) => {
    await gotoConsole(page);
    await navToRoute(page, "/setup");

    for (const title of MEMBER_SECTION_TITLES) {
      await expect(page.getByRole("heading", { name: title })).toBeVisible();
    }
    await expect(page.getByRole("heading", { name: "Pick your barrier" })).toHaveCount(0);
  });

  // Negative control: /admin/setup is still the operator funnel, and still
  // fires the admin-only site-config read a plain /setup load no longer does
  // (member-getting-started.tsx never reads it).
  test("a plain admin cold page.goto(\"/admin/setup\") still shows the funnel and requests /api/v1/site-config", async ({
    page,
  }) => {
    const requests: string[] = [];
    page.on("request", (req) => requests.push(req.url()));

    await page.goto("/admin/setup");
    await expect(page.getByRole("heading", { name: "Pick your barrier" })).toBeVisible();

    expect(requests.some((u) => new URL(u).pathname === "/api/v1/site-config")).toBe(true);
  });
});

// R4/F034: the guard in GettingStarted was `role === "member"` after role went
// three-valued, so a SECURITY ADMIN fell through to the deployer funnel — built
// from a SetupStatus the server redacts for every non-operator
// (redactSetupStatusForUser zeroes Checks/Providers/Secrets,
// internal/api/setup.go), over mutations that are super-admin-only.
// GettingStarted now checks `role !== "admin"` alongside the view (M-6's
// `view !== "admin"`), so a security admin is refused the funnel whether
// they land on plain /setup or reach /admin/setup directly — the latter
// passes ViewGate because viewAccess maps a security admin to the same
// "session-admin" tier as an admin.
//
// Browser-only: this is a ROUTE decision made from /me, so only a real
// navigation with a security-admin /me proves it.
test.describe("security admin at /setup (real per-person token)", () => {
  // ticket: R4/F034
  test.beforeEach(async ({ page }) => {
    await asRealSecurityAdmin(page);
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

  // W6-3. The account menu's Demos entry used to deep-link to
  // /setup?step=sealed-box, and the test above is the proof that this tier
  // lands on the member Getting Started, which DOES honor ?step=<id> — it
  // pre-opens the matching row, for a demo it actually offers (see
  // member-getting-started.test.tsx's own ?step= cases). The entry stays
  // gone regardless: a menu item pointing at a page this tier already lands
  // on by default would be a dead invitation either way.
  test("the account menu has no Demos entry either", async ({ page }) => {
    await gotoConsole(page);
    await page.locator("header").getByRole("button").last().click();
    const menu = page.getByRole("menu");
    await expect(menu).toBeVisible();
    await expect(menu.getByText("Demos")).toHaveCount(0);
  });
});
