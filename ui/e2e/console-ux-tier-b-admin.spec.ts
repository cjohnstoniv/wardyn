/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// 0.9.4 console UX Tier B (admin, settings, governance, workspaces screens): the pins that need a real layout
// engine. Everything text- or class-shaped is pinned in vitest next to its screen.
import type { Locator, Page } from "@playwright/test";
import { test, expect, asRealSecurityAdmin, expandCard, gotoConsole, navTo, navToRoute } from "./fixtures";
import { KIND, PERM } from "../src/app/lib/permissions-copy";
import { CHANGES, GOVERNANCE as GOV } from "../src/app/lib/governance-copy";
import { PROVIDERS_SUPER_ONLY } from "../src/app/components/wardyn/copy/console-view";
import { AVAILABILITY, IMAGES } from "../src/app/lib/availability-copy";
import { PROVIDERS } from "../src/app/lib/workspace-providers-copy";
import { HOST_CARD } from "../src/app/components/wardyn/copy/host-card";
import { POOL_FORM } from "../src/app/components/wardyn/copy/runner-pools";

const PHONE = { width: 390, height: 844 };
const WIDE = { width: 1440, height: 900 };

// The Who column of an add-a-grant row holds the type switch. Its options must sit on one line and the column must
// not clip them (#2900): that is a layout fact, so it is read from the engine, not from the class string.
async function expectWhoColumnFitsSwitch(whoField: Locator): Promise<void> {
  const options = whoField.getByRole("button").filter({ hasText: /\S/ });
  expect(await options.count()).toBeGreaterThan(1);
  const tops = await options.evaluateAll((els) => els.map((e) => Math.round(e.getBoundingClientRect().top)));
  expect(new Set(tops).size, `the switch wrapped: option tops ${tops.join(",")}`).toBe(1);
  const fits = await whoField.evaluate((el) => el.scrollWidth <= el.clientWidth + 1);
  expect(fits, "the Who column clips its content").toBe(true);
}

async function boxOf(target: Locator): Promise<{ x: number; y: number; width: number; height: number }> {
  const box = await target.boundingBox();
  if (!box) throw new Error("element has no layout box");
  return box;
}

// Load the console at the default viewport (at 390px the sidebar link gotoConsole waits for is not rendered), then
// shrink to a phone and client-side navigate, as providers.spec.ts does.
async function phoneAt(page: Page, path: string): Promise<void> {
  await gotoConsole(page, "admin");
  await page.setViewportSize(PHONE);
  await navToRoute(page, path);
}

test.describe("Tier B at desktop width", () => {
  // #2852: the actor chip and the outcome differ in width from row to row; the description and the run chip do not move.
  // The two run ids differ in their first 8 characters, so the chips stay distinct if the chip text is cut to 8.
  test("Audit: the description and the run chip start at the same x on every row", async ({ page }) => {
    const row = (id: string, actor_type: string, outcome: string) => ({
      id: `e2e-align-${id}`,
      time: new Date().toISOString(),
      actor_type,
      actor: actor_type === "human" ? "alice@example.com" : "wardyn",
      action: "run.create",
      target: `align-${id}`,
      outcome,
      run_id: `run_${id}`,
    });
    await page.route(/\/api\/v1\/audit(\?|$)/, (route) =>
      route.fulfill({ json: [row("alpha1xx", "human", "success"), row("bravo2xx", "system", "failure")] }),
    );
    await gotoConsole(page, "admin");
    await navTo(page, "Audit");
    const desc = page.locator('main [title^="Created the run"]');
    await expect(desc).toHaveCount(2);
    const chips = [page.getByRole("button", { name: "alpha1xx" }), page.getByRole("button", { name: "bravo2xx" })];
    await expect(chips[0]).toBeVisible();
    expect((await boxOf(desc.nth(0))).x).toBeCloseTo((await boxOf(desc.nth(1))).x, 0);
    expect((await boxOf(chips[0])).x).toBeCloseTo((await boxOf(chips[1])).x, 0);
    await page.unrouteAll({ behavior: "ignoreErrors" });
  });

  // #2899: the required marker no longer grows the Host label row, so its input sits level with its neighbours.
  test("Permissions: the required Host field sits level with the Who field beside it", async ({ page }) => {
    await gotoConsole(page, "admin");
    await navTo(page, "Permissions");
    // Compare with the Capability select: like Host it is a control directly under its label. (The Who control
    // sits under a type switch, so its box is lower by design.)
    const capability = page.locator("#grant-capability");
    const host = page.getByRole("textbox", { name: KIND.egress_host.valueLabel, exact: true });
    await expect(host).toBeVisible();
    await expect(capability).toBeVisible();
    // Hosted Chromium's font metrics move sub-pixels, so compare in whole pixels: the Host label row (marker
    // included) is as tall as the Capability one, and the two controls start on one line.
    const rowOf = (forId: string) => page.locator(`label[for="${forId}"]`).locator("xpath=..");
    expect(Math.abs((await boxOf(rowOf("grant-value"))).height - (await boxOf(rowOf("grant-capability"))).height)).toBeLessThanOrEqual(1);
    expect(Math.abs((await boxOf(host)).y - (await boxOf(capability)).y)).toBeLessThanOrEqual(2);
  });

  // #2892: Open and Delete… both lead with an icon, so their labels start at one x.
  test("Workspaces: the row menu's Open and Delete… start at the same x", async ({ page }) => {
    await gotoConsole(page, "admin");
    await navTo(page, "Workspaces");
    await page.getByRole("button", { name: "Workspace actions" }).first().click();
    const open = page.getByRole("menuitem", { name: "Open" });
    const del = page.getByRole("menuitem", { name: /^Delete/ });
    await expect(open).toBeVisible();
    await expect(open.locator("svg")).toHaveCount(1);
    // The menu scales in; measure once it has settled, or the two icons are read mid-animation.
    // Scoped to the menu: a page-wide wait would never end on a spinner or a pulse elsewhere.
    await page
      .getByRole("menu")
      .evaluate((el) => Promise.all(el.getAnimations({ subtree: true }).map((a) => a.finished.catch(() => undefined))));
    expect((await boxOf(open.locator("svg"))).x).toBeCloseTo((await boxOf(del.locator("svg"))).x, 0);
  });
});

test.describe("Tier B layout rulings at desktop width", () => {
  // #2900: the Who column sizes to its content instead of an equal third, so the type switch stays on one line.
  test("Governance: the add-a-grant Who column holds its type switch on one line", async ({ page }) => {
    await page.setViewportSize(WIDE);
    await gotoConsole(page, "admin");
    await navTo(page, "Governance");
    const grid = page.getByRole("heading", { name: GOV.ADD_TITLE }).locator("xpath=following-sibling::div[1]");
    await expect(grid).toBeVisible();
    await expectWhoColumnFitsSwitch(grid.locator("xpath=./*[1]"));
  });

  test("Permissions: the add-a-grant Who column holds its type switch on one line", async ({ page }) => {
    await page.setViewportSize(WIDE);
    await gotoConsole(page, "admin");
    await navTo(page, "Permissions");
    const who = page.getByRole("textbox", { name: PERM.FIELD_WHO, exact: true });
    await expect(who).toBeVisible();
    const grid = who.locator('xpath=ancestor::div[contains(concat(" ", normalize-space(@class), " "), " grid ")][1]');
    await expectWhoColumnFitsSwitch(grid.locator("xpath=./*[1]"));
  });

  // #2894: the back link leads the page: above the title, on the page's left edge.
  test("Runner pool form: the back link sits above the title, left edges level", async ({ page }) => {
    await page.setViewportSize(WIDE);
    await gotoConsole(page, "admin");
    await navToRoute(page, "/account/pools/new");
    const title = page.getByRole("heading", { name: POOL_FORM.TITLE_NEW });
    const back = page.getByRole("button", { name: POOL_FORM.BACK_MINE });
    await expect(title).toBeVisible();
    const [b, t] = [await boxOf(back), await boxOf(title)];
    expect(b.y + b.height, "back link is not above the title").toBeLessThanOrEqual(t.y + 1);
    // The ghost button insets its label by its own padding; -ml-3 cancels that, so compare the label's edge.
    const pad = await back.evaluate((el) => parseFloat(getComputedStyle(el).paddingLeft));
    expect(Math.abs(b.x + pad - t.x), "back link label and title left edges differ").toBeLessThanOrEqual(4);
  });
});

test.describe("Tier B owner options at desktop width", () => {
  // #2907 (option B): with nothing waiting, the card is one block whose button opens the audit trail.
  test("Governance Changes: one empty block, and its button opens the audit trail", async ({ page }) => {
    await page.route(/\/api\/v1\/governance\/changes(\?|$)/, (route) => route.fulfill({ json: [] }));
    await page.setViewportSize(WIDE);
    await gotoConsole(page, "admin");
    await navTo(page, "Governance");
    await page.getByRole("tab", { name: CHANGES.TAB, exact: true }).click();
    const block = page.getByTestId("changes-empty");
    await expect(block).toBeVisible();
    await expect(block.getByText(CHANGES.EMPTY_TITLE, { exact: true })).toBeVisible();
    await expect(block.getByText(CHANGES.EMPTY_BODY, { exact: true })).toBeVisible();
    await expect(page.getByText(CHANGES.LEAD, { exact: true })).toHaveCount(0);
    await block.getByTestId("changes-empty-audit").click();
    await expect(page).toHaveURL(/\/admin\/audit/);
    await page.unrouteAll({ behavior: "ignoreErrors" });
  });

  // #2876 (option A): the three facts are tiles in one row; the dash splits a status word from its sub-line.
  test("Settings Host card: three tiles in one row, the word before the dash in the pill and the rest under it", async ({ page }) => {
    await page.setViewportSize(WIDE);
    await gotoConsole(page, "admin");
    await navToRoute(page, "/admin/settings");
    await expandCard(page, "Host");
    const tiles = ["image-builder", "recording-store", "internet"].map((id) => page.getByTestId(`host-tile-${id}`));
    for (const t of tiles) await expect(t).toBeVisible();
    const boxes = [await boxOf(tiles[0]), await boxOf(tiles[1]), await boxOf(tiles[2])];
    expect(Math.abs(boxes[0].y - boxes[1].y)).toBeLessThanOrEqual(1);
    expect(Math.abs(boxes[1].y - boxes[2].y)).toBeLessThanOrEqual(1);
    expect(boxes[1].x).toBeGreaterThan(boxes[0].x + boxes[0].width - 1);
    expect(boxes[2].x).toBeGreaterThan(boxes[1].x + boxes[1].width - 1);
    // scripts/e2e-backend.sh boots -runner none with no image builder wired.
    await expect(page.getByTestId("host-tile-image-builder-pill")).toHaveText("Off");
    await expect(page.getByTestId("host-tile-image-builder-sub")).toHaveText(
      "devcontainer builds and --image wraps are unavailable",
    );
    await expect(page.getByTestId("host-state-pill")).toHaveText(HOST_CARD.CANT_LAUNCH);
  });
});

test.describe("Owner-approved refusal and empty states", () => {
  // #2880: a security admin who opens Workspace providers is told it is for super admins, with the way back.
  test("a security admin opening Workspace providers gets the super-admin card and a way back to Runs", async ({ page }) => {
    await asRealSecurityAdmin(page);
    await gotoConsole(page, "admin");
    await navToRoute(page, "/admin/providers");
    await expect(page.getByText(PROVIDERS_SUPER_ONLY.TITLE, { exact: true })).toBeVisible();
    await expect(page.getByText(PROVIDERS_SUPER_ONLY.BODY, { exact: true })).toBeVisible();
    await expect(page.getByText("Requires the admin role.")).toHaveCount(0);
    await page.getByRole("button", { name: PROVIDERS_SUPER_ONLY.CTA, exact: true }).click();
    await expect(page).toHaveURL(/\/admin\/runs/);
  });
});

// The computed value of a CSS colour token, read through a probe element, so a pin compares colours the engine
// resolved rather than class names.
async function tokenColour(page: Page, property: "color" | "backgroundColor" | "borderTopColor", token: string): Promise<string> {
  return page.evaluate(
    ([prop, tok]) => {
      const probe = document.createElement("div");
      if (prop === "borderTopColor") {
        probe.style.borderTop = `1px solid var(${tok})`;
      } else {
        probe.style[prop as "color" | "backgroundColor"] = `var(${tok})`;
      }
      document.body.appendChild(probe);
      const value = getComputedStyle(probe)[prop as "color" | "backgroundColor" | "borderTopColor"];
      probe.remove();
      return value;
    },
    [property, token] as const,
  );
}

test.describe("Quiet empty states inside cards (#2873)", () => {
  // The SSH keys card: a gold key in the standard tile, in a short block with a level-4 title.
  test("Admin SSH keys: the empty key is gold in the standard tile, and the block is short", async ({ page }) => {
    await page.route(/\/api\/v1\/me\/ssh-keys(\?|$)/, (route) =>
      route.request().method() === "GET" ? route.fulfill({ json: [] }) : route.continue(),
    );
    await gotoConsole(page, "admin");
    await navToRoute(page, "/admin/settings");
    await expandCard(page, "Admin SSH keys");
    const tile = page.getByTestId("admin-ssh-keys-card").getByTestId("empty-state-tile");
    await expect(tile).toBeVisible();
    const box = await boxOf(tile);
    expect(Math.round(box.width)).toBe(48);
    const style = await tile.evaluate((el) => {
      const cs = getComputedStyle(el);
      return { color: cs.color, background: cs.backgroundColor, border: cs.borderTopColor };
    });
    expect(style.color).toBe(await tokenColour(page, "color", "--vault-fg"));
    expect(style.background).toBe(await tokenColour(page, "backgroundColor", "--vault-bg"));
    expect(style.border).toBe(await tokenColour(page, "borderTopColor", "--vault-border"));
    const block = page.getByTestId("admin-ssh-keys-card").getByRole("heading", { name: "No admin keys", level: 4 });
    await expect(block).toBeVisible();
    await page.unrouteAll({ behavior: "ignoreErrors" });
  });

  // The Images tab: its own icon, in the standard grey tile.
  test("Images tab: the empty icon stays in the standard grey tile", async ({ page }) => {
    await page.route(/\/api\/v1\/base-images(\?|$)/, (route) =>
      route.request().method() === "GET" ? route.fulfill({ json: [] }) : route.continue(),
    );
    await gotoConsole(page, "admin");
    await navToRoute(page, "/admin/settings");
    await expandCard(page, "Workspace providers");
    await page.getByTestId("providers-card").getByText(PROVIDERS.CARD_OPEN).click();
    await page.getByRole("button", { name: IMAGES.TAB, exact: true }).click();
    const tile = page.getByTestId("empty-state-tile");
    await expect(tile).toBeVisible();
    expect(await tile.evaluate((el) => getComputedStyle(el).color)).toBe(await tokenColour(page, "color", "--muted-foreground"));
    await page.unrouteAll({ behavior: "ignoreErrors" });
  });
});

// Class names are not outcomes: these read what the engine resolved for the elements the Tier B rows restyled.
// (#2888 is pinned in credentials.spec.ts, where its dialog is already reached.)
test.describe("Tier B computed styles", () => {
  // #2875: the no-runner icon keeps its size at phone width instead of being squeezed by the text beside it.
  test("Host card: the no-runner icon does not shrink at 390", async ({ page }) => {
    await phoneAt(page, "/admin/settings");
    await expandCard(page, "Host");
    const icon = page.getByTestId("host-card").getByTestId("no-runner-icon");
    await expect(icon).toBeVisible();
    await expect(icon).toHaveCSS("flex-shrink", "0");
    expect(Math.round((await boxOf(icon)).width)).toBe(20);
  });

  // #2891: the count never wraps between the search box and Refresh.
  test("Workspaces: the list count does not wrap", async ({ page }) => {
    await gotoConsole(page, "admin");
    await navTo(page, "Workspaces");
    await expect(page.getByText(/^\d+ of \d+ workspaces?$/)).toHaveCSS("white-space", "nowrap");
  });

  // #2890: Save secret is the teal primary, not the info blue it was.
  test("Secrets: Save secret is filled with the primary colour", async ({ page }) => {
    await gotoConsole(page, "admin");
    await navTo(page, "Secrets");
    await page.getByRole("button", { name: "Add secret" }).first().click();
    const dialog = page.getByRole("dialog").filter({ hasText: "stored write-only" });
    const save = dialog.getByRole("button", { name: "Save secret" });
    await expect(save).toBeVisible();
    const fill = await save.evaluate((el) => getComputedStyle(el).backgroundColor);
    expect(fill).toBe(await tokenColour(page, "backgroundColor", "--primary"));
  });

  // #2874: the Available to placeholder is in the sans face; the typed value stays mono.
  test("Policies: the Available to placeholder is not in the mono face", async ({ page }) => {
    await gotoConsole(page, "admin");
    await navTo(page, "Policies");
    await page.getByRole("button", { name: "New policy" }).first().click();
    const input = page.getByRole("dialog").filter({ hasText: "New policy" }).getByPlaceholder(AVAILABILITY.ADD_PLACEHOLDER);
    await expect(input).toBeVisible();
    const faces = await input.evaluate((el) => ({
      value: getComputedStyle(el).fontFamily,
      placeholder: getComputedStyle(el, "::placeholder").fontFamily,
    }));
    // The pseudo-element must really have been resolved: a non-empty family that differs from the input's own.
    expect(faces.placeholder, "::placeholder computed no font family").not.toBe("");
    expect(faces.placeholder).not.toBe(faces.value);
    expect(faces.value.toLowerCase()).toContain("mono");
    expect(faces.placeholder.toLowerCase()).not.toContain("mono");
  });
});

test.describe("New policy template menu (#2836)", () => {
  // Option C: one toolbar button opens a menu of the five templates, each label over its hint; picking one
  // replaces the source.
  test("the menu opens with the five templates and their hints, and picking one replaces the source", async ({ page }) => {
    await gotoConsole(page, "admin");
    await navTo(page, "Policies");
    await page.getByRole("button", { name: "New policy" }).first().click();
    const dialog = page.getByRole("dialog").filter({ hasText: "New policy" });
    await expect(page.getByRole("button", { name: "CI baseline", exact: true })).toHaveCount(0);
    await dialog.getByTestId("policy-template-menu").click();
    const items = page.getByRole("menuitem");
    await expect(items).toHaveCount(5);
    for (const id of ["minimal", "model-provider", "registries", "ci", "allow-all"]) {
      await expect(page.getByTestId(`policy-template-${id}`)).toBeVisible();
    }
    await expect(page.getByTestId("policy-template-ci")).toContainText("No egress, no grants, stopped after an idle hour");
    await page.getByTestId("policy-template-ci").click();
    await expect(dialog.getByLabel(/^Spec \((YAML|JSON)\)/)).toHaveValue(/min_confinement_class: CC1/);
  });
});

test.describe("Page header (#2831)", () => {
  const header = (page: Page) => ({
    title: page.getByRole("heading", { name: "Policies", level: 1 }),
    actions: page.getByTestId("page-header-actions").first(),
    description: page.getByTestId("page-header-description").first(),
  });

  // Option B at 390: the action stays on the title's row, pinned right, and the description drops under both.
  test("at 390 the action is level with the title and the description sits below both at full width", async ({ page }) => {
    await phoneAt(page, "/admin/policies");
    const h = header(page);
    await expect(h.actions).toBeVisible();
    const [t, a, d] = [await boxOf(h.title), await boxOf(h.actions), await boxOf(h.description)];
    expect(Math.abs(a.y - t.y), "action top is not level with the title's first line").toBeLessThanOrEqual(4);
    expect(a.x + a.width, "action runs past the viewport").toBeLessThanOrEqual(PHONE.width);
    expect(a.x, "action is not on the right").toBeGreaterThan(PHONE.width / 2);
    expect(d.y, "description is not below the title").toBeGreaterThanOrEqual(t.y + t.height - 1);
    expect(d.y, "description is not below the action").toBeGreaterThanOrEqual(a.y + a.height - 1);
    expect(d.width, "description is squeezed").toBeGreaterThan(PHONE.width - 80);
  });

  // From sm up the header is the one it always was: description under the title, the action to its right.
  test("at 1440 the description sits under the title and the action beside it, as before", async ({ page }) => {
    await page.setViewportSize(WIDE);
    await gotoConsole(page, "admin");
    await navTo(page, "Policies");
    const h = header(page);
    await expect(h.actions).toBeVisible();
    const [t, a, d] = [await boxOf(h.title), await boxOf(h.actions), await boxOf(h.description)];
    expect(Math.abs(a.y - t.y)).toBeLessThanOrEqual(4);
    expect(Math.abs(d.x - t.x)).toBeLessThanOrEqual(1);
    expect(d.y).toBeGreaterThanOrEqual(t.y + t.height - 1);
    expect(a.x).toBeGreaterThan(d.x + d.width - 1);
  });
});

test.describe("Tier B at 390px", () => {
  // #2891: the count drops whole to one line instead of wrapping between the search box and Refresh.
  test("Workspaces: the list count stays on one line inside the viewport", async ({ page }) => {
    await phoneAt(page, "/admin/workspaces");
    const count = page.getByText(/^\d+ of \d+ workspaces?$/);
    await expect(count).toBeVisible();
    const box = await boxOf(count);
    expect(box.height).toBeLessThan(26);
    expect(box.x + box.width).toBeLessThanOrEqual(PHONE.width);
  });

  // #2876: below the width for three, the tiles stack in one column.
  test("Settings Host card: the tiles stack in one column", async ({ page }) => {
    await phoneAt(page, "/admin/settings");
    await expandCard(page, "Host");
    const tiles = ["image-builder", "recording-store", "internet"].map((id) => page.getByTestId(`host-tile-${id}`));
    for (const t of tiles) await expect(t).toBeVisible();
    const boxes = [await boxOf(tiles[0]), await boxOf(tiles[1]), await boxOf(tiles[2])];
    expect(Math.abs(boxes[0].x - boxes[1].x)).toBeLessThanOrEqual(1);
    expect(Math.abs(boxes[1].x - boxes[2].x)).toBeLessThanOrEqual(1);
    expect(boxes[1].y).toBeGreaterThanOrEqual(boxes[0].y + boxes[0].height - 1);
    expect(boxes[2].y).toBeGreaterThanOrEqual(boxes[1].y + boxes[1].height - 1);
    for (const b of boxes) expect(b.x + b.width).toBeLessThanOrEqual(PHONE.width);
  });
});
