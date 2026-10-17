/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New Run, step 5 (FUG-L369): a repository whose token or key lane create drops reads "Needs setup"
// with the server's sentence under it, not a ready row. The preview is the real server's, with its
// `access` block replaced by the New Run scenario fixture and the repository row set to that state.
import type { Page } from "@playwright/test";
import { test, expect, gotoConsole, navToRoute } from "./fixtures";
import { NR_DOCUMENT } from "../src/app/components/wardyn/copy/new-run/document";
import { NR_REVIEW } from "../src/app/components/wardyn/copy/new-run/review";
import { NR_START } from "../src/app/components/wardyn/copy/new-run/start";
import { NR_TOOLS } from "../src/app/components/wardyn/copy/new-run/tools";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { SCENARIO_HARNESSES, SCENARIO_HARNESS_SETS } from "../src/app/lib/new-run-fixtures/scenarios/harnesses";
import { SCENARIO_PREFLIGHT } from "../src/app/lib/new-run-fixtures/scenarios/preflight";
import { SCENARIO_ACCESS } from "../src/app/lib/new-run-fixtures/scenarios/tools";

const DOCUMENT = "api_version: wardyn/v1\nkind: Run\nintent:\n  shape: application\n  experience: background\n  workload:\n    kind: command\n    command: npm test\n  no_repositories_or_drives: true\n";
const reasons = JSON.parse(readFileSync(join(process.cwd(), "src/app/lib/access-reasons.golden.json"), "utf8")) as { key: string; text: string }[];
const SENTENCE = reasons.find((r) => r.key === "repo_no_credential")!.text;

const press = (page: Page, name: string) => page.getByRole("button", { name, exact: true }).click();

async function openTools(page: Page, dropped: boolean) {
  const access = {
    ...SCENARIO_ACCESS.member,
    rows: SCENARIO_ACCESS.member.rows.map((r) => (r.kind === "repository" && dropped ? { ...r, status: "needs_setup", reason: "repo_no_credential" } : r)),
  };
  await page.route("**/api/v1/runs/policy-preview", async (route) => {
    const response = await route.fetch();
    await route.fulfill({ response, json: { ...(await response.json()), access } });
  });
  await page.route("**/api/v1/runs/preflight", (route) => route.fulfill({ json: SCENARIO_PREFLIGHT.member }));
  let status: Record<string, unknown> | null = null;
  await page.route("**/api/v1/setup/status*", async (route) => {
    status ??= { ...((await (await route.fetch()).json()) as Record<string, unknown>), harnesses: SCENARIO_HARNESSES.full, harness_sets: SCENARIO_HARNESS_SETS.full };
    await route.fulfill({ json: status });
  });
  await gotoConsole(page);
  await navToRoute(page, "/runs/new");
  await press(page, NR_START.DOC_YAML);
  await page.getByRole("textbox", { name: NR_DOCUMENT.EDITOR_ARIA("YAML") }).fill(DOCUMENT);
  await press(page, NR_DOCUMENT.CONTINUE_REVIEW);
  await expect(page.getByRole("heading", { name: NR_REVIEW.TITLE })).toBeVisible();
  await press(page, `${NR_REVIEW.CHANGE} ${NR_REVIEW.GROUP_ACCESS}`);
  await press(page, NR_REVIEW.EDIT_TOOLS_ACCESS);
  await expect(page.getByRole("heading", { name: NR_TOOLS.TITLE })).toBeVisible();
}

test.describe("New Run, step 5: a repository with no credential", () => {
  test("reads Needs setup with the server's sentence under its name", async ({ page }) => {
    await openTools(page, true);
    const row = page.getByText("acme/web", { exact: true }).locator("xpath=ancestor::li[1]");
    await expect(row.getByText(SENTENCE, { exact: true })).toBeVisible();
    await expect(row.getByText(NR_TOOLS.STATUS_NEEDS_SETUP, { exact: true })).toBeVisible();
  });

  test("a repository that has its credential says nothing of the kind", async ({ page }) => {
    await openTools(page, false);
    await expect(page.getByText(SENTENCE, { exact: true })).toHaveCount(0);
  });
});
