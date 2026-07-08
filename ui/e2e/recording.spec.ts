/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, gotoConsole, navTo } from "./fixtures";
import type { Page } from "@playwright/test";

// E2E coverage for the redesigned Recordings screen
// (src/app/components/screens/recording.tsx).
//
// The redesign made Recordings a client-SYNTHESIZED library: there is no
// "list all recordings" API, so the screen lists every run (listRuns) and then
// probes each one's GET /runs/{id}/recording/{id}, building the card grid from
// whichever probes return a cast. That makes the seeded `none`-runner backend
// (where every getRecording 404s) exercise the honest empty/loading states, and
// lets us drive the with-recordings and failure paths purely via route intercepts.
//
// Recording-fetch URLs match /api/v1/runs/{id}/recording/{id}.
const RECORDING_GLOB = "**/api/v1/runs/*/recording/*";
const RUNS_LIST_GLOB = "**/api/v1/runs";

// A minimal, valid asciicast v2 document (header line + one output event).
const CAST =
  `{"version":2,"width":80,"height":24,"title":"e2e-cast"}\n` +
  `[0.5,"o","hello from the recording\\r\\n"]\n`;

const EMPTY_TITLE = "None of your runs have a recording yet";
const RUNS_ERROR = "Couldn't load the list of runs.";

async function openRecordings(page: Page): Promise<void> {
  await gotoConsole(page);
  await navTo(page, "Recordings");
  await expect(page.getByRole("heading", { name: "Recordings" })).toBeVisible();
}

test.describe("Recordings library", () => {
  test("renders the header, honest description and a refresh action", async ({ page }) => {
    await openRecordings(page);
    await expect(
      page.getByText(/Captured terminal sessions, replayed byte-for-byte/i),
    ).toBeVisible();
    // The description is honest about the client-synthesized nature of the list.
    await expect(page.getByText(/this library is built by checking each run/i)).toBeVisible();
    await expect(page.getByRole("button", { name: "Refresh" })).toBeVisible();
  });

  test("the none-runner backend has no recordings → settles to the empty library (no infinite spinner)", async ({
    page,
  }) => {
    await openRecordings(page);

    // After probing every run (all 404 on the `none` runner) the library settles
    // to the "no recordings" empty state — never an endless spinner.
    await expect(page.getByRole("heading", { name: EMPTY_TITLE })).toBeVisible();
    await expect(
      page.getByText(/A recording is produced once an agent process runs in the sandbox/i),
    ).toBeVisible();
    await expect(page.getByRole("link", { name: /Go to Runs/ })).toBeVisible();

    // No spinner remains once checking is done.
    await expect(page.locator("main svg.animate-spin")).toHaveCount(0);
  });

  test("a listRuns() failure renders its own error, and Retry recovers", async ({ page }) => {
    // Reach the console FIRST so the app's auth probe (which GETs /runs) succeeds
    // and the shell mounts; only THEN fail the runs list so the RecordingScreen's
    // own listRuns() hits the 500.
    await gotoConsole(page);
    let fail = true;
    await page.route(RUNS_LIST_GLOB, (route, request) => {
      if (request.method() === "GET" && fail) {
        return route.fulfill({ status: 500, contentType: "text/plain", body: "boom" });
      }
      return route.continue();
    });
    await navTo(page, "Recordings");

    await expect(page.getByText(RUNS_ERROR)).toBeVisible();
    await expect(page.getByRole("button", { name: /retry/i })).toBeVisible();

    // Heal the route, Retry, and the library recovers to its empty state.
    fail = false;
    await page.getByRole("button", { name: /retry/i }).click();
    await expect(page.getByText(RUNS_ERROR)).toHaveCount(0);
    await expect(page.getByRole("heading", { name: EMPTY_TITLE })).toBeVisible();
  });

  test("runs whose recording probe fails surface a check-failure notice with Retry", async ({
    page,
  }) => {
    // listRuns succeeds (9 runs) but every getRecording() probe 500s. The screen
    // must not pretend those runs simply have no recording — it reports how many
    // couldn't be checked and offers a Retry.
    await page.route(RECORDING_GLOB, (route) =>
      route.fulfill({ status: 500, contentType: "text/plain", body: "boom" }),
    );

    await openRecordings(page);

    await expect(page.getByText(/9 runs couldn't be checked for a recording\./i)).toBeVisible();
    await expect(page.getByRole("button", { name: /retry/i })).toBeVisible();
  });

  test("a synthesized library renders recording cards and opens the player", async ({ page }) => {
    // Every run's probe returns a valid cast, so the library fills with one card
    // per seeded run.
    await page.route(RECORDING_GLOB, (route) =>
      route.fulfill({ status: 200, contentType: "text/plain", body: CAST }),
    );

    await openRecordings(page);

    // Nine seeded runs → nine recording cards, each with an "Open run" deep link.
    await expect(page.getByRole("link", { name: /Open run/ })).toHaveCount(9);
    await expect(page.getByText("e2e fixture 4")).toBeVisible();

    // Clicking a card opens the replay dialog titled with the run's task.
    await page.getByText("e2e fixture 4").click();
    const dialog = page.getByRole("dialog");
    await expect(dialog).toBeVisible();
    await expect(dialog.getByText("e2e fixture 4")).toBeVisible();
  });
});
