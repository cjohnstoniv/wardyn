/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, gotoConsole, navToRoute } from "./fixtures";
import type { Page, Route } from "@playwright/test";

// E2E coverage for the Recordings screen (src/app/components/screens/
// recording.tsx) — REWRITTEN per VL-15 (verify ledger, v0.7.2): R4-F077 landed
// server-side has_recording/recording_bytes/recording_duration_sec projected
// straight onto listRuns()'s own response (?include=recording_meta), so the
// library is built from THAT ONE CALL. The mechanism this file drove before —
// intercepting a per-run GET /runs/{id}/recording/{id} probe fired for every
// row while the list rendered — no longer happens at all: recording.tsx's own
// header comment says so ("It used to ask every run individually
// (api.probeRecording, since removed here)"). A cast is now fetched
// (api.getRecording) exactly once, when a viewer presses play.
//
// So the browser-vs-API split here is: the LIST is spliced on the way past
// (route.fetch() + patch + refulfill, the fixtures.ts mockMemberRole
// technique) because the seeded `none`-runner backend never produces a real
// recording for any of its 9 fixtures — has_recording is genuinely false for
// all of them — while the PLAY fetch is proven against a real intercepted
// response, never a stub standing in for the server's own has_recording
// computation (which is A4's, pinned in Go).
const RUNS_LIST_GLOB = /\/api\/v1\/runs(\?|$)/;
const RECORDING_GLOB = "**/api/v1/runs/*/recording/*";

// A minimal, valid asciicast v2 document (header line + one output event).
const CAST =
  `{"version":2,"width":80,"height":24,"title":"e2e-cast"}\n` +
  `[0.5,"o","hello from the recording\\r\\n"]\n`;

const EMPTY_TITLE = "None of your runs have a recording yet";
const RUNS_ERROR = "Couldn't load the list of runs.";

async function openRecordings(page: Page): Promise<void> {
  await gotoConsole(page);
  await navToRoute(page, "/recordings");
  await expect(page.getByRole("heading", { name: "Recordings" })).toBeVisible();
}

// Splices has_recording/recording_bytes/recording_duration_sec onto whichever
// of the seeded runs `patch` names (by task text), on the ONE listRuns() call
// this screen makes (?include=recording_meta) — the real 9 fixtures, real
// ids, real every-other-field, with only the three R4-F077 projection fields
// stood in for what this `none`-runner harness can never produce for real.
// Registered BEFORE openRecordings() navigates, so it is in place for the
// screen's own mount-time fetch.
// `ids`, when given, is filled with the run id of every spliced row (keyed by
// task title) so a test can assert WHICH run a later fetch named.
async function mockRecordingMeta(
  page: Page,
  patch: Record<string, { bytes: number; durationSec: number }>,
  ids: Record<string, string> = {},
): Promise<void> {
  await page.route(RUNS_LIST_GLOB, async (route: Route) => {
    if (route.request().method() !== "GET") return route.fallback();
    const response = await route.fetch();
    const json = await response.json();
    if (!Array.isArray(json)) return route.fulfill({ response, json });
    for (const run of json) {
      const meta = patch[run.task];
      if (meta) {
        run.has_recording = true;
        run.recording_bytes = meta.bytes;
        run.recording_duration_sec = meta.durationSec;
        ids[run.task] = run.id;
      } else {
        run.has_recording = false;
      }
    }
    await route.fulfill({ response, json });
  });
}

test.describe("Recordings library", () => {
  test("renders the header, the run-list-alone description and a refresh action", async ({ page }) => {
    await openRecordings(page);
    await expect(page.getByText(/Captured terminal sessions, replayed byte-for-byte/i)).toBeVisible();
    // The description is honest about WHERE has_recording comes from now: the
    // run list itself, not a per-run probe — the exact sentence VL-15 exists
    // because the OLD one ("this library is built by checking each run") no
    // longer describes what the screen does.
    await expect(page.getByText(/Built from the run list alone/i)).toBeVisible();
    await expect(page.getByRole("button", { name: "Refresh" })).toBeVisible();
  });

  test("the none-runner backend genuinely has no recordings — no probe needed to know that", async ({ page }) => {
    // UNMOCKED: every one of the 9 seeded runs really does answer
    // has_recording:false from the real server, because this harness's `none`
    // runner never produces a cast. If has_recording were still being derived
    // by probing each run's recording endpoint, this would be the test most
    // likely to hang on nine outstanding requests instead of settling.
    const probes: string[] = [];
    await page.route(RECORDING_GLOB, (route) => {
      probes.push(route.request().url());
      return route.continue();
    });

    await openRecordings(page);

    await expect(page.getByRole("heading", { name: EMPTY_TITLE })).toBeVisible();
    await expect(page.getByText(/A recording is produced once an agent process runs in the sandbox/i)).toBeVisible();
    await expect(page.getByRole("link", { name: /Go to Runs/ })).toBeVisible();
    await expect(page.locator("main svg.animate-spin")).toHaveCount(0);

    // THE pin VL-15 is for: zero recording-probe requests were made to reach
    // that empty state. The old mechanism could only ever answer this
    // question by making nine of them.
    expect(probes, `expected no per-run recording probes while browsing the list, saw: ${probes.join(", ")}`).toEqual([]);
  });

  test("a listRuns() failure renders its own error, and Retry recovers", async ({ page }) => {
    // Reach the console FIRST so the app's own auth probe (which GETs /runs
    // without the recording_meta param) succeeds and the shell mounts; only
    // THEN fail the runs list so the RecordingScreen's own listRuns() call —
    // the one that asks for ?include=recording_meta — hits the 500.
    await gotoConsole(page);
    let fail = true;
    await page.route(RUNS_LIST_GLOB, (route, request) => {
      if (request.method() === "GET" && fail) {
        return route.fulfill({ status: 500, contentType: "text/plain", body: "boom" });
      }
      return route.continue();
    });
    await navToRoute(page, "/recordings");

    await expect(page.getByText(RUNS_ERROR)).toBeVisible();
    await expect(page.getByRole("button", { name: /retry/i })).toBeVisible();

    fail = false;
    await page.getByRole("button", { name: /retry/i }).click();
    await expect(page.getByText(RUNS_ERROR)).toHaveCount(0);
    await expect(page.getByRole("heading", { name: EMPTY_TITLE })).toBeVisible();
  });

  test("a library spliced onto the run list renders cards straight off its fields — no per-row fetch", async ({
    page,
  }) => {
    const probes: string[] = [];
    await page.route(RECORDING_GLOB, (route) => {
      probes.push(route.request().url());
      return route.continue();
    });
    await mockRecordingMeta(page, {
      "e2e fixture 4": { bytes: 40_960, durationSec: 125 },
      "e2e fixture 7": { bytes: 2048, durationSec: 0 },
    });

    await openRecordings(page);

    // Exactly the two rows the splice marked — the rest of the 9 fixtures
    // stayed has_recording:false and are absent from the library, not shown
    // greyed out: this screen has no "no recording" card state.
    await expect(page.getByText("e2e fixture 4")).toBeVisible();
    await expect(page.getByText("e2e fixture 7")).toBeVisible();
    await expect(page.getByRole("link", { name: /Open run/ })).toHaveCount(2);

    // The card's byte size and duration came straight off the LIST response —
    // rendered without ever asking the recording endpoint for anything.
    await expect(page.getByText("40.0 KB")).toBeVisible();
    await expect(page.getByText("2:05")).toBeVisible();
    expect(probes, `expected zero recording fetches to render the library, saw: ${probes.join(", ")}`).toEqual([]);
  });

  test("opening a card fetches that ONE run's cast, and only that one", async ({ page }) => {
    const probedIds: string[] = [];
    await page.route(RECORDING_GLOB, (route) => {
      probedIds.push(route.request().url());
      return route.fulfill({ status: 200, contentType: "text/plain", body: CAST });
    });
    const ids: Record<string, string> = {};
    await mockRecordingMeta(
      page,
      {
        "e2e fixture 4": { bytes: 512, durationSec: 3 },
        "e2e fixture 7": { bytes: 512, durationSec: 3 },
      },
      ids,
    );

    await openRecordings(page);
    await expect(page.getByRole("link", { name: /Open run/ })).toHaveCount(2);
    expect(probedIds).toEqual([]); // still nothing before a click

    await page.getByText("e2e fixture 4").click();
    const dialog = page.getByRole("dialog");
    await expect(dialog).toBeVisible();
    await expect(dialog.getByText("e2e fixture 4")).toBeVisible();

    // Exactly one recording fetch happened, and it named fixture 4's run —
    // the sibling card's cast was never touched.
    expect(probedIds).toHaveLength(1);
    expect(ids["e2e fixture 4"], "the list splice recorded fixture 4's id").toBeTruthy();
    expect(probedIds[0]).toContain(ids["e2e fixture 4"]);
    expect(probedIds[0]).not.toContain(ids["e2e fixture 7"]);
  });

  test("a getRecording() failure at play time surfaces its own error with Retry", async ({ page }) => {
    await page.route(RECORDING_GLOB, (route) => route.fulfill({ status: 500, contentType: "text/plain", body: "boom" }));
    await mockRecordingMeta(page, { "e2e fixture 4": { bytes: 512, durationSec: 3 } });

    await openRecordings(page);
    await page.getByText("e2e fixture 4").click();
    await expect(page.getByText("This run's recording could not be loaded.")).toBeVisible();
    await expect(page.getByRole("button", { name: /retry/i })).toBeVisible();
  });

  // R4-F077's other half: "Showing n of total" only earns its place past
  // MIN_CARDS_FOR_FILTERS (4) — fewer than that and the facet bar (with its
  // count) is noise over two cards. Five marked rows crosses it.
  test("past four cards the filter bar's count reads the library, and search narrows it live", async ({ page }) => {
    await mockRecordingMeta(page, {
      "e2e fixture 0": { bytes: 1, durationSec: 1 },
      "e2e fixture 1": { bytes: 1, durationSec: 1 },
      "e2e fixture 2": { bytes: 1, durationSec: 1 },
      "e2e fixture 4": { bytes: 1, durationSec: 1 },
      "e2e fixture 7": { bytes: 1, durationSec: 1 },
    });
    await openRecordings(page);

    await expect(page.getByRole("link", { name: /Open run/ })).toHaveCount(5);
    await expect(page.getByText("Showing 5 of 5 recordings")).toBeVisible();

    await page.getByPlaceholder(/Search tasks, repos, run IDs/).fill("fixture 4");
    await expect(page.getByText("Showing 1 of 5 recordings")).toBeVisible();
    await expect(page.getByRole("link", { name: /Open run/ })).toHaveCount(1);
  });

  // Regression for the CSP-vs-WASM replay bug: the recording player is an
  // asciinema-player whose VT core instantiates a WASM module. A CSP without
  // 'wasm-unsafe-eval' (the shipped default-src 'self') makes WebAssembly.instantiate()
  // throw a CompileError, so the player renders its chrome but never plays. That
  // shipped once precisely because no e2e asserted on the console — the visual
  // "dialog opened" check above passes even with a dead player. This guard fails
  // if opening the player logs any CSP/WASM error.
  test("opening the player instantiates its WASM core without a CSP violation", async ({ page }) => {
    const cspWasmErrors: string[] = [];
    const collect = (text: string) => {
      if (/content security policy|wasm|webassembly|compileerror|unsafe-eval/i.test(text)) {
        cspWasmErrors.push(text);
      }
    };
    page.on("console", (msg) => {
      if (msg.type() === "error") collect(msg.text());
    });
    page.on("pageerror", (err) => collect(err.message));

    await page.route(RECORDING_GLOB, (route) =>
      route.fulfill({ status: 200, contentType: "text/plain", body: CAST }),
    );
    await mockRecordingMeta(page, { "e2e fixture 4": { bytes: 512, durationSec: 3 } });
    await openRecordings(page);
    await page.getByText("e2e fixture 4").click();
    const dialog = page.getByRole("dialog");
    await expect(dialog).toBeVisible();

    // Give the player a beat to mount and attempt WASM instantiation, then assert
    // the console stayed clean of any CSP/WASM violation.
    await page.waitForTimeout(500);
    expect(
      cspWasmErrors,
      `recording player logged CSP/WASM errors (CSP missing 'wasm-unsafe-eval'?):\n${cspWasmErrors.join("\n")}`,
    ).toEqual([]);
  });
});
