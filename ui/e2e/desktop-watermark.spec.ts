/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { randomUUID } from "node:crypto";
import type { Page, Route } from "@playwright/test";
import { test, expect, sql, consoleAPI, gotoConsole, navToRoute } from "./fixtures";
import { DESKTOP } from "../src/app/components/wardyn/copy/desktop";

// ============================================================================
// The desktop watermark on the gateway's own viewer page (#2969). The `none` runner has no VM to open a session on,
// so the page gets an open boot that carries `watermark`, and its stream answers the RFB handshake so the viewer
// reaches "connected" (the layer is drawn only while connected). Who is looking and the rule come from the boot data;
// the Go gateway tests prove what the gateway puts there. The replay case does the same for the run page: the run's
// GET carries `desktop_watermark`, and the Recording tab passes it on to the replay.
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

const START = "2026-10-20T10:40:00Z";

// A server's side of an RFB 3.8 session: no auth, a 64x48 desktop, one orange Raw rectangle (as desktop-replay.spec.ts).
function rfbFrames(): string[] {
  const u16 = (n: number) => [n >> 8, n & 255];
  const u32 = (n: number) => [...u16(n >>> 16), ...u16(n & 0xffff)];
  const [w, h] = [64, 48];
  const pixelFormat = [32, 24, 0, 1, ...u16(255), ...u16(255), ...u16(255), 16, 8, 0, 0, 0, 0];
  const messages = [
    [...Buffer.from("RFB 003.008\n")],
    [1, 1],
    u32(0),
    [...u16(w), ...u16(h), ...pixelFormat, ...u32(2), ...Buffer.from("wd")],
    [0, 0, ...u16(1), ...u16(0), ...u16(0), ...u16(w), ...u16(h), ...u32(0), ...Array.from({ length: w * h }, () => [0xff, 0x40, 0, 0]).flat()],
  ];
  return messages.map((m, i) => `{${i * 10}{${Buffer.from(m).toString("base64")}`);
}

test.describe("Desktop watermark on the recording replay", () => {
  test("the run's watermark reaches the replay and sits over its screen box", async ({ page }) => {
    await gotoConsole(page, "admin");
    const created = await consoleAPI(page, "POST", "/api/v1/runs", { agent: "claude-code", task: "desktop-watermark e2e " + randomUUID().slice(0, 8) });
    expect(created.status, created.text).toBe(201);
    const id = JSON.parse(created.text).id as string;
    const key = `${id}~desktop-${randomUUID()}`;

    // The seeded `none` runner has no desktop: the run is shown with the watermark the server would hand a desktop run, and
    // its stored recording and audit row are spliced in, as desktop-replay.spec.ts does.
    await page.route(new RegExp(`/api/v1/runs/${id}$`), async (route: Route) => {
      if (route.request().method() !== "GET") return route.continue();
      const res = await route.fetch();
      await route.fulfill({ response: res, json: { ...((await res.json()) as object), recording_level: "screen_actions", desktop_watermark: { who: WHO, run: id, required: true } } });
    });
    await page.route(/\/api\/v1\/audit\?/, async (route: Route) => {
      const url = route.request().url();
      if (url.includes("action_prefix=desktop.recording")) {
        const write = { id: randomUUID(), time: new Date(Date.parse(START) + 850_000).toISOString(), actor_type: "system", actor: "wardynd", run_id: id, action: "desktop.recording.write", target: key, outcome: "success", data: { key, format: "novnc-playback" } };
        return route.fulfill({ json: [write] });
      }
      if (url.includes("action_prefix=ui.clipboard")) return route.fulfill({ json: [] });
      return route.continue();
    });
    await page.route(`**/api/v1/runs/${id}/recording/${encodeURIComponent(key)}`, (route: Route) =>
      route.fulfill({ contentType: "application/json", body: JSON.stringify({ version: 1, format: "novnc-playback", encoding: "base64", started_at: START, frames: rfbFrames(), gaps: [] }) }),
    );
    await page.route(new RegExp(`/api/v1/runs/${id}/recording-chain\\?`), (route: Route) =>
      route.fulfill({ json: { ok: true, first_seq: 1, last_seq: 2, checked: 2, marks: [] } }),
    );

    await navToRoute(page, `/runs/${id}`);
    await page.getByRole("tab", { name: "Recording" }).click();
    await page.getByRole("combobox", { name: "Recorded session" }).click();
    await page.getByRole("option", { name: /^Screen and actions · ended/ }).click();

    const screen = page.getByTestId("desktop-replay-screen");
    const layer = page.getByTestId("desktop-watermark");
    await expect(screen).toBeVisible();
    await expect(layer).toBeVisible();
    await expect(layer).toContainText(`${WHO} · ${id}`);
    await expect(page.getByText(DESKTOP.watermarkRequired)).toBeVisible();
    // Over the screen box, as a sibling of it: the layer spans the box and sits outside it.
    const box = (await screen.boundingBox())!;
    const mark = (await layer.boundingBox())!;
    expect(mark.x).toBeLessThanOrEqual(box.x + 1);
    expect(mark.y).toBeLessThanOrEqual(box.y + 1);
    expect(mark.x + mark.width).toBeGreaterThanOrEqual(box.x + box.width - 1);
    expect(mark.y + mark.height).toBeGreaterThanOrEqual(box.y + box.height - 1);
    await expect(screen.getByTestId("desktop-watermark")).toHaveCount(0);
  });
});
