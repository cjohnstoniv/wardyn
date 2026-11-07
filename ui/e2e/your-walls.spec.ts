/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Your walls (T4): a person saves walls on Your account, and the next run they set up shows them, on step 5
// and on Review. The walls are saved through the card against the real server, as a signed-in member (the
// admin token is no person and has no walls); the policy preview is the real server's, so the host the
// walls take out is the server's own answer. The preflight is stubbed, as the other New Run specs stub it
// (the seeded backend runs no barrier runtime); it carries the walls step's own warning sentence.
import type { Page } from "@playwright/test";
import { randomUUID } from "node:crypto";
import { test, expect, asRealMember, asRealOpenEgressMember, gotoConsole, navToRoute, ADMIN_TOKEN, MEMBER_TOKEN, OPEN_EGRESS_PRINCIPAL } from "./fixtures";
import { NR_DOCUMENT } from "../src/app/components/wardyn/copy/new-run/document";
import { NR_REVIEW } from "../src/app/components/wardyn/copy/new-run/review";
import { NR_START } from "../src/app/components/wardyn/copy/new-run/start";
import { NR_TOOLS } from "../src/app/components/wardyn/copy/new-run/tools";
import { NR_WALLS, YOUR_WALLS } from "../src/app/components/wardyn/copy/personal-walls";
import { SCENARIO_HARNESSES, SCENARIO_HARNESS_SETS } from "../src/app/lib/new-run-fixtures/scenarios/harnesses";
import { SCENARIO_PREFLIGHT } from "../src/app/lib/new-run-fixtures/scenarios/preflight";

const DOCUMENT = "api_version: wardyn/v1\nkind: Run\nintent:\n  shape: application\n  experience: background\n  workload:\n    kind: command\n    command: npm test\n  no_repositories_or_drives: true\n";
const ALLOWED = "api.example.com";
const LEFT_OUT = "pypi.org";

const press = (page: Page, name: string) => page.getByRole("button", { name, exact: true }).click();

async function stub(page: Page) {
  await page.route("**/api/v1/runs/preflight", (route) =>
    route.fulfill({ json: { ...SCENARIO_PREFLIGHT.member, warnings: ["your walls narrowed allowed_domains"] } }),
  );
  let status: Record<string, unknown> | null = null;
  await page.route("**/api/v1/setup/status*", async (route) => {
    status ??= {
      ...((await (await route.fetch()).json()) as Record<string, unknown>),
      harnesses: SCENARIO_HARNESSES.full,
      harness_sets: SCENARIO_HARNESS_SETS.full,
    };
    await route.fulfill({ json: status });
  });
}

// The member's walls live on the shared backend, and the policy preview applies them: walls left behind by
// one test (a failed one never reaches its own clean-up) would otherwise narrow the next test's preview.
async function clearWalls(page: Page) {
  const res = await page.request.delete("/api/v1/me/walls", { headers: { Authorization: `Bearer ${MEMBER_TOKEN}` } });
  expect(res.status(), await res.text()).toBeLessThan(300);
}

test.describe("Your walls", () => {
  test.beforeEach(({ page }) => clearWalls(page));
  test.afterEach(({ page }) => clearWalls(page));

  test("save walls on Your account, and New Run says which host they leave out", async ({ page }) => {
    await stub(page);
    await asRealMember(page);
    await gotoConsole(page);

    // Only these sites: one listed site. Choosing it also picks Block it for a site that is not listed.
    await navToRoute(page, "/account");
    await page.getByRole("button", { name: new RegExp(`^${YOUR_WALLS.TITLE}( |$)`) }).click();
    await page.getByRole("radio", { name: YOUR_WALLS.SITES_ONLY }).click();
    await expect(page.getByRole("button", { name: YOUR_WALLS.UNLISTED_BLOCK })).toHaveAttribute("aria-pressed", "true");
    await page.getByRole("textbox", { name: YOUR_WALLS.SITES_ONLY }).fill(ALLOWED);
    await page.getByRole("textbox", { name: YOUR_WALLS.SITES_ONLY }).press("Enter");
    await press(page, YOUR_WALLS.SAVE);
    await expect(page.getByRole("status").filter({ hasText: YOUR_WALLS.SAVED })).toBeVisible();

    // A reload reads the saved walls back.
    await page.reload();
    await page.getByRole("button", { name: new RegExp(`^${YOUR_WALLS.TITLE}( |$)`) }).click();
    await expect(page.getByRole("radio", { name: YOUR_WALLS.SITES_ONLY })).toBeChecked();
    await expect(page.getByText(ALLOWED, { exact: true })).toBeVisible();

    // A new run that asks for a host outside the list: step 5 reads it Not allowed, and names the walls.
    await navToRoute(page, "/runs/new");
    await press(page, NR_START.DOC_YAML);
    await page.getByRole("textbox", { name: NR_DOCUMENT.EDITOR_ARIA("YAML") }).fill(DOCUMENT);
    await press(page, NR_DOCUMENT.CONTINUE_REVIEW);
    await expect(page.getByRole("heading", { name: NR_REVIEW.TITLE })).toBeVisible();
    await press(page, `${NR_REVIEW.CHANGE} ${NR_REVIEW.GROUP_ACCESS}`);
    await press(page, NR_REVIEW.EDIT_TOOLS_ACCESS);
    await expect(page.getByRole("heading", { name: NR_TOOLS.TITLE })).toBeVisible();
    await press(page, NR_TOOLS.HOST_CARD_BUTTON);
    const dialog = page.getByRole("dialog", { name: NR_TOOLS.HOST_CARD_TITLE });
    await dialog.getByLabel(NR_TOOLS.HOST_LABEL).fill(LEFT_OUT);
    await dialog.getByRole("button", { name: NR_TOOLS.ADD_TO_RUN }).click();
    const row = page.getByRole("listitem").filter({ hasText: LEFT_OUT });
    await expect(row.getByText(NR_TOOLS.NET_NOT_ALLOWED)).toBeVisible();
    await expect(page.getByTestId("walls-leave-out")).toHaveText(NR_WALLS.LEAVE_OUT(LEFT_OUT));

    // Review names the left-out host in its Access group, in the console's words.
    await press(page, NR_TOOLS.REVIEW_RUN);
    await expect(page.getByTestId("walls-leave-out")).toHaveText(NR_WALLS.LEAVE_OUT(LEFT_OUT));

    // Take the walls away again so a later spec in this worker starts clean.
    await navToRoute(page, "/account");
    await page.getByRole("button", { name: new RegExp(`^${YOUR_WALLS.TITLE}( |$)`) }).click();
    await press(page, YOUR_WALLS.REMOVE_ALL);
    // Remove all asks first; only its confirm removes the walls.
    const confirm = page.getByRole("alertdialog", { name: YOUR_WALLS.REMOVE_ALL_TITLE });
    await confirm.getByRole("button", { name: YOUR_WALLS.REMOVE_ALL, exact: true }).click();
    await expect(confirm).toHaveCount(0);
    await page.reload();
    await page.getByRole("button", { name: new RegExp(`^${YOUR_WALLS.TITLE}( |$)`) }).click();
    await expect(page.getByRole("radio", { name: YOUR_WALLS.SITES_ANY })).toBeChecked();
  });

  test("Only the sites listed: the switch sends allow_all_egress false and Review keeps the run closed", async ({ page }) => {
    await stub(page);
    const bodies: string[] = [];
    page.on("request", (req) => {
      if (req.method() === "POST" && new URL(req.url()).pathname === "/api/v1/runs/policy-preview") bodies.push(req.postData() ?? "");
    });
    // The seeded member's ceiling never allows all egress, so its real preview would never offer the switch.
    // This person's ceiling does, and the preview is the server's own answer throughout.
    const auth = { Authorization: `Bearer ${ADMIN_TOKEN}` };
    const unit = await page.request.post("/api/v1/governance/units", {
      headers: auth,
      data: {
        name: `open-egress-${randomUUID().slice(0, 8)}`,
        ceiling: {
          allow_all_egress: true,
          allowed_domains: [],
          denied_domains: [],
          first_use_approval: "deny_with_review",
          min_confinement_class: "CC1",
          auto_stop_after_sec: 3600,
          eligible_grants: [],
          tool_rules: [{ tool: "*", effect: "allow" }],
        },
        limits: {},
      },
    });
    expect(unit.status(), await unit.text()).toBe(201);
    const unitID = (await unit.json()).unit.id;
    try {
      const assigned = await page.request.post("/api/v1/governance/memberships", {
        headers: auth,
        data: { subject_type: "user", subject: OPEN_EGRESS_PRINCIPAL, unit_id: unitID, priority: 0 },
      });
      expect(assigned.ok(), await assigned.text()).toBeTruthy();

      await asRealOpenEgressMember(page);
      await gotoConsole(page);
      await navToRoute(page, "/runs/new");
      await press(page, NR_START.DOC_YAML);
      await page.getByRole("textbox", { name: NR_DOCUMENT.EDITOR_ARIA("YAML") }).fill(DOCUMENT);
      await press(page, NR_DOCUMENT.CONTINUE_REVIEW);
      await press(page, `${NR_REVIEW.CHANGE} ${NR_REVIEW.GROUP_ACCESS}`);
      await press(page, NR_REVIEW.EDIT_TOOLS_ACCESS);
      // The switch is offered only while the preview says this run allows all egress.
      const only = page.getByRole("switch", { name: NR_WALLS.ONLY_LISTED });
      await expect(only).toBeVisible();
      await only.click();
      await expect(only).toHaveAttribute("aria-checked", "true");
      await expect.poll(() => bodies.some((b) => JSON.parse(b)?.overrides?.egress?.allow_all_egress === false)).toBe(true);
      await expect(page.getByText(NR_TOOLS.NET_POLICY_ALLOW_ALL)).toHaveCount(0);
    } finally {
      const snap = await (await page.request.get("/api/v1/governance", { headers: auth })).json();
      for (const a of snap.memberships ?? []) {
        if (a.unit_id === unitID) await page.request.delete(`/api/v1/governance/memberships/${a.id}`, { headers: auth });
      }
      await page.request.delete(`/api/v1/governance/units/${unitID}`, { headers: auth });
    }
  });

  test("a tool rule, a path never pushed and an idle stop are set on Your account, saved and read back exactly", async ({ page }) => {
    await asRealMember(page);
    await gotoConsole(page);
    await navToRoute(page, "/account");
    await page.getByRole("button", { name: new RegExp(`^${YOUR_WALLS.TITLE}( |$)`) }).click();

    await press(page, YOUR_WALLS.TOOLS_ADD);
    await page.getByRole("textbox", { name: YOUR_WALLS.TOOLS_TOOL_N(1) }).fill("WebFetch");
    await press(page, YOUR_WALLS.PATHS_ADD);
    await page.getByRole("textbox", { name: YOUR_WALLS.PATHS_PATH_N(1) }).fill(".github/workflows/");
    await page.getByRole("radio", { name: YOUR_WALLS.IDLE_AFTER }).click();
    await page.getByRole("textbox", { name: YOUR_WALLS.IDLE_NUMBER }).fill("90");
    await page.getByRole("combobox", { name: YOUR_WALLS.IDLE_UNIT_LABEL }).selectOption("seconds");
    await press(page, YOUR_WALLS.SAVE);
    await expect(page.getByRole("status").filter({ hasText: YOUR_WALLS.SAVED })).toBeVisible();

    // The server holds exactly what was set: the idle stop in seconds, not rounded to minutes.
    const res = await page.request.get("/api/v1/me/walls", { headers: { Authorization: `Bearer ${MEMBER_TOKEN}` } });
    expect(((await res.json()) as { spec: unknown }).spec).toEqual({
      tool_rules: [{ tool: "WebFetch", effect: "hold" }],
      push_rules: { deny_paths: [".github/workflows/"] },
      auto_stop_after_sec: 90,
    });

    // A reload shows the same three controls, the idle stop still 90 seconds.
    await page.reload();
    await page.getByRole("button", { name: new RegExp(`^${YOUR_WALLS.TITLE}( |$)`) }).click();
    await expect(page.getByRole("textbox", { name: YOUR_WALLS.TOOLS_TOOL_N(1) })).toHaveValue("WebFetch");
    await expect(page.getByRole("textbox", { name: YOUR_WALLS.PATHS_PATH_N(1) })).toHaveValue(".github/workflows/");
    await expect(page.getByRole("textbox", { name: YOUR_WALLS.IDLE_NUMBER })).toHaveValue("90");
    await expect(page.getByRole("combobox", { name: YOUR_WALLS.IDLE_UNIT_LABEL })).toHaveValue("seconds");
  });
});
