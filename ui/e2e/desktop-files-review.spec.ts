/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, sql } from "./fixtures";
import type { Locator, Page } from "@playwright/test";
import { DESKTOP_FILES as C } from "../src/app/components/wardyn/copy/desktop-files";

// ============================================================================
// The run's Files panel under desktop.download: review. Every endpoint the panel calls is STUBBED here
// with page.route (nothing reaches the real one; the seeded backend has no sandbox, see
// desktop-files.spec.ts):
//   GET  /api/v1/runs/{id}/desktop/files                   the rules and the listing
//   POST /api/v1/runs/{id}/desktop/files/{name}/review     a request to download
//   GET  /api/v1/runs/{id}/desktop/files/{name}?approval=  the approved download
//   GET  /api/v1/approvals?run_id={id}                     the run's download requests
// The server half is pinned by internal/api/desktop_review_test.go.
// ============================================================================

const RUN_TASK = "e2e fixture 2";
const SHA = "9f2c" + "0".repeat(56) + "41ab";

interface Req {
  id: string;
  name: string;
  state: "PENDING" | "APPROVED" | "DENIED" | "EXPIRED";
  minted_jti?: string;
  decided_by?: string;
  reason?: string;
}

function runId(): string {
  const id = sql(`SELECT id FROM agent_runs WHERE task = '${RUN_TASK}' LIMIT 1`);
  expect(id, `no seeded run with task ${RUN_TASK}`).toMatch(/^[0-9a-f-]{36}$/);
  return id;
}

const wire = (run: string, r: Req, bytes: number) => ({
  id: r.id,
  run_id: run,
  kind: "file_download",
  state: r.state,
  requested_at: new Date(Date.now() - 3_600_000).toISOString(),
  ...(r.state === "PENDING" ? {} : { decided_at: new Date(Date.now() - 1_800_000).toISOString() }),
  decided_by: r.decided_by,
  reason: r.reason,
  minted_jti: r.minted_jti,
  requested_scope: { name: r.name, bytes, sha256: SHA, requested_by: "dana@acme.example" },
});

interface Stub {
  files: { name: string; bytes: number }[];
  requests?: Req[];
  review?: (name: string) => { status: number; body: unknown };
  download?: (name: string, approval: string | null) => { status: number; body: unknown } | null;
}

// Returns the calls the panel made, so a test can read the exact URL.
async function stub(page: Page, id: string, s: Stub): Promise<{ posts: string[]; gets: string[] }> {
  const calls = { posts: [] as string[], gets: [] as string[] };
  let requests = s.requests ?? [];
  const bytesOf = (name: string) => s.files.find((f) => f.name === name)?.bytes ?? 0;
  await page.route(`**/api/v1/approvals*`, async (route) => {
    const u = new URL(route.request().url());
    const mine = !u.searchParams.has("state") && u.searchParams.get("run_id") === id;
    await route.fulfill({ contentType: "application/json", body: JSON.stringify(mine ? requests.map((r) => wire(id, r, bytesOf(r.name))) : []) });
  });
  await page.route(new RegExp(`/api/v1/runs/${id}/desktop/files(/[^/?]*)*(\\?.*)?$`), async (route) => {
    const req = route.request();
    const u = new URL(req.url());
    const rest = decodeURIComponent(u.pathname.split("/desktop/files")[1] ?? "").replace(/^\//, "");
    const json = (status: number, body: unknown) => route.fulfill({ status, contentType: "application/json", body: JSON.stringify(body) });
    if (!rest && req.method() === "GET") {
      return json(200, {
        folder: "~/Transfers",
        rules: { paste_in: "allowed", copy_out: "blocked", upload: "allowed", download: "review" },
        files: s.files,
      });
    }
    if (rest.endsWith("/review") && req.method() === "POST") {
      const name = rest.slice(0, -"/review".length);
      calls.posts.push(name);
      const out = s.review?.(name) ?? { status: 202, body: wire(id, { id: `ap_${name}`, name, state: "PENDING" }, bytesOf(name)) };
      if (out.status === 202) requests = [...requests, { id: `ap_${name}`, name, state: "PENDING" }];
      return json(out.status, out.body);
    }
    if (req.method() === "GET") {
      calls.gets.push(`${rest}?${u.searchParams.toString()}`);
      const out = s.download?.(rest, u.searchParams.get("approval"));
      if (out) return json(out.status, out.body);
      return route.fulfill({ status: 200, contentType: "application/octet-stream", body: "refund,total\n" });
    }
    return route.fallback();
  });
  return calls;
}

async function openFrom(page: Page, s: Stub): Promise<{ panel: Locator; calls: { posts: string[]; gets: string[] } }> {
  const id = runId();
  const calls = await stub(page, id, s);
  await page.goto(`/runs/${id}`);
  await expect(page.getByRole("heading", { name: RUN_TASK, level: 1 })).toBeVisible();
  const panel = page.getByTestId("desktop-files");
  await panel.scrollIntoViewIfNeeded();
  await panel.getByRole("tab", { name: C.fromTab }).click();
  return { panel, calls };
}

const row = (panel: Locator, name: string) => panel.getByTestId("desktop-download").filter({ hasText: name });

test.describe("Run detail — Files panel under review", () => {
  test("each file shows its newest request's state, line and button", async ({ page }) => {
    const { panel } = await openFrom(page, {
      files: [
        { name: "q3-forecast.xlsx", bytes: 1_200_000 },
        { name: "build-logs.tar.gz", bytes: 19_293_184 },
        { name: "invoice-export.csv", bytes: 84 * 1024 },
        { name: "customer-dump.sql", bytes: 212 * 1024 * 1024 },
        { name: "notes.md", bytes: 3 * 1024 },
        { name: "screenshot.png", bytes: 412 * 1024 },
      ],
      requests: [
        { id: "ap_2", name: "build-logs.tar.gz", state: "PENDING" },
        { id: "ap_3", name: "invoice-export.csv", state: "APPROVED", decided_by: "lee.chen@acme.example" },
        { id: "ap_4", name: "customer-dump.sql", state: "DENIED", decided_by: "sam.okafor@acme.example", reason: "Customer records leave through the data team." },
        { id: "ap_5", name: "notes.md", state: "APPROVED", minted_jti: "jti-1" },
        { id: "ap_6", name: "screenshot.png", state: "EXPIRED" },
      ],
    });
    await expect(row(panel, "q3-forecast.xlsx").getByRole("button", { name: C.requestDownload })).toBeVisible();
    const waiting = row(panel, "build-logs.tar.gz");
    await expect(waiting).toContainText(C.stateWaiting);
    await expect(waiting).toContainText(/Asked \d\d:\d\d · sha256 9f2c…41ab/);
    await expect(waiting).toContainText("a security admin");
    await expect(waiting.getByRole("button")).toHaveCount(0);
    const approved = row(panel, "invoice-export.csv");
    await expect(approved).toContainText(/Approved by lee\.chen@acme\.example · \d\d:\d\d/);
    await expect(approved.getByRole("button", { name: C.downloadApproved })).toBeVisible();
    const denied = row(panel, "customer-dump.sql");
    await expect(denied).toContainText(/sam\.okafor@acme\.example: “Customer records leave through the data team\.” · \d\d:\d\d/);
    await expect(denied.getByRole("button", { name: C.requestDownload })).toBeVisible();
    const released = row(panel, "notes.md");
    await expect(released).toContainText(/Downloaded \d\d:\d\d · sha256 9f2c…41ab/);
    await expect(released.getByRole("button", { name: C.requestDownload })).toBeVisible();
    const expired = row(panel, "screenshot.png");
    await expect(expired).toContainText(/Nobody decided it in time · \d\d:\d\d/);
    await expect(expired.getByRole("button", { name: C.requestDownload })).toBeVisible();
    await expect(panel.getByText(C.reviewFooter, { exact: true })).toBeVisible();
  });

  test("Request download posts to /review and the row waits", async ({ page }) => {
    const { panel, calls } = await openFrom(page, { files: [{ name: "q3-forecast.xlsx", bytes: 1024 }] });
    await row(panel, "q3-forecast.xlsx").getByRole("button", { name: C.requestDownload }).click();
    await expect(row(panel, "q3-forecast.xlsx")).toContainText(C.stateWaiting);
    expect(calls.posts).toEqual(["q3-forecast.xlsx"]);
  });

  test("Download approved fetches with ?approval=<id> and reports the hash", async ({ page }) => {
    const { panel, calls } = await openFrom(page, {
      files: [{ name: "invoice-export.csv", bytes: 13 }],
      requests: [{ id: "ap_3", name: "invoice-export.csv", state: "APPROVED", decided_by: "lee.chen@acme.example" }],
    });
    const download = page.waitForEvent("download");
    await row(panel, "invoice-export.csv").getByRole("button", { name: C.downloadApproved }).click();
    expect((await download).suggestedFilename()).toBe("invoice-export.csv");
    await expect(row(panel, "invoice-export.csv")).toContainText(/Downloaded \d\d:\d\d · sha256 [0-9a-f]{4}…[0-9a-f]{4}/);
    expect(calls.gets.some((g) => g === "invoice-export.csv?approval=ap_3")).toBe(true);
  });

  test("a file changed since review prints the server's sentence under the row", async ({ page }) => {
    const sentence = "the file changed after it was reviewed: its bytes are not the ones approved. Nothing was sent; ask for another review.";
    const { panel } = await openFrom(page, {
      files: [{ name: "invoice-export.csv", bytes: 13 }],
      requests: [{ id: "ap_3", name: "invoice-export.csv", state: "APPROVED", decided_by: "lee.chen@acme.example" }],
      download: () => ({ status: 409, body: { error: sentence, reason: "file_changed_since_review" } }),
    });
    await row(panel, "invoice-export.csv").getByRole("button", { name: C.downloadApproved }).click();
    await expect(row(panel, "invoice-export.csv")).toContainText(`${C.downloadFailed}: ${sentence}`);
  });

  test("at 380px the name and the button do not overlap", async ({ page }) => {
    await page.setViewportSize({ width: 380, height: 900 });
    const { panel } = await openFrom(page, {
      files: [{ name: "invoice-export-with-a-long-name-2026.csv", bytes: 84 * 1024 }],
      requests: [{ id: "ap_3", name: "invoice-export-with-a-long-name-2026.csv", state: "APPROVED", decided_by: "lee.chen@acme.example" }],
    });
    const li = row(panel, "invoice-export");
    const name = await li.getByText("invoice-export-with-a-long-name-2026.csv", { exact: true }).boundingBox();
    const button = await li.getByRole("button", { name: C.downloadApproved }).boundingBox();
    expect(name).not.toBeNull();
    expect(button).not.toBeNull();
    // The name takes its own line: the button starts below it.
    expect(button!.y).toBeGreaterThanOrEqual(name!.y + name!.height - 1);
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
  });
});
