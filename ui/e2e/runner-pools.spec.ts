/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, gotoConsole } from "./fixtures";
import { POOLS, RUNNER_V6 } from "../src/app/components/wardyn/copy/runner-pools";
import { RUNNERS } from "../src/app/components/wardyn/copy/runners";

// Admin -> Runners against the real server (mock M-POOLS v6). The harness has no container runtime, so
// it is set up with no remote-hosted runner and its first boot makes no organization pool, and runners
// are off: this pins the page's two tabs and what each says when the real routes answer with nothing
// (the remote-hosted runners, the pool list, the organization default). What the tabs show with runners
// and pools in them is pinned in the component suites beside each screen.
test("Runners has one tab strip, and the tab is in the address", async ({ page }) => {
  await gotoConsole(page, "admin");
  await page.goto("/admin/runners");
  const strip = page.getByRole("navigation", { name: RUNNER_V6.TABS_LABEL });
  await expect(strip.getByRole("button", { name: RUNNER_V6.TAB_RUNNERS })).toHaveAttribute("aria-current", "page");
  await expect(page.getByText(RUNNER_V6.LEAD)).toBeVisible();
  await expect(page.getByText(RUNNER_V6.TAB_PURPOSE)).toBeVisible();
  // No remote-hosted runner is set up, and people's runners are off.
  await expect(page.getByRole("heading", { name: RUNNERS.OFF_TITLE })).toBeVisible();
  await expect(page.getByRole("list", { name: RUNNER_V6.LIST_LABEL })).toHaveCount(0);

  await strip.getByRole("button", { name: RUNNER_V6.TAB_POOLS }).click();
  await expect(page).toHaveURL(/\/admin\/runners\?tab=pools$/);
  await expect(strip.getByRole("button", { name: RUNNER_V6.TAB_POOLS })).toHaveAttribute("aria-current", "page");
  await expect(page.getByText(POOLS.LEAD)).toBeVisible();
  await expect(page.getByText(RUNNER_V6.TAB_PURPOSE)).toHaveCount(0);

  await strip.getByRole("button", { name: RUNNER_V6.TAB_RUNNERS }).click();
  await expect(page).toHaveURL(/\/admin\/runners$/);
  await expect(page.getByRole("heading", { name: RUNNERS.OFF_TITLE })).toBeVisible();
});

test("the Pools tab says how an organization pool is made and that there are none yet", async ({ page }) => {
  await gotoConsole(page, "admin");
  await page.goto("/admin/runners?tab=pools");
  await expect(page.getByRole("heading", { name: POOLS.ORG_TITLE, exact: true })).toBeVisible();
  await expect(page.getByText(POOLS.ORG_LEDE)).toBeVisible();
  await expect(page.getByText(POOLS.ORG_CLI_COMMAND)).toBeVisible();
  await expect(page.getByRole("group", { name: POOLS.ORG_EMPTY_TITLE })).toContainText(POOLS.ORG_EMPTY_BODY);
  await expect(page.getByRole("heading", { name: POOLS.USER_TITLE, exact: true })).toBeVisible();
  await expect(page.getByRole("main").getByRole("link", { name: POOLS.GOVERNANCE_LINK })).toHaveAttribute("href", /\/admin\/governance$/);
  await expect(page.getByRole("group", { name: POOLS.USER_EMPTY_TITLE })).toContainText(POOLS.USER_EMPTY_BODY);
  // Nothing here is the server refusing: no error, no "unavailable".
  await expect(page.getByText(POOLS.UNAVAILABLE_TITLE)).toHaveCount(0);
  await expect(page.getByText(POOLS.ERROR_TITLE)).toHaveCount(0);
});
