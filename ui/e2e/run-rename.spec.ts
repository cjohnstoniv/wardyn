/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Run rename e2e (#1197 L2, design.md §3.4): PATCH /runs/{id}/title's UI
// door on the run page. The owner renames a run in ANY state — a live one and
// an ended one — sees the RENAMED toast, and the new title renders in place
// with no reload; a member reaching for someone else's run gets the same
// byte-identical 404 every other owner-only run route already answers
// (member-console.spec.ts's own "real member cannot access another person's
// run" pins the same shape for kill/get).
import { randomUUID } from "node:crypto";
import { test, expect, asRealMember, consoleAPI, gotoConsole, navToRoute } from "./fixtures";
import { RUN_COCKPIT } from "../src/app/components/wardyn/copy";

// Seeds a fresh run as whoever the page is currently authenticated as (the
// admin token by default) via the real create door — same helper shape
// member-console.spec.ts and approvals.spec.ts already use for a
// dedicated, throwaway fixture per test.
async function seedRun(page: import("@playwright/test").Page, task: string): Promise<string> {
  const created = await consoleAPI(page, "POST", "/api/v1/runs", { agent: "claude-code", task });
  expect(created.status, created.text).toBe(201);
  return JSON.parse(created.text).id as string;
}

test.describe("Run rename", () => {
  test("the owner renames a RUNNING run — RENAMED toast, title updates with no reload", async ({
    page,
  }) => {
    await gotoConsole(page);
    const id = await seedRun(page, "run-rename e2e running " + randomUUID().slice(0, 8));
    await navToRoute(page, `/runs/${id}`);

    const heading = page.getByRole("heading", { level: 1 });
    await expect(heading).toBeVisible();
    // No title yet, so the h1 falls back to the task (runHeadline) — the run
    // is untitled until this test renames it.

    await page.getByRole("button", { name: "Rename", exact: true }).click();
    const titleInput = page.getByLabel("Title");
    await titleInput.fill("Renamed while running");
    await page.getByRole("button", { name: "Save", exact: true }).click();

    await expect(page.getByText(RUN_COCKPIT.renamed)).toBeVisible();
    // The new title renders in place — the same GET /runs/{id} refetch kill
    // and the decision handlers already use (run-detail.tsx's load(false)),
    // never a full page reload.
    await expect(heading).toHaveText("Renamed while running");

    const stored = await consoleAPI(page, "GET", `/api/v1/runs/${id}`);
    expect(JSON.parse(stored.text).title).toBe("Renamed while running");
  });

  test("the owner renames an ENDED run", async ({ page }) => {
    await gotoConsole(page);
    const id = await seedRun(page, "run-rename e2e ended " + randomUUID().slice(0, 8));
    const killed = await consoleAPI(page, "POST", `/api/v1/runs/${id}/kill`, {});
    expect(killed.status, killed.text).toBe(202);

    await navToRoute(page, `/runs/${id}`);
    await expect(page.getByText("Killed", { exact: true })).toBeVisible();
    // A terminal run's Kill button is disabled, but Rename is not — a title
    // is a display field, not part of the lease (design.md §3.4).
    await expect(page.getByRole("button", { name: "Kill", exact: true })).toBeDisabled();

    await page.getByRole("button", { name: "Rename", exact: true }).click();
    await page.getByLabel("Title").fill("Renamed after ending");
    await page.getByRole("button", { name: "Save", exact: true }).click();

    await expect(page.getByText(RUN_COCKPIT.renamed)).toBeVisible();
    await expect(page.getByRole("heading", { level: 1 })).toHaveText("Renamed after ending");

    const stored = await consoleAPI(page, "GET", `/api/v1/runs/${id}`);
    expect(JSON.parse(stored.text).title).toBe("Renamed after ending");
    expect(JSON.parse(stored.text).state).toBe("KILLED");
  });

  test("a member cannot rename someone else's run", async ({ page }) => {
    // A same-origin document first — consoleAPI reads storage via
    // page.evaluate, which throws on the default about:blank document.
    await gotoConsole(page);
    // Seeded as the admin-token session (the page's default identity).
    const foreignId = await seedRun(page, "run-rename e2e foreign " + randomUUID().slice(0, 8));

    await asRealMember(page);
    const renamed = await consoleAPI(page, "PATCH", `/api/v1/runs/${foreignId}/title`, {
      title: "hijacked",
    });
    // Byte-identical to a missing run — no existence oracle (getRunAuthorized).
    const missing = await consoleAPI(page, "PATCH", `/api/v1/runs/${randomUUID()}/title`, {
      title: "hijacked",
    });
    expect(renamed.status, renamed.text).toBe(404);
    expect(renamed).toEqual(missing);
  });
});
