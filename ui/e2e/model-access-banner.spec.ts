/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, gotoConsole, navToRoute, mockMemberRole, sql } from "./fixtures";
import { RUN_COCKPIT } from "../src/app/components/wardyn/copy";
import { MODEL_ACCESS_BANNER } from "../src/app/components/wardyn/model-access-copy";
import { AGENTS } from "../src/app/lib/workspace-providers-copy";

// The model-access strip (0.7.6, field-report finding 2) — the CLIENT half.
//
// Same harness ceiling as member-mode.spec.ts: the seeded e2e backend
// authenticates with a bare admin bearer token and has no per-user AWS session
// to grade, so `model_access` is spliced onto GET /setup/status exactly as
// agents.spec.ts already splices it for the Getting Started chip. What this
// file proves is what only a browser can: the strip is on EVERY screen, its
// button opens the sign-in without leaving the page, and it is withheld where
// the page is already the door.
//
// The REAL states, from a real per-user AWS SSO sign-in on the kind cluster,
// are live case I (lane e2e-sso-path).

/** Splices a graded model_access + the per_user roster row onto /setup/status.
 *  Cached and served, never re-fetched per match: the landing redirect, the
 *  shell's own poll and a screen's mount all hit this endpoint, and a real
 *  round trip per match races Playwright disposing an in-flight response. */
async function mockModelAccess(
  page: import("@playwright/test").Page,
  access: { state: string; action?: string; deadline?: string },
  credentialSource = "per_user",
): Promise<void> {
  let cached: Record<string, unknown> | null = null;
  await page.route("**/api/v1/setup/status*", async (route) => {
    if (!cached) {
      const body = (await (await route.fetch()).json()) as Record<string, unknown>;
      body.model_access = access;
      body.harnesses = ((body.harnesses ?? []) as { id: string }[]).map((h) =>
        h.id === "claude-code"
          ? { ...h, enabled: true, mechanism: "bedrock_sso", credential_source: credentialSource }
          : h,
      );
      cached = body;
    }
    await route.fulfill({ json: cached! });
  });
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

    const strip = page.getByText(MODEL_ACCESS_BANNER.NOT_SIGNED_IN);
    await expect(strip).toBeVisible();

    // …and it travels: the same state, still stated, one navigation later.
    await navToRoute(page, "/runs/new");
    await expect(page.getByText(MODEL_ACCESS_BANNER.NOT_SIGNED_IN)).toBeVisible();

    // The strip is a DOOR, not a signpost: the sign-in opens here, on this
    // page, rather than sending the reader to a destination to find it.
    await navToRoute(page, "/runs");
    const before = new URL(page.url()).pathname;
    await page.getByRole("button", { name: AGENTS.SIGN_IN_AWS }).click();
    await expect(page.getByRole("heading", { name: MODEL_ACCESS_BANNER.DIALOG_TITLE })).toBeVisible();
    await expect(page.getByTestId("harness-login-pane")).toBeVisible();
    expect(new URL(page.url()).pathname).toBe(before);

    // Escape routes through the pane's own cancellation (no login run has been
    // launched yet, so nothing is killed — the dialog simply closes).
    await page.keyboard.press("Escape");
    await expect(page.getByRole("heading", { name: MODEL_ACCESS_BANNER.DIALOG_TITLE })).toHaveCount(0);
  });

  // "NOT an error — it is the first-run state" (modelaccess.go). A member who
  // never launches an agent must be able to set it aside; the rail and Getting
  // Started keep saying it.
  test("'Not now' clears the first-run strip for this session", async ({ page }) => {
    await mockMemberRole(page);
    await mockModelAccess(page, { state: "not_configured", action: AGENTS.SIGN_IN_AWS });
    await gotoConsole(page);
    await navToRoute(page, "/runs");

    await page.getByRole("button", { name: MODEL_ACCESS_BANNER.NOT_NOW }).click();
    await expect(page.getByText(MODEL_ACCESS_BANNER.NOT_SIGNED_IN)).toHaveCount(0);
    // …and it stays cleared across a navigation, not just a render.
    await navToRoute(page, "/workspaces");
    await expect(page.getByText(MODEL_ACCESS_BANNER.NOT_SIGNED_IN)).toHaveCount(0);
  });

  test("Getting Started carries no strip — that page IS the door", async ({ page }) => {
    await mockMemberRole(page);
    await mockModelAccess(page, { state: "not_configured", action: AGENTS.SIGN_IN_AWS });
    await gotoConsole(page);
    await navToRoute(page, "/setup");

    await expect(page.getByText(MODEL_ACCESS_BANNER.NOT_SIGNED_IN)).toHaveCount(0);
    // The page's own action line is still there — this is a suppression of the
    // duplicate, not of the fact.
    await expect(page.getByText(AGENTS.MODEL_ACCESS_NOT_CONFIGURED)).toBeVisible();
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
    const strip = page.getByText(MODEL_ACCESS_BANNER.NOT_SIGNED_IN);
    await expect(strip).toBeVisible();
    await page.getByRole("button", { name: AGENTS.SIGN_IN_AWS }).click();
    await expect(page.getByRole("heading", { name: MODEL_ACCESS_BANNER.DIALOG_TITLE })).toBeVisible();
    await expect(page.getByTestId("harness-login-pane")).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(page.getByRole("heading", { name: MODEL_ACCESS_BANNER.DIALOG_TITLE })).toHaveCount(0);
    // …and that Escape closed the DOOR, not the cockpit around it.
    await expect(page.getByRole("button", { name: RUN_COCKPIT.exitFocus })).toBeVisible();
  });

  test("a live credential says nothing at all", async ({ page }) => {
    await mockModelAccess(page, { state: "live" });
    await gotoConsole(page);

    await expect(page.getByText(MODEL_ACCESS_BANNER.NOT_SIGNED_IN)).toHaveCount(0);
    await expect(page.getByRole("button", { name: AGENTS.SIGN_IN_AWS })).toHaveCount(0);
  });
});
