/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { Page } from "@playwright/test";
import { test, expect, ADMIN_TOKEN, expandCard, gotoConsole, mockMemberRole, navToRoute } from "./fixtures";
import { S } from "../src/app/components/screens/settings/connection-cards";
import { CONNECTIONS, KEY_DOOR, REMOVE_CONFIRM } from "../src/app/components/wardyn/copy/door";

// E2E coverage for Settings' Host card and Model provider card
// (src/app/components/screens/settings/{admin-settings-screen,connection-cards}.tsx)
// — X2-F1/F3/F23. corp-network.spec.ts was deleted in b97afcdc "for when
// Settings gains the Host card"; Settings has had one since. fixtures.ts's
// mockMemberRole comment used to cite that dead file as the precedent for
// the splice technique below — re-cited onto this file now that it exists.
//
// Settings replaced the old standalone corp-network step's own e2e: the Host
// card SUMMARIZES the deployment's proxy posture and LINKS into the same
// Corporate network step the old page tested directly (admin-settings-screen.tsx's
// header comment) — this spec proves the summary, the link, and the
// redirect-probe BYPASS verdict the linked step renders. It does not re-walk
// the whole corp-network gate ladder (steps.test.ts/corp-network-step.test.tsx
// already do that against a mock); this is what only an e2e can prove: the
// real wiring, against the real seeded backend.
//
// M-5 (#636) split Settings in two: Host and Model provider both stayed in
// Admin Settings (/admin/settings), so the tests below are unchanged except
// the one that used to check /account for a member — Host isn't there at all
// any more.
//
// #1200 compact cards: every card here now collapses to a one-line summary by
// default and expands on click (collapsible-card.tsx) — every test that reads
// a card's BODY (a row, a field, a button beyond the header itself) expands
// it first via fixtures.ts's `expandCard`.

const auth = { Authorization: `Bearer ${ADMIN_TOKEN}` };

// Serial: the model-connections test writes and removes a real provider and
// credential, and the corp-proxy test writes a real site-config redirect —
// one backend, no mutating test may race a read about the same rows
// (policies.spec.ts's own rule, for the same reason).
test.describe.configure({ mode: "serial" });

test.describe("Settings — Host card", () => {
  // ticket: X2-F1
  test("renders this host's deployment facts, operator-only rows included", async ({ page }) => {
    await gotoConsole(page);
    await navToRoute(page, "/admin/settings");

    const hostCard = page
      .locator("section")
      .filter({ has: page.getByRole("heading", { name: "Host", level: 3 }) });
    await expect(hostCard).toBeVisible();
    await expandCard(page, "Host");

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

  // M-5 (#636) moved Host to Admin Settings only — a member's Your account has
  // no Host card at all now, so there is no Internet row or proxy landing to
  // hide there any more; this supersedes the old per-row hiding test.
  test("a member's Your account has no Host card at all", async ({ page }) => {
    await mockMemberRole(page);
    await gotoConsole(page);
    await navToRoute(page, "/account");

    await expect(page.getByRole("heading", { name: "Your account", level: 1 })).toBeVisible();
    await expect(page.getByRole("heading", { name: "Host" })).toHaveCount(0);
    await expect(page.getByText("Internet")).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Corporate proxy & egress" })).toHaveCount(0);
  });
});

test.describe("Settings — the corp-proxy landing and its BYPASS verdict", () => {
  // ticket: X2-F1/F23
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
    await expandCard(page, "Host");
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

// #548: the Settings Model provider card no longer holds a credential — its
// API-key, subscription and Bedrock lanes held the deployment's own model
// credentials, which no run reads since 0.8.2. The card now says where model
// access is set up (S.MODEL_MOVED), and each person's own key lives on Your
// account's "Your model connections". The round-trip this block used to pin
// against GET /secrets moved there with it, against the provider credential
// endpoint and the caller's own provider_access.
test.describe("Settings — Model provider card and Your model connections", () => {
  // ticket: X2-F3
  test("the Settings card points at model providers and offers no credential of its own", async ({ page }) => {
    await gotoConsole(page);
    await navToRoute(page, "/admin/settings");
    await expandCard(page, S.MODEL_TITLE);

    const card = page.getByTestId("model-provider-card");
    await expect(card.getByText(S.MODEL_MOVED)).toBeVisible();
    await expect(card.getByRole("radio")).toHaveCount(0);
    await expect(card.getByRole("textbox")).toHaveCount(0);
  });

  test("a key connect, a replace, and a remove on Your model connections all round-trip against the provider", async ({
    page,
  }) => {
    const provider = { id: "e2e-anthropic-key", name: "E2E Anthropic", kind: "anthropic_api_key", harnesses: [{ harness: "claude-code" }] };
    const before = await putProviders(page, (existing) => [...existing, provider]);
    try {
      const state = async () => {
        const res = await page.request.get("/api/v1/setup/status", { headers: auth });
        const access: { provider: string; state: string }[] = (await res.json()).provider_access ?? [];
        return access.find((a) => a.provider === provider.id)?.state;
      };
      expect(await state()).toBe("not_configured");

      await gotoConsole(page);
      await navToRoute(page, "/account");
      await expandCard(page, CONNECTIONS.TITLE);
      const card = page.getByTestId("model-connections-card");

      // Connect.
      await card.getByRole("button", { name: CONNECTIONS.ADD_KEY, exact: true }).click();
      let door = page.getByRole("dialog", { name: KEY_DOOR.TITLE(false, provider.name) });
      await door.getByLabel(KEY_DOOR.FIELD(false)).fill("sk-ant-e2e-connect-0000000000");
      await door.getByRole("button", { name: KEY_DOOR.SAVE, exact: true }).click();
      await expect(page.getByText(KEY_DOOR.SAVED_TOAST(false))).toBeVisible();
      await expect.poll(state).toBe("live");

      // Replace. The state reads "live" before and after, so gate on the real
      // write instead: the PUT to the provider's credential endpoint.
      await card.getByRole("button", { name: CONNECTIONS.REPLACE, exact: true }).click();
      door = page.getByRole("dialog", { name: KEY_DOOR.TITLE_REPLACE(false, provider.name) });
      await door.getByLabel(KEY_DOOR.FIELD(false)).fill("sk-ant-e2e-replaced-000000000");
      const replacePut = page.waitForResponse(
        (r) => r.url().includes(`/api/v1/model-providers/${provider.id}/credential`) && r.request().method() === "PUT",
      );
      await door.getByRole("button", { name: KEY_DOOR.SAVE, exact: true }).click();
      expect((await replacePut).ok()).toBe(true);
      await expect.poll(state).toBe("live");

      // Remove.
      await card.getByRole("button", { name: CONNECTIONS.REPLACE, exact: true }).click();
      door = page.getByRole("dialog", { name: KEY_DOOR.TITLE_REPLACE(false, provider.name) });
      await door.getByRole("button", { name: KEY_DOOR.REMOVE, exact: true }).click();
      const confirm = page.getByRole("dialog", { name: REMOVE_CONFIRM.TITLE(false, provider.name) });
      await confirm.getByRole("button", { name: REMOVE_CONFIRM.CONFIRM, exact: true }).click();
      await expect(page.getByText(REMOVE_CONFIRM.REMOVED_TOAST(false))).toBeVisible();
      await expect.poll(state).toBe("not_configured");
      await expect(card.getByRole("button", { name: CONNECTIONS.ADD_KEY, exact: true })).toBeVisible();
    } finally {
      await putProviders(page, () => before);
    }
  });
});

/** Replaces the stored model-provider block with edit(current), returning the
 *  block as it was so a test can put it back. */
async function putProviders(page: Page, edit: (existing: unknown[]) => unknown[]): Promise<unknown[]> {
  const cur = await page.request.get("/api/v1/model-providers", { headers: auth });
  expect(cur.status()).toBe(200);
  const existing: unknown[] = (await cur.json()).providers ?? [];
  const headers: Record<string, string> = { ...auth };
  const etag = cur.headers()["etag"];
  if (etag) headers["If-Match"] = etag;
  const res = await page.request.put("/api/v1/model-providers", { headers, data: { providers: edit(existing) } });
  expect(res.status(), await res.text()).toBe(200);
  return existing;
}

// M-1b: /integrations and /integrations/:id are deleted, clean break, no
// alias (admin-member-modes-design.md §2.3) — the redirect this described no
// longer exists, so the describe block goes with it rather than being
// repointed to a route it would no longer prove anything about.
