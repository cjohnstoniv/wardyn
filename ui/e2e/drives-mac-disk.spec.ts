/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New Run, Files: a person's disk drive kept on a Mac (backend mac_disk). The catalogue answers one such drive
// with its Mac's name, and the card it becomes says where it is stored. The routes are stubbed: the harness has
// no Mac. The wording is read from the copy module, not retyped.
import type { Page } from "@playwright/test";
import { test, expect, gotoConsole, navToRoute, walkGuidedToFiles } from "./fixtures";
import { MAC_DRIVES } from "../src/app/components/wardyn/copy/mac-drives";
import { NR_FILES } from "../src/app/components/wardyn/copy/new-run/files";
import { NR_SHARED } from "../src/app/components/wardyn/copy/new-run/shared";
import { NR_START } from "../src/app/components/wardyn/copy/new-run/start";

const POOL = { id: "f2222222-2222-4222-8222-222222222222", name: "General", hosting_type: "remote_provided" };
const MAC = "mac-studio-07";
const DISK = {
  id: "f4444444-4444-4444-8444-444444444444", name: "Investigation files", section: "user", source: "personal", backend: "mac_disk",
  target: "/Users/agent/drives/investigation-files", writable: true, enforcement: "none", legacy: false, owned: true, team: false, mac: MAC,
};

test.describe("New Run, Files: a disk kept on a Mac", () => {
  test("the card's Stored on value names the Mac", async ({ page }: { page: Page }) => {
    await page.route("**/api/v1/me/drives**", (route) => route.fulfill({ json: { drives: [DISK], pool_checked: true } }));
    await page.route("**/api/v1/runs/preflight", async (route) => {
      const body = route.request().postDataJSON() as { drives?: { drive_id: string }[] };
      return route.fulfill({
        json: {
          setup_items: [],
          enforced_confinement_class: "CC2",
          runner_pool: { ...POOL, revision: 1, selection: "organisation_default" },
          runner_pools: [{ ...POOL, availability: "available", limits: [] }],
          main_drive: { applied_mib: 10240, basis: "default", soft: false },
          attachments: (body.drives ?? []).map((d) => ({ kind: "drive", drive_id: d.drive_id, name: DISK.name, target: DISK.target, read_only: false, starts_here: true })),
        },
      });
    });
    await gotoConsole(page);
    await navToRoute(page, "/runs/new");
    await expect(page.getByRole("heading", { name: NR_START.TITLE })).toBeVisible();
    await walkGuidedToFiles(page, { shape: "agent_task" });

    await page.getByRole("button", { name: NR_FILES.ADD_ARIA }).click();
    const dialog = page.getByRole("dialog");
    await dialog.getByRole("button", { name: new RegExp(`^${NR_FILES.KIND_USER_DRIVE}`) }).click();
    await expect(dialog.getByText(MAC_DRIVES.D01(MAC))).toBeVisible();
    await dialog.getByRole("radio", { name: new RegExp(`^${DISK.name}`) }).check();
    await dialog.getByRole("button", { name: NR_SHARED.CONTINUE, exact: true }).click();
    await dialog.getByRole("button", { name: NR_FILES.ADD_CONFIRM, exact: true }).click();

    const card = page.getByTestId("nr-attach-card").filter({ hasText: DISK.name });
    await card.locator("button[aria-expanded]").click();
    await expect(card.getByText(MAC_DRIVES.D01(MAC))).toBeVisible();
  });
});
