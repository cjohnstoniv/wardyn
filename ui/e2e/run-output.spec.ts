/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Run-detail Output tab (0.8.6 out-o5, mock packet M8). The seeded `none`
// runner never keeps real output, so GET /runs/{id}/output is spliced with a
// fixed body (a headless run's output, an interactive run's pane snapshot);
// the real run, route and tab wiring are the harness's own.
import { randomUUID } from "node:crypto";
import type { Page } from "@playwright/test";
import { test, expect, consoleAPI, gotoConsole, navToRoute } from "./fixtures";
import { RUN_COCKPIT, RUN_OUTPUT } from "../src/app/components/wardyn/copy";
import { STATES } from "../src/app/components/wardyn/states";

const OUTPUT_GLOB = "**/api/v1/runs/*/output";
const POLL_MS = 4000;

const body = (o: Record<string, unknown>) => ({
  output: "$ go test ./...\nok   example.com/app   4.118s\n<script>alert(1)</script>",
  truncated: false,
  complete: true,
  source: "stdout",
  incomplete: false,
  capture_gap: false,
  mask_scope: "run",
  captured_at: "2026-10-03T14:02:00Z",
  ...o,
});

async function openOutputTab(page: Page): Promise<string> {
  await gotoConsole(page);
  const created = await consoleAPI(page, "POST", "/api/v1/runs", {
    agent: "claude-code",
    task: "run-output e2e " + randomUUID().slice(0, 8),
  });
  expect(created.status, created.text).toBe(201);
  const id = JSON.parse(created.text).id as string;
  await navToRoute(page, `/runs/${id}`);
  await page.getByRole("tab", { name: RUN_OUTPUT.tab }).click();
  return id;
}

test.describe("Run output tab", () => {
  test("a spliced headless run: the panel polls while live and stops once complete", async ({ page }) => {
    let calls = 0;
    await page.route(OUTPUT_GLOB, async (route) => {
      calls++;
      await route.fulfill({
        json: calls < 2 ? body({ complete: false, captured_at: undefined, output: "$ go test ./..." }) : body({}),
      });
    });
    const id = await openOutputTab(page);

    await expect(page.getByText(RUN_OUTPUT.sourceStdout)).toBeVisible();
    await expect(page.getByText(RUN_OUTPUT.final)).toBeVisible({ timeout: POLL_MS * 3 });
    const pre = page.getByTestId("run-output-text");
    // Sandbox markup shows as literal text, never as an element.
    await expect(pre).toContainText("<script>alert(1)</script>");
    await expect(pre.locator("script")).toHaveCount(0);
    await expect(page.getByText(`wardyn run output ${id}`)).toBeVisible();

    const settled = calls;
    await page.waitForTimeout(POLL_MS + 2000);
    expect(calls).toBe(settled);
  });

  test("a spliced pane snapshot: Last screen and its caption", async ({ page }) => {
    await page.route(OUTPUT_GLOB, (route) =>
      route.fulfill({ json: body({ source: "pane_snapshot", output: "user@sandbox:~$ " }) }),
    );
    await openOutputTab(page);
    await expect(page.getByText(RUN_OUTPUT.sourcePane, { exact: true })).toBeVisible();
    await expect(page.getByText(RUN_OUTPUT.paneCaption)).toBeVisible();
    await expect(page.getByTestId("run-output-text")).toContainText("user@sandbox:~$");
  });

  // Mock packet M-O (#1831). The seeded runner records nothing, so each server
  // answer is spliced: what the owner of a recording-on Kubernetes run reads,
  // and what everyone else does.
  const recording = (o: Record<string, unknown>) => body({ source: "recording", incomplete: true, ...o });
  const refuse = (status: number, reason: string) => ({ status, json: { error: "refused", reason } });

  test("recovered from a recording: the source, the limit, and a focused region that survives polling", async ({
    page,
    context,
  }) => {
    await context.grantPermissions(["clipboard-read", "clipboard-write"]);
    let calls = 0;
    await page.route(OUTPUT_GLOB, async (route) => {
      calls++;
      await route.fulfill({
        json:
          calls < 3
            ? recording({ complete: false, captured_at: undefined, output: "line 1\n".repeat(calls) })
            : recording({ output: "line 1\nline 2\nline 3\n" }),
      });
    });
    await openOutputTab(page);

    const pre = page.getByRole("region", { name: RUN_OUTPUT.sourceRecording });
    await expect(pre).toBeVisible();
    await expect(page.getByText(RUN_OUTPUT.live)).toBeVisible();
    await expect(page.getByText(RUN_OUTPUT.recordingRecovered)).toBeVisible();
    await expect(pre).toHaveAccessibleDescription(RUN_OUTPUT.recordingRecovered);
    await expect(page.getByText(RUN_OUTPUT.sourceStdout, { exact: true })).toHaveCount(0);

    // Focus stays on the region while two more reads replace its text.
    await pre.focus();
    await expect(page.getByText(RUN_OUTPUT.final)).toBeVisible({ timeout: POLL_MS * 4 });
    // textContent, not toHaveText: the bytes are compared unnormalized.
    await expect.poll(() => pre.evaluate((el) => el.textContent)).toBe("line 1\nline 2\nline 3\n");
    await expect(pre).toBeFocused();
    // Final is the state of the capture, not a claim that everything arrived.
    await expect(page.getByText(RUN_OUTPUT.recordingRecovered)).toBeVisible();
    await expect(page.getByText(RUN_OUTPUT.incomplete)).toHaveCount(0);

    const copy = page.getByRole("button", { name: RUN_OUTPUT.copyLabel });
    await copy.focus();
    await page.keyboard.press("Enter");
    await expect(copy.getByText("Copied")).toBeAttached();
    expect(await page.evaluate(() => navigator.clipboard.readText())).toBe("line 1\nline 2\nline 3\n");
    await expect(copy).toBeFocused();
  });

  test("a capture gap with nothing recovered is a gap, not a run that printed nothing", async ({ page, context }) => {
    await context.grantPermissions(["clipboard-read", "clipboard-write"]);
    await page.route(OUTPUT_GLOB, (route) => route.fulfill({ json: body({ capture_gap: true, output: "" }) }));
    await openOutputTab(page);

    const pre = page.getByRole("region", { name: RUN_OUTPUT.sourceStdout });
    await expect(pre).toBeVisible();
    await expect(page.getByText(RUN_OUTPUT.captureGap)).toBeVisible();
    await expect(pre).toHaveAccessibleDescription(RUN_OUTPUT.captureGap);
    expect(await pre.evaluate((el) => el.textContent)).toBe("");
    await expect(page.getByText(RUN_OUTPUT.emptyFinal)).toHaveCount(0);
    await expect(page.getByText(RUN_OUTPUT.recordingRecovered)).toHaveCount(0);
    await expect(page.getByText(RUN_OUTPUT.notCapturedDesc)).toHaveCount(0);
    await expect(page.getByTestId("run-output-refusal")).toHaveCount(0);

    // Copy hands over the empty answer itself: it replaces what the clipboard held.
    await page.evaluate(() => navigator.clipboard.writeText("held before"));
    await page.getByRole("button", { name: RUN_OUTPUT.copyLabel }).click();
    await expect(page.getByRole("button", { name: RUN_OUTPUT.copyLabel }).getByText("Copied")).toBeAttached();
    expect(await page.evaluate(() => navigator.clipboard.readText())).toBe("");
  });

  test("a recording the server could not recover from is drawn as a plain gap, with no recovery claim", async ({
    page,
  }) => {
    // The server's own row for a missing, invalid or mask-uncovered recovery.
    await page.route(OUTPUT_GLOB, (route) =>
      route.fulfill({ json: recording({ capture_gap: true, output: "" }) }),
    );
    await openOutputTab(page);
    const pre = page.getByRole("region", { name: RUN_OUTPUT.sourceStdout });
    await expect(pre).toBeVisible();
    await expect(pre).toHaveAccessibleDescription(RUN_OUTPUT.captureGap);
    await expect(page.getByText(RUN_OUTPUT.sourceRecording)).toHaveCount(0);
    await expect(page.getByText(RUN_OUTPUT.recordingRecovered)).toHaveCount(0);
    await expect(page.getByText(RUN_OUTPUT.incomplete)).toHaveCount(0);
    await expect(page.getByText(RUN_OUTPUT.emptyFinal)).toHaveCount(0);
    expect(await pre.evaluate((el) => el.textContent)).toBe("");
  });

  test("every notice at 390 px: recovery, mask, gap, tail-limit in order, nothing clipped", async ({ page }) => {
    await page.route(OUTPUT_GLOB, (route) =>
      route.fulfill({ json: recording({ mask_scope: "globals_only", capture_gap: true, truncated: true }) }),
    );
    await openOutputTab(page);
    await page.setViewportSize({ width: 390, height: 844 });

    const pre = page.getByTestId("run-output-text");
    await expect(pre).toHaveAccessibleDescription(`${RUN_OUTPUT.recordingRecovered} ${RUN_OUTPUT.captureGap}`);
    const tops: number[] = [];
    for (const s of [RUN_OUTPUT.recordingRecovered, RUN_OUTPUT.globalsOnly, RUN_OUTPUT.captureGap, RUN_OUTPUT.truncated]) {
      const box = await page.getByText(s).boundingBox();
      expect(box, s).not.toBeNull();
      // Each warning wraps inside the card instead of running off the page.
      expect(box!.x).toBeGreaterThanOrEqual(0);
      expect(box!.x + box!.width).toBeLessThanOrEqual(390);
      tops.push(box!.y);
    }
    expect([...tops].sort((a, b) => a - b)).toEqual(tops);
    expect((await pre.boundingBox())!.y).toBeGreaterThan(tops[3]);
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
    await expect(page.getByRole("button", { name: RUN_OUTPUT.copyLabel })).toBeVisible();
    await pre.focus();
    await expect(pre).toBeFocused();
  });

  test("the owner's 410 for an erased recording is the erased state, with no bytes and no Copy", async ({ page }) => {
    await page.route(OUTPUT_GLOB, (route) => route.fulfill(refuse(410, "recording_erased")));
    await openOutputTab(page);
    const refusal = page.getByTestId("run-output-refusal");
    await expect(refusal.getByText(RUN_OUTPUT.erasedTitle)).toBeVisible();
    await expect(refusal.getByText(RUN_OUTPUT.erasedDesc)).toBeVisible();
    await expect(page.getByText(RUN_COCKPIT.loadError)).toHaveCount(0);
    await expect(page.getByRole("button", { name: STATES.RETRY })).toHaveCount(0);
    await expect(page.getByTestId("run-output-text")).toHaveCount(0);
    await expect(page.getByRole("button", { name: RUN_OUTPUT.copyLabel })).toHaveCount(0);
  });

  test("a viewer who is neither owner nor operator sees the existing not-captured state, nothing about a recording row", async ({
    page,
  }) => {
    await page.route(OUTPUT_GLOB, (route) => route.fulfill(refuse(409, "run_output_not_captured")));
    await openOutputTab(page);
    const refusal = page.getByTestId("run-output-refusal");
    await expect(refusal.getByText(RUN_OUTPUT.notCapturedTitle)).toBeVisible();
    await expect(refusal.getByText(RUN_OUTPUT.notCapturedDesc)).toBeVisible();
    await expect(page.getByText(RUN_OUTPUT.sourceRecording)).toHaveCount(0);
    await expect(page.getByText(RUN_OUTPUT.recordingRecovered)).toHaveCount(0);
    await expect(page.getByText(RUN_OUTPUT.erasedTitle)).toHaveCount(0);
    await expect(page.getByTestId("run-output-text")).toHaveCount(0);
    await expect(page.getByRole("button", { name: RUN_OUTPUT.copyLabel })).toHaveCount(0);
  });

  test("a failed read: Retry keeps the error surface and its focus until the read answers", async ({ page }) => {
    let calls = 0;
    let release: () => void = () => {};
    await page.route(OUTPUT_GLOB, async (route) => {
      calls++;
      if (calls === 2) await new Promise<void>((r) => (release = r));
      if (calls < 3) return route.fulfill(refuse(500, "internal"));
      return route.fulfill({ json: recording({}) });
    });
    await openOutputTab(page);
    const refusal = page.getByTestId("run-output-refusal");
    await expect(refusal.getByText(RUN_COCKPIT.loadError)).toBeVisible();
    const retry = page.getByRole("button", { name: STATES.RETRY });

    await retry.focus();
    await page.keyboard.press("Enter");
    // The second read is held: the surface stays, busy, with Retry still focused.
    await expect(refusal).toHaveAttribute("aria-busy", "true");
    await expect(retry).toBeFocused();
    await expect(page.getByText(RUN_OUTPUT.loading)).toHaveCount(0);
    // A second press while that read is in flight sends nothing.
    await page.keyboard.press("Enter");
    expect(calls).toBe(2);

    release();
    await expect(refusal).toHaveAttribute("aria-busy", "false");
    await expect(retry).toBeFocused();
    expect(calls).toBe(2);

    await page.keyboard.press("Enter");
    await expect(page.getByRole("region", { name: RUN_OUTPUT.sourceRecording })).toBeVisible();
    await expect(refusal).toHaveCount(0);
    expect(calls).toBe(3);
  });
});
