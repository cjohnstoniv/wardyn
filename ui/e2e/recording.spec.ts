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
  await navToRoute(page, "/admin/recordings");
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
    await navToRoute(page, "/admin/recordings");

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

    // X2-F8 (fix pass — the first attempt didn't deliver this): a blind
    // waitForTimeout(500) was the whole detection window, and `.ap-term`
    // is not a WASM readiness signal either — it's the terminal's CHROME,
    // returned synchronously by the Terminal component with no Show/Match
    // guard around it, while the VT core's build() promise is awaited only
    // inside onMount's own vtReady.then(...) (asciinema-player-ui.js). A
    // dead CompileError still paints `.ap-term`. Output only reaches the DOM
    // through the `output` listener that vtReady.then(...) registers, so
    // pressing Play and polling the rendered cast TEXT is downstream of the
    // WASM instantiate this test exists to guard.
    await dialog.getByRole("button", { name: "Play" }).click();
    await expect(dialog.locator(".ap-term-text")).toContainText("hello from the recording", {
      timeout: 5_000,
    });
    expect(
      cspWasmErrors,
      `recording player logged CSP/WASM errors (CSP missing 'wasm-unsafe-eval'?):\n${cspWasmErrors.join("\n")}`,
    ).toEqual([]);
  });
});

// #159 — the Recordings screen pages instead of stopping at LIST_LIMIT
// (1000). This harness always seeds 9 runs (one page, whichever way you slice
// it), so a genuine multi-page walk needs FABRICATED rows, not real ones —
// mockPagedRuns answers GET /runs?...&limit=&offset=&include=recording_meta
// with whatever a test hands it, keyed by the offset each page request
// carries, and falls back to the real backend for every other call (the
// shell's own mount fetch, the auth probe) so those stay unmocked and real.
function fakeRun(id: string, overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    id,
    created_at: "2026-06-01T00:00:00.000Z",
    updated_at: "2026-06-01T00:00:00.000Z",
    created_by: "op",
    agent: "claude-code",
    repo: "acme/widgets",
    task: id,
    confinement_class: "CC1",
    state: "COMPLETED",
    spiffe_id: `spiffe://wardyn/${id}`,
    runner_target: "docker",
    has_recording: true,
    recording_bytes: 1024,
    recording_duration_sec: 30,
    ...overrides,
  };
}

type PageAnswer = { status?: number; runs?: Record<string, unknown>[]; truncated?: boolean; delayMs?: number };

async function mockPagedRuns(page: Page, answer: (offset: number, call: number) => PageAnswer): Promise<void> {
  let call = 0;
  await page.route(RUNS_LIST_GLOB, async (route: Route) => {
    const req = route.request();
    const url = new URL(req.url());
    if (req.method() !== "GET" || !url.searchParams.has("offset") || url.searchParams.get("include") !== "recording_meta") {
      return route.fallback();
    }
    const offset = Number(url.searchParams.get("offset") ?? "0");
    const a = answer(offset, call++);
    if (a.delayMs) await new Promise((r) => setTimeout(r, a.delayMs));
    if (a.status && a.status !== 200) {
      return route.fulfill({ status: a.status, contentType: "application/json", body: '{"error":"boom"}' });
    }
    const headers: Record<string, string> = { "content-type": "application/json" };
    if (a.truncated) headers["x-wardyn-truncated"] = "true";
    await route.fulfill({ status: 200, headers, body: JSON.stringify(a.runs ?? []) });
  });
}

test.describe("Recordings paging (#159)", () => {
  // One page only: fewer rows than PAGE_SIZE (100), never truncated — the
  // real 9-fixture backend already behaves exactly this way, so this is the
  // one state in the walk that needs no fabricated rows at all.
  test("one page — nothing more to load, no Load more control", async ({ page }) => {
    await mockRecordingMeta(page, {
      "e2e fixture 4": { bytes: 1, durationSec: 1 },
      "e2e fixture 7": { bytes: 1, durationSec: 1 },
    });
    await openRecordings(page);

    await expect(page.getByText("All 2 recordings are loaded.")).toBeVisible();
    await expect(page.getByRole("button", { name: /Load .* more/ })).toHaveCount(0);
  });

  test("more available — the note and the Load more text link appear, never a total", async ({ page }) => {
    await gotoConsole(page);
    await mockPagedRuns(page, () => ({ runs: [fakeRun("rec-a"), fakeRun("rec-b")], truncated: true }));
    await navToRoute(page, "/admin/recordings");
    await expect(page.getByRole("heading", { name: "Recordings" })).toBeVisible();

    await expect(page.getByText("2 recordings loaded so far — there are more on the server.")).toBeVisible();
    // #159's binding decision: the board's existing text link, not a second
    // (outline-button) convention — same class the board's own "Load N more"
    // renders (runs.tsx#RunsTable).
    const loadMore = page.getByRole("button", { name: "Load 100 more" });
    await expect(loadMore).toBeVisible();
    await expect(loadMore).toHaveClass(/text-info/);
    await expect(page.getByText(/of 1,000|100 of/)).toHaveCount(0);
  });

  test("loading more — the control disables and swaps its label while the next page is in flight", async ({
    page,
  }) => {
    await gotoConsole(page);
    await mockPagedRuns(page, (offset) => {
      if (offset === 0) return { runs: [fakeRun("rec-a"), fakeRun("rec-b")], truncated: true };
      return { runs: [fakeRun("rec-c")], truncated: false, delayMs: 600 };
    });
    await navToRoute(page, "/admin/recordings");
    await page.getByRole("button", { name: "Load 100 more" }).click();

    const loading = page.getByRole("button", { name: "Loading…" });
    await expect(loading).toBeVisible();
    await expect(loading).toBeDisabled();

    // …and it resolves cleanly once the delayed page lands.
    await expect(page.getByText("All 3 recordings are loaded.")).toBeVisible();
  });

  test("last page — once the final page lands, Load more is replaced by All N loaded", async ({ page }) => {
    await gotoConsole(page);
    await mockPagedRuns(page, (offset) => {
      if (offset === 0) return { runs: [fakeRun("rec-a"), fakeRun("rec-b")], truncated: true };
      return { runs: [fakeRun("rec-c")], truncated: false };
    });
    await navToRoute(page, "/admin/recordings");
    await page.getByRole("button", { name: "Load 100 more" }).click();

    await expect(page.getByText("All 3 recordings are loaded.")).toBeVisible();
    await expect(page.getByRole("button", { name: /Load .* more/ })).toHaveCount(0);
  });

  test("error mid-page — Retry recovers and keeps what already loaded", async ({ page }) => {
    await gotoConsole(page);
    let secondCallFails = true;
    await mockPagedRuns(page, (offset) => {
      if (offset === 0) return { runs: [fakeRun("rec-a"), fakeRun("rec-b")], truncated: true };
      if (secondCallFails) {
        secondCallFails = false;
        return { status: 500 };
      }
      return { runs: [fakeRun("rec-c")], truncated: false };
    });
    await navToRoute(page, "/admin/recordings");
    await page.getByRole("button", { name: "Load 100 more" }).click();

    await expect(page.getByText("Couldn't load more recordings")).toBeVisible();
    await expect(
      page.getByText(
        "Wardyn stopped answering partway through. The 2 already loaded are still here — retry to continue from where it stopped.",
      ),
    ).toBeVisible();
    // What was already loaded stays on screen through the failure.
    await expect(page.getByText("rec-a")).toBeVisible();
    await expect(page.getByText("rec-b")).toBeVisible();

    await page.getByRole("button", { name: "Retry" }).click();
    await expect(page.getByText("Couldn't load more recordings")).toHaveCount(0);
    await expect(page.getByText("All 3 recordings are loaded.")).toBeVisible();
  });

  test("filters applied across pages — the count carries the load-more caveat", async ({ page }) => {
    await gotoConsole(page);
    const rows = [
      fakeRun("rec-alpha", { task: "alpha task" }),
      fakeRun("rec-beta", { task: "beta task" }),
      fakeRun("rec-gamma", { task: "gamma task" }),
      fakeRun("rec-delta", { task: "delta task" }),
      fakeRun("rec-echo", { task: "search-me task" }),
    ];
    await mockPagedRuns(page, () => ({ runs: rows, truncated: true }));
    await navToRoute(page, "/admin/recordings");
    await expect(page.getByRole("heading", { name: "Recordings" })).toBeVisible();

    await page.getByPlaceholder(/Search tasks, repos, run IDs/).fill("search-me");
    await expect(
      page.getByText(
        "Showing 1 of 5 recordings · Filters cover the 5 recordings loaded so far. Load more to search further back.",
      ),
    ).toBeVisible();
  });
});
