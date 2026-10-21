/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New Run, Files and Review: "Reuse it across runs" keeps the run's work folder as the person's own drive
// (#1991). The seeded backend has no drive that can be created or deleted and no pool, so the drive catalogue
// and the preflight are stubbed; the catalogue's create door is the switch, and the specs turn it per case.
// The preflight echoes the body it was sent, so what is asserted on the request is what Review and create send.
import type { Page } from "@playwright/test";
import { test, expect, chooseFiles, continueNewRun, gotoConsole, navToRoute, walkGuidedToFiles } from "./fixtures";
import { NR_FILES } from "../src/app/components/wardyn/copy/new-run/files";
import { NR_REVIEW } from "../src/app/components/wardyn/copy/new-run/review";
import { NR_SHARED } from "../src/app/components/wardyn/copy/new-run/shared";
import { NR_START } from "../src/app/components/wardyn/copy/new-run/start";
import { NR_TOOLS } from "../src/app/components/wardyn/copy/new-run/tools";

const POOL = { id: "f2222222-2222-4222-8222-222222222222", name: "General", hosting_type: "remote_provided" };
const OPEN = { allowed: true, count: 1, limit: 3, keep_work_folder: true, enforcement: "none" };
const KEPT_ROW = {
  id: "f4444444-4444-4444-8444-444444444444", name: "nightly-tests 1a2b3c4d", section: "user", source: "personal", backend: "docker_volume",
  target: "/home/agent/drives/nightly-tests", writable: true, enforcement: "none", legacy: false, owned: true, kept_from_run: "7c1d0f4e-2b8a-4e53-9a60-1f0e5d3c8b21", kept_from_run_title: "nightly-tests",
};

type Create = Record<string, unknown>;
interface Stubs { sent: Record<string, unknown>[]; setCreate: (c: Create | undefined) => void; setDrives: (d: object[]) => void }

/** The drive catalogue with a movable create door, and a clear preflight that records the body it is sent. */
async function stub(page: Page, first: Create | undefined): Promise<Stubs> {
  let create = first;
  let drives: object[] = [];
  const sent: Record<string, unknown>[] = [];
  await page.route("**/api/v1/me/drives**", (route) =>
    route.fulfill({ json: { drives, pool_checked: true, ...(create ? { create } : {}) } }),
  );
  await page.route("**/api/v1/runs/preflight", (route) => {
    sent.push(route.request().postDataJSON() as Record<string, unknown>);
    return route.fulfill({
      json: {
        setup_items: [],
        enforced_confinement_class: "CC2",
        runner_pool: { ...POOL, revision: 1, selection: "organisation_default" },
        runner_pools: [{ ...POOL, availability: "available", limits: [] }],
        main_drive: { applied_mib: 10240, basis: "default", soft: false },
        attachments: [],
      },
    });
  });
  return { sent, setCreate: (c) => { create = c; }, setDrives: (d) => { drives = d; } };
}

/** An autonomous run, so the main drive is deleted when it ends and "What happens to it" says so. */
async function toFiles(page: Page): Promise<void> {
  await gotoConsole(page);
  await navToRoute(page, "/runs/new");
  await expect(page.getByRole("heading", { name: NR_START.TITLE })).toBeVisible();
  await walkGuidedToFiles(page, { shape: "agent_task", mode: "autonomous" });
}

const tick = (page: Page) => page.getByRole("checkbox", { name: NR_FILES.REUSE_LABEL });
const lastSent = (s: Stubs) => s.sent[s.sent.length - 1] ?? {};
// The main drive group only: the review and the mount map repeat some of its words.
const main = (page: Page) => page.locator("section[aria-labelledby=nr-main-title]");
const summary = (page: Page) => page.getByTestId("nr-files-summary");
const FILES_SUMMARY = "Main drive 10 GiB · starts in the main drive · the work folder is kept as a drive after the run";

test.describe("New Run: keep the work folder", () => {
  test("with the switch off the row is today's: a disabled tick and Coming soon", async ({ page }) => {
    await stub(page, { allowed: true, count: 1, limit: 3, keep_work_folder: false });
    await toFiles(page);
    await expect(tick(page)).toBeDisabled();
    await expect(page.getByText(NR_SHARED.COMING_SOON).first()).toBeVisible();
    await expect(page.getByTestId("nr-keep-work-folder")).toHaveCount(0);
    await expect(main(page).getByText(NR_FILES.LIFE_DELETED, { exact: true })).toBeVisible();
  });

  test("ticking explains what is kept, says what comes back, and Review and the request carry it", async ({ page }) => {
    const s = await stub(page, OPEN);
    await toFiles(page);
    await expect(tick(page)).toBeEnabled();
    await expect(main(page).getByText(NR_FILES.LIFE_DELETED, { exact: true })).toBeVisible();
    await chooseFiles(page);
    await expect.poll(() => lastSent(s).keep_work_folder).toBeUndefined();

    await tick(page).check();
    const box = page.getByTestId("nr-keep-work-folder");
    await expect(box).toContainText(NR_FILES.KEEP_WHAT);
    await expect(box.getByRole("note")).toHaveText(NR_FILES.KEEP_WARNING);
    await expect(box).toContainText(`${NR_FILES.KEEP_COUNTS(1, 3)} ${NR_FILES.KEEP_UNENFORCED}`);
    await expect(main(page).getByText(NR_FILES.KEEP_LIFE, { exact: true })).toBeVisible();
    await expect(main(page).getByText(NR_FILES.LIFE_DELETED, { exact: true })).toHaveCount(0);
    await expect(summary(page)).toHaveText(FILES_SUMMARY);
    await expect.poll(() => lastSent(s).keep_work_folder).toBe(true);

    await continueNewRun(page, NR_TOOLS.TITLE);
    await continueNewRun(page, NR_REVIEW.TITLE);
    const work = page.locator("[data-review-group=work]");
    await expect(work).toContainText(FILES_SUMMARY);
    await expect(work).toContainText(NR_FILES.KEEP_LIFE);

    await work.getByRole("button", { name: new RegExp(`^${NR_REVIEW.CHANGE} `) }).click();
    await work.getByRole("button", { name: NR_REVIEW.EDIT_FILES }).click();
    await tick(page).uncheck();
    await expect(page.getByTestId("nr-keep-work-folder")).toHaveCount(0);
    await expect(main(page).getByText(NR_FILES.LIFE_DELETED, { exact: true })).toBeVisible();
    await expect.poll(() => "keep_work_folder" in lastSent(s)).toBe(false);
  });

  test("on a runner that enforces the size it does not say the size is unenforced", async ({ page }) => {
    await stub(page, { ...OPEN, enforcement: "filesystem" });
    await toFiles(page);
    await tick(page).check();
    const box = page.getByTestId("nr-keep-work-folder");
    await expect(box).toContainText(NR_FILES.KEEP_COUNTS(1, 3));
    await expect(box).not.toContainText(NR_FILES.KEEP_UNENFORCED);
  });

  test("with member drives off the tick is closed and says why", async ({ page }) => {
    await stub(page, { allowed: false, reason: "disabled", count: 0, limit: 0, keep_work_folder: true });
    await toFiles(page);
    await expect(page.getByText(NR_FILES.CREATE_DISABLED)).toBeVisible();
    await expect(tick(page)).toBeDisabled();
    await expect(tick(page)).toHaveAccessibleDescription(NR_FILES.CREATE_DISABLED);
    await expect(page.getByText(NR_SHARED.COMING_SOON)).toHaveCount(0);
  });

  test("at the drive limit the tick is closed, and a draft that asked is stopped at Review until it lets go", async ({ page }) => {
    const s = await stub(page, OPEN);
    await toFiles(page);
    await chooseFiles(page);
    await tick(page).check();
    await expect.poll(() => lastSent(s).keep_work_folder).toBe(true);

    // The person reaches their limit (a second tab, a rerun): the next read of the catalogue says so.
    s.setCreate({ allowed: false, reason: "limit_reached", count: 3, limit: 3, keep_work_folder: true });
    await continueNewRun(page, NR_TOOLS.TITLE);
    await continueNewRun(page, NR_REVIEW.TITLE);
    const blockers = page.locator("#nr-blockers");
    const sentence = "You have 3 of 3 drives. Delete one to keep this run's work folder.";
    await expect(blockers.getByRole("button", { name: sentence })).toBeVisible();
    await expect(blockers).toContainText("Or untick “Reuse it across runs” to launch without keeping it.");
    await expect(page.getByRole("button", { name: NR_REVIEW.LAUNCH_RUN })).toBeDisabled();

    // The blocker takes the person to Files, where the tick is still theirs to take back.
    await blockers.getByRole("button", { name: sentence }).click();
    await expect(page.getByRole("heading", { name: NR_FILES.TITLE })).toBeVisible();
    await expect(page.getByText(sentence)).toBeVisible();
    await expect(tick(page)).toBeChecked();
    await expect(tick(page)).toBeEnabled();
    await tick(page).uncheck();
    await expect(tick(page)).toBeDisabled();
    await expect(tick(page)).toHaveAccessibleDescription(sentence);
    await continueNewRun(page, NR_TOOLS.TITLE);
    await continueNewRun(page, NR_REVIEW.TITLE);
    await expect(blockers).toBeEmpty();
  });

  test("a kept work folder is listed under the user drives with its run, its lifetime and, on Docker, its size", async ({ page }) => {
    const s = await stub(page, OPEN);
    s.setDrives([KEPT_ROW]);
    await toFiles(page);
    await page.getByRole("button", { name: NR_FILES.ADD_ARIA }).click();
    const dialog = page.getByRole("dialog");
    await dialog.getByRole("button", { name: new RegExp(`^${NR_FILES.KIND_USER_DRIVE}`) }).click();
    const option = dialog.getByRole("radiogroup").locator("label").filter({ hasText: KEPT_ROW.name });
    await expect(option).toContainText(NR_FILES.DRIVE_KEPT_FROM("nightly-tests"));
    await expect(option).not.toContainText(KEPT_ROW.kept_from_run);
    await expect(option).toContainText(KEPT_ROW.target);
    await expect(option.getByText(NR_FILES.DRIVE_SIZE_NOT_ENFORCED)).toBeVisible();
    await option.getByRole("button", { name: NR_FILES.DRIVE_SIZE_NOT_ENFORCED }).click();
    await expect(page.getByText(NR_FILES.DRIVE_SIZE_NOT_ENFORCED_TIP)).toBeVisible();
    // Its own Delete drive sits beside the list, as for any drive the person made.
    await expect(dialog.getByTestId("nr-own-drives").getByRole("button", { name: NR_FILES.DELETE_DRIVE })).toBeVisible();
  });
});
