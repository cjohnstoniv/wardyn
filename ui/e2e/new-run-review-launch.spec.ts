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
import { NR_RUN } from "../src/app/components/wardyn/copy/new-run/run";
import { NR_START } from "../src/app/components/wardyn/copy/new-run/start";
import { NR_TOOLS } from "../src/app/components/wardyn/copy/new-run/tools";
import { AGENTS } from "../src/app/lib/workspace-providers-copy";
import { SCENARIO_HARNESSES, SCENARIO_HARNESS_SETS } from "../src/app/lib/new-run-fixtures/scenarios/harnesses";
import { SCENARIO_PREFLIGHT } from "../src/app/lib/new-run-fixtures/scenarios/preflight";
import { limitsFor } from "../src/app/lib/new-run-fixtures/scenarios/files";
import { CLAUDE_UNGATED } from "../src/app/lib/new-run-fixtures/scenarios/tools-harness";
import { SCENARIO_ACCESS } from "../src/app/lib/new-run-fixtures/scenarios/tools";
import { NR_SAVE } from "../src/app/components/wardyn/copy/new-run/save";
import { NR_TOOLS as TOOLS_COPY } from "../src/app/components/wardyn/copy/new-run/tools";
import { ENDS_NONE } from "../src/app/components/wardyn/copy/run-lifetime";

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

/** The Review header chip. Each blamed group also renders the same words in an sr-only span, so a page-wide text match is ambiguous. */
function setupChip(page: Page, text: string) {
  return page
    .locator("header")
    .filter({ has: page.getByRole("heading", { name: NR_REVIEW.SETUP_TITLE }) })
    .getByText(text, { exact: true });
}

test.describe("New Run, Review: blockers and Check again", () => {
  test("a missing setup item holds Launch, names itself, and links to Tools and access", async ({ page }) => {
    await stub(page, { preflight: MISSING });
    await openReview(page);
    await expect(page.getByText(NR_REVIEW.BLOCKERS_ONE)).toBeVisible();
    await expect(launch(page)).toBeDisabled();
    // The held button is described by the one blocker list.
    await expect(launch(page)).toHaveAttribute("aria-describedby", "nr-blockers");

    // #3043: a blocker that names something is red: the chip says Needs attention and the card has no Check again.
    await expect(setupChip(page, NR_REVIEW.BADGE_ATTENTION)).toBeVisible();
    await expect(page.getByText(NR_REVIEW.BADGE_CHECK, { exact: true })).toHaveCount(0);
    await expect(page.locator("#nr-blockers").getByRole("button", { name: NR_REVIEW.CHECK_AGAIN })).toHaveCount(0);

    await page.locator("#nr-blockers").getByRole("button", { name: SECRET_LABEL }).click();
    await expect(page.getByRole("heading", { name: NR_TOOLS.TITLE })).toBeVisible();
    await expect(page).toHaveURL(/step=tools/);
  });

  // #3043: the pool-required refusal points at "Where it runs", which says there is no pool instead of going blank.
  test("a pool-required refusal makes Where it runs read 'No pool chosen yet'", async ({ page }) => {
    await stub(page, { preflight: { status: 422, body: { error: NR_RUN.POOL_HELP, reason: "runner_pool_required" } } });
    await openReview(page);
    await expect(page.locator("#nr-blockers").getByText(NR_RUN.POOL_HELP)).toBeVisible();
    await expect(page.locator("[data-review-group=where]").getByText(NR_REVIEW.WHERE_NONE, { exact: true })).toBeVisible();
    await expect(setupChip(page, NR_REVIEW.BADGE_ATTENTION)).toBeVisible();
  });

  test("group titles are sentence-case headings, not a caps eyebrow (#3044)", async ({ page }) => {
    await stub(page, { preflight: CLEAR });
    await openReview(page);
    const title = page.locator("[data-review-group=where]").getByRole("heading", { name: NR_REVIEW.GROUP_WHERE });
    await expect(title).toBeVisible();
    await expect(title).toHaveCSS("text-transform", "none");
  });

  test("a preflight refusal is a blocker in the server's own sentence", async ({ page }) => {
    await stub(page, { preflight: { status: 422, body: { error: BREACH, reason: "namespace_quota_exceeded" } } });
    await openReview(page);
    await expect(page.locator("#nr-blockers").getByText(BREACH)).toBeVisible();
    await expect(launch(page)).toBeDisabled();
  });

  test("a document edited after it was accepted holds Launch with a sentence of its own (FR-0099, doc_invalid)", async ({ page }) => {
    await stub(page, { preflight: CLEAR });
    await openReview(page);
    await expect(launch(page)).toBeEnabled();
    await page.goBack();
    const editor = page.getByRole("textbox", { name: NR_DOCUMENT.EDITOR_ARIA("YAML") });
    await editor.fill(`${DOCUMENT}# edited\n`);
    await page.goForward();
    await expect(page.getByRole("heading", { name: NR_REVIEW.TITLE })).toBeVisible();
    await expect(page.locator("#nr-blockers").getByText(NR_DOCUMENT.DOC_INVALID)).toBeVisible();
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

  test("Cancel while the create is out cannot be followed by a second run (DR-0269)", async ({ page }) => {
    const seen = await stub(page, { preflight: CLEAR });
    let release!: () => void;
    const held = new Promise<void>((resolve) => { release = resolve; });
    const posts: string[] = [];
    await page.route("**/api/v1/runs", async (route) => {
      if (route.request().method() !== "POST") return route.fallback();
      posts.push(route.request().postData() ?? "");
      await held;
      return route.fulfill({ status: 201, json: { id: RUN_ID, state: "PENDING" } });
    });
    await openReview(page);
    await openNameDialog(page);
    await nameDialog(page).getByLabel(NR_LAUNCH.TITLE_LABEL).fill("held create");
    await nameDialog(page).getByRole("button", { name: NR_LAUNCH.LAUNCH, exact: true }).click();
    await expect.poll(() => posts.length).toBe(1);
    await press(page, NR_LAUNCH.CANCEL);
    await expect(nameDialog(page)).toHaveCount(0);

    await launch(page).click();
    await expect(nameDialog(page)).toBeVisible();
    await expect(nameDialog(page).getByRole("button", { name: NR_LAUNCH.LAUNCH, exact: true })).toBeDisabled();
    release();
    await expect(page).toHaveURL(new RegExp(`\\?launched=${RUN_ID}$`));
    expect(posts).toHaveLength(1);
    expect(seen.created).toHaveLength(0);
  });

  test("leaving Review and coming back does not forget the create that is still out (DR-0269)", async ({ page }) => {
    await stub(page, { preflight: CLEAR });
    let release!: () => void;
    const held = new Promise<void>((resolve) => { release = resolve; });
    const posts: string[] = [];
    await page.route("**/api/v1/runs", async (route) => {
      if (route.request().method() !== "POST") return route.fallback();
      posts.push(route.request().postData() ?? "");
      await held;
      return route.fulfill({ status: 201, json: { id: RUN_ID, state: "PENDING" } });
    });
    await openReview(page);
    await openNameDialog(page);
    await nameDialog(page).getByLabel(NR_LAUNCH.TITLE_LABEL).fill("held create");
    await nameDialog(page).getByRole("button", { name: NR_LAUNCH.LAUNCH, exact: true }).click();
    await expect.poll(() => posts.length).toBe(1);
    await press(page, NR_LAUNCH.CANCEL);
    await expect(nameDialog(page)).toHaveCount(0);

    await page.goBack();
    await expect(page.getByRole("heading", { name: NR_REVIEW.TITLE })).toHaveCount(0);
    await press(page, NR_DOCUMENT.CONTINUE_REVIEW);
    await expect(page.getByRole("heading", { name: NR_REVIEW.TITLE })).toBeVisible();
    await launch(page).click();
    await expect(nameDialog(page)).toBeVisible();
    await expect(nameDialog(page).getByRole("button", { name: NR_LAUNCH.LAUNCH, exact: true })).toBeDisabled();
    release();
    await expect(page).toHaveURL(new RegExp(`\\?launched=${RUN_ID}$`));
    expect(posts).toHaveLength(1);
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

/** Review with a document of its own: the intent lines go under `intent:`. */
async function openReviewWith(page: Page, intentLines: string[]) {
  await gotoConsole(page);
  await navToRoute(page, "/runs/new");
  await press(page, NR_START.DOC_YAML);
  await page.getByRole("textbox", { name: NR_DOCUMENT.EDITOR_ARIA("YAML") }).fill(["api_version: wardyn/v1", "kind: Run", "intent:", ...intentLines.map((l) => `  ${l}`), ""].join("\n"));
  await press(page, NR_DOCUMENT.CONTINUE_REVIEW);
  await expect(page.getByRole("heading", { name: NR_REVIEW.TITLE })).toBeVisible();
}

const ENV = ["shape: environment", "experience: interactive", "no_repositories_or_drives: true"];
const SAVED_LIMITS = { ...limitsFor("interactive"), lifetime_sec: { default: { value: 3600 }, cap: { value: 7200 } }, no_end_allowed: true };

test.describe("New Run, Review: what the summaries say the server will do", () => {
  test("a tool default the server resolved to deny reads Block it, not Run automatically (DR-0244)", async ({ page }) => {
    const denied = { ...CLAUDE_UNGATED, default: { ...CLAUDE_UNGATED.default, effective: "deny" } };
    await stub(page, { preflight: { ...CLEAR, access: { ...SCENARIO_ACCESS.member, harnesses: [denied] } } as never });
    await openReviewWith(page, ENV);
    const tools = page.locator("[data-policy-row=tools]");
    await expect(tools).toContainText(TOOLS_COPY.EFFECT_DENY);
    await expect(tools).not.toContainText(NR_REVIEW.POLICY_AUTOMATIC);
  });

  test("harnesses with different tool defaults are each named in the Tools and pushes row (DR-0244)", async ({ page }) => {
    const held = { ...CLAUDE_UNGATED, harness: "claude-code", default: { ...CLAUDE_UNGATED.default, effective: "hold" } };
    const denied = { ...CLAUDE_UNGATED, harness: "codex-cli", default: { ...CLAUDE_UNGATED.default, effective: "deny" } };
    await stub(page, { preflight: { ...CLEAR, access: { ...SCENARIO_ACCESS.member, harnesses: [held, denied] } } as never });
    await openReviewWith(page, ENV);
    await expect(page.locator("[data-policy-row=tools]")).toContainText(
      new RegExp(`(Claude Code|claude-code): ${NR_REVIEW.POLICY_HOLD} · (Codex[^:·]*|codex-cli): ${TOOLS_COPY.EFFECT_DENY}`),
    );
  });

  test("the Tools and pushes row says the harness's inner walls the server resolved (#2822)", async ({ page }) => {
    const walled = { ...CLAUDE_UNGATED, inner_walls: { sandbox_mode: "off", approvals: "wardyn" } };
    await stub(page, { preflight: { ...CLEAR, access: { ...SCENARIO_ACCESS.member, harnesses: [walled] } } as never });
    await openReviewWith(page, ENV);
    await expect(page.locator("[data-policy-row=tools]")).toContainText(
      NR_REVIEW.INNER_WALLS(NR_REVIEW.INNER_SANDBOX_OFF, NR_REVIEW.INNER_APPROVALS_WARDYN),
    );
  });

  test("a CPU cut and a main-drive cut keep the shared resources-capped line beside the narrowed notice (DR-0263)", async ({ page }) => {
    const line = "resources capped to operator maximum";
    const cut = { source: "governance", source_name: "Platform team", capped: true };
    await stub(page, {
      preflight: {
        ...CLEAR,
        warnings: [line],
        resources_applied: { cpu_millis: { requested: 16000, applied: 8000, ...cut }, memory_mib: { requested: 4096, applied: 4096 } },
        main_drive: { requested_mib: 40960, applied_mib: 20480, basis: "request", clamped_by: "governance", soft: false },
      } as never,
    });
    await openReviewWith(page, ENV);
    await expect(page.getByText(NR_REVIEW.NARROWED_BY("Platform team"))).toBeVisible();
    await expect(page.getByText(line, { exact: true })).toBeVisible();
  });

  test("a CPU cut and a PIDs cut keep the shared resources-capped line beside the narrowed notice (DR-0263)", async ({ page }) => {
    const line = "resources capped to operator maximum";
    const cut = { source: "governance", source_name: "Platform team", capped: true };
    await stub(page, {
      preflight: {
        ...CLEAR,
        warnings: [line],
        resources_applied: {
          cpu_millis: { requested: 16000, applied: 8000, ...cut },
          memory_mib: { requested: 4096, applied: 4096 },
          pids_limit: { requested: 4096, applied: 256, ...cut },
        },
      } as never,
    });
    await openReviewWith(page, ENV);
    await expect(page.getByText(NR_REVIEW.NARROWED_BY("Platform team"))).toBeVisible();
    await expect(page.getByText(line, { exact: true })).toBeVisible();
  });

  test("the lifetime is the one the server resolves: held to its cap, or No end when asked for and allowed (DR-0245)", async ({ page }) => {
    await stub(page, { preflight: { ...CLEAR, limits: SAVED_LIMITS } as never });
    await openReviewWith(page, [...ENV, "ends_after_sec: 36000"]);
    const limits = page.locator("[data-review-group=limits]");
    await expect(limits).toContainText("2 hours");
    await expect(limits).not.toContainText("10 hours");
    await page.goto("about:blank");
    await openReviewWith(page, [...ENV, "no_end: true"]);
    await expect(page.locator("[data-review-group=limits]")).toContainText(ENDS_NONE);
  });
});

test.describe("New Run, Review: what the summaries say the server will do (defaults)", () => {
  test("a default of no end, with no end asked for, reads No end", async ({ page }) => {
    const unlimited = { default: { value: 0, unlimited: true }, cap: { value: 0, unlimited: true } };
    await stub(page, { preflight: { ...CLEAR, limits: { ...SAVED_LIMITS, lifetime_sec: unlimited } } as never });
    await openReviewWith(page, ENV);
    await expect(page.locator("[data-review-group=limits]")).toContainText(ENDS_NONE);
  });

  test("an interactive Claude Code default that accepts file edits says so in the Tools and pushes row", async ({ page }) => {
    const accepting = { ...CLAUDE_UNGATED, default: { ...CLAUDE_UNGATED.default, effective: "hold", source: { kind: "harness", id: "accept_edits", name: "Claude Code settings" } } };
    await stub(page, { preflight: { ...CLEAR, access: { ...SCENARIO_ACCESS.member, harnesses: [accepting] } } as never });
    await openReviewWith(page, ENV);
    await expect(page.locator("[data-policy-row=tools]")).toContainText(NR_REVIEW.POLICY_ACCEPTS_EDITS);
    // #2428: scoped to tools no rule names; named allow and deny rules still apply.
    await expect(page.locator("[data-policy-row=tools]")).toContainText("Named rules still apply.");
    await expect(page.locator("[data-policy-row=tools]")).not.toContainText("Every other tool asks first");
  });
});

test.describe("New Run, Review: Save as and the saver's own drives (DR-0221)", () => {
  const MINE = "c1111111-1111-4111-8111-111111111111";
  const TEAM = "c2222222-2222-4222-8222-222222222222";
  const drive = (id: string, name: string, owned: boolean) => ({ id, name, section: owned ? "user" : "shared", source: owned ? "personal" : "team", backend: "docker_volume", target: `/home/agent/${name}`, writable: true, enforcement: "filesystem", legacy: false, owned });

  test("a template shared with a group leaves out the member drive and the start folder on it", async ({ page }) => {
    await stub(page, { preflight: CLEAR });
    await page.route("**/api/v1/me/drives*", (route) => route.fulfill({ json: { drives: [drive(MINE, "scratch", true), drive(TEAM, "share", false)], pool_checked: true } }));
    const posted: string[] = [];
    await page.route("**/api/v1/templates", (route) => {
      if (route.request().method() === "POST") {
        posted.push(route.request().postData() ?? "");
        return route.fulfill({ status: 201, json: { id: "t1", kind: "template" } });
      }
      return route.fulfill({ json: { templates: [], publish: { org: false, groups: ["g-1"], group_labels: { "g-1": "Platform" }, groups_unverified: false } } });
    });
    await openReviewWith(page, ["shape: environment", "experience: interactive", `drives:\n    - drive_id: ${MINE}\n    - drive_id: ${TEAM}`, `start_folder:\n    kind: attachment\n    drive_id: ${MINE}`]);
    await press(page, NR_REVIEW.SAVE_TEMPLATE);
    const dialog = page.getByRole("dialog");
    await dialog.getByLabel("Available to").selectOption({ label: "Platform group" });
    await dialog.getByLabel(NR_SAVE.NAME_LABEL).fill("Team");
    await dialog.getByRole("button", { name: NR_SAVE.CONFIRM_TEMPLATE, exact: true }).click();
    await expect.poll(() => posted.length).toBe(1);
    const intent = JSON.parse(posted[0]).document.intent;
    expect(intent.drives).toEqual([{ drive_id: TEAM }]);
    expect(intent).not.toHaveProperty("start_folder");
  });
});
