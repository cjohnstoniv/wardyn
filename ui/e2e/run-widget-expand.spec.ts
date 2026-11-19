/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, gotoConsole, layoutSettled, sql } from "./fixtures";
import { randomUUID } from "node:crypto";
import type { Page } from "@playwright/test";
import { RUN_COCKPIT } from "../src/app/components/wardyn/copy";
import { DESKTOP } from "../src/app/components/wardyn/copy/desktop";

// A run page tile that holds more than it shows says so, and every tile but the Terminal opens the same live widget in
// a large view. The unit tests pin the moves; this is what only a real browser can read: the scroll offsets, the
// scrollbar, the focus order, the gutter above the dialog and the approvals under it.
test.use({ colorScheme: "dark" });

const PATHS = (n: number) =>
  Array.from({ length: n }, (_, i) => ({
    path: `internal/billing/ledger/generated/part_${String(i).padStart(2, "0")}/reconcile_pending_invoices_with_upstream_provider_and_retry.go`,
    status: "M",
    added: i + 1,
    deleted: 0,
  }));

let paths = 60;
async function stubFiles(page: Page) {
  await page.route("**/api/v1/runs/*/files", (route) =>
    route.fulfill({ json: { vcs: "git", files: PATHS(paths), truncated: false, path: "/workspace" } }),
  );
}

async function openRun(page: Page, viewport = { width: 1440, height: 900 }): Promise<string> {
  const id = sql("SELECT id FROM agent_runs ORDER BY created_at LIMIT 1");
  sql(`UPDATE agent_runs SET state = 'RUNNING' WHERE id = '${id}'`);
  await stubFiles(page);
  // Load the console at the default size: below the sidebar's breakpoint the "Runs" link gotoConsole waits for is hidden.
  await gotoConsole(page);
  await page.setViewportSize(viewport);
  await page.goto(`/runs/${id}`);
  return id;
}

const filesTile = (page: Page) => page.locator('[data-widget="files"]');
const filesBody = (page: Page) => filesTile(page).locator("section > :last-child");

test("#3940: a Files tile that scrolls opens whole, from where it was scrolled to, and goes back there", async ({ page }) => {
  paths = 60;
  await openRun(page);
  const body = filesBody(page);
  await expect(body).toBeVisible();
  expect(await body.evaluate((el) => el.scrollHeight > el.clientHeight)).toBe(true);
  // The cue: the scrollbar's space is held, and drawn.
  expect(await body.evaluate((el) => (el as HTMLElement).offsetWidth - el.clientWidth)).toBeGreaterThan(0);

  await body.evaluate((el) => (el.scrollTop = 300));
  await page.getByRole("button", { name: RUN_COCKPIT.expandWidget("Files changed") }).click();
  const dialog = page.getByRole("dialog", { name: "Files changed" });
  await expect(dialog).toBeVisible();
  const big = dialog.locator("[data-widget-body]");
  expect(await big.evaluate((el) => el.scrollTop)).toBeGreaterThan(0);
  // A long path is whole.
  const first = big.getByText("part_00", { exact: false }).first();
  expect(await first.evaluate((el) => el.scrollWidth <= el.clientWidth)).toBe(true);

  await page.keyboard.press("Escape");
  await expect(dialog).toHaveCount(0);
  expect(await filesBody(page).evaluate((el) => el.scrollTop)).toBeGreaterThan(0);
});

test("opening or closing it, and a row count that grows, move nothing in the tile's header", async ({ page }) => {
  paths = 2;
  await openRun(page);
  const control = page.getByRole("button", { name: RUN_COCKPIT.expandWidget("Files changed") });
  // Measured from the tile's own corner: the claim is about the tile's header, and the page above the tile can still
  // move it as a whole while other tiles load (a 4px shift of the whole tile failed this once).
  const boxes = async () => {
    const tile = await filesTile(page).boundingBox();
    const at = async (b: { x: number; y: number; width: number; height: number } | null) =>
      b && tile && { x: b.x - tile.x, y: b.y - tile.y, width: b.width, height: b.height };
    return [tile && { width: tile.width, height: tile.height }, await at(await filesTile(page).locator("section > :first-child").boundingBox()), await at(await control.boundingBox())];
  };
  // The 2-row state is on screen before it is measured: the last of the two is there and the sixtieth is not.
  await expect(filesTile(page).getByText("part_01", { exact: false })).toHaveCount(1);
  await expect(filesTile(page).getByText("part_59", { exact: false })).toHaveCount(0);
  await layoutSettled(filesTile(page));
  const before = await boxes();
  paths = 60;
  // The next poll brings the sixtieth row in; wait for it rendered, not for a fixed time.
  await expect(filesTile(page).getByText("part_59", { exact: false })).toHaveCount(1, { timeout: 15_000 });
  await layoutSettled(filesTile(page));
  expect(await boxes()).toEqual(before);
  // Opening the large view and closing it again leaves the header where it was.
  await control.click();
  const dialog = page.getByRole("dialog", { name: "Files changed" });
  await expect(dialog).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(dialog).toHaveCount(0);
  await layoutSettled(filesTile(page));
  expect(await boxes()).toEqual(before);
});

test("keyboard: Enter opens it, focus lands in the dialog, Tab stays inside, Escape returns to the control", async ({ page }) => {
  paths = 60;
  await openRun(page);
  const control = page.getByRole("button", { name: RUN_COCKPIT.expandWidget("Files changed") });
  await control.focus();
  await page.keyboard.press("Enter");
  const dialog = page.getByRole("dialog", { name: "Files changed" });
  await expect(dialog).toBeVisible();
  await expect(dialog.locator("[data-widget-body]")).toBeFocused();
  const top = await dialog.locator("[data-widget-body]").evaluate((el) => el.scrollTop);
  await page.keyboard.press("PageDown");
  await expect.poll(() => dialog.locator("[data-widget-body]").evaluate((el) => el.scrollTop)).toBeGreaterThan(top);
  for (let i = 0; i < 6; i++) {
    await page.keyboard.press("Tab");
    expect(await page.evaluate(() => !!document.activeElement?.closest('[role="dialog"]'))).toBe(true);
  }
  await page.keyboard.press("Escape");
  await expect(dialog).toHaveCount(0);
  await expect(control).toBeFocused();
});

test("a click in the moved widget does not close it, and the dialog starts 64px down so the daemon banner stays above", async ({ page }) => {
  paths = 60;
  await openRun(page);
  await page.getByRole("button", { name: RUN_COCKPIT.expandWidget("Identity") }).click();
  const dialog = page.getByRole("dialog", { name: "Identity" });
  await expect(dialog).toBeVisible();
  const box = (await dialog.boundingBox())!;
  expect(box.y).toBeGreaterThanOrEqual(63);
  expect(box.y + box.height).toBeLessThanOrEqual(900 - 43);
  await dialog.locator("[data-widget-body]").click();
  await expect(dialog).toBeVisible();
});

test("a held approval is not covered by the dialog and Approve works from it, at 1440 and 390", async ({ page }) => {
  paths = 60;
  const id = await openRun(page);
  const CMD = "bash.exec --run the-nightly-integration-suite-with-extra-long-name";
  // Its own approval, removed after, so a later test on the same run sees one Deny, not two.
  const approvalId = randomUUID();
  sql(`INSERT INTO approvals (id, run_id, kind, requested_scope, state, requested_at)
       VALUES ('${approvalId}', '${id}', 'tool_call', '${JSON.stringify({ tool: "bash.exec", cmd: CMD })}'::jsonb, 'PENDING', now() - interval '5 min')`);
  try {
    for (const viewport of [{ width: 1440, height: 900 }, { width: 390, height: 844 }]) {
      await page.setViewportSize(viewport);
      await page.goto(`/runs/${id}`);
      await page.getByRole("button", { name: RUN_COCKPIT.expandWidget("Egress") }).click();
      const dialog = page.getByRole("dialog", { name: "Egress" });
      const approve = dialog.getByRole("button", { name: "Approve", exact: true });
      await expect(approve).toBeVisible();
      const [x, y] = await approve.evaluate((el) => {
        const r = el.getBoundingClientRect();
        return [r.x + r.width / 2, r.y + r.height / 2];
      });
      expect(await page.evaluate(([px, py]) => document.elementFromPoint(px, py)?.closest("button")?.textContent?.trim(), [x, y])).toContain("Approve");
      await page.keyboard.press("Escape");
      await expect(dialog).toHaveCount(0);
    }
  } finally {
    sql(`DELETE FROM approvals WHERE id = '${approvalId}'`);
  }
});

// 844 tall, not 568: at 320x568 the host banner and run header leave the canvas no clickable height (a run page layout
// limit apart from expand; follow-up filed in the rc.11 tracker).
test("at 320 wide the control is inside the tile once scrolled to it, and the dialog does not scroll the page sideways", async ({ page }) => {
  paths = 60;
  await openRun(page, { width: 320, height: 844 });
  const control = page.getByRole("button", { name: RUN_COCKPIT.expandWidget("Egress") });
  await layoutSettled(control);
  const [c, t] = [(await control.boundingBox())!, (await page.locator('[data-widget="egress"]').boundingBox())!];
  expect(c.x).toBeGreaterThanOrEqual(t.x);
  expect(c.x + c.width).toBeLessThanOrEqual(t.x + t.width + 1);
  await control.click();
  await expect(page.getByRole("dialog", { name: "Egress" })).toBeVisible();
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= document.documentElement.clientWidth)).toBe(true);
  await expect(page.getByRole("button", { name: "Close" })).toBeVisible();
});

test("edit layout draws no visible control, and focus mode's dock none", async ({ page }) => {
  paths = 60;
  await openRun(page);
  await page.getByRole("button", { name: RUN_COCKPIT.editLayout }).click();
  await expect(page.locator("[data-widget-expand]").first()).toBeHidden();
  await page.getByRole("button", { name: RUN_COCKPIT.doneEditing }).click();
  await page.getByRole("button", { name: RUN_COCKPIT.enterFocus }).click();
  // The dock opens on its first widget, Egress, so show another one.
  await page.getByRole("button", { name: RUN_COCKPIT.showWidget("Files changed") }).click();
  await expect(page.getByRole("region", { name: "Files changed" })).toBeVisible();
  await expect(page.getByRole("button", { name: /^Expand / })).toHaveCount(0);
});

// What a widget lets a person press, read from the node itself, less the two controls that only exist in one place.
const names = (root: import("@playwright/test").Locator) =>
  root.evaluate((el) =>
    [...el.querySelectorAll("button, a[href], summary, [role=button]")]
      .filter((n) => !n.hasAttribute("data-widget-expand") && n.textContent?.trim() !== "Close")
      .map((n) => n.getAttribute("aria-label") ?? n.textContent?.trim() ?? ""),
  );

test("each widget offers the same controls expanded as in its tile", async ({ page }) => {
  paths = 60;
  await openRun(page);
  for (const [id, label] of [["egress", "Egress"], ["files", "Files changed"], ["sandbox", "Sandbox"], ["credentials", "Credentials"], ["identity", "Identity"]] as const) {
    const inTile = await names(page.locator(`[data-widget="${id}"]`));
    await page.getByRole("button", { name: RUN_COCKPIT.expandWidget(label) }).click();
    const dialog = page.getByRole("dialog", { name: label });
    await expect(dialog).toBeVisible();
    expect(await names(dialog), label).toEqual(inTile);
    await page.keyboard.press("Escape");
    await expect(dialog).toHaveCount(0);
  }
});

test("the admin view has no ssh control, and a Mac whose Screen Sharing is view only stays view only expanded", async ({ page }) => {
  paths = 60;
  const id = await openRun(page);
  await gotoConsole(page, "admin");
  await page.goto(`/admin/runs/${id}`);
  await expect(page.getByRole("button", { name: RUN_COCKPIT.expandWidget("Egress") })).toBeVisible();
  await expect(page.getByRole("button", { name: RUN_COCKPIT.expandWidget("Attach from your terminal") })).toHaveCount(0);

  await page.route(`**/api/v1/runs/${id}`, async (route) => {
    const res = await route.fetch();
    await route.fulfill({ response: res, body: JSON.stringify({ ...(await res.json()), machine_kind: "macos_vm", macos: { desktop: "observe" } }) });
  });
  await page.route(`**/api/v1/runs/${id}/desktop/sign-ins`, (route) =>
    route.fulfill({ json: { store: "run_disk", paused: false, can_pause: true, agent: "Claude Code" } }),
  );
  await gotoConsole(page);
  await page.goto(`/runs/${id}`);
  await page.getByRole("button", { name: RUN_COCKPIT.expandWidget("Desktop") }).click();
  const dialog = page.getByRole("dialog", { name: "Desktop" });
  await expect(dialog.getByText(DESKTOP.viewOnlyChip)).toBeVisible();
  await expect(dialog.getByRole("button", { name: DESKTOP.watch })).toBeVisible();
  await expect(dialog.getByRole("button", { name: DESKTOP.pauseToSignIn })).toHaveCount(0);
  await expect(dialog.getByText(DESKTOP.phoneBefore)).toHaveCount(0);
});

test("Escape over a Deny confirm closes only the confirm, and the next Escape closes the expanded view", async ({ page }) => {
  paths = 60;
  const id = await openRun(page);
  const approvalId = randomUUID();
  sql(
    `INSERT INTO approvals (id, run_id, kind, requested_scope, state, requested_at)
     VALUES ('${approvalId}','${id}','tool_call','{"tool":"expand-escape.exec"}'::jsonb,'PENDING',now())`,
  );
  try {
    await page.goto(`/runs/${id}`);
    await page.getByRole("button", { name: RUN_COCKPIT.expandWidget("Egress") }).click();
    const dialog = page.getByRole("dialog", { name: "Egress" });
    await dialog.getByRole("button", { name: "Deny", exact: true }).click();
    const confirm = page.getByRole("alertdialog");
    await expect(confirm).toBeVisible();
    // The confirm takes focus as it opens; an Escape sent before that reaches the view under it.
    await expect.poll(() => page.evaluate(() => !!document.activeElement?.closest('[role="alertdialog"]'))).toBe(true);
    // Focus lands before Radix makes the confirm the top layer and attaches its Escape listener, so an earlier key
    // reaches the view under it. Radix react-dismissable-layer 1.2.1 turns off pointer events on the view under the top
    // layer in that same update, so wait for that, then one frame for the listener effect.
    // The open confirm hides the view under it from ARIA, so this read must include hidden elements.
    const under = page.getByRole("dialog", { name: "Egress", includeHidden: true });
    await expect.poll(() => under.evaluate((el) => getComputedStyle(el).pointerEvents)).toBe("none");
    await page.evaluate(() => new Promise<void>((r) => requestAnimationFrame(() => r())));
    await page.keyboard.press("Escape");
    await expect(confirm).toHaveCount(0);
    await expect(dialog).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(dialog).toHaveCount(0);
  } finally {
    sql(`DELETE FROM approvals WHERE id = '${approvalId}'`);
  }
});

// The daemon banner is the one thing that separates a quiet fleet from a dead daemon, so the dialog must not cover it:
// whole above the dialog, one line at 1440 or two at 390, and at 320 at least its headline is above it.
test.describe("the control-plane banner stays above the expanded view", () => {
  for (const [name, viewport, wraps] of [
    ["one line at 1440", { width: 1440, height: 900 }, false],
    ["two lines at 390", { width: 390, height: 844 }, true],
  ] as const) {
    test(name, async ({ page }) => {
      paths = 60;
      await page.route("**/readyz", (route) => route.fulfill({ status: 503, json: { status: "unavailable" } }));
      await openRun(page, viewport);
      const banner = page.getByRole("status").filter({ hasText: "Control plane not available" });
      await expect(banner).toBeVisible();
      const b = (await banner.boundingBox())!;
      if (wraps) expect(b.height).toBeGreaterThan(40);
      await page.getByRole("button", { name: RUN_COCKPIT.expandWidget("Egress") }).click();
      const dialog = (await page.getByRole("dialog", { name: "Egress" }).boundingBox())!;
      expect(b.y + b.height).toBeLessThanOrEqual(dialog.y);
    });
  }

  test("at 320 the headline is in sight", async ({ page }) => {
    paths = 60;
    await page.route("**/readyz", (route) => route.fulfill({ status: 503, json: { status: "unavailable" } }));
    await openRun(page, { width: 320, height: 568 });
    // A CSS locator: the open modal hides the rest of the page from the accessibility tree.
    const banner = page.locator('[role="status"]', { hasText: "Control plane not available" });
    await expect(banner).toBeVisible();
    await page.getByRole("button", { name: RUN_COCKPIT.expandWidget("Egress") }).click();
    const dialog = (await page.getByRole("dialog", { name: "Egress" }).boundingBox())!;
    const first = await banner.evaluate((el) => {
      const r = document.createRange();
      r.selectNodeContents(el.querySelector("span")!);
      return r.getClientRects()[0].bottom;
    });
    expect(first).toBeLessThanOrEqual(dialog.y);
  });
});

test("Full history in Audit, from the expanded view, lands on Audit with the page's pointer events back", async ({ page }) => {
  paths = 60;
  await openRun(page);
  await page.getByRole("button", { name: RUN_COCKPIT.expandWidget("Egress") }).click();
  const dialog = page.getByRole("dialog", { name: "Egress" });
  const link = dialog.getByRole("button", { name: /Full history in Audit/ });
  // The widget draws the link only when the run has more than its 8 newest rows.
  test.skip((await link.count()) === 0, "the seeded run has 8 or fewer egress decisions");
  await link.click();
  await expect(page.getByRole("dialog")).toHaveCount(0);
  await expect(page.getByRole("tab", { name: "Audit" })).toHaveAttribute("aria-selected", "true");
  expect(await page.evaluate(() => getComputedStyle(document.body).pointerEvents)).not.toBe("none");
});
