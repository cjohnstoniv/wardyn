/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { FORM_ACTIONS } from "../src/app/components/wardyn/copy/form-actions";
import { randomBytes } from "node:crypto";
import type { Page } from "@playwright/test";
import { test, expect, asRealMember, consoleAPI, gotoConsole, layoutSettled, navTo } from "./fixtures";
import { POOLS, POOL_DRAWER, POOL_FORM, RUNNER_V6 } from "../src/app/components/wardyn/copy/runner-pools";
import { MY_RUNNER, RUNNERS } from "../src/app/components/wardyn/copy/runners";
import { entry, member, pool, remoteRunner } from "../src/app/components/screens/runners/pools/pool-test-fixtures";
import { aheadByHours } from "../src/app/lib/test-clock";

// Admin -> Runners against the real server (mock M-POOLS v6). The harness has no container runtime, so
// it is set up with no remote-hosted runner and its first boot makes no organization pool, and runners
// are off: this pins the page's two tabs and what each says when the real routes answer with nothing
// (the remote-hosted runners, the pool list, the organization default). What the tabs show with runners
// and pools in them is pinned in the component suites beside each screen.
test("Runners has one tab strip, and the tab is in the address", async ({ page }) => {
  await gotoConsole(page, "admin");
  await page.goto("/admin/runners");
  const strip = page.getByRole("tablist", { name: RUNNER_V6.TABS_LABEL });
  await expect(strip.getByRole("tab", { name: RUNNER_V6.TAB_RUNNERS })).toHaveAttribute("aria-selected", "true");
  // #3060: one sentence on both tabs, the rest closed under How runners work, and the tabs never move.
  const lede = page.getByTestId("page-header-description");
  await expect(lede).toHaveText(RUNNERS.PAGE_LEDE);
  const how = page.getByTestId("how-runners");
  await expect(how).not.toHaveAttribute("open");
  await expect(page.getByText(RUNNERS.LEAD)).toBeHidden();
  await layoutSettled(strip);
  const stripY = (await strip.boundingBox())!.y;
  // No remote-hosted runner is set up, and people's runners are off.
  await expect(page.getByRole("heading", { name: RUNNERS.OFF_TITLE })).toBeVisible();
  await expect(page.getByRole("list", { name: RUNNER_V6.LIST_LABEL })).toHaveCount(0);

  await strip.getByRole("tab", { name: RUNNER_V6.TAB_POOLS }).click();
  await expect(page).toHaveURL(/\/admin\/runners\?tab=pools$/);
  await expect(strip.getByRole("tab", { name: RUNNER_V6.TAB_POOLS })).toHaveAttribute("aria-selected", "true");
  await expect(lede).toHaveText(RUNNERS.PAGE_LEDE);
  await layoutSettled(strip);
  expect((await strip.boundingBox())!.y).toBe(stripY);
  await how.getByText(RUNNERS.HOW_TITLE).click();
  await expect(how.getByText(RUNNER_V6.TAB_PURPOSE, { exact: false })).toBeVisible();
  await expect(how.getByText(POOLS.LEAD, { exact: false })).toBeVisible();

  await strip.getByRole("tab", { name: RUNNER_V6.TAB_RUNNERS }).click();
  await expect(page).toHaveURL(/\/admin\/runners$/);
  await expect(page.getByRole("heading", { name: RUNNERS.OFF_TITLE })).toBeVisible();
});

test("the Pools tab says how an organization pool is made and that there are none yet", async ({ page }) => {
  await gotoConsole(page, "admin");
  await page.goto("/admin/runners?tab=pools");
  await expect(page.getByRole("heading", { name: POOLS.ORG_TITLE, exact: true })).toBeVisible();
  const empty = page.getByRole("group", { name: POOLS.ORG_EMPTY_TITLE });
  await expect(empty).toContainText(POOLS.ORG_CLI);
  await expect(empty.getByText(POOLS.ORG_CLI_COMMAND)).toBeVisible();
  await expect(empty.getByRole("button", { name: POOLS.ORG_CLI_COPY })).toBeVisible();
  await expect(empty).toContainText(POOLS.ORG_EMPTY_THEN);
  await expect(page.getByText(POOLS.ORG_LEDE)).toHaveCount(0);
  await expect(page.getByText(POOLS.ORG_CLI_EDIT)).toHaveCount(0);
  await expect(page.getByRole("heading", { name: POOLS.USER_TITLE, exact: true })).toBeVisible();
  await expect(page.getByRole("main").getByRole("link", { name: POOLS.GOVERNANCE_LINK })).toHaveAttribute("href", /\/admin\/governance$/);
  await expect(page.getByRole("group", { name: POOLS.USER_EMPTY_TITLE })).toContainText(POOLS.USER_EMPTY_BODY);
  // Nothing here is the server refusing: no error, no "unavailable".
  await expect(page.getByText(POOLS.UNAVAILABLE_TITLE)).toHaveCount(0);
  await expect(page.getByText(POOLS.ERROR_TITLE)).toHaveCount(0);
});

// A person's own user pools through the real routes. A pool needs no runner to exist, so the harness's lack of a
// container runtime does not matter: these are the form's create and edit, the server's answers, and what the
// server holds afterwards.
interface PoolRow { id: string; name: string; scope: string }
const mine = async (page: Page): Promise<PoolRow[]> => {
  const res = await consoleAPI(page, "GET", "/api/v1/me/runner-pools");
  expect(res.status, res.text).toBe(200);
  return (JSON.parse(res.text) as { pools: PoolRow[] }).pools.filter((p) => p.scope === "user");
};
const poolNamed = async (page: Page, name: string) => (await mine(page)).find((p) => p.name === name);
const uniqueName = (stem: string) => `${stem} ${randomBytes(3).toString("hex")}`;
const lifetimeUnit = (page: Page, caption: string) =>
  page.getByRole("combobox", { name: `${POOL_DRAWER.BACKGROUND}, ${POOL_FORM.LIFETIME}, ${caption}, ${POOL_FORM.UNIT}` });
// The unit is the shared Select: open it, then choose the option by its word.
const pickUnit = async (page: Page, caption: string, unit: string) => {
  await lifetimeUnit(page, caption).click();
  await page.getByRole("option", { name: unit, exact: true }).click();
};

// #3062: the strip is the shared underline Tabs, so an arrow key moves selection and the address follows.
test("ArrowRight on the Runners tab moves to Pools and the address follows", async ({ page }) => {
  await gotoConsole(page, "admin");
  await page.goto("/admin/runners");
  const strip = page.getByRole("tablist", { name: RUNNER_V6.TABS_LABEL });
  const runners = strip.getByRole("tab", { name: RUNNER_V6.TAB_RUNNERS });
  await runners.focus();
  await page.keyboard.press("ArrowRight");
  await expect(strip.getByRole("tab", { name: RUNNER_V6.TAB_POOLS })).toHaveAttribute("aria-selected", "true");
  await expect(strip.getByRole("tab", { name: RUNNER_V6.TAB_POOLS })).toBeFocused();
  await expect(page).toHaveURL(/\/admin\/runners\?tab=pools$/);
});

test.describe("user pools against the real server", () => {
  test("a user pool is created, then edited, through the form (FUG-L409)", async ({ page }) => {
    const first = uniqueName("Desk");
    const second = uniqueName("Laptop");
    await asRealMember(page);
    await page.goto("/account/pools/new");
    await expect(page.getByRole("heading", { name: POOL_FORM.TITLE_NEW })).toBeVisible();
    // The shared form bar counts what changed since the form opened.
    await expect(page.getByTestId("form-actions-unsaved")).toHaveText("");
    await page.locator("#f-name").fill(first);
    await expect(page.getByTestId("form-actions-unsaved")).toHaveText(FORM_ACTIONS.UNSAVED(1));
    await page.getByRole("button", { name: POOL_FORM.CREATE }).click();
    await expect(page.getByText(POOL_FORM.CREATED(first))).toBeVisible();
    const created = await poolNamed(page, first);
    expect(created).toBeTruthy();

    await page.goto(`/account/pools/${created!.id}/edit`);
    await expect(page.getByRole("heading", { name: POOL_FORM.TITLE_EDIT(first) })).toBeVisible();
    await page.locator("#f-name").fill(second);
    await page.getByRole("button", { name: POOL_FORM.SAVE }).click();
    await expect(page.getByText(POOL_FORM.SAVED(second))).toBeVisible();
    expect((await poolNamed(page, second))?.id).toBe(created!.id);
    expect(await poolNamed(page, first)).toBeUndefined();
  });

  test("a duration that is not a whole minute shows in seconds, exact, and saves unchanged (FUG-L452, FR-0151)", async ({ page }) => {
    const name = uniqueName("Odd");
    await asRealMember(page);
    const made = await consoleAPI(page, "POST", "/api/v1/me/runner-pools", {
      name,
      limits: { barriers: ["CC1"], background: { lifetime: { default_sec: 90, max_sec: 150 } } },
    });
    expect(made.status, made.text).toBe(201);
    const id = (JSON.parse(made.text) as PoolRow).id;

    await page.goto(`/account/pools/${id}/edit`);
    await expect(page.getByRole("heading", { name: POOL_FORM.TITLE_EDIT(name) })).toBeVisible();
    await expect(page.locator("#f-bg-lifeDn")).toHaveValue("90");
    await expect(lifetimeUnit(page, POOL_FORM.DEFAULT)).toHaveText("seconds");
    await expect(page.locator("#f-bg-lifeMn")).toHaveValue("150");
    await expect(lifetimeUnit(page, POOL_FORM.MAXIMUM)).toHaveText("seconds");

    // Renaming alone must not round the durations to the minute.
    await page.locator("#f-name").fill(`${name} b`);
    await page.getByRole("button", { name: POOL_FORM.SAVE }).click();
    await expect(page.getByText(POOL_FORM.SAVED(`${name} b`))).toBeVisible();
    const kept = JSON.parse((await consoleAPI(page, "GET", `/api/v1/runner-pools/${id}`)).text) as {
      limits: { background: { lifetime: { default_sec: number; max_sec: number } } };
    };
    expect(kept.limits.background.lifetime).toMatchObject({ default_sec: 90, max_sec: 150 });
  });

  test("a Maximum below the Default is refused at the Maximum (FR-0117)", async ({ page }) => {
    await asRealMember(page);
    await page.goto("/account/pools/new");
    await page.locator("#f-name").fill(uniqueName("Bounds"));
    await page.locator("#f-bg-lifeDn").fill("3");
    await pickUnit(page, POOL_FORM.DEFAULT, "hours");
    await page.locator("#f-bg-lifeMn").fill("2");
    await pickUnit(page, POOL_FORM.MAXIMUM, "hours");
    await page.getByRole("button", { name: POOL_FORM.CREATE }).click();
    await expect(page.locator("#f-bg-lifeMn")).toHaveAttribute("aria-invalid", "true");
    await expect(page.locator("#f-bg-lifeDn")).not.toHaveAttribute("aria-invalid", "true");
    await expect(page).toHaveURL(/\/account\/pools\/new$/);
  });

  test("the qualifier is said once per fieldset, not under every resource field (#3063)", async ({ page }) => {
    await asRealMember(page);
    await page.goto("/account/pools/new");
    await expect(page.getByRole("heading", { name: POOL_FORM.TITLE_NEW })).toBeVisible();
    for (const rt of ["bg", "it"]) {
      await expect(page.getByTestId(`f-${rt}-qualifier`)).toHaveText(`${POOL_FORM.QUALIFIER} ${POOL_FORM.RUNNERS_NOT_CHOSEN}`);
    }
    await expect(page.getByText(POOL_FORM.NARROWER)).toHaveCount(1);
  });

  test("when Reload cannot re-read the pool, the form says so in its own words and keeps Reload (FR-0116)", async ({ page }) => {
    const name = uniqueName("Stale");
    await asRealMember(page);
    const made = await consoleAPI(page, "POST", "/api/v1/me/runner-pools", { name, limits: { barriers: ["CC1"], background: { lifetime: { default_sec: 3600, max_sec: 7200 } } } });
    expect(made.status, made.text).toBe(201);
    const id = (JSON.parse(made.text) as PoolRow).id;

    await page.goto(`/account/pools/${id}/edit`);
    await expect(page.getByRole("heading", { name: POOL_FORM.TITLE_EDIT(name) })).toBeVisible();
    await page.route(`**/api/v1/me/runner-pools/${id}`, (route) =>
      route.request().method() === "PUT"
        ? route.fulfill({ status: 409, json: { error: `${name} changed while this was open.`, reason: "runner_pool_stale" } })
        : route.fallback(),
    );
    await page.locator("#f-name").fill(`${name} b`);
    await page.getByRole("button", { name: POOL_FORM.SAVE }).click();
    await expect(page.getByRole("button", { name: POOL_FORM.RELOAD })).toBeVisible();

    await page.route(`**/api/v1/runner-pools/${id}`, (route) => route.fulfill({ status: 500, json: { error: "boom" } }));
    await page.getByRole("button", { name: POOL_FORM.RELOAD }).click();
    await expect(page.getByText(POOL_FORM.RELOAD_FAILED)).toBeVisible();
    await expect(page.getByText(POOLS.ERROR_BODY)).toHaveCount(0);
    await expect(page.getByRole("button", { name: POOL_FORM.RELOAD })).toBeVisible();
  });
});

// A remote-hosted runner and an organization pool that holds it. The harness has no remote-hosted runner, so
// the routes the two screens read are answered here; what is proved is what the screens do with the answers.
test.describe("Admin Runners with a remote-hosted runner", () => {
  const RUNNER = remoteRunner({ id: "rr-eu", name: "eu-cluster", revision: 3 });
  const POOL = pool({ id: "p-ci", name: "CI jobs" });

  async function stubRunners(page: Page, current: () => ReturnType<typeof remoteRunner>) {
    await page.route(/\/api\/v1\/remote-runners(\?.*)?$/, (route) => route.fulfill({ json: [current()] }));
    await page.route(/\/api\/v1\/runners(\?.*)?$/, (route) => route.fulfill({ json: [] }));
    await page.route(/\/api\/v1\/runner-pools(\?.*)?$/, (route) => route.fulfill({ json: { pools: [entry(POOL)] } }));
    await page.route(`**/api/v1/runner-pools/${POOL.id}`, (route) => route.fulfill({ json: POOL }));
    await page.route(`**/api/v1/runner-pools/${POOL.id}/members`, (route) => route.fulfill({ json: [member(current())] }));
  }

  test("Back from a runner opened out of a pool returns to that pool (FR-0122)", async ({ page }) => {
    await stubRunners(page, () => RUNNER);
    await gotoConsole(page, "admin");
    await page.goto(`/admin/runners?tab=pools&pool=${POOL.id}`);
    const sheet = page.getByRole("dialog");
    await expect(sheet).toContainText(POOL.name);
    await sheet.getByRole("button", { name: RUNNER.name, exact: true }).click();
    await expect(page).not.toHaveURL(/pool=/);
    await expect(page.getByRole("dialog").getByRole("button", { name: "Edit limits" })).toBeVisible();

    await page.goBack();
    await expect(page).toHaveURL(new RegExp(`pool=${POOL.id}`));
    await expect(page.getByRole("dialog")).toContainText(POOL.name);
  });

  test("the runner's people policy names the security tier that sets it", async ({ page }) => {
    await stubRunners(page, () => RUNNER);
    await gotoConsole(page, "admin");
    await page.goto("/admin/runners");
    await page.getByRole("button", { name: RUNNER.name, exact: true }).click();
    const section = page.getByRole("dialog").locator("[data-available]");
    await expect(section).toContainText("Security admin or admin");
    await expect(section.getByRole("checkbox")).toBeEnabled();
  });

  test("a stale save offers Reload, which re-reads the runner and saves on its new revision (FUG-L453)", async ({ page }) => {
    let live = RUNNER;
    await stubRunners(page, () => live);
    const puts: { revision: number }[] = [];
    await page.route(`**/api/v1/remote-runners/${RUNNER.id}`, (route) => {
      if (route.request().method() !== "PUT") return route.fallback();
      puts.push(route.request().postDataJSON());
      if (puts.length === 1) {
        live = { ...RUNNER, revision: 4 };
        return route.fulfill({ status: 409, json: { error: `${RUNNER.name} changed while this was open.`, reason: "runner_pool_stale" } });
      }
      return route.fulfill({ json: live });
    });
    await gotoConsole(page, "admin");
    await page.goto("/admin/runners");
    await page.getByRole("button", { name: RUNNER.name, exact: true }).click();
    await page.getByRole("dialog").getByRole("button", { name: "Edit limits" }).click();
    const dialog = page.getByRole("dialog", { name: `Limits for ${RUNNER.name}` });
    await dialog.locator("#lm-rn").fill("5");
    await dialog.getByRole("button", { name: "Save" }).click();
    await expect(dialog.getByText(`${RUNNER.name} changed while this was open.`, { exact: false })).toBeVisible();

    await dialog.getByRole("button", { name: POOL_FORM.RELOAD }).click();
    await expect(dialog.getByText(`${RUNNER.name} changed while this was open.`, { exact: false })).toHaveCount(0);
    await dialog.locator("#lm-rn").fill("6");
    await dialog.getByRole("button", { name: "Save" }).click();
    await expect.poll(() => puts.length).toBe(2);
    expect(puts[1].revision).toBe(4);
  });
});

// DR-0210: claiming a runner into a new pool and making it the default. The person's runner list, the claim and
// the pool-member and default writes are answered here (the harness has no runner daemon); the pool itself is
// made on the real server, so the claim dialog's pool step runs against it.
test("a default that fails after the runner joined the pool does not say the runner is not in the pool (DR-0210)", async ({ page }) => {
  const poolName = uniqueName("Desk");
  const FULL = "SHA256:3Jd1d8XqTHEWHOLEFINGERPRINTVALUEk2Q9";
  const none = { runs: 0, cpu_millis: 0, memory_mib: 0, disk_mib: 0 };
  const waiting = {
    id: "r-mbp", name: "mbp-ben", state: "unclaimed", online: false, key_fingerprint: "SHA256:3Jd1d8…k2Q9", key_fingerprint_abbreviated: true,
    version: "0.9.0", created_at: aheadByHours(-0.03), claim_expires_at: aheadByHours(23), minted_by: "e2e", kind: "self_hosted", pools: [],
    usage: { online: false, in_use: none, available: none, disk_capped: true, reported: { cpu_millis: 8000, memory_mib: 16384, disk_mib: 512000 } },
  };
  await page.route(/\/api\/v1\/me\/runners(\?.*)?$/, (route) => route.fulfill({ json: [waiting] }));
  await page.route("**/api/v1/me/runners/r-mbp/claim", (route) => route.fulfill({ json: { runner_id: "r-mbp", fingerprint: FULL, state: "claimed", org_url_sha256: "" } }));
  await page.route("**/api/v1/me/runner-pools/*/runners/r-mbp", (route) => route.fulfill({ json: {} }));
  await page.route("**/api/v1/me/runner-pool-defaults", (route) =>
    route.request().method() === "PUT" ? route.fulfill({ status: 500, json: { error: "Defaults could not be saved." } }) : route.fulfill({ json: {} }),
  );
  await asRealMember(page);
  await page.goto("/account");
  await page.getByRole("button", { name: /^My runner/ }).click();
  await page.getByRole("button", { name: "Claim", exact: true }).click();
  const dialog = page.getByRole("dialog");
  await dialog.getByRole("textbox", { name: /Fingerprint from your computer/ }).fill(FULL);
  await dialog.getByRole("combobox", { name: /Put it in a pool/ }).fill(poolName);
  await dialog.getByRole("checkbox", { name: "Make it my default pool" }).click();
  await dialog.getByRole("button", { name: "Claim runner" }).click();

  await expect(page.getByText(MY_RUNNER.CLAIM_DEFAULT_FAILED("mbp-ben", poolName, "Defaults could not be saved."))).toBeVisible();
  await expect(page.getByText(MY_RUNNER.CLAIM_POOL_FAILED("mbp-ben", poolName, "Defaults could not be saved."))).toHaveCount(0);
  expect(await poolNamed(page, poolName)).toBeTruthy();
});

// DR-0147: My pools says who can see the pools and how many a person may keep. The e2e server keeps runners off,
// which hides the My runner card, so the person's runner list is answered here (empty: no computer registered yet).
test("Your account: My pools says administrators can see the pools and the cap is 10", async ({ page }) => {
  await page.route("**/api/v1/me/runners*", (route) => (route.request().method() === "GET" ? route.fulfill({ json: [] }) : route.fallback()));
  await gotoConsole(page, "user");
  await navTo(page, "Your account");
  await page.getByRole("button", { name: /^My runner/ }).click();
  await expect(page.getByText("Only you can change these. Administrators can see them.", { exact: true })).toBeVisible();
  await expect(page.getByText("You can have up to 10 pools, for example Laptop and Home server.")).toBeVisible();
});

// Board D2a: a runner that registered but never connected names the service and its log on My runner, with no
// limits set. The e2e server keeps runners off, so the person's runner list is answered here.
test("My runner: a runner that registered but never connected names the service and its log", async ({ page }) => {
  const none = { runs: 0, cpu_millis: 0, memory_mib: 0, disk_mib: 0 };
  const never = {
    id: "r-dev", name: "mbp-dev", state: "claimed", online: false, key_fingerprint: "SHA256:3Jd1d8…k2Q9", key_fingerprint_abbreviated: true,
    version: "0.9.6", created_at: aheadByHours(-1), claimed_at: aheadByHours(-0.5), kind: "self_hosted", pools: [],
    usage: { online: false, in_use: none, available: none, disk_capped: true },
  };
  await page.route("**/api/v1/me/runners*", (route) => (route.request().method() === "GET" ? route.fulfill({ json: [never] }) : route.fallback()));
  await gotoConsole(page, "user");
  await navTo(page, "Your account");
  await page.getByRole("button", { name: /^My runner/ }).click();
  await expect(page.getByText("Never connected", { exact: true })).toBeVisible();
  await expect(page.locator("[data-offline-note]")).toHaveText(
    "This runner registered but has never connected to this server. On that computer, check that wardyn-runnerd runs as a service (wardyn-runnerd setup installs one) and read its log: ~/Library/Logs/wardyn/runnerd.log on macOS, journalctl --user -u wardyn-runnerd on Linux. Behind a proxy, the service needs its own HTTPS_PROXY.",
  );
});
