/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Admin -> User drives: the list of people's own drives, with a disk kept on a Mac in it (board J4, "Admin view → User
// drives"). The harness has no Mac, so GET /drives/member and the runner list are answered here; what is pinned is that
// the console draws the owner, Storage, Mac, Size and State from those fields, and fits at 1440 wide with its Delete
// button in view. The wording is read from the copy modules.
import { test, expect, gotoConsole, navToRoute } from "./fixtures";
import { aheadByHours } from "../src/app/lib/test-clock";
import { MAC_DRIVES } from "../src/app/components/wardyn/copy/mac-drives";
import { MEMBER_DRIVES } from "../src/app/components/wardyn/copy/member-drives";
import { NR_FILES } from "../src/app/components/wardyn/copy/new-run/files";

const MAC_ID = "00000000-0000-4000-8000-0000000000d1";
const item = (id: string, name: string, owner: string, extra: Record<string, unknown> = {}) => ({
  id, name, owner, backend: "docker_volume", object: `vol-${id}`, created_at: aheadByHours(-48), deleting: false, ...extra,
});
const DRIVES = [
  item("00000000-0000-4000-8000-0000000000e1", "Investigation files", "dana.long-address@example.com", { backend: "mac_disk", home_runner_id: MAC_ID, size_mib: 65536 }),
  item("00000000-0000-4000-8000-0000000000e2", "Build cache", "kofi@example.com", { backend: "mac_disk", home_runner_id: MAC_ID, size_mib: 65536, deleting: true }),
  item("00000000-0000-4000-8000-0000000000e3", "Notes", "lena@example.com"),
];

test.use({ viewport: { width: 1440, height: 900 } });

test("people's drives name the Mac a disk is kept on, and fit the page", async ({ page }) => {
  await page.route(/\/api\/v1\/drives\/member(\?|$)/, (route) => route.fulfill({ json: { drives: DRIVES, total: DRIVES.length } }));
  await page.route(/\/api\/v1\/runners\?/, (route) => route.fulfill({ json: [{ id: MAC_ID, name: "mac-studio-07" }] }));
  await gotoConsole(page, "admin");
  await navToRoute(page, "/admin/drives");

  const section = page.getByTestId("member-drives");
  await expect(section.getByRole("heading", { name: MEMBER_DRIVES.TITLE })).toBeVisible();
  const disk = section.getByRole("row", { name: /Investigation files/ });
  await expect(disk.getByText(MAC_DRIVES.D16)).toBeVisible();
  await expect(disk.getByText("mac-studio-07")).toBeVisible();
  await expect(disk.getByText("64 GiB")).toBeVisible();
  await expect(section.getByRole("row", { name: /Build cache/ }).getByText(MEMBER_DRIVES.STATE_DELETING)).toBeVisible();
  await expect(section.getByRole("row", { name: /Notes/ }).getByText(NR_FILES.FAMILY_DOCKER_VOLUME)).toBeVisible();

  // No sideways scroll, and Delete is inside the card.
  const fits = await section.evaluate((el) => {
    const table = el.querySelector("table")!.parentElement!;
    const del = el.querySelector("tbody tr button")!.getBoundingClientRect();
    return { scrolls: table.scrollWidth > table.clientWidth, deleteInside: del.right <= el.getBoundingClientRect().right };
  });
  expect(fits).toEqual({ scrolls: false, deleteInside: true });
});
