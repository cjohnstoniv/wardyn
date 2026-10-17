/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Templates and what feeds them: the Templates screen (filters, rows, Use,
// Rename, Delete and the reasons a row cannot be changed), Save as template and
// Save as policy from Review, the Start-from pickers, and Run again.
//
// The catalogue is stubbed at its routes: the specs name the audiences, the
// writability and the refusals they need, and the seeded backend holds none of
// them. Review is reached through the run document path.
import type { Page } from "@playwright/test";
import { test, expect, gotoConsole, navToRoute } from "./fixtures";
import { NR_DOCUMENT } from "../src/app/components/wardyn/copy/new-run/document";
import { NR_LAUNCH } from "../src/app/components/wardyn/copy/new-run/launch";
import { NR_REVIEW } from "../src/app/components/wardyn/copy/new-run/review";
import { NR_SAVE } from "../src/app/components/wardyn/copy/new-run/save";
import { NR_SHAPE } from "../src/app/components/wardyn/copy/new-run/shape";
import { NR_SHARED } from "../src/app/components/wardyn/copy/new-run/shared";
import { NR_START } from "../src/app/components/wardyn/copy/new-run/start";
import { NR_TEMPLATES as T } from "../src/app/components/wardyn/copy/new-run/templates";
import { SCENARIO_PREFLIGHT } from "../src/app/lib/new-run-fixtures/scenarios/preflight";
import { TEMPLATE_REFUSAL } from "../src/app/lib/template-refusals";
import type { Template, TemplateList, TemplatePublish, TemplateSummary } from "../src/app/lib/types/templates";
import { aheadByHours } from "../src/app/lib/test-clock";

const ME_ONLY: TemplatePublish = { org: false, groups: [], groups_unverified: false };
const PLATFORM = "g-platform";
const intent = { shape: "environment", experience: "interactive", no_repositories_or_drives: true };

// Relative to the run of the test: a literal date ages into a different "saved … ago".
const SAVED_AT = aheadByHours(-48);

const summary = (over: Partial<TemplateSummary> & Pick<TemplateSummary, "id" | "name">): TemplateSummary => ({
  scope: "person", kind: "template", revision: 1, coverage: "full", includes: ["runner"], fields: [], needs_setup: [],
  writable: true, updated_at: SAVED_AT, ...over,
});
const NIGHTLY = summary({ id: "a0000000-0000-4000-8000-000000000001", name: "Nightly tests", revision: 2, includes: ["runner", "tools_image"] });
const WEEKLY = summary({ id: "a0000000-0000-4000-8000-000000000002", name: "Weekly report" });
const PACKAGES = summary({ id: "a0000000-0000-4000-8000-000000000003", name: "Package access", kind: "policy", includes: ["egress"] });
const PLATFORM_BASELINE = summary({
  id: "a0000000-0000-4000-8000-000000000004", name: "Platform baseline", scope: "group", group_id: PLATFORM, group_label: "Platform",
  coverage: "partial", needs_setup: ["inline_policy.model_provider"], writable: false, includes: ["tools_image"],
});
const ORG_BASELINE = summary({
  id: "a0000000-0000-4000-8000-000000000005", name: "Organisation baseline", scope: "org", kind: "policy", coverage: "partial",
  writable: false, includes: ["access_rules"],
});
const CATALOGUE = [NIGHTLY, WEEKLY, PACKAGES, PLATFORM_BASELINE, ORG_BASELINE];

const press = (page: Page, name: string) => page.getByRole("button", { name, exact: true }).click();

/** The full revision a summary stands for, as the seed read and the rename read answer it. */
const revisionOf = (s: TemplateSummary, needs: { field: string }[] = []): Template => ({
  id: s.id, scope: s.scope, group_id: s.group_id, kind: s.kind, name: s.name, revision: s.revision, coverage: s.coverage, writable: s.writable,
  created_at: s.updated_at, updated_at: s.updated_at,
  document: { api_version: "wardyn/v1", kind: "RunTemplate", name: s.name, intent, needs_setup: needs },
});

interface Catalogue {
  rows: TemplateSummary[];
  publish: TemplatePublish;
}

/** GET /templates, plus the revision, rename and delete routes of every row it lists. Those writes are recorded. */
async function stubCatalogue(page: Page, state: Catalogue): Promise<{ put: unknown[]; deleted: string[] }> {
  const seen = { put: [] as unknown[], deleted: [] as string[] };
  await page.route("**/api/v1/templates", (route) => {
    if (route.request().method() === "POST") return route.fallback();
    const list: TemplateList = { templates: state.rows, publish: state.publish };
    return route.fulfill({ json: list });
  });
  for (const row of CATALOGUE) {
    const needs = row.needs_setup.map((field) => ({ field }));
    await page.route(`**/api/v1/templates/${row.id}/revisions/*`, (route) => route.fulfill({ json: revisionOf(row, needs) }));
    await page.route(`**/api/v1/templates/${row.id}`, (route) => {
      const method = route.request().method();
      if (method === "PUT") {
        seen.put.push(route.request().postDataJSON());
        const { name } = route.request().postDataJSON() as { name: string };
        state.rows = state.rows.map((r) => (r.id === row.id ? { ...r, name, revision: r.revision + 1 } : r));
        return route.fulfill({ json: revisionOf({ ...row, name }) });
      }
      if (method === "DELETE") {
        seen.deleted.push(row.id);
        state.rows = state.rows.filter((r) => r.id !== row.id);
        return route.fulfill({ status: 204, body: "" });
      }
      return route.fulfill({ json: revisionOf(row, needs) });
    });
  }
  return seen;
}

async function openTemplates(page: Page) {
  await gotoConsole(page);
  await navToRoute(page, "/templates");
  await expect(page.getByRole("heading", { name: T.TITLE, level: 1 })).toBeVisible();
}

const list = (page: Page) => page.getByRole("region", { name: T.LIST_ARIA });
const rowOf = (page: Page, name: string) => list(page).getByRole("listitem").filter({ has: page.getByText(name, { exact: true }) });
const names = (page: Page) => list(page).getByRole("listitem").locator("strong");

test.describe("Templates: filters and rows", () => {
  test("All, Only me, Groups and Organisation; the kinds; and the name search narrow the list", async ({ page }) => {
    await stubCatalogue(page, { rows: CATALOGUE, publish: ME_ONLY });
    await openTemplates(page);
    await expect(names(page)).toHaveText(CATALOGUE.map((r) => r.name));

    const scope = page.getByRole("group", { name: T.SCOPE_FILTER_ARIA });
    await scope.getByRole("button", { name: T.SCOPE_ME, exact: true }).click();
    await expect(names(page)).toHaveText([NIGHTLY.name, WEEKLY.name, PACKAGES.name]);
    await scope.getByRole("button", { name: T.SCOPE_GROUPS, exact: true }).click();
    await expect(names(page)).toHaveText([PLATFORM_BASELINE.name]);
    await scope.getByRole("button", { name: T.SCOPE_ORG, exact: true }).click();
    await expect(names(page)).toHaveText([ORG_BASELINE.name]);
    await scope.getByRole("button", { name: T.SCOPE_ALL, exact: true }).click();

    const kind = page.getByRole("group", { name: T.KIND_FILTER_ARIA });
    await kind.getByRole("button", { name: T.KIND_POLICIES, exact: true }).click();
    await expect(names(page)).toHaveText([PACKAGES.name, ORG_BASELINE.name]);
    await kind.getByRole("button", { name: T.KIND_TEMPLATES, exact: true }).click();
    await expect(names(page)).toHaveText([NIGHTLY.name, WEEKLY.name, PLATFORM_BASELINE.name]);
    await kind.getByRole("button", { name: T.KIND_ALL, exact: true }).click();

    await page.getByRole("searchbox", { name: T.SEARCH_LABEL }).fill("BASELINE");
    await expect(names(page)).toHaveText([PLATFORM_BASELINE.name, ORG_BASELINE.name]);
  });

  test("each row shows its kind, what it needs, who it is for, what it includes and its revision", async ({ page }) => {
    await stubCatalogue(page, { rows: CATALOGUE, publish: ME_ONLY });
    await openTemplates(page);

    const nightly = rowOf(page, NIGHTLY.name);
    await expect(nightly.getByText(T.SCOPE_ME, { exact: true })).toBeVisible();
    await expect(nightly.getByText(T.PART_WHERE, { exact: true })).toBeVisible();
    await expect(nightly.getByText(T.PART_TOOLS, { exact: true })).toBeVisible();
    await expect(nightly.getByText(T.REVISION(2))).toBeVisible();
    await expect(nightly.getByText(T.POLICY_BADGE, { exact: true })).toHaveCount(0);

    await expect(rowOf(page, PACKAGES.name).getByText(T.POLICY_BADGE, { exact: true })).toBeVisible();
    await expect(rowOf(page, PACKAGES.name).getByText(T.PART_ACCESS, { exact: true })).toBeVisible();

    const platform = rowOf(page, PLATFORM_BASELINE.name);
    await expect(platform.getByText(T.NEEDS_SETUP("Model provider"))).toBeVisible();
    await expect(platform.getByText(T.GROUP_LABEL("Platform"), { exact: true })).toBeVisible();
    await expect(platform.getByText(T.TEMPLATE_MANAGERS, { exact: true })).toBeVisible();
    await expect(rowOf(page, ORG_BASELINE.name).getByText(T.ORG_ADMINS, { exact: true })).toBeVisible();
  });

  test("an empty catalogue and a search with no match say so and offer the way on", async ({ page }) => {
    const state: Catalogue = { rows: [], publish: ME_ONLY };
    await stubCatalogue(page, state);
    await openTemplates(page);
    await expect(page.getByText(T.EMPTY_TITLE)).toBeVisible();
    await expect(page.getByText(T.EMPTY_BODY)).toBeVisible();
    await page.getByRole("link", { name: T.EMPTY_ACTION }).click();
    await expect(page.getByRole("heading", { name: NR_START.TITLE, level: 1 })).toBeVisible();

    await page.unrouteAll({ behavior: "ignoreErrors" });
    await stubCatalogue(page, { rows: CATALOGUE, publish: ME_ONLY });
    await openTemplates(page);
    await page.getByRole("searchbox", { name: T.SEARCH_LABEL }).fill("no such template");
    await expect(page.getByText(T.NO_MATCH_TITLE)).toBeVisible();
    await expect(page.getByText(T.NO_MATCH_BODY)).toBeVisible();
    await press(page, T.CLEAR_FILTERS);
    await expect(names(page)).toHaveText(CATALOGUE.map((r) => r.name));
    await expect(page.getByRole("searchbox", { name: T.SEARCH_LABEL })).toHaveValue("");
  });
});

test.describe("Templates: Use", () => {
  test("Use opens New Run at Review on that revision, with what it left to set up as blockers", async ({ page }) => {
    await page.route("**/api/v1/runs/preflight", (route) => route.fulfill({ json: SCENARIO_PREFLIGHT.member }));
    await stubCatalogue(page, { rows: CATALOGUE, publish: ME_ONLY });
    await openTemplates(page);
    await rowOf(page, PLATFORM_BASELINE.name).getByRole("link", { name: T.USE }).click();

    await expect(page).toHaveURL(new RegExp(`/runs/new\\?template=${PLATFORM_BASELINE.id}&rev=1$`));
    await expect(page.getByRole("heading", { name: NR_REVIEW.TITLE })).toBeVisible();
    await expect(page.getByText(NR_REVIEW.BLOCKERS_ONE)).toBeVisible();
    // What a template leaves to set up has no step of its own, so the blocker is a line, not a link.
    await expect(page.locator("#nr-blockers").getByText(T.NEEDS_SETUP("Model provider"))).toBeVisible();
    await expect(page.getByRole("button", { name: NR_REVIEW.LAUNCH_RUN })).toBeDisabled();
  });

  test("a template with nothing left to set up opens Review with no blocker and an open Launch", async ({ page }) => {
    await page.route("**/api/v1/runs/preflight", (route) => route.fulfill({ json: SCENARIO_PREFLIGHT.member }));
    await stubCatalogue(page, { rows: CATALOGUE, publish: ME_ONLY });
    await openTemplates(page);
    await rowOf(page, NIGHTLY.name).getByRole("link", { name: T.USE }).click();
    await expect(page.getByRole("heading", { name: NR_REVIEW.TITLE })).toBeVisible();
    await expect(page.getByText(NR_REVIEW.BLOCKERS_ONE)).toHaveCount(0);
    await expect(page.getByRole("button", { name: NR_REVIEW.LAUNCH_RUN })).toBeEnabled();
  });
});

test.describe("Templates: Rename, Delete and the reasons a row cannot change", () => {
  test("Rename refuses a name another template of the same audience holds, and accepts one only another audience holds", async ({ page }) => {
    const seen = await stubCatalogue(page, { rows: CATALOGUE, publish: ME_ONLY });
    await openTemplates(page);
    await rowOf(page, WEEKLY.name).getByRole("button", { name: T.RENAME, exact: true }).click();
    const dialog = page.getByRole("dialog", { name: T.RENAME_TITLE });
    const name = dialog.getByLabel(T.NAME_LABEL);
    await expect(name).toBeFocused();

    await name.fill("");
    await press(page, T.RENAME_CONFIRM);
    await expect(dialog.getByRole("alert")).toHaveText(T.NAME_EMPTY);

    await name.fill("nightly TESTS");
    await press(page, T.RENAME_CONFIRM);
    await expect(dialog.getByRole("alert")).toHaveText(TEMPLATE_REFUSAL.NAME_TAKEN());
    expect(seen.put).toHaveLength(0);

    // "Platform baseline" belongs to a group's audience, not this one: no clash.
    await name.fill(PLATFORM_BASELINE.name);
    await press(page, T.RENAME_CONFIRM);
    await expect(dialog).toHaveCount(0);
    await expect(page.getByText(T.RENAMED(PLATFORM_BASELINE.name))).toBeVisible();
    expect(seen.put).toHaveLength(1);
    expect(seen.put[0]).toMatchObject({ scope: "person", name: PLATFORM_BASELINE.name, expected_revision: 1 });
  });

  test("a rename the server refuses keeps the dialog and the typed name, in the server's sentence", async ({ page }) => {
    await stubCatalogue(page, { rows: CATALOGUE, publish: ME_ONLY });
    await page.route(`**/api/v1/templates/${WEEKLY.id}`, (route) =>
      route.request().method() === "PUT"
        ? route.fulfill({ status: 409, json: { error: TEMPLATE_REFUSAL.NAME_TAKEN(), reason: "template_name_taken" } })
        : route.fallback(),
    );
    await openTemplates(page);
    await rowOf(page, WEEKLY.name).getByRole("button", { name: T.RENAME, exact: true }).click();
    const dialog = page.getByRole("dialog", { name: T.RENAME_TITLE });
    await dialog.getByLabel(T.NAME_LABEL).fill("Monthly report");
    await press(page, T.RENAME_CONFIRM);
    await expect(dialog.getByRole("alert")).toHaveText(TEMPLATE_REFUSAL.NAME_TAKEN());
    await expect(dialog.getByLabel(T.NAME_LABEL)).toHaveValue("Monthly report");
  });

  test("Delete asks in the page, says who it affects, and removes the row only on confirm", async ({ page }) => {
    const seen = await stubCatalogue(page, { rows: CATALOGUE, publish: ME_ONLY });
    await openTemplates(page);
    await rowOf(page, WEEKLY.name).getByRole("button", { name: T.DELETE, exact: true }).click();
    const confirm = page.getByRole("alertdialog", { name: T.DELETE_TITLE(WEEKLY.name) });
    await expect(confirm).toContainText(T.DELETE_BODY_ME);

    await confirm.getByRole("button", { name: T.CANCEL }).click();
    await expect(confirm).toHaveCount(0);
    expect(seen.deleted).toEqual([]);
    await expect(rowOf(page, WEEKLY.name)).toHaveCount(1);

    await rowOf(page, WEEKLY.name).getByRole("button", { name: T.DELETE, exact: true }).click();
    await confirm.getByRole("button", { name: T.DELETE_CONFIRM }).click();
    await expect(page.getByText(T.DELETED(WEEKLY.name))).toBeVisible();
    await expect(rowOf(page, WEEKLY.name)).toHaveCount(0);
    expect(seen.deleted).toEqual([WEEKLY.id]);
  });

  test("a delete the server refuses is shown inside the confirmation, which stays open", async ({ page }) => {
    const refusal = "That template is in use by a schedule.";
    await stubCatalogue(page, { rows: CATALOGUE, publish: ME_ONLY });
    await page.route(`**/api/v1/templates/${WEEKLY.id}`, (route) =>
      route.request().method() === "DELETE" ? route.fulfill({ status: 409, json: { error: refusal } }) : route.fallback(),
    );
    await openTemplates(page);
    await rowOf(page, WEEKLY.name).getByRole("button", { name: T.DELETE, exact: true }).click();
    const confirm = page.getByRole("alertdialog", { name: T.DELETE_TITLE(WEEKLY.name) });
    await confirm.getByRole("button", { name: T.DELETE_CONFIRM }).click();
    await expect(confirm).toContainText(refusal);
    // The open confirmation hides the page behind it from role queries, so the row is read by its element.
    await expect(page.locator("li").filter({ has: page.getByText(WEEKLY.name, { exact: true }) })).toHaveCount(1);
  });

  test("Rename and Delete stay disabled on a row the person cannot write, with who can change it", async ({ page }) => {
    await stubCatalogue(page, { rows: CATALOGUE, publish: ME_ONLY });
    await openTemplates(page);
    for (const [row, why] of [
      [ORG_BASELINE, T.WHY_ORG],
      [PLATFORM_BASELINE, T.WHY_GROUP(T.GROUP_LABEL("Platform"))],
    ] as const) {
      const item = rowOf(page, row.name);
      await expect(item.getByText(why)).toBeVisible();
      for (const action of [T.RENAME, T.DELETE]) {
        const button = item.getByRole("button", { name: action, exact: true });
        await expect(button).toHaveAttribute("aria-disabled", "true");
        await button.click({ force: true });
      }
      await expect(page.getByRole("dialog")).toHaveCount(0);
      await expect(page.getByRole("alertdialog")).toHaveCount(0);
    }
  });
});

const DOCUMENT = [
  "api_version: wardyn/v1", "kind: Run", "intent:", "  shape: environment", "  experience: interactive",
  "  no_repositories_or_drives: true", "  inline_policy:", "    allowed_domains:", "      - pypi.org", "",
].join("\n");

async function openReview(page: Page) {
  await page.route("**/api/v1/runs/preflight", (route) => route.fulfill({ json: SCENARIO_PREFLIGHT.member }));
  await gotoConsole(page);
  await navToRoute(page, "/runs/new");
  await press(page, NR_START.DOC_YAML);
  await page.getByRole("textbox", { name: NR_DOCUMENT.EDITOR_ARIA("YAML") }).fill(DOCUMENT);
  await press(page, NR_DOCUMENT.CONTINUE_REVIEW);
  await expect(page.getByRole("heading", { name: NR_REVIEW.TITLE })).toBeVisible();
}

test.describe("Save as template and Save as policy", () => {
  test("only the audiences the person can write are offered, and never the organisation from here", async ({ page }) => {
    const publish: TemplatePublish = { org: true, groups: [PLATFORM], group_labels: { [PLATFORM]: "Platform" }, groups_unverified: false };
    await stubCatalogue(page, { rows: CATALOGUE, publish });
    await openReview(page);
    await press(page, NR_REVIEW.SAVE_TEMPLATE);
    const dialog = page.getByRole("dialog", { name: NR_SAVE.TITLE_TEMPLATE });
    await expect(dialog.getByLabel(NR_SAVE.NAME_LABEL)).toBeFocused();
    await expect(dialog.getByLabel(NR_SAVE.AVAILABLE_LABEL).locator("option")).toHaveText([NR_SAVE.SCOPE_ME, NR_SAVE.SCOPE_GROUP("Platform")]);
    await expect(dialog.getByText(NR_SAVE.HELP_MANAGER)).toBeVisible();
  });

  test("a group list that cannot be verified offers none and says to sign in again", async ({ page }) => {
    const publish: TemplatePublish = { org: false, groups: [PLATFORM], groups_unverified: true };
    await stubCatalogue(page, { rows: CATALOGUE, publish });
    await openReview(page);
    await press(page, NR_REVIEW.SAVE_TEMPLATE);
    const dialog = page.getByRole("dialog", { name: NR_SAVE.TITLE_TEMPLATE });
    await expect(dialog.getByLabel(NR_SAVE.AVAILABLE_LABEL).locator("option")).toHaveText([NR_SAVE.SCOPE_ME]);
    await expect(dialog.getByText(TEMPLATE_REFUSAL.GROUP_UNVERIFIED())).toBeVisible();
  });

  test("a name already taken for the audience is shown under Name and the typed text stays", async ({ page }) => {
    await stubCatalogue(page, { rows: CATALOGUE, publish: ME_ONLY });
    await openReview(page);
    await page.route("**/api/v1/templates", (route) =>
      route.request().method() === "POST"
        ? route.fulfill({ status: 409, json: { error: TEMPLATE_REFUSAL.NAME_TAKEN(), reason: "template_name_taken", diagnostics: [] } })
        : route.fallback(),
    );
    await press(page, NR_REVIEW.SAVE_TEMPLATE);
    const dialog = page.getByRole("dialog", { name: NR_SAVE.TITLE_TEMPLATE });
    const name = dialog.getByLabel(NR_SAVE.NAME_LABEL);
    await name.fill("Nightly tests");
    await press(page, NR_SAVE.CONFIRM_TEMPLATE);
    await expect(dialog.locator("#save-as-name-error")).toHaveText(TEMPLATE_REFUSAL.NAME_TAKEN());
    await expect(name).toHaveAttribute("aria-invalid", "true");
    await expect(name).toHaveValue("Nightly tests");

    await name.fill("Nightly tests 2");
    await expect(dialog.locator("#save-as-name-error")).toHaveCount(0);
  });

  test("Save as template writes the draft without a title and leaves the draft as it was", async ({ page }) => {
    await stubCatalogue(page, { rows: CATALOGUE, publish: ME_ONLY });
    await openReview(page);
    let body: { kind?: string; scope?: string; name?: string; document?: { intent?: Record<string, unknown> } } = {};
    await page.route("**/api/v1/templates", (route) => {
      if (route.request().method() !== "POST") return route.fallback();
      body = route.request().postDataJSON();
      return route.fulfill({ status: 201, json: revisionOf(summary({ id: "a0000000-0000-4000-8000-0000000000aa", name: "From review" })) });
    });
    await press(page, NR_REVIEW.SAVE_TEMPLATE);
    const dialog = page.getByRole("dialog", { name: NR_SAVE.TITLE_TEMPLATE });
    await dialog.getByLabel(NR_SAVE.NAME_LABEL).fill("From review");
    await press(page, NR_SAVE.CONFIRM_TEMPLATE);
    await expect(dialog).toHaveCount(0);
    await expect(page.getByText(NR_SAVE.SAVED(NR_SHAPE.TEMPLATE_LABEL))).toBeAttached();
    expect(body).toMatchObject({ kind: "template", scope: "person", name: "From review" });
    expect(body.document?.intent).toMatchObject(intent);
    expect(JSON.stringify(body)).not.toContain("title");
    // Saving changed nothing about the run: still on Review, still ready to launch.
    await expect(page.getByRole("heading", { name: NR_REVIEW.TITLE })).toBeVisible();
    await expect(page.getByRole("button", { name: NR_REVIEW.LAUNCH_RUN })).toBeEnabled();
  });

  test("Save as policy keeps only network, credential and access rules, as a policy", async ({ page }) => {
    await stubCatalogue(page, { rows: CATALOGUE, publish: ME_ONLY });
    await openReview(page);
    let body: { kind?: string; scope?: string; name?: string; document?: { intent?: Record<string, unknown> } } = {};
    await page.route("**/api/v1/templates", (route) => {
      if (route.request().method() !== "POST") return route.fallback();
      body = route.request().postDataJSON();
      return route.fulfill({ status: 201, json: revisionOf(summary({ id: "a0000000-0000-4000-8000-0000000000bb", name: "From review", kind: "policy" })) });
    });
    await press(page, NR_REVIEW.SAVE_POLICY);
    const dialog = page.getByRole("dialog", { name: NR_SAVE.TITLE_POLICY });
    await dialog.getByLabel(NR_SAVE.NAME_LABEL).fill("Python packages");
    await press(page, NR_SAVE.CONFIRM_POLICY);
    await expect(dialog).toHaveCount(0);
    await expect(page.getByText(NR_SAVE.SAVED(T.POLICY_BADGE))).toBeAttached();
    expect(body).toMatchObject({ kind: "policy", scope: "person", name: "Python packages" });
    expect(Object.keys(body.document?.intent ?? {})).toEqual(["inline_policy"]);
    expect(JSON.stringify(body)).not.toContain("title");
  });
});

test.describe("Start from a saved template and Run again", () => {
  const RUN_ID = "b0000000-0000-4000-8000-000000000001";
  const PREVIOUS = { run_id: RUN_ID, title: "Fix the flaky retry", shape: "environment", created_at: aheadByHours(-24) };

  async function startFrom(page: Page, source: string) {
    await gotoConsole(page);
    await navToRoute(page, "/runs/new");
    await page.getByRole("button", { name: NR_START.GUIDED_FOOT }).click();
    await press(page, source);
  }

  test("Run again lists the person's earlier runs and opens Review with that run's settings", async ({ page }) => {
    const preflights: string[] = [];
    await page.route("**/api/v1/runs/preflight", (route) => {
      preflights.push(route.request().postData() ?? "");
      return route.fulfill({ json: SCENARIO_PREFLIGHT.member });
    });
    await page.route("**/api/v1/me/launch-intents*", (route) => route.fulfill({ json: { intents: [PREVIOUS] } }));
    await page.route(`**/api/v1/runs/${RUN_ID}/launch-intent`, (route) =>
      route.fulfill({ json: { run_id: RUN_ID, created_at: PREVIOUS.created_at, document: { api_version: "wardyn/v1", kind: "Run", intent } } }),
    );
    await startFrom(page, NR_SHAPE.SOURCE_AGAIN);
    await expect(page.getByText(NR_SHAPE.AGAIN_NOTE)).toBeVisible();
    await page.getByRole("combobox", { name: NR_SHAPE.PREVIOUS_LABEL }).click();
    await page.getByRole("option", { name: new RegExp(PREVIOUS.title) }).click();
    // The picker only shows a row; Continue opens Review on it.
    await press(page, NR_SHARED.CONTINUE);

    await expect(page).toHaveURL(new RegExp(`again=${RUN_ID}&step=review$`));
    await expect(page.getByRole("heading", { name: NR_REVIEW.TITLE })).toBeVisible();
    await expect(page.getByText(NR_REVIEW.SETUP_ENVIRONMENT)).toBeVisible();
    await expect.poll(() => preflights.length).toBeGreaterThan(0);
    expect(JSON.parse(preflights.at(-1)!)).toMatchObject({ ...intent, mode_contract: "0.9" });

    // The earlier title is not carried: the Name dialog opens empty.
    await expect(page.getByRole("button", { name: NR_REVIEW.LAUNCH_RUN })).toBeEnabled();
    await press(page, NR_REVIEW.LAUNCH_RUN);
    await expect(page.getByRole("dialog", { name: NR_LAUNCH.TITLE }).getByLabel(NR_LAUNCH.TITLE_LABEL)).toHaveValue("");
  });

  test("with no earlier run the picker says so", async ({ page }) => {
    await page.route("**/api/v1/me/launch-intents*", (route) => route.fulfill({ json: { intents: [] } }));
    await startFrom(page, NR_SHAPE.SOURCE_AGAIN);
    await expect(page.getByText(NR_SHAPE.EMPTY_AGAIN)).toBeVisible();
  });

  test("Saved template lists templates and policies together and opens Review on the one chosen", async ({ page }) => {
    await page.route("**/api/v1/runs/preflight", (route) => route.fulfill({ json: SCENARIO_PREFLIGHT.member }));
    await stubCatalogue(page, { rows: CATALOGUE, publish: ME_ONLY });
    await startFrom(page, NR_SHAPE.SOURCE_TEMPLATE);
    await page.getByRole("combobox", { name: NR_SHAPE.TEMPLATE_LABEL }).click();
    // Both kinds are plain options: the policy is listed beside the template.
    await expect(page.getByRole("option", { name: new RegExp(PACKAGES.name) })).toBeVisible();
    await page.getByRole("option", { name: new RegExp(NIGHTLY.name) }).click();
    await press(page, NR_SHARED.CONTINUE);
    await expect(page).toHaveURL(new RegExp(`template=${NIGHTLY.id}&rev=2&step=review$`));
    await expect(page.getByRole("heading", { name: NR_REVIEW.TITLE })).toBeVisible();
  });

  test("with no saved template the picker says so", async ({ page }) => {
    await stubCatalogue(page, { rows: [], publish: ME_ONLY });
    await startFrom(page, NR_SHAPE.SOURCE_TEMPLATE);
    await expect(page.getByText(NR_SHAPE.EMPTY_TEMPLATES)).toBeVisible();
  });
});

test.describe("Templates: Publish to organisation (FR-0120)", () => {
  test("asks first with what a shared template shows, and publishes only on confirm, with a toast", async ({ page }) => {
    await stubCatalogue(page, { rows: CATALOGUE, publish: { org: true, groups: [], groups_unverified: false } });
    const copies: unknown[] = [];
    await page.route("**/api/v1/templates/*/copy", (route) => {
      copies.push(route.request().postDataJSON());
      return route.fulfill({ status: 201, json: revisionOf(NIGHTLY) });
    });
    await gotoConsole(page, "admin");
    await navToRoute(page, "/admin/templates");
    await expect(page.getByRole("heading", { name: T.TITLE, level: 1 })).toBeVisible();

    await rowOf(page, NIGHTLY.name).getByRole("button", { name: T.PUBLISH }).click();
    const dialog = page.getByRole("alertdialog", { name: T.PUBLISH_TITLE(NIGHTLY.name) });
    await expect(dialog.getByText(NR_SAVE.SHARED_DISCLOSURE)).toBeVisible();
    expect(copies).toHaveLength(0);

    await dialog.getByRole("button", { name: T.CANCEL }).click();
    await expect(dialog).toHaveCount(0);
    expect(copies).toHaveLength(0);

    await rowOf(page, NIGHTLY.name).getByRole("button", { name: T.PUBLISH }).click();
    await page.getByRole("alertdialog").getByRole("button", { name: T.PUBLISH }).click();
    await expect(page.getByText(T.PUBLISHED(NIGHTLY.name))).toBeVisible();
    expect(copies).toEqual([{ scope: "org" }]);
  });
});

