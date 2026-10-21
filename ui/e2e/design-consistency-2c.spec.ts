/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Console design-consistency pins that need a real layout or a real backend row: one eyebrow look,
// one duration voice and the quiet in-card empty state.
import { randomUUID } from "node:crypto";
import type { Page } from "@playwright/test";
import { test, expect, consoleAPI, expandCard, gotoConsole, navToRoute, openNewRun, sql } from "./fixtures";

async function seedRun(page: Page): Promise<string> {
  const created = await consoleAPI(page, "POST", "/api/v1/runs", {
    agent: "claude-code",
    task: "design 2c " + randomUUID().slice(0, 8),
  });
  expect(created.status, created.text).toBe(201);
  return JSON.parse(created.text).id as string;
}

test.describe("Design consistency", () => {
  // The step eyebrow is the shared micro-label with the primary variant: uppercase, tracked at 0.12em, in the primary colour.
  test("New Run: the step eyebrow is the shared primary eyebrow", async ({ page }) => {
    await gotoConsole(page);
    await openNewRun(page);
    const eyebrow = page.locator(".label-eyebrow.label-eyebrow-primary").first();
    await expect(eyebrow).toBeVisible();
    const style = await eyebrow.evaluate((el) => {
      const cs = getComputedStyle(el);
      const probe = document.createElement("div");
      probe.style.color = "var(--primary)";
      document.body.appendChild(probe);
      const primary = getComputedStyle(probe).color;
      probe.remove();
      return { transform: cs.textTransform, spacing: cs.letterSpacing, size: cs.fontSize, color: cs.color, primary };
    });
    expect(style.transform).toBe("uppercase");
    expect(style.size).toBe("11px");
    expect(style.spacing).toBe("1.32px");
    expect(style.color).toBe(style.primary);
  });

  // One duration voice: a finished run's total time reads "1m 15s", not "75s" or "00:01:15".
  test("Run header: a finished run's total time reads in minutes and seconds", async ({ page }) => {
    await gotoConsole(page);
    const id = await seedRun(page);
    sql(`UPDATE agent_runs SET state = 'COMPLETED', updated_at = created_at + interval '75 seconds' WHERE id = '${id}'`);
    await navToRoute(page, `/runs/${id}`);
    await expect(page.locator('[aria-label="Total run time 1m 15s"]')).toHaveCount(1);
  });

  // The compact empty state is the quiet in-card block, not the page-sized hero.
  test("Admin SSH keys: the empty block is the compact one", async ({ page }) => {
    await page.route(/\/api\/v1\/me\/ssh-keys(\?|$)/, (route) =>
      route.request().method() === "GET" ? route.fulfill({ json: [] }) : route.continue(),
    );
    await gotoConsole(page, "admin");
    await navToRoute(page, "/admin/settings");
    await expandCard(page, "Admin SSH keys");
    const block = page.getByTestId("admin-ssh-keys-card").locator('[data-size="compact"]');
    await expect(block).toBeVisible();
    const box = (await block.boundingBox())!;
    expect(box.height).toBeLessThan(220);
    await page.unrouteAll({ behavior: "ignoreErrors" });
  });

  // The same quiet block on the screens the compact size changed: People, Permissions and Governance.
  for (const c of [
    { name: "People", path: "/admin/people", api: /\/api\/v1\/people(\?|$)/, json: { people: [] }, title: "No people yet" },
    { name: "Permissions", path: "/admin/permissions", api: /\/api\/v1\/permissions(\?|$)/, json: { grants: [], enforcement: {} }, title: "No grants yet" },
    { name: "Governance", path: "/admin/governance", api: /\/api\/v1\/governance(\?|$)/, json: { profiles: [], assignments: [] }, title: "No profiles yet" },
  ]) {
    test(`${c.name}: the empty block is the compact one`, async ({ page }) => {
      await page.route(c.api, (route) => (route.request().method() === "GET" ? route.fulfill({ json: c.json }) : route.fallback()));
      await gotoConsole(page, "admin");
      await navToRoute(page, c.path);
      const heading = page.getByRole("heading", { name: c.title, level: 4 });
      await expect(heading).toBeVisible();
      const block = page.locator('[data-size="compact"]').filter({ has: heading });
      // The compact block is padded 32px top and bottom; the page-sized hero is 64px. Its height depends on the copy.
      expect(await block.evaluate((el) => getComputedStyle(el).paddingTop)).toBe("32px");
      await page.unrouteAll({ behavior: "ignoreErrors" });
    });
  }

  // The run's Audit tab reads the Audit screen's sentences, keeps the raw key in the tooltip, and does not
  // print the viewed run's own id on every row.
  test("Run Audit tab: sentences, raw key in the tooltip, no repeated run id", async ({ page }) => {
    await gotoConsole(page);
    const id = await seedRun(page);
    await navToRoute(page, `/runs/${id}`);
    await page.getByRole("tab", { name: /^Audit/ }).click();
    const created = page.getByTestId("run-audit-action").filter({ hasText: "Created the run" }).first();
    await expect(created).toBeVisible();
    await expect(created).toHaveAttribute("title", "Created the run (run.create)");
    await expect(page.getByTestId("run-audit-row").filter({ hasText: id })).toHaveCount(0);
  });
});
