/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// 0.9.4 console UX Tier B, run and setup screens: the pins that need a real layout engine (widths,
// wrapping, computed positions). Text-shaped fixes are pinned in vitest next to their screen.
import { randomUUID } from "node:crypto";
import type { Locator, Page } from "@playwright/test";
import { RUN_COCKPIT, RUN_OUTPUT } from "../src/app/components/wardyn/copy";
import {
  test, expect, asRealMember, chooseFiles, chooseRunMode, chooseShape, consoleAPI, continueNewRun, firstHarness, gotoConsole,
  mockPreflightClear, navToRoute, sql, startGuided, walkGuidedToFiles, walkGuidedToReview,
} from "./fixtures";
import { NR_FILES } from "../src/app/components/wardyn/copy/new-run/files";
import { NR_HARNESSES } from "../src/app/components/wardyn/copy/new-run/harnesses";
import { NR_REVIEW } from "../src/app/components/wardyn/copy/new-run/review";
import { NR_SHARED } from "../src/app/components/wardyn/copy/new-run/shared";
import { NR_TOOLS } from "../src/app/components/wardyn/copy/new-run/tools";

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
    await expect(unrecorded.getByTestId("episode-play")).toHaveCount(0);
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
    const composer = page.getByTestId("runs-composer");
    const empty = page.getByTestId("runs-member-empty");
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
      // The run header's breadcrumb is also a "Runs" link, so look inside the sidebar only.
      await expect(page.locator("aside").getByRole("link", { name: /^Runs/ })).toHaveAttribute("aria-current", "page");
    });
  }
});

const CAPACITY = {
  holding: 14, unknown: 3,
  agent_cpu_request_millis: 30000, agent_cpu_limit_millis: 30000, agent_memory_request_mib: 53248, agent_memory_limit_mib: 53248,
  proxy_cpu_millis: 6000, proxy_memory_mib: 9216, proxy_cpu_uncapped: 0, held_cpu_millis: 36000, held_memory_mib: 62464,
};

test.describe("Tier B runs list at 390px", () => {

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
    const summary = page.getByTestId("fleet-capacity-card-summary");
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
    await page.setViewportSize({ width: 1440, height: 900 });
    const id = await seedRun(page);
    sql(`UPDATE agent_runs SET state = 'WAITING_FOR_CONFIRMATION' WHERE id = '${id}'`);
    sql(
      `INSERT INTO approvals (id, run_id, kind, requested_scope, state, requested_at) VALUES
       ('${randomUUID()}','${id}','tool_call','{"tool":"Bash","cmd":"ls"}'::jsonb,'PENDING',now())`,
    );
    await navToRoute(page, `/runs/${id}`);
    const chip = page.getByTestId("pending-approval-chip-text");
    await expect(chip).toBeVisible();
    expect(await chip.evaluate((el) => el.scrollWidth <= el.clientWidth + 1)).toBe(true);
    // The chip is a link to the Approvals tab (#3032).
    await expect(chip).toHaveText(/Review/);
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

test.describe("Tier B guided setup", () => {
  const top = async (l: Locator) => Math.round((await l.boundingBox())!.y);

  // #2862, #2863: the Folder and Subfolder fields start on one line, and Required and Optional share the
  // neutral muted colour of the text beside them.
  test("Files: Folder and Subfolder line up; Required and Optional are neutral", async ({ page }) => {
    await gotoConsole(page);
    await navToRoute(page, "/runs/new");
    await walkGuidedToFiles(page);
    const main = page.getByRole("region", { name: NR_FILES.MAIN_TITLE });
    const start = page.getByRole("region", { name: NR_FILES.START_TITLE });
    await expect(start).toBeVisible();
    expect(await top(page.locator("#nr-starts-here"))).toBe(await top(page.locator("#nr-start-subfolder")));
    const color = (l: Locator) => l.evaluate((el) => getComputedStyle(el).color);
    expect(await color(main.getByText(NR_SHARED.REQUIRED, { exact: true }).first())).toBe(await color(main.getByText(NR_FILES.MAIN_BODY)));
    expect(await color(start.getByText(NR_SHARED.OPTIONAL, { exact: true }).first())).toBe(await color(start.getByText(NR_FILES.START_BODY)));
  });

  // #2869: the main switch's label is body size, not 11px.
  test("Harnesses: the 'Start the run in this harness' label is body size", async ({ page }) => {
    await gotoConsole(page);
    await navToRoute(page, "/runs/new");
    await startGuided(page);
    await chooseShape(page, "agent_task");
    await chooseRunMode(page, "interactive");
    await continueNewRun(page, NR_HARNESSES.TITLE);
    await firstHarness(page);
    const label = page.getByText(NR_HARNESSES.START_IN_HARNESS, { exact: true });
    expect(await label.evaluate((el) => parseFloat(getComputedStyle(el).fontSize))).toBeGreaterThanOrEqual(14);
  });

  // #2866: with no harness the empty box and the Add button sit one step gap apart, not two.
  test("Harnesses (none added): the empty box and the Add button are one gap apart", async ({ page }) => {
    await gotoConsole(page);
    await navToRoute(page, "/runs/new");
    await startGuided(page);
    await chooseShape(page, "environment");
    await continueNewRun(page, NR_HARNESSES.TITLE);
    const box = page.getByText(NR_HARNESSES.EMPTY_TITLE, { exact: true }).locator("xpath=..");
    await expect(box).toBeVisible();
    const b = (await box.boundingBox())!;
    const add = (await page.locator("#nr-harness").boundingBox())!;
    expect(add.y - (b.y + b.height)).toBeLessThan(32);
  });

  // #2867, #2868: sentence-case timing options, and the singular count line on Review.
  test("Autonomous run: 'Run now' on the Run step, and one harness reads singular on Review", async ({ page }) => {
    await mockPreflightClear(page);
    await gotoConsole(page);
    await navToRoute(page, "/runs/new");
    await startGuided(page);
    await chooseShape(page, "agent_task");
    await chooseRunMode(page, "autonomous");
    await expect(page.getByRole("radio", { name: "Run now", exact: true })).toBeVisible();
    await expect(page.getByRole("radio", { name: "Schedule for later", exact: true })).toBeVisible();
    await continueNewRun(page, NR_HARNESSES.TITLE);
    await firstHarness(page, "e2e task");
    await continueNewRun(page, NR_FILES.TITLE);
    await chooseFiles(page);
    await continueNewRun(page, NR_TOOLS.TITLE);
    await continueNewRun(page, NR_REVIEW.TITLE);
    await expect(page.getByText("Autonomous · Run now").first()).toBeVisible();
    await expect(page.getByText("1 harness starts automatically").first()).toBeVisible();
    await expect(page.getByText(/1 of 1 harnesses/)).toHaveCount(0);
  });
});

test.describe("Tier B run tabs at 390px", () => {

  // #2917: the time stays on one line and the target gets a second line of its own instead of vanishing.
  test("Audit tab: rows keep the time whole and show the target on a second line", async ({ page }) => {
    await gotoConsole(page);
    const id = await seedRun(page);
    await page.setViewportSize(PHONE);
    await navToRoute(page, `/runs/${id}`);
    await page.getByRole("tab", { name: /^Audit/ }).click();
    const row = page.getByTestId("run-audit-row").first();
    await expect(row).toBeVisible();
    const target = row.getByTestId("run-audit-target");
    const [r, t, time] = [(await row.boundingBox())!, (await target.boundingBox())!, (await row.getByTestId("run-audit-time").boundingBox())!];
    expect(time.height).toBeLessThan(20);
    expect(t.y).toBeGreaterThan(time.y + time.height - 1);
    expect(t.width).toBeGreaterThan(100);
    expect(t.x + t.width).toBeLessThanOrEqual(r.x + r.width + 1);
  });

  // #2924: the requested command wraps inside its block, so none of it is hidden behind a sideways scroll.
  test("Approvals tab: a pending tool call's command is not clipped", async ({ page }) => {
    await gotoConsole(page);
    const id = await seedRun(page);
    sql(`UPDATE agent_runs SET state = 'WAITING_FOR_CONFIRMATION' WHERE id = '${id}'`);
    sql(
      `INSERT INTO approvals (id, run_id, kind, requested_scope, state, requested_at) VALUES
       ('${randomUUID()}','${id}','tool_call','{"tool":"Bash","command":"rm -rf node_modules && pnpm install --frozen-lockfile"}'::jsonb,'PENDING',now())`,
    );
    await page.setViewportSize(PHONE);
    await navToRoute(page, `/runs/${id}`);
    await page.getByRole("tab", { name: /^Approvals/ }).click();
    // The scope is collapsed behind its own disclosure; opening it is what puts the command on screen.
    await page.getByText("View requested scope").first().click();
    const pre = page.locator("pre", { hasText: "rm -rf node_modules" }).first();
    await expect(pre).toBeVisible();
    expect(await pre.evaluate((el) => el.scrollWidth <= el.clientWidth + 1)).toBe(true);
  });
});

test.describe("Tier B run tabs at desktop width", () => {
  // #2925: the empty Approvals card stops at the width the populated list and the other tabs use.
  test("Approvals tab (none): the empty card is no wider than the populated list", async ({ page }) => {
    await gotoConsole(page);
    const id = await seedRun(page);
    await navToRoute(page, `/runs/${id}`);
    await page.getByRole("tab", { name: /^Approvals/ }).click();
    const card = page.getByTestId("run-approvals-empty");
    await expect(card).toBeVisible();
    expect((await card.boundingBox())!.width).toBeLessThanOrEqual(768 + 1);
  });
});

test.describe("Tier B review footer", () => {
  // #2864 Option B: at phone width Launch run stays on screen in a bottom bar and the Save actions sit behind More.
  test.describe("at 390px", () => {
    test("Review: Launch run is on screen without scrolling, and both Save actions are in More", async ({ page }) => {
      await mockPreflightClear(page);
      await gotoConsole(page);
      await page.setViewportSize(PHONE);
      await navToRoute(page, "/runs/new");
      await walkGuidedToReview(page);
      const launch = page.getByRole("button", { name: NR_REVIEW.LAUNCH_RUN });
      await expect(launch).toBeVisible();
      await page.evaluate(() => window.scrollTo(0, 0));
      await page.getByRole("main").evaluate((el) => el.scrollTo(0, 0));
      const box = (await launch.boundingBox())!;
      expect(box.y + box.height).toBeLessThanOrEqual(PHONE.height);
      expect(box.height).toBeGreaterThanOrEqual(40);
      await expect(page.getByRole("button", { name: NR_REVIEW.SAVE_TEMPLATE })).toHaveCount(0);
      const more = page.getByRole("button", { name: NR_REVIEW.MORE_ARIA });
      await more.click();
      await expect(page.getByRole("menuitem", { name: NR_REVIEW.SAVE_TEMPLATE })).toBeVisible();
      await page.keyboard.press("Escape");
      await more.click();
      await expect(page.getByRole("menuitem", { name: NR_REVIEW.SAVE_POLICY })).toBeVisible();
    });
  });

  // From sm up the footer is what it was: the Save buttons in the footer, no More, a regular-height Launch run.
  test("Review at 1440: the Save buttons are in the footer, with no More menu", async ({ page }) => {
    await mockPreflightClear(page);
    await gotoConsole(page);
    await page.setViewportSize({ width: 1440, height: 900 });
    await navToRoute(page, "/runs/new");
    await walkGuidedToReview(page);
    await expect(page.getByRole("button", { name: NR_REVIEW.SAVE_TEMPLATE })).toBeVisible();
    await expect(page.getByRole("button", { name: NR_REVIEW.SAVE_POLICY })).toBeVisible();
    await expect(page.getByRole("button", { name: NR_REVIEW.MORE_ARIA })).toHaveCount(0);
    expect((await page.getByRole("button", { name: NR_REVIEW.LAUNCH_RUN }).boundingBox())!.height).toBeLessThan(40);
    expect(await page.getByTestId("step-footer").evaluate((el) => getComputedStyle(el).position)).toBe("static");
  });
});
