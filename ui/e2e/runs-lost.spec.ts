/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// F1 (#1197 L5, PR #1317 review) — briefs/1197/design.md:158's own pin: "a
// lost run is in Needs you, Revive moves it to Running; a lease-ended run is
// grey under Ended". Same GET /runs splice technique runs-landing.spec.ts
// uses (a precise attention/lost_reason combination no seed script produces).
import { test, expect, gotoConsole, navTo } from "./fixtures";
import type { Page } from "@playwright/test";

const RUNS_LIST_RE = /\/api\/v1\/runs(\?.*)?$/;

function baseRun(id: string, task: string, over: Record<string, unknown> = {}): Record<string, unknown> {
  const now = new Date().toISOString();
  return {
    id,
    task,
    title: task,
    created_at: now,
    updated_at: now,
    created_by: "admin-token",
    agent: "claude-code",
    repo: "acme/widgets",
    confinement_class: "CC2",
    state: "RUNNING",
    spiffe_id: `spiffe://${id}`,
    runner_target: "docker",
    ...over,
  };
}

// Mutable across the test's own lifetime — a real Revive changes what the
// NEXT poll returns, which is exactly the "moves it to Running" half of
// design.md's own pin.
async function mockMutableRunsList(page: Page, initial: unknown[]) {
  let rows = initial;
  await page.route(RUNS_LIST_RE, async (route) => {
    const url = new URL(route.request().url());
    if (!url.searchParams.has("view")) return route.fallback();
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      headers: { "X-Wardyn-Hidden-Older": "0", "X-Wardyn-Hidden-Killed": "0" },
      body: JSON.stringify(rows),
    });
  });
  return { setRows: (next: unknown[]) => { rows = next; } };
}

test.describe("Runs landing — lost and lease-ended rows (F1, design.md:158)", () => {
  test("a lost run (by=you) is in Needs you, and Revive moves it to Running", async ({ page }) => {
    const lostId = "lost-run-1";
    const list = await mockMutableRunsList(page, [
      baseRun(lostId, "e2e lost run", { attention: { kind: "lost", by: "you", pending: 0 } }),
    ]);
    let revived = false;
    await page.route(`**/api/v1/runs/${lostId}/revive`, async (route) => {
      revived = true;
      list.setRows([baseRun(lostId, "e2e lost run")]); // the server's own post-revive shape: RUNNING, no attention
      await route.fulfill({ json: { run_id: lostId, denied_added: [], proxy_release: "r1" } });
    });

    await gotoConsole(page);
    await expect(page.getByRole("heading", { name: "Needs you" })).toBeVisible();
    const row = page.getByTestId("run-row").filter({ hasText: "e2e lost run" });
    await expect(row).toBeVisible();
    await expect(row.getByText("Sandbox stopped")).toBeVisible();

    const reviveButton = row.getByRole("button", { name: "Revive" });
    await expect(reviveButton).toBeVisible();
    await reviveButton.click();
    await expect.poll(() => revived).toBe(true);

    // "Revive moves it to Running": the next poll drops the row from Needs
    // you and the row itself now reads Running.
    await expect(page.getByRole("heading", { name: "Needs you" })).toHaveCount(0);
    await expect(page.getByTestId("run-row").filter({ hasText: "e2e lost run" }).getByText("Running")).toBeVisible();
  });

  // A revive on a runner can outlast the console's 60s wait (the server allows it 3 minutes): the wait ending
  // is not a failure, so the toast says the runner is still working and no error toast invites a second click.
  test("a revive the console stopped waiting for says the runner is still starting the run", async ({ page }) => {
    const lostId = "lost-run-slow";
    // The console's 60s wait, shortened for the one call made after the flag is set, so the test need not wait a minute.
    await page.addInitScript(() => {
      const w = window as unknown as { __shortWait?: boolean };
      const timeout = AbortSignal.timeout.bind(AbortSignal);
      AbortSignal.timeout = (ms: number) => {
        if (!w.__shortWait || ms < 60_000) return timeout(ms);
        w.__shortWait = false;
        return timeout(300);
      };
    });
    await mockMutableRunsList(page, [baseRun(lostId, "e2e slow revive", { attention: { kind: "lost", by: "you", pending: 0 } })]);
    await page.route(`**/api/v1/runs/${lostId}/revive`, () => {
      /* the runner has not answered */
    });

    await gotoConsole(page);
    const row = page.getByTestId("run-row").filter({ hasText: "e2e slow revive" });
    await expect(row).toBeVisible();
    await page.evaluate(() => ((window as unknown as { __shortWait?: boolean }).__shortWait = true));
    await row.getByRole("button", { name: "Revive" }).click();
    await expect(page.getByText("The runner is still starting this run. Check back in a minute.")).toBeVisible();
    await expect(page.getByText("Couldn't revive this run")).toHaveCount(0);
    await expect(row.getByRole("button", { name: "Revive" })).toBeVisible();
  });

  test("a lease-ended run is grey under Ended, never in Needs you, with no Revive button", async ({ page }) => {
    const endedId = "ended-run-1";
    await mockMutableRunsList(page, [
      baseRun(endedId, "e2e lease-ended run", {
        lost_reason: "ended",
        // An hour ago, but never before today's midnight: a run that ended
        // yesterday sits under the collapsed "Earlier" group, not "Ended today".
        lost_at: new Date(Math.max(Date.now() - 3600_000, new Date().setHours(0, 0, 1, 0))).toISOString(),
      }),
    ]);

    await gotoConsole(page);
    await expect(page.getByRole("heading", { name: "Needs you" })).toHaveCount(0);
    const row = page.getByTestId("run-row").filter({ hasText: "e2e lease-ended run" });
    await expect(row).toBeVisible();
    await expect(row.getByText("Ended at its end time")).toBeVisible();
    await expect(row.getByRole("button", { name: "Revive" })).toHaveCount(0);
  });
});

// DR-0261: a local run its runner lost contact with says its network was stopped on time only on evidence. The
// evidence is the audit read; a read that fails is no evidence, so the banner makes no on-time or late claim then, but
// it keeps its title and Revive / Extend / End run, which are the only way to act on the run.
test.describe("Run detail — the local lost-run banner", () => {
  async function openLostLocalRun(page: Page, audit: "fail" | "empty", online: boolean | "unreported" = true) {
    await page.route("**/api/v1/setup/status*", async (route) => {
      const response = await route.fetch();
      const json = await response.json();
      json.runner = { ...json.runner, driver: "docker", confinement_classes: ["CC1"] };
      await route.fulfill({ response, json });
    });
    await page.route("**/api/v1/runs/*", async (route) => {
      if (route.request().method() !== "GET") return route.fallback();
      const response = await route.fetch();
      const json = await response.json();
      if (json.task === "e2e fixture 2") {
        Object.assign(json, {
          placement: "local",
          lost_reason: "outage",
          lost_at: new Date(Date.now() - 3_600_000).toISOString(),
          runner_online: online === "unreported" ? undefined : online,
          runner_name: "laptop-7",
        });
      }
      await route.fulfill({ response, json });
    });
    await page.route("**/api/v1/audit?*run.containment.reassert*", async (route) => {
      if (audit === "fail") return route.fulfill({ status: 500, json: { error: "boom" } });
      return route.fulfill({ json: [] });
    });
    await gotoConsole(page);
    await navTo(page, "Runs");
    await page.getByText("e2e fixture 2").click();
    await expect(page.getByRole("heading", { name: "e2e fixture 2", level: 1 })).toBeVisible();
  }

  test("with the late-stop read confirming nothing late, it says the network was stopped", async ({ page }) => {
    await openLostLocalRun(page, "empty");
    await expect(page.getByTestId("run-lifetime-local-stopped")).toContainText("This run's network was stopped");
  });

  test("with the late-stop read failing, it keeps the actions and makes no claim about when the network was stopped", async ({ page }) => {
    await openLostLocalRun(page, "fail");
    const banner = page.getByTestId("run-lifetime-local-stopped");
    await expect(banner).toContainText("This run's network was stopped");
    await expect(banner.getByRole("button", { name: "Revive" })).toBeVisible();
    await expect(banner.getByRole("button", { name: "Extend" })).toBeVisible();
    await expect(banner.getByRole("button", { name: "End run" })).toBeVisible();
    await expect(banner).not.toContainText("reports its runner stopped this run's network");
    await expect(banner).not.toContainText("was still on when it came back");
  });

  // FR-0114: with several server replicas the one asked may not know whether the runner is connected.
  test("a runner connection the server does not report says it cannot tell and keeps Revive and Extend", async ({ page }) => {
    await openLostLocalRun(page, "empty", "unreported");
    const banner = page.getByTestId("run-lifetime-local-unknown");
    await expect(banner).toContainText("Wardyn can't tell from here whether laptop-7 is connected");
    await expect(banner).not.toContainText("offline for over an hour");
    await expect(banner.getByRole("button", { name: "Revive" })).toBeVisible();
    await expect(banner.getByRole("button", { name: "Extend" })).toBeVisible();
    await expect(banner.getByRole("button", { name: "End run" })).toBeVisible();
  });
});
