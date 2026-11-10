/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New Run, Review (A3.1, A3.2): open egress says the listed hosts and that others are allowed, and a virtual machine
// names no container barrier. The preflight is stubbed from the scenario fixtures. Runs on hosted CI only.
import type { Page } from "@playwright/test";
import { test, expect, gotoConsole, navToRoute } from "./fixtures";
import { NR_DOCUMENT } from "../src/app/components/wardyn/copy/new-run/document";
import { NR_REVIEW } from "../src/app/components/wardyn/copy/new-run/review";
import { NR_START } from "../src/app/components/wardyn/copy/new-run/start";
import { SCENARIO_HARNESSES, SCENARIO_HARNESS_SETS } from "../src/app/lib/new-run-fixtures/scenarios/harnesses";
import { SCENARIO_POOLS } from "../src/app/lib/new-run-fixtures/scenarios/pools";
import { SCENARIO_PREFLIGHT } from "../src/app/lib/new-run-fixtures/scenarios/preflight";
import { SCENARIO_ACCESS } from "../src/app/lib/new-run-fixtures/scenarios/tools";
import { NR_MACHINE } from "../src/app/components/wardyn/copy/new-run/machine";


const DOCUMENT = "api_version: wardyn/v1\nkind: Run\nintent:\n  shape: environment\n  experience: interactive\n  machine_kind: linux_vm\n  no_repositories_or_drives: true\n";
const OPEN = { ...SCENARIO_ACCESS.member, unlisted: { mode: "deny_with_review", allow_all: true, decider: "you" } };

async function openReview(page: Page) {
  const pools = SCENARIO_POOLS.admin.pools.map((p) => ({
    ...p,
    run_types: ["background", "interactive"],
    run_types_source: "pool",
    barrier_options: [
      { class: "CC1", available: true, default: true, default_source: "server" },
      { class: "CC2", available: p.barriers?.includes("CC2") ?? false },
      { class: "CC3", available: p.barriers?.includes("CC3") ?? false },
    ],
  }));
  await page.route("**/api/v1/runner-pools*", (route) =>
    route.fulfill({ json: { pools, default: { pool_id: SCENARIO_POOLS.admin.pools[0].id, selection: "organisation_default" } } }),
  );
  let status: Record<string, unknown> | null = null;
  await page.route("**/api/v1/setup/status*", async (route) => {
    status ??= { ...((await (await route.fetch()).json()) as Record<string, unknown>), harnesses: SCENARIO_HARNESSES.full, harness_sets: SCENARIO_HARNESS_SETS.full };
    await route.fulfill({ json: status });
  });
  await page.route("**/api/v1/runs/preflight", (route) => route.fulfill({ json: { ...SCENARIO_PREFLIGHT.member, access: OPEN } }));
  await gotoConsole(page);
  await navToRoute(page, "/runs/new");
  await page.getByRole("button", { name: NR_START.DOC_YAML, exact: true }).click();
  await page.getByRole("textbox", { name: NR_DOCUMENT.EDITOR_ARIA("YAML") }).fill(DOCUMENT);
  await page.getByRole("button", { name: NR_DOCUMENT.CONTINUE_REVIEW, exact: true }).click();
  await expect(page.getByRole("heading", { name: NR_REVIEW.TITLE })).toBeVisible();
}

test.describe("New Run, Review: polish", () => {
  test("open egress says the listed hosts and that others are allowed, in the Access group and the policy card", async ({ page }) => {
    await openReview(page);
    const n = SCENARIO_ACCESS.member.hosts.length;
    const line = n === 1 ? NR_REVIEW.POLICY_HOST_OPEN_ONE : NR_REVIEW.POLICY_HOSTS_OPEN(n);
    await expect(page.getByText(line, { exact: true })).toHaveCount(2);
  });

  test("a virtual machine names The virtual machine as its barrier, and no container barrier", async ({ page }) => {
    await openReview(page);
    await expect(page.getByTestId("where-vm-barrier")).toHaveText(`${NR_MACHINE.BARRIER_LABEL} · ${NR_MACHINE.BARRIER_VM}`);
    await expect(page.locator("[data-policy-row=barrier]").getByText(NR_MACHINE.BARRIER_VM, { exact: true })).toBeVisible();
    await expect(page.locator("[data-policy-row=barrier]").getByText(/Default/)).toHaveCount(0);
  });
});
