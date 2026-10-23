/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #2968 (c): a recorded desktop session on the run's Recording tab (board M7-Replay). The seeded `none`
// runner has no desktop, so the stored playback, the desktop's audit rows and the chain verdict are
// spliced on the way past; the real range check is pinned in Go (internal/store/auditchain_range_pg_test.go,
// internal/api/recording_chain_test.go), and the real route's refusal is read here unspliced. The screen
// itself is not drawn: the noVNC decoder is not a console dependency (PLAYER_PENDING).
import { randomUUID } from "node:crypto";
import type { Page, Route } from "@playwright/test";
import { test, expect, consoleAPI, gotoConsole, navToRoute } from "./fixtures";
import { RECORDING_REPLAY as C } from "../src/app/components/wardyn/copy/recording-level";

const START = "2026-10-20T10:40:00Z";
const at = (s: number) => new Date(Date.parse(START) + s * 1000).toISOString();

async function seedRun(page: Page): Promise<string> {
  const created = await consoleAPI(page, "POST", "/api/v1/runs", { agent: "claude-code", task: "desktop-replay e2e " + randomUUID().slice(0, 8) });
  expect(created.status, created.text).toBe(201);
  return JSON.parse(created.text).id as string;
}

test.describe("Desktop replay", () => {
  test("a recorded session marks its gap, jumps from the action log and shows the chain verified", async ({ page }) => {
    await gotoConsole(page, "admin");
    const id = await seedRun(page);
    const key = `${id}~desktop-${randomUUID()}`;
    const ids = { pause: randomUUID(), resume: randomUUID(), paste: randomUUID() };

    // The real route, unspliced: the seeded run has no stored desktop recording, so it answers the
    // recording read's own refusal.
    const refused = await consoleAPI(page, "GET", `/api/v1/runs/${id}/recording-chain?key=${encodeURIComponent(key)}&from=${START}`);
    expect(refused.status, refused.text).toBe(404);
    expect(refused.text.trim()).toBe("recording not found");

    await page.route(new RegExp(`/api/v1/runs/${id}$`), async (route: Route) => {
      if (route.request().method() !== "GET") return route.continue();
      const res = await route.fetch();
      await route.fulfill({ response: res, json: { ...((await res.json()) as object), recording_level: "screen_actions" } });
    });
    const row = (rid: string, action: string, time: string, target: string, data: Record<string, unknown>, actor_type = "system") =>
      ({ id: rid, time, actor_type, actor: "wardynd", run_id: id, action, target, outcome: "success", data });
    await page.route(/\/api\/v1\/audit\?/, async (route: Route) => {
      const url = route.request().url();
      if (url.includes("action_prefix=desktop.recording")) {
        return route.fulfill({
          json: [
            row(ids.pause, "desktop.recording.pause", at(138), id, { reason: "you took over to sign in", gap_start: at(138) }),
            row(ids.resume, "desktop.recording.resume", at(272), id, { reason: "you took over to sign in", gap_start: at(138), gap_end: at(272), gap_ms: 134_000 }),
            row(randomUUID(), "desktop.recording.write", at(850), key, { key, format: "novnc-playback" }),
          ],
        });
      }
      if (url.includes("action_prefix=ui.clipboard")) {
        return route.fulfill({ json: [row(ids.paste, "ui.clipboard.transfer", at(240), "paste_in", { direction: "paste_in", bytes: 214, sha256: "9f2c5d0e7a41a41e" }, "human")] });
      }
      return route.continue();
    });
    await page.route(`**/api/v1/runs/${id}/recording/${encodeURIComponent(key)}`, (route: Route) =>
      route.fulfill({
        contentType: "application/json",
        body: JSON.stringify({ version: 1, format: "novnc-playback", encoding: "base64", started_at: START,
          frames: ["{0{UkZCIDAwMy4wMDgK", "{850000{AAAA"], gaps: [{ start_ms: 138_100, end_ms: 272_000, reason: "you took over to sign in" }] }),
      }),
    );
    await page.route(new RegExp(`/api/v1/runs/${id}/recording-chain\\?`), (route: Route) =>
      route.fulfill({ json: { ok: true, first_seq: 4801, last_seq: 4838, checked: 38, marks: [{ id: ids.pause, seq: 4815 }, { id: ids.resume, seq: 4822 }] } }),
    );

    await navToRoute(page, `/runs/${id}`);
    await page.getByRole("tab", { name: "Recording" }).click();
    await page.getByRole("combobox", { name: "Recorded session" }).click();
    await page.getByRole("option", { name: /^Screen and actions · ended/ }).click();
    const replay = page.getByTestId("desktop-replay");
    const playhead = replay.getByTestId("desktop-replay-playhead");

    // Play runs the clock; Pause stops it.
    await expect(playhead).toHaveAttribute("data-ms", "0");
    await replay.getByRole("button", { name: "4×" }).click();
    await replay.getByRole("button", { name: C.PLAY }).click();
    await expect.poll(async () => Number(await playhead.getAttribute("data-ms"))).toBeGreaterThan(0);
    await replay.getByRole("button", { name: C.PAUSE }).click();

    // The gap is on the timeline; jumping into it from the action log shows the pause, not frames.
    await expect(replay.getByTestId("desktop-replay-gap-mark")).toContainText(C.GAP("2 min 14 s"));
    const actions = replay.getByTestId("desktop-replay-action");
    await expect(actions).toHaveCount(3);
    await actions.filter({ hasText: C.ACT_PASTE }).click();
    await expect(playhead).toHaveAttribute("data-ms", "240000");
    await expect(actions.filter({ hasText: C.ACT_PASTE })).toHaveAttribute("aria-current", "true");
    const gap = replay.getByTestId("desktop-replay-gap");
    await expect(gap).toContainText(C.PAUSED_FOR("2 min 14 s"));
    await gap.getByRole("button", { name: C.SKIP_GAP }).click();
    await expect(playhead).toHaveAttribute("data-ms", "272000");
    await expect(gap).toHaveCount(0);

    // The chain verdict for the recording's range, with the gap's entries.
    const chain = replay.getByTestId("desktop-replay-chain");
    await expect(chain.getByText(C.CHAIN_OK)).toBeVisible();
    await expect(chain.getByTestId("desktop-replay-chain-verdict")).toHaveText(C.CHAIN_RANGE(4801, 4838));
    await expect(chain.getByTestId("desktop-replay-log-row")).toContainText("Entry 4,815 · ");
    await expect(chain.getByTestId("desktop-replay-log-row")).toContainText(C.WRITTEN_BY_VALUE);
    await expect(replay).toContainText(C.KEYSTROKES);
    const download = page.waitForEvent("download");
    await replay.getByRole("button", { name: C.DOWNLOAD }).click();
    expect((await download).suggestedFilename()).toBe(`${key}.json`);
  });
});
