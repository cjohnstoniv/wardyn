/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New Run, Review (FUG-L749 N3): a host the ceiling dropped from the run's allowed list is said in one
// plain sentence, not the server's operator words. The preflight is stubbed from the scenario fixtures
// with the server's own two drop warnings; every other warning stays as it came.
import type { Page } from "@playwright/test";
import { test, expect, gotoConsole, navToRoute } from "./fixtures";
import { NR_DOCUMENT } from "../src/app/components/wardyn/copy/new-run/document";
import { NR_REVIEW } from "../src/app/components/wardyn/copy/new-run/review";
import { NR_START } from "../src/app/components/wardyn/copy/new-run/start";
import { SCENARIO_HARNESSES, SCENARIO_HARNESS_SETS } from "../src/app/lib/new-run-fixtures/scenarios/harnesses";
import { SCENARIO_POOLS } from "../src/app/lib/new-run-fixtures/scenarios/pools";
import { SCENARIO_PREFLIGHT } from "../src/app/lib/new-run-fixtures/scenarios/preflight";
import { DROPPED_HOSTS } from "../src/app/components/screens/new-run-flow/review/drop-warnings";

const DOCUMENT = "api_version: wardyn/v1\nkind: Run\nintent:\n  shape: environment\n  experience: interactive\n  no_repositories_or_drives: true\n";
const SERVER_WARNINGS = [
  "dropped 2 egress domain(s) not in operator allowlist: pastebin.com,files.example.test",
  'dropped private address entry "10.0.0.5": not in operator allowlist, and allow_all_egress does not cover a private address',
  'confinement raised from "CC1" to operator minimum "CC2"',
];

async function openReviewWith(page: Page, warnings: string[]) {
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
  await page.route("**/api/v1/runs/preflight", (route) => route.fulfill({ json: { ...SCENARIO_PREFLIGHT.member, warnings } }));
  await gotoConsole(page);
  await navToRoute(page, "/runs/new");
  await page.getByRole("button", { name: NR_START.DOC_YAML, exact: true }).click();
  await page.getByRole("textbox", { name: NR_DOCUMENT.EDITOR_ARIA("YAML") }).fill(DOCUMENT);
  await page.getByRole("button", { name: NR_DOCUMENT.CONTINUE_REVIEW, exact: true }).click();
  await expect(page.getByRole("heading", { name: NR_REVIEW.TITLE })).toBeVisible();
}

test.describe("New Run, Review: a host the ceiling dropped", () => {
  test("is said in the ceiling's words, and another server warning is left as it came", async ({ page }) => {
    await openReviewWith(page, SERVER_WARNINGS);
    await expect(page.getByText(DROPPED_HOSTS.MANY("pastebin.com and files.example.test"), { exact: true })).toBeVisible();
    await expect(page.getByText(DROPPED_HOSTS.ONE("10.0.0.5"), { exact: true })).toBeVisible();
    await expect(page.getByText(SERVER_WARNINGS[2], { exact: true })).toBeVisible();
    await expect(page.getByText("not in operator allowlist")).toHaveCount(0);
  });
});
