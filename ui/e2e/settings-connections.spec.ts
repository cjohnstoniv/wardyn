/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, ADMIN_TOKEN, gotoConsole, mockMemberRole, navToRoute } from "./fixtures";

// E2E coverage for Settings' Host card and Model provider card
// (src/app/components/screens/settings/{settings-screen,connection-cards}.tsx)
// — X2-F1/F3/F23. corp-network.spec.ts was deleted in b97afcdc "for when
// Settings gains the Host card"; Settings has had one since. fixtures.ts's
// mockMemberRole comment used to cite that dead file as the precedent for
// the splice technique below — re-cited onto this file now that it exists.
//
// Settings replaced the old standalone corp-network step's own e2e: the Host
// card SUMMARIZES the deployment's proxy posture and LINKS into the same
// Corporate network step the old page tested directly (settings-screen.tsx's
// header comment) — this spec proves the summary, the link, and the
// redirect-probe BYPASS verdict the linked step renders. It does not re-walk
// the whole corp-network gate ladder (steps.test.ts/corp-network-step.test.tsx
// already do that against a mock); this is what only an e2e can prove: the
// real wiring, against the real seeded backend.

const auth = { Authorization: `Bearer ${ADMIN_TOKEN}` };

// Serial: the model-provider test writes and deletes a real secret, and the
// corp-proxy test writes a real site-config redirect — one backend, no
// mutating test may race a read about the same rows (policies.spec.ts's own
// rule, for the same reason).
test.describe.configure({ mode: "serial" });

test.describe("Settings — Host card (X2-F1)", () => {
  test("renders this host's deployment facts, operator-only rows included", async ({ page }) => {
    await gotoConsole(page);
    await navToRoute(page, "/admin/settings");

    const hostCard = page
      .locator("section")
      .filter({ has: page.getByRole("heading", { name: "Host", level: 3 }) });
    await expect(hostCard).toBeVisible();

    // scripts/e2e-backend.sh boots -runner none with no ImageBuilder wired,
    // and WARDYN_AGE_KEY set (a durable recording store).
    await expect(hostCard.getByText("Image builder")).toBeVisible();
    await expect(
      hostCard.getByText("Off — devcontainer builds and --image wraps are unavailable"),
    ).toBeVisible();
    await expect(hostCard.getByText("Recording store")).toBeVisible();
    await expect(hostCard.getByText("Enabled", { exact: true })).toBeVisible();
    // Internet: operator-only (HostCard gates it on useOperator()) — the
    // admin bearer this harness always authenticates with sees it.
    await expect(hostCard.getByText("Internet")).toBeVisible();
    await expect(hostCard.getByText("Direct", { exact: true })).toBeVisible();
  });

  test("a member sees no Internet row and no Corporate proxy landing", async ({ page }) => {
    await mockMemberRole(page);
    await gotoConsole(page);
    await navToRoute(page, "/account");

    await expect(page.getByRole("heading", { name: "Host", level: 3 })).toBeVisible();
    // Fix pass (review F3): the Host h3 above paints immediately, before the
    // site-config read that gates Internet/the button resolves — an absence
    // asserted right after it is "not yet", not "never". Wait for the fetch
    // to actually land first.
    await page.waitForLoadState("networkidle");
    // Image builder/Recording store are NOT operator-gated (checks_redacted
    // is a server-side fact this mocked /me splice never touches) — only
    // Internet and the proxy landing button are.
    await expect(page.getByText("Internet")).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Corporate proxy & egress" })).toHaveCount(0);
  });
});

test.describe("Settings — the corp-proxy landing and its BYPASS verdict (X2-F1/F23)", () => {
  test("the Host card's Corporate proxy & egress row lands on the Network step, and a bypassed redirect renders 'Redirect not enforced'", async ({
    page,
  }) => {
    // The setup ROUTE renders the welcome hero instead of the funnel until
    // this per-browser flag is set (onboardingSeen()) — same pre-seed
    // demos.spec.ts uses for its own ?step= deep links, needed here because
    // Settings' link is a client-side navigate() to /admin/setup?step=corp_network,
    // and the flag is read at GettingStarted's mount regardless of the query
    // string it carries.
    await page.addInitScript(() => {
      try {
        localStorage.setItem("wardyn-onboarding-seen", "1");
      } catch {
        /* private mode — ignore */
      }
    });

    await gotoConsole(page);
    await navToRoute(page, "/admin/settings");
    await page.getByRole("button", { name: "Corporate proxy & egress" }).click();

    // The Admin view's funnel, never plain /setup: that is the User view's
    // Getting Started, which has no Network step (M-6).
    await expect(page).toHaveURL(/\/admin\/setup\?step=corp_network/);
    await expect(page.getByRole("heading", { name: "Network", level: 2 })).toBeVisible();

    await page.getByRole("tab", { name: "Egress redirection" }).click();

    // The seeded backend runs -runner none, so the real POST
    // .../test-redirect can never deterministically produce "bypass" —
    // stubbed, same as the deleted corp-network.spec.ts's own test-redirect
    // interception (its header comment explains why: a throwaway sandbox
    // launch this harness cannot run).
    await page.route("**/api/v1/site-config/test-redirect", (route) =>
      route.fulfill({
        json: {
          state: "bypass",
          detail: "The mirror answered, but registry.npmjs.org is still reachable from a sandbox.",
        },
      }),
    );

    await page.getByRole("combobox").click();
    await page.getByText("https://registry.npmjs.org", { exact: true }).click();
    await page
      .getByPlaceholder(/artifactory\.corp\.internal/i)
      .fill("https://artifactory.corp.internal/api/npm/npm-remote");
    await page.getByRole("button", { name: /\+ add redirect/i }).click();

    const row = page.getByTitle(/^https:\/\/registry\.npmjs\.org →/);
    await expect(row).toBeVisible();
    await row.getByRole("button", { name: /^test$/i }).click();
    await expect(row.getByText("Redirect not enforced")).toBeVisible();
  });
});

test.describe("Settings — Model provider Connect / Replace / Disconnect (X2-F3)", () => {
  test("an API-key connect, a replace, and a disconnect all round-trip against GET /secrets", async ({
    page,
  }) => {
    await gotoConsole(page);
    await navToRoute(page, "/admin/settings");

    await page.getByRole("radio", { name: "API key" }).click();
    const field = page.getByLabel("Anthropic API key");
    await expect(field).toBeVisible();

    // Connect. Two lanes render an unstored "Save" button at once (Anthropic
    // and OpenAI, api_key's two SecretLanes) — Anthropic's is first in the
    // DOM (also the only one enabled, since only it carries a value).
    await field.fill("sk-ant-e2e-connect");
    await page.getByRole("button", { name: "Save", exact: true }).first().click();
    await expect(page.getByText(/Stored as\s*anthropic-api-key/)).toBeVisible();
    await expect(page.getByRole("radio", { name: "API key" }).getByText("Connected")).toBeVisible();

    let secretsRes = await page.request.get("/api/v1/secrets", { headers: auth });
    let names: string[] = (await secretsRes.json()).names ?? [];
    expect(names).toContain("anthropic-api-key");

    // Replace. Fix pass (review F2): the secret NAME doesn't change on a
    // replace, so "Stored as anthropic-api-key" and `names.toContain(...)`
    // are the SAME claim already proven above — a no-op or a 500 from "Save
    // replacement" leaves both green. Gate the click on the real write
    // instead: wait for the actual non-GET /secrets response and assert it
    // succeeded.
    await page.getByRole("button", { name: "Replace" }).click();
    const replaceField = page.getByLabel("Anthropic API key");
    await expect(replaceField).toBeVisible();
    await replaceField.fill("sk-ant-e2e-replaced");
    const replacePut = page.waitForResponse(
      (r) => r.url().includes("/api/v1/secrets") && r.request().method() !== "GET",
    );
    await page.getByRole("button", { name: "Save replacement", exact: true }).click();
    expect((await replacePut).ok()).toBe(true);
    await expect(page.getByText(/Stored as\s*anthropic-api-key/)).toBeVisible();

    secretsRes = await page.request.get("/api/v1/secrets", { headers: auth });
    names = (await secretsRes.json()).names ?? [];
    expect(names).toContain("anthropic-api-key");

    // Disconnect.
    await page.getByRole("button", { name: "Disconnect" }).click();
    await expect(page.getByLabel("Anthropic API key")).toBeVisible();
    await expect(page.getByRole("radio", { name: "API key" }).getByText("Connected")).toHaveCount(0);

    secretsRes = await page.request.get("/api/v1/secrets", { headers: auth });
    names = (await secretsRes.json()).names ?? [];
    expect(names).not.toContain("anthropic-api-key");
  });
});

// M-1b: /integrations and /integrations/:id are deleted, clean break, no
// alias (admin-member-modes-design.md §2.3) — the redirect this described no
// longer exists, so the describe block goes with it rather than being
// repointed to a route it would no longer prove anything about.
