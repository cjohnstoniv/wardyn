/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// 0.9.4 console UX Tier B, run and setup screens: the pins that need a real layout engine (widths,
// wrapping, computed positions). Text-shaped fixes are pinned in vitest next to their screen.
import { randomUUID } from "node:crypto";
import type { Locator, Page } from "@playwright/test";
import { RUN_COCKPIT, RUN_OUTPUT } from "../src/app/components/wardyn/copy";
import { test, expect, asRealMember, consoleAPI, gotoConsole, navToRoute, sidebarLink, sql } from "./fixtures";

const PHONE = { width: 390, height: 844 };

// Load the console at the default viewport (at 390px the sidebar's "Runs" link that gotoConsole waits
// for is not rendered), then shrink to a phone and client-side navigate, as providers.spec.ts does.
async function phoneAt(page: Page, view: "user" | "admin", path: string): Promise<void> {
  await gotoConsole(page, view);
  await page.setViewportSize(PHONE);
  await navToRoute(page, path);
}

async function seedRun(page: Page): Promise<string> {
  const created = await consoleAPI(page, "POST", "/api/v1/runs", {
    agent: "claude-code",
    task: "tier-b e2e " + randomUUID().slice(0, 8),
  });
  expect(created.status, created.text).toBe(201);
  return JSON.parse(created.text).id as string;
}

test.describe("Tier B at 390px", () => {
  // #2846: an unrecorded video row shows no play glyph, and a long title wraps instead of ending in an ellipsis.
  test("Getting started: unrecorded videos have no play icon and no title is cut off", async ({ page }) => {
    await phoneAt(page, "user", "/setup");
    const titles = page.getByTestId("episode-title");
    await expect(titles.first()).toBeVisible();
    const unrecorded = page.locator("div:has(> [data-testid=episode-title])").filter({ hasText: "Not recorded yet" });
    expect(await unrecorded.count()).toBeGreaterThan(0);
    await expect(unrecorded.locator("svg.lucide-play")).toHaveCount(0);
    const cut = await titles.evaluateAll((els) => els.filter((e) => e.scrollWidth > e.clientWidth + 1).length);
    expect(cut).toBe(0);
  });
});

test.describe("Tier B at desktop width", () => {
  // #2848: the five "How it works" cards share one top and one height instead of centring at their own.
  test("Setup intro: the five cards line up", async ({ page }) => {
    await gotoConsole(page, "admin");
    await navToRoute(page, "/admin/setup");
    await page.getByRole("button", { name: "Show intro" }).click();
    const cards = page.getByTestId("how-it-works-card");
    await expect(cards).toHaveCount(5);
    const boxes = await cards.evaluateAll((els) =>
      els.map((e) => {
        const r = e.getBoundingClientRect();
        return { top: Math.round(r.top), height: Math.round(r.height) };
      }),
    );
    expect(new Set(boxes.map((b) => b.top)).size).toBe(1);
    expect(new Set(boxes.map((b) => b.height)).size).toBe(1);
  });
});

test.describe("Tier B runs list", () => {
  // #2905: the member's empty card sits apart from the composer card above it instead of touching it.
  test("Runs (member, no runs): the empty card does not butt against the composer", async ({ page }) => {
    await asRealMember(page);
    await page.route(/\/api\/v1\/runs(\?|$)/, (route) =>
      route.request().method() === "GET" ? route.fulfill({ json: [] }) : route.continue(),
    );
    await gotoConsole(page);
    await navToRoute(page, "/runs");
    const card = (l: Locator) => l.locator("xpath=ancestor-or-self::*[contains(@class,'rounded-xl')][1]");
    const composer = card(page.getByRole("form", { name: "Start a run" }));
    const empty = card(page.getByText("Runs you launch appear here"));
    await expect(empty).toBeVisible();
    const [c, e] = [(await composer.boundingBox())!, (await empty.boundingBox())!];
    expect(e.y - (c.y + c.height)).toBeGreaterThanOrEqual(8);
  });
});

test.describe("Tier B run pages", () => {
  // #2908: a run's own page keeps its parent item lit, in both views.
  for (const view of ["user", "admin"] as const) {
    test(`Run detail (${view} view): the sidebar's Runs item stays highlighted`, async ({ page }) => {
      await gotoConsole(page, view);
      const id = await seedRun(page);
      await navToRoute(page, `${view === "admin" ? "/admin" : ""}/runs/${id}`);
      await expect(page.getByRole("tab", { name: /Overview/ })).toBeVisible();
      await expect(sidebarLink(page, "Runs")).toHaveAttribute("aria-current", "page");
    });
  }
});

const CAPACITY = {
  holding: 14, unknown: 3,
  agent_cpu_request_millis: 30000, agent_cpu_limit_millis: 30000, agent_memory_request_mib: 53248, agent_memory_limit_mib: 53248,
  proxy_cpu_millis: 6000, proxy_memory_mib: 9216, proxy_cpu_uncapped: 0, held_cpu_millis: 36000, held_memory_mib: 62464,
};

test.describe("Tier B runs list at 390px", () => {
  test.use({ viewport: PHONE });

  // #2915: the Fleet capacity summary is plain text under the header's truncate, so a line that does not fit
  // ends in an ellipsis instead of being cut mid-word by a flex wrapper.
  test("Fleet capacity: the summary line is truncatable text", async ({ page }) => {
    await page.route("**/api/v1/admin/runs/capacity", (route) =>
      route.fulfill({
        json: {
          generated_at: new Date().toISOString(),
          basis: "configured_reservations",
          states: { RUNNING: 9, PENDING: 3 },
          paused: 0, kept: 0,
          totals: { basis: "requests", ...CAPACITY },
          age_buckets: [],
          by_runner: { k8s: { basis: "requests", ...CAPACITY } },
          by_owner: [],
          by_owner_truncated: false,
          unschedulable: [],
          unschedulable_total: 0,
        },
      }),
    );
    await phoneAt(page, "admin", "/admin/runs");
    const summary = page.getByTestId("fleet-capacity-card").locator("span.truncate").first();
    await expect(summary).toContainText("configured reservations");
    const direct = await summary.evaluate((el) =>
      [...el.childNodes].some((n) => n.nodeType === Node.TEXT_NODE && (n.textContent ?? "").includes("configured reservations")),
    );
    expect(direct).toBe(true);
    expect(await summary.evaluate((el) => getComputedStyle(el).textOverflow)).toBe("ellipsis");
  });
});

test.describe("Tier B run header", () => {
  // #2916: with room to spare at desktop width, the pending-approval chip shows its whole sentence.
  test("Run detail header: '1 waiting · sandbox held' is not cut off at 1440", async ({ page }) => {
    await gotoConsole(page);
    const id = await seedRun(page);
    sql(`UPDATE agent_runs SET state = 'WAITING_FOR_CONFIRMATION' WHERE id = '${id}'`);
    sql(
      `INSERT INTO approvals (id, run_id, kind, requested_scope, state, requested_at) VALUES
       ('${randomUUID()}','${id}','tool_call','{"tool":"Bash","cmd":"ls"}'::jsonb,'PENDING',now())`,
    );
    await navToRoute(page, `/runs/${id}`);
    const chip = page.locator("span.truncate", { hasText: /^1 waiting · sandbox held$/ });
    await expect(chip).toBeVisible();
    expect(await chip.evaluate((el) => el.scrollWidth <= el.clientWidth + 1)).toBe(true);
  });
});

test.describe("Tier B run Overview", () => {
  // #2919: a failed run with no recognised ending and no hint still says what it knows, not a lone button.
  test("Failed run with no recorded cause: the card says so above Open audit trail", async ({ page }) => {
    await gotoConsole(page);
    const id = await seedRun(page);
    sql(`UPDATE agent_runs SET state = 'FAILED' WHERE id = '${id}'`);
    await navToRoute(page, `/runs/${id}`);
    const block = page.getByTestId("run-failure-block");
    await expect(block).toBeVisible();
    await expect(block).toContainText("What happened");
    await expect(block).toContainText("no recorded cause");
    await expect(block.getByRole("button", { name: /Open audit trail/ })).toBeVisible();
  });
});

test.describe("Tier B run tabs", () => {
  // #2920: a known limit of this server is not titled as a transient load error.
  test("Output tab: the live-output limit has its own title, not 'Couldn't load right now'", async ({ page }) => {
    await page.route("**/api/v1/runs/*/output", (route) =>
      route.fulfill({ status: 503, json: { error: "refused", reason: "mask_state_unavailable" } }),
    );
    await gotoConsole(page);
    const id = await seedRun(page);
    await navToRoute(page, `/runs/${id}`);
    await page.getByRole("tab", { name: RUN_OUTPUT.tab }).click();
    await expect(page.getByText(RUN_OUTPUT.maskTitle)).toBeVisible();
    await expect(page.getByText(RUN_OUTPUT.maskUnavailable)).toBeVisible();
    await expect(page.getByText(RUN_COCKPIT.loadError)).toHaveCount(0);
  });
});

test.describe("Tier B run layout editor", () => {
  // #2928: the editor and the cockpit name the main tile the same.
  test("Edit layout: an autonomous run's main tile handle reads Output, like its pane", async ({ page }) => {
    await gotoConsole(page);
    const id = await seedRun(page);
    await navToRoute(page, `/runs/${id}`);
    await page.getByRole("button", { name: "Edit layout" }).click();
    const handle = page.getByTestId("run-terminal-pane").locator("[data-drag-handle]");
    await expect(handle).toContainText("Output");
  });
});
