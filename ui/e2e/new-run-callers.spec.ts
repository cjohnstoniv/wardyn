/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New Run, the caller picker (r17 T2-a): a port marked callable from outside gets "Who can call it" as chips and an
// "Add a caller" menu built from the person's own groups, the services they sponsor and an email typed in. The
// pool list, preflight, /me/capabilities and /me/service-identities are stubbed; nothing is created.
import type { Page } from "@playwright/test";
import { test, expect, gotoConsole, navToRoute } from "./fixtures";
import { NR_RUN } from "../src/app/components/wardyn/copy/new-run/run";
import { NR_SHAPE } from "../src/app/components/wardyn/copy/new-run/shape";
import { NR_START } from "../src/app/components/wardyn/copy/new-run/start";
import { SCENARIO_POOLS } from "../src/app/lib/new-run-fixtures/scenarios/pools";
import { SCENARIO_PREFLIGHT } from "../src/app/lib/new-run-fixtures/scenarios/preflight";

async function stub(page: Page) {
  const pools = SCENARIO_POOLS.member.pools.map((p) => ({
    ...p,
    run_types: ["background", "interactive"],
    run_types_source: "pool",
    barrier_options: [{ class: "CC2", available: true, default: true, default_source: "server" }],
  }));
  await page.route("**/api/v1/runner-pools*", (route) =>
    route.fulfill({ json: { pools, default: { pool_id: pools[0].id, selection: "organisation_default" } } }),
  );
  await page.route("**/api/v1/runs/preflight", (route) => route.fulfill({ json: SCENARIO_PREFLIGHT.member }));
  await page.route("**/api/v1/me/capabilities", (route) =>
    route.fulfill({ json: { grants: [], enforcement: {}, session_groups: ["payments", "platform"], groups_snapshot_stale: false, kinds_version: 1 } }),
  );
  await page.route("**/api/v1/me/service-identities*", (route) =>
    route.fulfill({ json: [{ name: "billing", state: "active" }, { name: "ledger-sync", state: "active" }] }),
  );
}

test.describe("New Run: who can call a port", () => {
  test("adds a group, a service identity and a person from the menu, and removes one", async ({ page }) => {
    await stub(page);
    await gotoConsole(page);
    await navToRoute(page, "/runs/new");
    await page.getByRole("button", { name: NR_START.GUIDED_FOOT }).click();
    await page.getByRole("button", { name: NR_SHAPE.APP_TITLE, exact: true }).click();
    await page.getByRole("radio", { name: NR_RUN.APP_SERVER }).click();
    await page.getByRole("checkbox", { name: NR_RUN.PORT_INBOUND_LABEL }).check();
    await expect(page.getByText(NR_RUN.CALLERS_NONE)).toBeVisible();

    await page.getByRole("button", { name: NR_RUN.CALLERS_ADD }).click();
    await page.getByRole("button", { name: "payments" }).click();
    await page.getByRole("button", { name: "billing" }).click();
    await expect(page.getByText(NR_RUN.CALLER_ADDED)).toHaveCount(2);
    await page.getByLabel(NR_RUN.CALLERS_MENU_PERSON).fill("ana@");
    await page.getByRole("dialog").getByRole("button", { name: NR_RUN.CALLER_ADD_PERSON, exact: true }).click();
    await expect(page.getByRole("alert").filter({ hasText: NR_RUN.CALLER_EMAIL_INVALID })).toBeVisible();
    await page.getByLabel(NR_RUN.CALLERS_MENU_PERSON).fill("li.wei@example.com");
    await page.getByRole("dialog").getByRole("button", { name: NR_RUN.CALLER_ADD_PERSON, exact: true }).click();
    await page.keyboard.press("Escape");

    await expect(page.locator("[data-caller]")).toHaveCount(3);
    await expect(page.getByText(NR_RUN.CALLERS_HELP)).toBeVisible();
    await page.getByRole("button", { name: NR_RUN.CALLER_REMOVE_ARIA("billing") }).click();
    await expect(page.locator("[data-caller]")).toHaveCount(2);
    await expect(page.locator("[data-caller='group:payments']")).toBeVisible();
    await expect(page.locator("[data-caller='user:li.wei@example.com']")).toBeVisible();
  });
});
