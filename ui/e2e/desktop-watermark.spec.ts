/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { Page } from "@playwright/test";
import { test, expect, sql, consoleAPI, gotoConsole } from "./fixtures";
import { DESKTOP } from "../src/app/components/wardyn/copy/desktop";

// ============================================================================
// The desktop watermark on the gateway's own viewer page (#2969). The `none` runner has no VM to open a session on,
// so the page gets an open boot that carries `watermark`, and its stream answers the RFB handshake so the viewer
// reaches "connected" (the layer is drawn only while connected). Who is looking and the rule come from the boot data;
// the Go gateway tests prove what the gateway puts there.
// ============================================================================

const RUN_TASK = "e2e fixture 2";
const WHO = "alex@example.com";

// The bytes that bring the viewer to "connected", sent in one message the way viewer.test.tsx's HANDSHAKE is.
const HANDSHAKE = [...new TextEncoder().encode("RFB 003.008\n"), 1, 1, 0, 0, 0, 0, 0, 2, 0, 1, ...new Array(16).fill(0), 0, 0, 0, 0];

async function openViewer(page: Page, required: boolean): Promise<string> {
  const runId = sql(`SELECT id FROM agent_runs WHERE task = '${RUN_TASK}' LIMIT 1`);
  expect(runId, `no seeded run with task ${RUN_TASK}`).toMatch(/^[0-9a-f-]{36}$/);
  await gotoConsole(page);
  const health = JSON.parse((await consoleAPI(page, "GET", "/healthz")).text);
  const viewer = new URL(`../r/${runId}/__desktop/`, health.ui_sandbox.enter_post_url as string);
  const path = viewer.pathname.replace(/\/$/, "");
  const boot = { state: "open", expires: 4102444800, rfb: `${path}/__rfb`, close: `${path}/__close`, watermark: { who: WHO, run: runId, required } };
  await page.route(viewer.href, async (route) => {
    const res = await route.fetch();
    const body = (await res.text()).replace(/data-wardyn-desktop="[^"]*"/, `data-wardyn-desktop="${JSON.stringify(boot).replace(/"/g, "&quot;")}"`);
    await route.fulfill({ response: res, status: 200, body });
  });
  await page.routeWebSocket(`**${path}/__rfb`, (ws) => {
    ws.send(Buffer.from(HANDSHAKE));
  });
  await page.goto(viewer.href);
  await expect(page.getByText(DESKTOP.connected)).toBeVisible();
  return runId;
}

test.describe("Desktop watermark on the live viewer", () => {
  test("a required watermark is over the picture, with no toggle", async ({ page }) => {
    const runId = await openViewer(page, true);
    const layer = page.getByTestId("desktop-watermark");
    await expect(layer).toBeVisible();
    await expect(layer).toContainText(`${WHO} · ${runId}`);
    await expect(page.getByText(DESKTOP.watermarkRequired)).toBeVisible();
    await expect(page.getByRole("button", { name: DESKTOP.watermarkToggle })).toHaveCount(0);
    // It covers the picture area and not the header: the layer sits inside the picture wrapper, below the header.
    const picture = (await page.getByTestId("desktop-picture").boundingBox())!;
    const mark = (await layer.boundingBox())!;
    const header = (await page.getByRole("banner").boundingBox())!;
    expect(mark.y).toBeGreaterThanOrEqual(header.y + header.height - 1);
    expect(mark.y).toBeCloseTo(picture.y, 0);
    expect(mark.height).toBeCloseTo(picture.height, 0);
  });

  test("an optional watermark is off until the toggle turns it on", async ({ page }) => {
    await openViewer(page, false);
    await expect(page.getByTestId("desktop-watermark")).toHaveCount(0);
    const toggle = page.getByRole("button", { name: DESKTOP.watermarkToggle });
    await expect(toggle).toHaveAttribute("aria-pressed", "false");
    await toggle.click();
    await expect(toggle).toHaveAttribute("aria-pressed", "true");
    await expect(page.getByTestId("desktop-watermark")).toBeVisible();
    await toggle.click();
    await expect(page.getByTestId("desktop-watermark")).toHaveCount(0);
  });
});
