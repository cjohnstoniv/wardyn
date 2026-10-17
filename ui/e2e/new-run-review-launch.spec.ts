/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New Run, Review and Launch: the blocker list and the screens its rows link
// to, Check again, the Name dialog, the Launched screen and its reload, and
// the refusals a create can answer with.
//
// Review is reached through the run document path (a complete environment
// sandbox), so this spec does not repeat the Guided walk. The preflight answer
// is stubbed, as the seeded backend runs no barrier runtime.
import type { Page } from "@playwright/test";
import { test, expect, gotoConsole, navToRoute } from "./fixtures";
import { MODEL_ACCESS_BANNER } from "../src/app/components/wardyn/model-access-copy";
import { NR_DOCUMENT } from "../src/app/components/wardyn/copy/new-run/document";
import { NR_LAUNCH } from "../src/app/components/wardyn/copy/new-run/launch";
import { NR_REVIEW } from "../src/app/components/wardyn/copy/new-run/review";
import { NR_START } from "../src/app/components/wardyn/copy/new-run/start";
import { NR_TOOLS } from "../src/app/components/wardyn/copy/new-run/tools";
import { AGENTS } from "../src/app/lib/workspace-providers-copy";
import { SCENARIO_HARNESSES, SCENARIO_HARNESS_SETS } from "../src/app/lib/new-run-fixtures/scenarios/harnesses";
import { SCENARIO_PREFLIGHT } from "../src/app/lib/new-run-fixtures/scenarios/preflight";

const RUN_ID = "b2222222-2222-4222-8222-222222222222";
const CLEAR = SCENARIO_PREFLIGHT.member;
const MISSING = SCENARIO_PREFLIGHT.app;
const SECRET_LABEL = "deploy-key";
const DOCUMENT = "api_version: wardyn/v1\nkind: Run\nintent:\n  shape: environment\n  experience: interactive\n  no_repositories_or_drives: true\n";

const BEDROCK = {
  id: "bedrock-e2e",
  name: "Bedrock (e2e)",
  kind: "bedrock_sso",
  harnesses: ["claude-code"],
  host: "bedrock-runtime.us-east-1.amazonaws.com",
};
const BREACH =
  "this run needs 2 CPU, 4Gi memory, more than quota runs-quota has left (1 CPU, 2Gi memory). Stop a run, or ask your admin to raise the quota.";

const press = (page: Page, name: string) => page.getByRole("button", { name, exact: true }).click();
const launch = (page: Page) => page.getByRole("button", { name: NR_REVIEW.LAUNCH_RUN });
const nameDialog = (page: Page) => page.getByRole("dialog", { name: NR_LAUNCH.TITLE });

/** What the next preflight answers. Flipped by a test between two checks. */
interface Checks {
  preflight: typeof CLEAR | typeof MISSING | { status: number; body: object };
}

async function stub(page: Page, checks: Checks, withBedrock = false): Promise<{ created: string[] }> {
  const seen = { created: [] as string[] };
  await page.route("**/api/v1/runs/preflight", (route) => {
    const next = checks.preflight;
    return "status" in next ? route.fulfill({ status: next.status, json: next.body }) : route.fulfill({ json: next });
  });
  let status: Record<string, unknown> | null = null;
  await page.route("**/api/v1/setup/status*", async (route) => {
    status ??= {
      ...((await (await route.fetch()).json()) as Record<string, unknown>),
      harnesses: SCENARIO_HARNESSES.full,
      harness_sets: SCENARIO_HARNESS_SETS.full,
      ...(withBedrock && {
        llm_ready: true,
        model_providers: [{ ...BEDROCK, default_for: ["claude-code"] }],
        provider_access: [{ provider: BEDROCK.id, state: "not_configured", action: AGENTS.SIGN_IN_AWS }],
      }),
    };
    await route.fulfill({ json: status });
  });
  await page.route(`**/api/v1/runs/${RUN_ID}`, (route) => route.fulfill({ json: { id: RUN_ID, state: "PENDING" } }));
  await page.route("**/api/v1/runs", (route) => {
    if (route.request().method() !== "POST") return route.fallback();
    seen.created.push(route.request().postData() ?? "");
    return route.fulfill({ status: 201, json: { id: RUN_ID, state: "PENDING" } });
  });
  return seen;
}

/** Start, then the document path to Review with a complete draft. */
async function openReview(page: Page) {
  await gotoConsole(page);
  await navToRoute(page, "/runs/new");
  await press(page, NR_START.DOC_YAML);
  await page.getByRole("textbox", { name: NR_DOCUMENT.EDITOR_ARIA("YAML") }).fill(DOCUMENT);
  await press(page, NR_DOCUMENT.CONTINUE_REVIEW);
  await expect(page.getByRole("heading", { name: NR_REVIEW.TITLE })).toBeVisible();
}

async function openNameDialog(page: Page) {
  await expect(launch(page)).toBeEnabled();
  await launch(page).click();
  await expect(nameDialog(page)).toBeVisible();
  await expect(nameDialog(page).getByLabel(NR_LAUNCH.TITLE_LABEL)).toBeFocused();
}

test.describe("New Run, Review: blockers and Check again", () => {
  test("a missing setup item holds Launch, names itself, and links to Tools and access", async ({ page }) => {
    await stub(page, { preflight: MISSING });
    await openReview(page);
    await expect(page.getByText(NR_REVIEW.BLOCKERS_ONE)).toBeVisible();
    await expect(launch(page)).toBeDisabled();
    // The held button is described by the one blocker list.
    await expect(launch(page)).toHaveAttribute("aria-describedby", "nr-blockers");

    await page.locator("#nr-blockers").getByRole("button", { name: SECRET_LABEL }).click();
    await expect(page.getByRole("heading", { name: NR_TOOLS.TITLE })).toBeVisible();
    await expect(page).toHaveURL(/step=tools/);
  });

  test("a preflight refusal is a blocker in the server's own sentence", async ({ page }) => {
    await stub(page, { preflight: { status: 422, body: { error: BREACH, reason: "namespace_quota_exceeded" } } });
    await openReview(page);
    await expect(page.locator("#nr-blockers").getByText(BREACH)).toBeVisible();
    await expect(launch(page)).toBeDisabled();
  });

  test("Check again reads the server again and releases Launch", async ({ page }) => {
    const checks: Checks = { preflight: MISSING };
    await stub(page, checks);
    await openReview(page);
    await expect(launch(page)).toBeDisabled();

    checks.preflight = CLEAR;
    await press(page, NR_REVIEW.CHECK_AGAIN);
    await expect(page.getByText(NR_REVIEW.BLOCKERS_ONE)).toHaveCount(0);
    await expect(launch(page)).toBeEnabled();
    await expect(page.getByText(NR_REVIEW.CHECKED_ANNOUNCE)).toBeAttached();
  });
});

test.describe("New Run, Launch: the Name dialog and the Launched screen", () => {
  test("Name, Launch, Launched; the create carries the title; a reload keeps the Launched screen", async ({ page }) => {
    const seen = await stub(page, { preflight: CLEAR });
    await openReview(page);
    await openNameDialog(page);
    const dialog = nameDialog(page);
    await dialog.getByLabel(NR_LAUNCH.TITLE_LABEL).fill("review and launch");
    await dialog.getByLabel(NR_LAUNCH.DESCRIPTION_LABEL).fill("for the e2e");
    await expect(dialog.getByText(NR_LAUNCH.CHECKED_JUST_NOW)).toBeVisible();
    await dialog.getByRole("button", { name: NR_LAUNCH.LAUNCH, exact: true }).click();

    await expect(page).toHaveURL(new RegExp(`\\?launched=${RUN_ID}$`));
    await expect(page.getByText(NR_LAUNCH.BADGE_STARTING)).toBeVisible();
    await expect(page.getByText(NR_LAUNCH.STARTING_BODY)).toBeVisible();
    expect(seen.created).toHaveLength(1);
    expect(JSON.parse(seen.created[0])).toMatchObject({ mode_contract: "0.9", title: "review and launch", description: "for the e2e" });

    // The reload re-reads the run by its id, not the draft: the screen survives, and no second create is sent.
    await page.reload();
    await expect(page.getByText(NR_LAUNCH.BADGE_STARTING)).toBeVisible();
    expect(seen.created).toHaveLength(1);

    await page.getByRole("link", { name: NR_LAUNCH.VIEW_RUN }).click();
    await expect(page).toHaveURL(new RegExp(`/runs/${RUN_ID}`));
  });

  test("a queued run with under an hour left names the time it gives up, not 'In 1 hour' (FR-0101)", async ({ page }) => {
    await stub(page, { preflight: CLEAR });
    const deadline = new Date(Date.now() + 30 * 60_000).toISOString();
    const queued = new Date(Date.now() - 2 * 60_000).toISOString();
    await page.route(`**/api/v1/runs/${RUN_ID}`, (route) =>
      route.fulfill({ json: { id: RUN_ID, state: "QUEUED", queue: { pool_id: "p-e2e", queued_at: queued, deadline_at: deadline, position: 1, reason: "runner_pool_at_capacity" } } }),
    );
    await gotoConsole(page);
    await page.goto(`/runs/new?launched=${RUN_ID}`);
    const givesUp = page.getByText(NR_LAUNCH.GIVES_UP, { exact: true }).locator("xpath=following-sibling::dd");
    const clock = await page.evaluate((iso) => new Date(iso).toLocaleTimeString(undefined, { hour: "numeric", minute: "2-digit" }), deadline);
    await expect(givesUp).toHaveText(clock);
    await expect(givesUp).not.toContainText(/In 1 hour/);
  });

  test("Start another run opens a blank Start", async ({ page }) => {
    await stub(page, { preflight: CLEAR });
    await openReview(page);
    await openNameDialog(page);
    await nameDialog(page).getByLabel(NR_LAUNCH.TITLE_LABEL).fill("another");
    await nameDialog(page).getByRole("button", { name: NR_LAUNCH.LAUNCH, exact: true }).click();
    await expect(page.getByText(NR_LAUNCH.BADGE_STARTING)).toBeVisible();
    await press(page, NR_LAUNCH.START_ANOTHER);
    await expect(page).toHaveURL(/\/runs\/new$/);
    await expect(page.getByRole("heading", { name: NR_START.TITLE, level: 1 })).toBeVisible();
  });

  test("Cancel closes the dialog and leaves Review as it was", async ({ page }) => {
    const seen = await stub(page, { preflight: CLEAR });
    await openReview(page);
    await openNameDialog(page);
    await press(page, NR_LAUNCH.CANCEL);
    await expect(nameDialog(page)).toHaveCount(0);
    await expect(page.getByRole("heading", { name: NR_REVIEW.TITLE })).toBeVisible();
    expect(seen.created).toHaveLength(0);
  });

  test("a create the check missed is shown inside the dialog, and the typed text stays", async ({ page }) => {
    await stub(page, { preflight: CLEAR });
    await page.route("**/api/v1/runs", (route) =>
      route.request().method() === "POST"
        ? route.fulfill({ status: 422, json: { error: BREACH, reason: "namespace_quota_exceeded" } })
        : route.fallback(),
    );
    await openReview(page);
    await openNameDialog(page);
    const dialog = nameDialog(page);
    await dialog.getByLabel(NR_LAUNCH.TITLE_LABEL).fill("quota refusal");
    await dialog.getByRole("button", { name: NR_LAUNCH.LAUNCH, exact: true }).click();
    await expect(dialog.getByRole("alert")).toContainText(BREACH);
    await expect(dialog.getByLabel(NR_LAUNCH.TITLE_LABEL)).toHaveValue("quota refusal");
    await expect(page).toHaveURL(/step=review/);
  });

  test("a model_credential refusal that names its provider opens that provider's sign-in", async ({ page }) => {
    const refusal = `This run's model provider is ${BEDROCK.name}, and you are not signed in to AWS for it.`;
    const body = { error: refusal, reason: "model_credential", provider: BEDROCK.id };
    // Preflight runs create's checks, so it refuses the same way; that refusal must not hold Launch on Review.
    await stub(page, { preflight: { status: 422, body } }, true);
    await page.route("**/api/v1/runs", (route) =>
      route.request().method() === "POST" ? route.fulfill({ status: 422, json: body }) : route.fallback(),
    );
    await openReview(page);
    await openNameDialog(page);
    await nameDialog(page).getByLabel(NR_LAUNCH.TITLE_LABEL).fill("model access refusal");
    await nameDialog(page).getByRole("button", { name: NR_LAUNCH.LAUNCH, exact: true }).click();
    await expect(page.getByRole("heading", { name: MODEL_ACCESS_BANNER.DIALOG_TITLE })).toBeVisible();
    await expect(page.getByTestId("harness-login-pane")).toBeVisible();

    // Escape closes the sign-in; nothing launched and the page never left.
    await page.keyboard.press("Escape");
    await expect(page.getByTestId("harness-login-pane")).toHaveCount(0);
    await expect(page).toHaveURL(/step=review/);
    // The Name dialog under the door is still open with what was typed, so Launch can be pressed again.
    await expect(nameDialog(page).getByLabel(NR_LAUNCH.TITLE_LABEL)).toHaveValue("model access refusal");
  });

  test("a refusal that names no provider opens no sign-in", async ({ page }) => {
    const refusal = "this run's policy does not allow it";
    await stub(page, { preflight: CLEAR }, true);
    await page.route("**/api/v1/runs", (route) =>
      route.request().method() === "POST" ? route.fulfill({ status: 422, json: { error: refusal } }) : route.fallback(),
    );
    await openReview(page);
    await openNameDialog(page);
    await nameDialog(page).getByLabel(NR_LAUNCH.TITLE_LABEL).fill("plain refusal");
    await nameDialog(page).getByRole("button", { name: NR_LAUNCH.LAUNCH, exact: true }).click();
    await expect(nameDialog(page).getByRole("alert")).toContainText(refusal);
    await expect(page.getByTestId("harness-login-pane")).toHaveCount(0);
    await expect(page.getByRole("heading", { name: MODEL_ACCESS_BANNER.DIALOG_TITLE })).toHaveCount(0);
  });
});
