/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, gotoConsole, navToRoute, mockMemberRole, sql } from "./fixtures";
import { RUN_COCKPIT } from "../src/app/components/wardyn/copy";
import { MODEL_ACCESS_BANNER } from "../src/app/components/wardyn/model-access-copy";
import { AGENTS } from "../src/app/lib/workspace-providers-copy";
import { BANNER, CONNECTIONS } from "../src/app/components/wardyn/copy/door";

// The model-access strip (0.7.6, field-report finding 2) — the CLIENT half,
// over a Bedrock SSO provider that is claude-code's default.
//
// Same harness ceiling as member-mode.spec.ts: the seeded e2e backend
// authenticates with a bare admin bearer token and has no per-user AWS session
// to grade, so the provider block and this person's access to it are spliced
// onto GET /setup/status. What this file proves is what only a browser can:
// the strip is on EVERY screen, its button opens the sign-in without leaving
// the page, and it is withheld where the page is already the door.
// model-access-strip-providers.spec.ts pins the strip's per-kind lines.
//
// The REAL states, from a real per-user AWS SSO sign-in on the kind cluster,
// are live case I (lane e2e-sso-path).

const BEDROCK = {
  id: "bedrock-prod",
  name: "Bedrock (prod)",
  kind: "bedrock_sso",
  harnesses: ["claude-code"],
  default_for: ["claude-code"],
  host: "bedrock-runtime.us-east-1.amazonaws.com",
};
const NOT_SIGNED_IN = BANNER.B1("Claude Code", BEDROCK.name);

/** Splices the provider and this person's access to it onto /setup/status.
 *  Cached and served, never re-fetched per match: the landing redirect, the
 *  shell's own poll and a screen's mount all hit this endpoint, and a real
 *  round trip per match races Playwright disposing an in-flight response. The
 *  door starts its sign-in at once; nothing here signs in. */
async function mockModelAccess(
  page: import("@playwright/test").Page,
  access: { state: string; action?: string; deadline?: string },
): Promise<void> {
  let cached: Record<string, unknown> | null = null;
  await page.route("**/api/v1/setup/status*", async (route) => {
    if (!cached) {
      const body = (await (await route.fetch()).json()) as Record<string, unknown>;
      body.model_providers = [BEDROCK];
      body.provider_access = [{ provider: BEDROCK.id, ...access }];
      cached = body;
    }
    await route.fulfill({ json: cached! });
  });
  await page.route("**/api/v1/model-providers/*/sign-in", (route) =>
    route.fulfill({ status: 503, json: { error: "e2e: no sign-in here" } }),
  );
}

test.describe("the model-access strip", () => {
  test("a member who has never signed in is told on the board AND on New Run, and can sign in from the strip", async ({
    page,
  }) => {
    await mockMemberRole(page);
    await mockModelAccess(page, { state: "not_configured", action: AGENTS.SIGN_IN_AWS });
    await gotoConsole(page);
    // A member with no runs LANDS on Getting Started (FirstRunLanding), which
    // is the one screen the strip is withheld on — so the board is a step away.
    await navToRoute(page, "/runs");

    const strip = page.getByText(NOT_SIGNED_IN);
    await expect(strip).toBeVisible();

    // …and it travels: the same state, still stated, one navigation later.
    await navToRoute(page, "/runs/new");
    await expect(page.getByText(NOT_SIGNED_IN)).toBeVisible();

    // The strip is a DOOR, not a signpost: the sign-in opens here, on this
    // page, rather than sending the reader to a destination to find it.
    await navToRoute(page, "/runs");
    const before = new URL(page.url()).pathname;
    await page.getByRole("button", { name: AGENTS.SIGN_IN_AWS }).click();
    await expect(page.getByRole("heading", { name: MODEL_ACCESS_BANNER.DIALOG_TITLE })).toBeVisible();
    await expect(page.getByTestId("harness-login-pane")).toBeVisible();
    expect(new URL(page.url()).pathname).toBe(before);

    // Escape closes the door; its sign-in never started (the launch is
    // refused here), so nothing is killed.
    await page.keyboard.press("Escape");
    await expect(page.getByRole("heading", { name: MODEL_ACCESS_BANNER.DIALOG_TITLE })).toHaveCount(0);
  });

  // "NOT an error — it is the first-run state". A member who
  // never launches an agent must be able to set it aside; the rail and Getting
  // Started keep saying it.
  test("'Not now' clears the first-run strip for this session", async ({ page }) => {
    await mockMemberRole(page);
    await mockModelAccess(page, { state: "not_configured", action: AGENTS.SIGN_IN_AWS });
    await gotoConsole(page);
    await navToRoute(page, "/runs");

    await page.getByRole("button", { name: MODEL_ACCESS_BANNER.NOT_NOW }).click();
    await expect(page.getByText(NOT_SIGNED_IN)).toHaveCount(0);
    // …and it stays cleared across a navigation, not just a render.
    await navToRoute(page, "/workspaces");
    await expect(page.getByText(NOT_SIGNED_IN)).toHaveCount(0);
  });

  test("Getting Started carries no strip — that page IS the door", async ({ page }) => {
    await mockMemberRole(page);
    await mockModelAccess(page, { state: "not_configured", action: AGENTS.SIGN_IN_AWS });
    await gotoConsole(page);
    await navToRoute(page, "/setup");

    await expect(page.getByText(NOT_SIGNED_IN)).toHaveCount(0);
    // The page's own summary chip is still there — this is a suppression of
    // the duplicate, not of the fact.
    await expect(page.getByText(CONNECTIONS.SUMMARY_NEEDS_YOU)).toBeVisible();
  });

  // Risk (a), settled in a real browser rather than by reading z-indices: the
  // cockpit's focus mode paints a fixed z-40 overlay over the whole shell, and
  // this strip is the one band that carries its own repair — 0.7.6's mid-run
  // re-authentication needs it exactly there. The Dialog is z-50 and portals to
  // the body; the CSP admits the inline style the width override needs.
  test("focus mode does not bury the strip, and the door still opens over it", async ({ page }) => {
    await mockModelAccess(page, { state: "not_configured", action: AGENTS.SIGN_IN_AWS });
    const runId = sql("SELECT id FROM agent_runs ORDER BY created_at LIMIT 1");
    await page.goto(`/runs/${runId}`);

    await page.getByRole("button", { name: RUN_COCKPIT.enterFocus }).click();
    await expect(page.getByRole("button", { name: RUN_COCKPIT.exitFocus })).toBeVisible();

    // Visible means HIT-TESTABLE here: a band painted under the overlay would
    // still be "visible" to a DOM query and unclickable to a person.
    const strip = page.getByText(NOT_SIGNED_IN);
    await expect(strip).toBeVisible();
    await page.getByRole("button", { name: AGENTS.SIGN_IN_AWS }).click();
    await expect(page.getByRole("heading", { name: MODEL_ACCESS_BANNER.DIALOG_TITLE })).toBeVisible();
    await expect(page.getByTestId("harness-login-pane")).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(page.getByRole("heading", { name: MODEL_ACCESS_BANNER.DIALOG_TITLE })).toHaveCount(0);
    // …and that Escape closed the DOOR, not the cockpit around it.
    await expect(page.getByRole("button", { name: RUN_COCKPIT.exitFocus })).toBeVisible();
  });

  // §4.2 (M-3): a provider's strip is each person's own
  // credential, which belongs to the User view. Seen there first, so its
  // absence after the switch is the view rule at work, not a strip that had
  // not loaded yet.
  test("the Admin view carries no per-user strip; the User view does", async ({ page }) => {
    await mockModelAccess(page, { state: "not_configured", action: AGENTS.SIGN_IN_AWS });
    await gotoConsole(page);
    await navToRoute(page, "/runs");
    await expect(page.getByText(NOT_SIGNED_IN)).toBeVisible();

    await navToRoute(page, "/admin/runs");
    await expect(page.getByText(NOT_SIGNED_IN)).toHaveCount(0);
  });

  test("a live credential says nothing at all", async ({ page }) => {
    await mockModelAccess(page, { state: "live" });
    await gotoConsole(page);

    await expect(page.getByText(NOT_SIGNED_IN)).toHaveCount(0);
    await expect(page.getByRole("button", { name: AGENTS.SIGN_IN_AWS })).toHaveCount(0);
  });
});
