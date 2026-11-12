/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, gotoConsole, sql } from "./fixtures";
import type { Page } from "@playwright/test";
import { MODEL_USAGE } from "../src/app/components/wardyn/copy";

// The run page's Model usage tile: the counts the run proxy read, the line for calls it could not count, the line for
// encrypted connections it did not open, and the tile's own line on a server that has no such route.
test.use({ colorScheme: "dark" });

const tokens = (o: Record<string, number>) => ({
  input_tokens: 0,
  output_tokens: 0,
  cache_read_tokens: 0,
  cache_write_tokens: 0,
  reasoning_tokens: 0,
  calls: 0,
  uncounted_calls: 0,
  partial_calls: 0,
  ...o,
});

const USAGE = {
  totals: tokens({ input_tokens: 1280, output_tokens: 320, cache_read_tokens: 900, calls: 9, uncounted_calls: 3 }),
  by_model: [
    { provider: "anthropic", vendor: "anthropic", host: "api.anthropic.com", model: "claude-sonnet-4-5", uncounted_reasons: { undecodable: 3 }, ...tokens({ input_tokens: 1200, output_tokens: 300, cache_read_tokens: 900, calls: 7, uncounted_calls: 3 }) },
    { provider: "anthropic", vendor: "anthropic", host: "api.anthropic.com", model: "claude-haiku-4-5", uncounted_reasons: {}, ...tokens({ input_tokens: 80, output_tokens: 20, calls: 2 }) },
  ],
  opaque_tunnels: 2,
};

async function openRun(page: Page): Promise<void> {
  const id = sql("SELECT id FROM agent_runs ORDER BY created_at LIMIT 1");
  sql(`UPDATE agent_runs SET state = 'RUNNING' WHERE id = '${id}'`);
  await gotoConsole(page);
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.goto(`/runs/${id}`);
}

const tile = (page: Page) => page.locator('[data-widget="model-usage"]');

test("the tile shows the totals, the uncounted line and the encrypted-connections line", async ({ page }) => {
  await page.route("**/api/v1/runs/*/model-usage", (route) => route.fulfill({ json: USAGE }));
  await openRun(page);
  const t = tile(page);
  await t.scrollIntoViewIfNeeded();
  await expect(t.getByRole("heading", { name: MODEL_USAGE.title })).toBeVisible();
  await expect(t.getByText("1,280")).toBeVisible();
  await expect(t.getByText(MODEL_USAGE.uncountedHead(3))).toBeVisible();
  await expect(t.getByText(/all arrived compressed in a way Wardyn can't read/)).toBeVisible();
  await expect(t.getByText(MODEL_USAGE.encryptedHead(2))).toBeVisible();
  await expect(t.getByText(MODEL_USAGE.footnote(["anthropic"]))).toBeVisible();
});

test("a run with nothing to report has no uncounted or encrypted line", async ({ page }) => {
  await page.route("**/api/v1/runs/*/model-usage", (route) => route.fulfill({ json: { ...USAGE, totals: tokens({ input_tokens: 5, output_tokens: 1, calls: 1 }), by_model: [{ ...USAGE.by_model[1], ...tokens({ input_tokens: 5, output_tokens: 1, calls: 1 }) }], opaque_tunnels: 0 } }));
  await openRun(page);
  const t = tile(page);
  await t.scrollIntoViewIfNeeded();
  await expect(t.getByText("claude-haiku-4-5 · Anthropic · 1 call")).toBeVisible();
  await expect(t.getByText(/not counted/)).toHaveCount(0);
  await expect(t.getByText(/encrypted connection/)).toHaveCount(0);
});

test("a server with no model-usage route keeps the tile and says so", async ({ page }) => {
  await page.route("**/api/v1/runs/*/model-usage", (route) => route.fulfill({ status: 404, contentType: "text/plain", body: "404 page not found" }));
  await openRun(page);
  const t = tile(page);
  await t.scrollIntoViewIfNeeded();
  await expect(t.getByText(MODEL_USAGE.unsupported)).toBeVisible();
});
