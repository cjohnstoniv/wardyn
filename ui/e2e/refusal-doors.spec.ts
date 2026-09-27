/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #543 (packet 1, door-cards.html): a refusal opens the door of its OWN model
// provider — the one the refusal names, never the one selected on screen
// (#146's ruling). Pinned here, in the real console against the seeded backend:
// the failure block's door per provider kind, no door for anyone but the owner
// or in the Admin view, a New Run refusal opening the right door, and the
// approvals reauth card.
//
// The seeded daemon has no provider block and its `none` runner never
// dispatches, so the provider block, the refusal's audit row and the 422 are
// spliced at the wire; the rest is the real console and the real backend.

import { randomUUID } from "node:crypto";
import type { Page, Route } from "@playwright/test";
import { test, expect, gotoConsole, navTo, navToRoute } from "./fixtures";
import { MODEL_ACCESS_BANNER, MODEL_ACCESS_RUN_DOOR, REAUTH_ROW, REAUTH_TITLE } from "../src/app/components/wardyn/model-access-copy";
import { CLAUDE_DOOR, CONNECTIONS, KEY_DOOR, RUN_FACTS } from "../src/app/components/wardyn/copy/door";
import { CONSOLE_VIEW } from "../src/app/components/wardyn/copy/console-view";
import { AGENTS } from "../src/app/lib/workspace-providers-copy";

const VIEWER = "bob@acme.example";
const FAILED_TASK = "e2e fixture 6";

const P = {
  // The claude-code default: the AWS door anything but the refusal would open.
  bedrockDev: { id: "bedrock-dev", name: "Bedrock (dev)", kind: "bedrock_sso", host: "bedrock-runtime.us-west-2.amazonaws.com" },
  bedrock: { id: "bedrock-prod", name: "Bedrock (prod)", kind: "bedrock_sso", host: "bedrock-runtime.us-east-1.amazonaws.com" },
  claude: { id: "claude-sub", name: "Claude subscription", kind: "anthropic_subscription", host: "api.anthropic.com" },
  key: { id: "anthropic-key", name: "Anthropic API key", kind: "anthropic_api_key", host: "api.anthropic.com" },
  gateway: { id: "corp-gateway", name: "Corp gateway", kind: "custom_endpoint", host: "gateway.corp.example" },
};

// The server's sentences (canon Table 3), verbatim.
const CONNECT = "connect it from Getting started in the console, or from the banner the console shows on every page.";
const refusal = (name: string, state: string) =>
  `This run's model provider is ${name}, and ${state} — ${CONNECT} Wardyn does not substitute a different model provider.`;

/** GET /me as `principal`, a user unless `admin`. */
async function asViewer(page: Page, principal: string, admin = false): Promise<void> {
  await page.route("**/api/v1/me", async (route) => {
    const response = await route.fetch();
    const json = await response.json();
    json.principal = principal;
    if (!admin) {
      json.role = "user";
      json.operator = false;
      json.security_operator = false;
    }
    await route.fulfill({ response, json });
  });
}

/** /setup/status with the provider block, fetched once and served from cache
 *  (one-door.spec.ts's reason). */
async function withProviders(page: Page): Promise<void> {
  let cached: Record<string, unknown> | null = null;
  await page.route("**/api/v1/setup/status*", async (route: Route) => {
    if (!cached) {
      cached = (await (await route.fetch()).json()) as Record<string, unknown>;
      cached.model_providers = Object.values(P).map((p) => ({
        ...p,
        harnesses: ["claude-code", "codex-cli"],
        default_for: p.id === P.bedrockDev.id ? ["claude-code"] : [],
      }));
      cached.provider_access = Object.values(P).map((p) => ({ provider: p.id, state: "not_configured" }));
    }
    await route.fulfill({ json: cached });
  });
  // The AWS and Claude doors start their sign-in at once; nothing here signs in.
  await page.route("**/api/v1/model-providers/*/sign-in", (route) =>
    route.fulfill({ status: 503, json: { error: "e2e: no sign-in here" } }),
  );
}

/** Fixture 6 (FAILED) refused over `provider`'s credential, created by `owner`. */
async function refusedRun(page: Page, provider: (typeof P)[keyof typeof P], sentence: string, owner = VIEWER): Promise<void> {
  await page.route("**/api/v1/runs/*", async (route) => {
    if (route.request().method() !== "GET") return route.fallback();
    const response = await route.fetch();
    const json = await response.json();
    if (json.task === FAILED_TASK) {
      json.failure_hint = sentence;
      json.created_by = owner;
      json.model_provider_id = provider.id;
    }
    await route.fulfill({ response, json });
  });
  // The run.create/failure row #532's dispatch refusal writes.
  await page.route("**/api/v1/audit*", async (route) => {
    const response = await route.fetch();
    const rows = (await response.json()) as Record<string, unknown>[];
    if (!Array.isArray(rows) || rows.length === 0) return route.fulfill({ response, json: rows });
    rows.unshift({
      id: randomUUID(),
      time: new Date().toISOString(),
      run_id: rows[0].run_id,
      actor_type: "system",
      actor: "wardynd",
      action: "run.create",
      target: String(rows[0].run_id),
      outcome: "failure",
      data: { error: sentence, reason: "model_credential", provider: provider.id, kind: provider.kind, mechanism: provider.kind },
    });
    await route.fulfill({ response, json: rows });
  });
}

async function openFailedRun(page: Page, view: "user" | "admin" = "user"): Promise<void> {
  await gotoConsole(page);
  await navTo(page, "Runs");
  await page.getByText(FAILED_TASK).click();
  await expect(page).toHaveURL(/^[^?]*\/runs\/[^/]+$/);
  if (view === "admin") {
    // The same run in the Admin view (its twin path).
    await navToRoute(page, `/admin${new URL(page.url()).pathname}`);
    await expect(page).toHaveURL(/\/admin\/runs\/.+/);
  }
}

test.describe("the failure block opens the run's own provider's door (state 2)", () => {
  const cases = [
    { p: P.bedrock, state: "you are not signed in to AWS for it", aria: MODEL_ACCESS_RUN_DOOR.SIGN_IN_ARIA, label: AGENTS.SIGN_IN_AWS, note: MODEL_ACCESS_RUN_DOOR.NOTE, dialog: MODEL_ACCESS_BANNER.DIALOG_TITLE },
    { p: P.gateway, state: "you have not added your token for it", aria: MODEL_ACCESS_RUN_DOOR.ADD_TOKEN_ARIA, label: CONNECTIONS.ADD_TOKEN, note: MODEL_ACCESS_RUN_DOOR.NOTE_KEY, dialog: KEY_DOOR.TITLE(true, P.gateway.name) },
    { p: P.key, state: "you have not added your key for it", aria: MODEL_ACCESS_RUN_DOOR.ADD_KEY_ARIA, label: CONNECTIONS.ADD_KEY, note: MODEL_ACCESS_RUN_DOOR.NOTE_KEY, dialog: KEY_DOOR.TITLE(false, P.key.name) },
    { p: P.claude, state: "you are not signed in to Claude for it", aria: MODEL_ACCESS_RUN_DOOR.SIGN_IN_CLAUDE_ARIA, label: CONNECTIONS.SIGN_IN_CLAUDE, note: MODEL_ACCESS_RUN_DOOR.NOTE, dialog: CLAUDE_DOOR.TITLE },
  ];
  for (const c of cases) {
    test(`${c.p.kind}: the owner's "${c.label}" opens ${c.p.name}'s door`, async ({ page }) => {
      const sentence = refusal(c.p.name, c.state);
      await asViewer(page, VIEWER);
      await withProviders(page);
      await refusedRun(page, c.p, sentence);
      await openFailedRun(page);

      // State 1: the run's provider, in the header.
      await expect(page.getByText(RUN_FACTS.PROVIDER(c.p.name, false), { exact: true })).toBeVisible();
      const block = page.getByTestId("run-failure-block");
      await expect(block).toHaveAttribute("data-ending", "credential");
      await expect(block.getByText(sentence)).toHaveCount(1);
      await expect(block.getByText(c.note)).toBeVisible();
      const button = block.getByRole("button", { name: c.aria });
      await expect(button).toHaveText(c.label);
      await button.click();
      const dialog = page.getByRole("dialog", { name: c.dialog });
      await expect(dialog).toBeVisible();
      if (c.p.kind === "bedrock_sso" || c.p.kind === "anthropic_subscription") {
        await expect(dialog).toContainText(`For ${c.p.name}`);
      }
      await expect(page.getByRole("dialog")).not.toContainText(`For ${P.bedrockDev.name}`);
    });
  }
});

test.describe("no door for anyone but the owner, or in the Admin view (state 3)", () => {
  const sentence = refusal(P.bedrock.name, "you are not signed in to AWS for it");

  test("another user's run: whose credential it ran on, and no door", async ({ page }) => {
    await asViewer(page, "carol@acme.example");
    await withProviders(page);
    await refusedRun(page, P.bedrock, sentence);
    await openFailedRun(page);
    const block = page.getByTestId("run-failure-block");
    await expect(block.getByText(MODEL_ACCESS_RUN_DOOR.NOT_OWNER(VIEWER))).toBeVisible();
    await expect(block.getByRole("button", { name: MODEL_ACCESS_RUN_DOOR.SIGN_IN_ARIA })).toHaveCount(0);
    await expect(block.getByRole("button", { name: CONSOLE_VIEW.OPEN_IN_USER })).toHaveCount(0);
  });

  test("the admin's own run in the Admin view: no door, and Open in user view leads to it", async ({ page }) => {
    await asViewer(page, "ann@acme.example", true);
    await withProviders(page);
    await refusedRun(page, P.bedrock, sentence, "ann@acme.example");
    await openFailedRun(page, "admin");
    const block = page.getByTestId("run-failure-block");
    await expect(block.getByText(MODEL_ACCESS_RUN_DOOR.NOT_OWNER("ann@acme.example"))).toBeVisible();
    await expect(block.getByRole("button", { name: MODEL_ACCESS_RUN_DOOR.SIGN_IN_ARIA })).toHaveCount(0);
    await block.getByRole("button", { name: CONSOLE_VIEW.OPEN_IN_USER }).click();
    await expect(page).toHaveURL(/^[^?]*\/runs\/[^/]+$/);
    await expect(page).not.toHaveURL(/\/admin\//);
    await expect(page.getByTestId("run-failure-block").getByRole("button", { name: MODEL_ACCESS_RUN_DOOR.SIGN_IN_ARIA })).toBeVisible();
  });
});

test.describe("a New Run refusal opens the door its provider names (state 4)", () => {
  test("a refusal over the gateway's token opens the token door, not AWS (#146 defect 2)", async ({ page }) => {
    const sentence = refusal(P.gateway.name, "you have not added your token for it");
    await asViewer(page, VIEWER);
    await withProviders(page);
    await page.route("**/api/v1/runs", async (route) => {
      if (route.request().method() !== "POST") return route.fallback();
      await route.fulfill({
        status: 422,
        json: { error: sentence, reason: "model_credential", provider: P.gateway.id, kind: P.gateway.kind },
      });
    });
    await gotoConsole(page);
    await navToRoute(page, "/runs/new");
    await expect(page.getByRole("heading", { name: "New run" })).toBeVisible();
    await page.getByLabel("Title").fill("e2e provider refusal");
    await page.getByRole("button", { name: "Launch run" }).click();
    await expect(page.getByRole("dialog", { name: KEY_DOOR.TITLE(true, P.gateway.name) })).toBeVisible();
    await expect(page.getByRole("dialog", { name: MODEL_ACCESS_BANNER.DIALOG_TITLE })).toHaveCount(0);
    await page.keyboard.press("Escape");
    // The rail's launch-error line carries the server's sentence.
    await expect(page.getByRole("alert")).toContainText(sentence);
  });
});

test.describe("the approvals reauth card names and opens the hold's own provider (state 5)", () => {
  async function holdFor(page: Page, owner: string): Promise<void> {
    await page.route("**/api/v1/approvals*", async (route) => {
      if (route.request().method() !== "GET") return route.fallback();
      const pending = new URL(route.request().url()).searchParams.get("state") === "PENDING";
      await route.fulfill({
        json: pending
          ? [
              {
                id: randomUUID(),
                run_id: randomUUID(),
                kind: "credential_reauth",
                requested_scope: { mechanism: "bedrock_sso", credential_source: "per_user", owner, provider: P.bedrock.id },
                state: "PENDING",
                requested_at: new Date().toISOString(),
              },
            ]
          : [],
      });
    });
  }

  test("the owner: title, provider line, and Sign in to AWS opens Bedrock (prod), not the default", async ({ page }) => {
    await asViewer(page, VIEWER);
    await withProviders(page);
    await holdFor(page, VIEWER);
    await gotoConsole(page);
    await navToRoute(page, "/approvals");
    await expect(page.getByText(REAUTH_TITLE)).toBeVisible();
    await expect(page.getByText(REAUTH_ROW.PROVIDER(P.bedrock.name), { exact: true })).toBeVisible();
    await page.getByRole("button", { name: REAUTH_ROW.ariaLabel }).click();
    const dialog = page.getByRole("dialog", { name: MODEL_ACCESS_BANNER.DIALOG_TITLE });
    await expect(dialog).toContainText(`For ${P.bedrock.name}`);
  });

  test("the admin's own hold in the Admin view: no door, and Open in user view leads to it", async ({ page }) => {
    await asViewer(page, "ann@acme.example", true);
    await withProviders(page);
    await holdFor(page, "ann@acme.example");
    await gotoConsole(page, "admin");
    await navToRoute(page, "/admin/approvals");
    await expect(page.getByText(REAUTH_TITLE)).toBeVisible();
    await expect(page.getByText(REAUTH_ROW.notYoursHint("ann@acme.example"))).toBeVisible();
    await expect(page.getByRole("button", { name: REAUTH_ROW.ariaLabel })).toHaveCount(0);
    await page.getByRole("button", { name: CONSOLE_VIEW.OPEN_IN_USER }).click();
    await expect(page).toHaveURL(/^[^?]*\/approvals$/);
    await expect(page).not.toHaveURL(/\/admin\//);
    await expect(page.getByRole("button", { name: REAUTH_ROW.ariaLabel })).toBeVisible();
  });
});
