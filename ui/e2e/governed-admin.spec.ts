/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { Page } from "@playwright/test";
import { test, expect, gotoConsole, navToRoute } from "./fixtures";
import { GOVERNED_ADMIN_BANNER } from "../src/app/lib/access-posture-copy";

// Constrained-admin mode (WARDYN_GOVERN_ADMIN_RUNS, mock M10). The hermetic
// backend signs in with the admin token, which is no signed-in person, so the
// SSO posture is forced the way ado-getting-started.spec.ts does it: fetch the
// REAL /setup/status, splice in the fields under test, fulfill. The refusal
// itself is proven against the real handler in internal/api's
// record_govern_admin_test.go, and the Admin runs setup-check row by
// TestGovernAdminRunsCheck; here the console's side is what is under test.

type Splice = (json: Record<string, unknown>) => void;

async function spliceStatus(page: Page, splice: Splice): Promise<void> {
  // Cache-and-serve, not a round trip per match: the landing redirect and the
  // screen's own mount both read /setup/status.
  let cached: Record<string, unknown> | null = null;
  await page.route("**/api/v1/setup/status*", async (route) => {
    if (!cached) {
      const json = await (await route.fetch()).json();
      splice(json);
      cached = json;
    }
    await route.fulfill({ json: cached! });
  });
}

const governed =
  (exempt?: string[]): Splice =>
  (json) => {
    json.auth = {
      ...(json.auth as Record<string, unknown>),
      mode: "sso",
      govern_admin_runs: true,
      ...(exempt ? { govern_admin_runs_exempt: exempt } : {}),
    };
  };

test.describe("constrained-admin mode (mock M10)", () => {
  test("the Admin view shows the approved band, and Hide dismisses it", async ({ page }) => {
    await spliceStatus(page, governed());
    await gotoConsole(page, "admin");
    const band = page.getByRole("status").filter({ hasText: GOVERNED_ADMIN_BANNER.TITLE });
    await expect(band).toBeVisible();
    await expect(band).toContainText(GOVERNED_ADMIN_BANNER.BODY);
    await band.getByRole("button", { name: GOVERNED_ADMIN_BANNER.HIDE }).click();
    await expect(page.getByText(GOVERNED_ADMIN_BANNER.TITLE)).toHaveCount(0);
  });

  test("with recording exempt the band says Record Mode runs as before", async ({ page }) => {
    await spliceStatus(page, governed(["recording"]));
    await gotoConsole(page, "admin");
    const band = page.getByRole("status").filter({ hasText: GOVERNED_ADMIN_BANNER.TITLE });
    await expect(band).toContainText(GOVERNED_ADMIN_BANNER.BODY_RECORDING_EXEMPT);
    await expect(band).not.toContainText(GOVERNED_ADMIN_BANNER.BODY);
  });

  test("no band with the switch off", async ({ page }) => {
    await gotoConsole(page, "admin");
    await expect(page.getByText("No barrier can be built on this host")).toBeVisible();
    await expect(page.getByText(GOVERNED_ADMIN_BANNER.TITLE)).toHaveCount(0);
  });

  test("no band in the User view while the switch is on", async ({ page }) => {
    await spliceStatus(page, governed());
    await gotoConsole(page, "user");
    await expect(page.getByText(GOVERNED_ADMIN_BANNER.TITLE)).toHaveCount(0);
  });

  test("no band when the switch is on but the caller is not a signed-in person", async ({ page }) => {
    await spliceStatus(page, (json) => {
      json.auth = { ...(json.auth as Record<string, unknown>), govern_admin_runs: true };
    });
    await gotoConsole(page, "admin");
    // Positive control: the shell's other Admin-view band rendered, so the
    // absence below is the rule and not a band still in flight.
    await expect(page.getByText("No barrier can be built on this host")).toBeVisible();
    await expect(page.getByText(GOVERNED_ADMIN_BANNER.TITLE)).toHaveCount(0);
  });

  test("a refused Record Mode launch shows the server's sentence verbatim", async ({ page }) => {
    const WS_ID = "e2e-governed-admin-ws";
    const SENTENCE =
      "Record Mode is refused for admins whose runs are governed. Your operator can allow it with `WARDYN_GOVERN_ADMIN_RUNS_EXEMPT=recording`.";
    await page.route(`**/api/v1/workspaces/${WS_ID}`, (route) =>
      route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          id: WS_ID,
          name: "governed-admin-e2e",
          kind: "repo",
          source: "acme/governed-admin",
          status: "scanned",
          created_at: "2026-01-01T00:00:00Z",
          updated_at: "2026-01-01T00:00:00Z",
          requirements: {},
          record_results: {},
        }),
      }),
    );
    await page.route(`**/api/v1/workspaces/${WS_ID}/record`, (route) =>
      route.fulfill({
        status: 403,
        contentType: "application/json",
        body: JSON.stringify({ error: SENTENCE, reason: "recording_governed" }),
      }),
    );
    await gotoConsole(page);
    await navToRoute(page, `/workspaces/${WS_ID}`);
    await expect(page.getByRole("heading", { name: "governed-admin-e2e" })).toBeVisible();
    await page.getByRole("button", { name: "Start recording" }).click();
    await expect(page.getByText("Recording failed to start")).toBeVisible();
    await expect(page.getByText(SENTENCE)).toBeVisible();
  });
});
