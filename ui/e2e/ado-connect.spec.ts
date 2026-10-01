/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The per-person Azure DevOps token console (#1428, #1430): the person's card,
// New Run's token line and launch note, the run page's token list and the
// approval card's added line. The server lanes that produce these answers
// (mint, run lifecycle, own token) build in parallel and may not be on this
// backend yet, and the hermetic backend runs no Entra tenant, so every answer
// is spliced at the route: the real /setup/status with `scm_access` replaced,
// and the token routes fulfilled here. Every string is imported from
// ado-pat-copy.ts, never retyped: the vitest pin owns the characters, this
// spec owns that a browser draws them and that no state scrolls sideways at 390px.
import type { Page } from "@playwright/test";
import { test, expect, gotoConsole, navToRoute, expandCard, sql } from "./fixtures";
import { ADO_PAT } from "../src/app/lib/ado-pat-copy";
import { adoCapName } from "../src/app/lib/ado-access-copy";

const ORG = "https://dev.azure.com/wardyn-e2e";
const clock = (d: Date) => d.toLocaleTimeString("en-GB", { hour: "2-digit", minute: "2-digit", hourCycle: "h23" });

// The real /setup/status with the caller's own Azure DevOps answer swapped in.
// `get` is read on every match, so a spec flips the answer the way the server
// would after a connect or a store. One real fetch is cached (a real round trip
// per match raced Playwright disposing the response).
async function spliceAccess(page: Page, get: () => Record<string, unknown> | undefined): Promise<void> {
  let base: Record<string, unknown> | null = null;
  await page.route("**/api/v1/setup/status*", async (route) => {
    if (!base) base = (await (await route.fetch()).json()) as Record<string, unknown>;
    const access = get();
    const body = { ...base! };
    if (access) body.scm_access = { org: ORG, kind: "azure_devops", ...access };
    else delete body.scm_access;
    await route.fulfill({ json: body });
  });
}

async function openAccountCard(page: Page, title = "Azure DevOps"): Promise<void> {
  await gotoConsole(page);
  await navToRoute(page, "/account");
  await expandCard(page, title);
}

// Tokens a run held, seeded as ado_run_pats rows: the run page reads them from
// the daemon's own GET /runs/{id}/ado-tokens, not from a splice. A token value
// is never stored, so the rows carry none. The caller removes what it seeded.
interface SeededToken {
  createdAt: Date;
  validTo: Date;
  revokedAt?: Date;
  reason?: string;
  lastError?: string;
  /** Space-joined, as the column stores it; defaults to a read scope. */
  scope?: string;
}
function seedRunTokens(runId: string, tokens: SeededToken[]): void {
  const at = (d?: Date) => (d ? `'${d.toISOString()}'::timestamptz` : "NULL");
  for (const t of tokens) {
    sql(
      `INSERT INTO ado_run_pats (run_id, authorization_id, owner, provider_row_id, org, scope, valid_to, created_at, revoked_at, revoke_reason, last_error) ` +
        `VALUES ('${runId}', gen_random_uuid(), 'e2e-owner', 'azure_devops', 'wardyn-e2e', '${t.scope ?? "vso.code vso.project"}', ${at(t.validTo)}, ${at(t.createdAt)}, ${at(t.revokedAt)}, '${t.reason ?? ""}', '${t.lastError ?? ""}')`,
    );
  }
}
function clearRunTokens(runId: string): void {
  sql(`DELETE FROM ado_run_pats WHERE run_id = '${runId}'`);
}

const noHorizontalScroll = (page: Page) =>
  page.evaluate(() => document.documentElement.scrollWidth <= document.documentElement.clientWidth);

test.describe("the person's Azure DevOps card, a row that creates a token for each run", () => {
  test("connect once: the card asks, the popup opens the sign-in door, and the card then reads Connected", async ({ page, context }) => {
    let access: Record<string, unknown> = { token_mode: "minted_pat", state: "not_configured" };
    await spliceAccess(page, () => access);
    // The connect poll trusts only this route, never the popup's own page.
    await page.route("**/api/v1/me/scm-access", (route) =>
      route.fulfill({ json: access.state === "live" ? [{ state: "live", source: "org" }] : [] }),
    );
    await openAccountCard(page);
    await expect(page.getByText(ADO_PAT.MEMBER_NOT_CONNECTED)).toBeVisible();

    const [popup] = await Promise.all([
      context.waitForEvent("page"),
      page.getByRole("button", { name: ADO_PAT.MEMBER_CONNECT }).click(),
    ]);
    await expect(popup).toHaveURL(/\/api\/v1\/scm\/azure-devops\/signin/);
    access = {
      token_mode: "minted_pat",
      state: "live",
      source: "org",
      last_token: { created_at: new Date(2000, 8, 29, 9, 2).toISOString(), revoked_at: new Date(2000, 8, 29, 9, 41).toISOString() },
    };
    await popup.waitForEvent("close");

    await expect(page.getByText(ADO_PAT.MEMBER_CONNECTED)).toBeVisible();
    await expect(page.getByText(ADO_PAT.MEMBER_LAST_TOKEN("09:02", "09:41"))).toBeVisible();
    await expect(page.getByRole("button", { name: ADO_PAT.MEMBER_CONNECT })).toHaveCount(0);
  });

  test("Disconnect asks first, says what it revokes, and only then calls the daemon", async ({ page }) => {
    let access: Record<string, unknown> = { token_mode: "minted_pat", state: "live", source: "org" };
    await spliceAccess(page, () => access);
    let deletes = 0;
    await page.route("**/api/v1/scm/azure-devops/connection", async (route) => {
      deletes++;
      access = { token_mode: "minted_pat", state: "not_configured" };
      await route.fulfill({ status: 204 });
    });
    await openAccountCard(page);
    await page.getByRole("button", { name: ADO_PAT.MEMBER_DISCONNECT }).click();
    const dialog = page.getByRole("alertdialog");
    await expect(dialog.getByText(ADO_PAT.DISCONNECT_BODY)).toBeVisible();
    expect(deletes).toBe(0);
    await dialog.getByRole("button", { name: ADO_PAT.DISCONNECT_CANCEL }).click();
    expect(deletes).toBe(0);

    await page.getByRole("button", { name: ADO_PAT.MEMBER_DISCONNECT }).click();
    await page.getByRole("alertdialog").getByRole("button", { name: ADO_PAT.MEMBER_DISCONNECT }).click();
    await expect(page.getByText(ADO_PAT.MEMBER_NOT_CONNECTED)).toBeVisible();
    expect(deletes).toBe(1);
  });

  test("sign in again: the organisation asked, and Connect is offered", async ({ page }) => {
    await spliceAccess(page, () => ({ token_mode: "minted_pat", state: "expired_signin", cause: "ended", source: "org" }));
    await openAccountCard(page);
    await expect(page.getByText(ADO_PAT.SIGN_IN_AGAIN_BODY)).toBeVisible();
    await expect(page.getByRole("button", { name: ADO_PAT.MEMBER_CONNECT })).toBeVisible();
  });

  test("blocked by the organisation: the card names the allow list and offers nothing to press", async ({ page }) => {
    await spliceAccess(page, () => ({ token_mode: "minted_pat", state: "expired_signin", cause: "blocked" }));
    await openAccountCard(page);
    await expect(page.getByText(ADO_PAT.BLOCKED_BODY)).toBeVisible();
    await expect(page.getByRole("button", { name: ADO_PAT.MEMBER_CONNECT })).toHaveCount(0);
  });
});

test.describe("the person's Azure DevOps card, a row where each person adds their own token", () => {
  const own = {
    token_mode: "own_pat",
    max_days: 30,
    token_scopes: ["Code (Read & write)", "Project and Team (Read)", "Work Items (Read)"],
  };

  test("add: refusals sit under the field they are about, then a stored token reads Connected with its expiry", async ({ page }) => {
    let access: Record<string, unknown> = { ...own, state: "not_configured" };
    await spliceAccess(page, () => access);
    const refusals = ["ado_own_pat_identity_mismatch", "ado_own_pat_expiry_too_long", "ado_own_pat_rejected"];
    const bodies: Record<string, unknown>[] = [];
    await page.route("**/api/v1/me/scm/azure-devops/token", async (route) => {
      bodies.push(route.request().postDataJSON() as Record<string, unknown>);
      const reason = refusals[bodies.length - 1];
      if (reason) return route.fulfill({ status: 422, json: { error: "refused", reason } });
      access = { ...own, state: "live", source: "own", expires_on: "2000-10-27" };
      await route.fulfill({ json: { state: "live", org: ORG } });
    });
    await openAccountCard(page);
    await page.getByRole("button", { name: ADO_PAT.OWN_ADD_CTA }).click();
    const dialog = page.getByRole("dialog", { name: ADO_PAT.OWN_DIALOG_TITLE });
    await expect(dialog.getByText(ADO_PAT.OWN_DIALOG_LEAD("wardyn-e2e", 30))).toBeVisible();
    await expect(dialog.getByRole("listitem")).toHaveText(["Code (Read & write)", "Project and Team (Read)", "Work Items (Read)"]);
    await expect(dialog.getByRole("link", { name: ADO_PAT.OWN_OPEN_TOKENS })).toHaveAttribute(
      "href",
      "https://dev.azure.com/wardyn-e2e/_usersSettings/tokens",
    );

    await dialog.getByLabel(ADO_PAT.OWN_FIELD_TOKEN).fill("pasted-secret");
    await dialog.getByLabel(ADO_PAT.OWN_FIELD_EXPIRES).fill("2000-03-01");
    await dialog.getByRole("button", { name: ADO_PAT.OWN_DIALOG_ADD }).click();
    await expect(dialog.getByText(ADO_PAT.OWN_MISMATCH)).toBeVisible();
    await dialog.getByRole("button", { name: ADO_PAT.OWN_DIALOG_ADD }).click();
    await expect(dialog.getByText(ADO_PAT.OWN_TOO_LONG(30))).toBeVisible();
    await dialog.getByRole("button", { name: ADO_PAT.OWN_DIALOG_ADD }).click();
    await expect(dialog.getByText(ADO_PAT.OWN_REJECTED)).toBeVisible();

    await dialog.getByLabel(ADO_PAT.OWN_FIELD_EXPIRES).fill("2000-10-27");
    await dialog.getByRole("button", { name: ADO_PAT.OWN_DIALOG_ADD }).click();
    await expect(dialog).toHaveCount(0);
    expect(bodies.at(-1)).toEqual({ org: ORG, token: "pasted-secret", expires_on: "2000-10-27" });
    await expect(page.getByText(ADO_PAT.OWN_EXPIRING_LINE("wardyn-e2e", "27 October"))).toBeVisible();
    await expect(page.getByRole("button", { name: ADO_PAT.OWN_REPLACE })).toBeVisible();
  });

  test("refused before expiry (#1445): Refused, the one refusal line, Replace primary, and a new paste reads Connected", async ({ page }) => {
    let access: Record<string, unknown> = { ...own, state: "live", source: "own", expires_on: "2000-10-27", refused_at: new Date(2000, 9, 2, 9, 30).toISOString() };
    await spliceAccess(page, () => access);
    await page.route("**/api/v1/me/scm/azure-devops/token", async (route) => {
      access = { ...own, state: "live", source: "own", expires_on: "2000-11-20" };
      await route.fulfill({ json: { state: "live", org: ORG } });
    });
    await openAccountCard(page);
    await expect(page.getByText(ADO_PAT.OWN_CHIP_REFUSED, { exact: true })).toBeVisible();
    await expect(page.getByText(ADO_PAT.OWN_REFUSED_LINE("2 October", "27 October"))).toBeVisible();
    await expect(page.getByText(ADO_PAT.OWN_EXPIRING_LINE("wardyn-e2e", "27 October"))).toHaveCount(0);
    await expect(page.getByText("Connected", { exact: true })).toHaveCount(0);
    await page.getByRole("button", { name: ADO_PAT.OWN_REPLACE }).click();
    const dialog = page.getByRole("dialog", { name: ADO_PAT.OWN_DIALOG_TITLE });
    await dialog.getByLabel(ADO_PAT.OWN_FIELD_TOKEN).fill("pasted-secret");
    await dialog.getByLabel(ADO_PAT.OWN_FIELD_EXPIRES).fill("2000-11-20");
    await dialog.getByRole("button", { name: ADO_PAT.OWN_DIALOG_ADD }).click();
    await expect(dialog).toHaveCount(0);
    await expect(page.getByText(ADO_PAT.OWN_EXPIRING_LINE("wardyn-e2e", "20 November"))).toBeVisible();
    await expect(page.getByText(ADO_PAT.OWN_CHIP_REFUSED, { exact: true })).toHaveCount(0);
  });

  test("expiring, expired and the Server row each read as the mock draws them", async ({ page }) => {
    const soon = new Date(Date.now() + 3 * 86_400_000);
    const day = `${soon.getFullYear()}-${String(soon.getMonth() + 1).padStart(2, "0")}-${String(soon.getDate()).padStart(2, "0")}`;
    let access: Record<string, unknown> = { ...own, state: "expiring", source: "own", expires_on: day };
    await spliceAccess(page, () => access);
    await openAccountCard(page);
    await expect(page.getByText(ADO_PAT.OWN_CHIP_EXPIRING(3))).toBeVisible();
    await expect(page.getByRole("button", { name: ADO_PAT.OWN_REPLACE })).toBeVisible();

    access = { ...own, state: "expired_signin", cause: "token_expired", source: "own", expires_on: "2000-09-01" };
    await page.reload();
    await expandCard(page, "Azure DevOps");
    await expect(page.getByText(ADO_PAT.OWN_EXPIRED_BODY)).toBeVisible();
    await expect(page.getByRole("button", { name: ADO_PAT.OWN_ADD_CTA })).toBeVisible();

    access = { ...own, org: "https://tfs.example.com/collection", state: "live", source: "own", expires_on: "2000-10-27" };
    await page.reload();
    await expandCard(page, ADO_PAT.OWN_SERVER_TITLE);
    await expect(page.getByText(ADO_PAT.OWN_SERVER_NOTE)).toBeVisible();
  });
});

test.describe("New Run, a row that creates a token for each run", () => {
  // `saved`: the token line sits under the saved-policy picker, so the tests
  // that read it open that; a launch needs the ordinary form, where Launch is
  // enabled without picking a policy.
  async function openNewRun(page: Page, saved = true): Promise<void> {
    await gotoConsole(page);
    await page.getByRole("button", { name: "New run" }).click();
    await expect(page.getByRole("heading", { name: "New run" })).toBeVisible();
    if (saved) await page.getByRole("button", { name: /^Reuse a saved policy/ }).click();
  }

  test("the run's token line names the row's default access, and not connected says to connect first", async ({ page, context }) => {
    let access: Record<string, unknown> = { token_mode: "minted_pat", state: "not_configured", default_profile: ["code_read"] };
    await spliceAccess(page, () => access);
    await page.route("**/api/v1/me/scm-access", (route) =>
      route.fulfill({ json: access.state === "live" ? [{ state: "live", source: "org" }] : [] }),
    );
    await openNewRun(page);
    await expect(page.getByTestId("ado-run-token-line")).toHaveText(
      `${ADO_PAT.NEWRUN_LINE_PREFIX}${adoCapName("code_read")}${ADO_PAT.NEWRUN_LINE_SUFFIX}`,
    );
    const note = page.getByTestId("ado-launch-note");
    await expect(note.getByText(ADO_PAT.LAUNCH_NOT_CONNECTED)).toBeVisible();
    const [popup] = await Promise.all([context.waitForEvent("page"), note.getByRole("button", { name: ADO_PAT.MEMBER_CONNECT }).click()]);
    await expect(popup).toHaveURL(/\/api\/v1\/scm\/azure-devops\/signin/);
    access = { token_mode: "minted_pat", state: "live", source: "org", default_profile: ["code_read"] };
    await popup.waitForEvent("close");
  });

  test("a launch the organisation refuses on its token policy raises the blocked note", async ({ page }) => {
    await spliceAccess(page, () => ({ token_mode: "minted_pat", state: "live", source: "org", default_profile: ["code_read"] }));
    await page.route("**/api/v1/runs", async (route) => {
      if (route.request().method() !== "POST") return route.fallback();
      await route.fulfill({ status: 403, json: { error: ADO_PAT.LAUNCH_POLICY_REFUSED, reason: "ado_pat_policy_blocked" } });
    });
    await openNewRun(page, false);
    await expect(page.getByTestId("ado-launch-note")).toHaveCount(0);
    await page.getByLabel("Title").fill("e2e ado token policy");
    await page.getByRole("button", { name: "Launch run" }).click();
    await expect(page.getByTestId("ado-launch-note")).toHaveText(ADO_PAT.LAUNCH_POLICY_REFUSED);
  });

  test("a launch refused for want of a usable sign-in asks to connect again", async ({ page }) => {
    await spliceAccess(page, () => ({ token_mode: "minted_pat", state: "live", source: "org", default_profile: ["code_read"] }));
    await page.route("**/api/v1/runs", async (route) => {
      if (route.request().method() !== "POST") return route.fallback();
      await route.fulfill({ status: 403, json: { error: "cannot create a token", reason: "ado_pat_consent_needed" } });
    });
    await openNewRun(page, false);
    await page.getByLabel("Title").fill("e2e ado consent");
    await page.getByRole("button", { name: "Launch run" }).click();
    await expect(page.getByTestId("ado-launch-note").getByText(ADO_PAT.LAUNCH_NOT_CONNECTED)).toBeVisible();
  });

  test("a row on the Entra sign-in lane draws neither line", async ({ page }) => {
    await spliceAccess(page, () => ({ token_mode: "bearer", state: "live", source: "org" }));
    await openNewRun(page);
    await expect(page.getByTestId("ado-run-token-line")).toHaveCount(0);
    await expect(page.getByTestId("ado-launch-note")).toHaveCount(0);
  });
});

test.describe("the run page", () => {
  test("lists each token oldest first from the daemon: renewed, revoked with its reason, and a revoke that failed (Credentials widget)", async ({ page }) => {
    const runId = sql("SELECT id FROM agent_runs ORDER BY created_at LIMIT 1");
    const t = (h: number, m: number) => new Date(2000, 8, 29, h, m);
    clearRunTokens(runId);
    try {
      seedRunTokens(runId, [
        // Replaced by a token of the same scope, and closed at its own expiry.
        { createdAt: t(9, 2), validTo: t(17, 2), revokedAt: t(17, 2), reason: "expired" },
        // Replaced by a wider token: access was added.
        { createdAt: t(15, 2), validTo: t(23, 2), revokedAt: t(23, 2), reason: "expired" },
        // Ended with its run, and Azure DevOps refused the revoke: it lives to 23:30 on its own.
        { createdAt: t(16, 0), validTo: t(23, 30), revokedAt: t(16, 40), reason: "run_end", lastError: "revoke refused", scope: "vso.code vso.project vso.code_write" },
      ]);
      await gotoConsole(page);
      await navToRoute(page, `/runs/${runId}`);
      const tokens = page.getByTestId("ado-run-tokens");
      await expect(tokens.getByRole("heading", { name: ADO_PAT.RUN_TOKEN_TITLE })).toBeVisible();
      const lines = tokens.locator("p:not([role=alert])");
      await expect(lines.nth(0)).toHaveText(`Azure DevOps token: created 09:02 · expires 17:02 (renewed)`);
      await expect(lines.nth(1)).toHaveText(`Azure DevOps token: created 15:02 · expires 23:02 (access added)`);
      await expect(lines.nth(2)).toHaveText(`Azure DevOps token: created 16:00 · expires 23:30 · revoked 16:40 (run ended)`);
      await expect(tokens.getByRole("alert")).toHaveText(ADO_PAT.RUN_REVOKE_FAILED(clock(t(23, 30))));
    } finally {
      clearRunTokens(runId);
    }
  });

  test("a run that holds no token draws no Azure DevOps token list", async ({ page }) => {
    const runId = sql("SELECT id FROM agent_runs ORDER BY created_at LIMIT 1");
    clearRunTokens(runId);
    await gotoConsole(page);
    // The daemon's own answer for a run that holds none: an empty list.
    const answered = page.waitForResponse((r) => r.url().includes(`/api/v1/runs/${runId}/ado-tokens`));
    await navToRoute(page, `/runs/${runId}`);
    const res = await answered;
    expect(res.status()).toBe(200);
    expect(await res.json()).toEqual([]);
    await expect(page.getByTestId("ado-run-tokens")).toHaveCount(0);
  });

  test("the approval card adds the token line only for a run whose token Wardyn creates", async ({ page }) => {
    const runId = sql("SELECT id FROM agent_runs ORDER BY created_at LIMIT 1");
    const row = {
      id: "e2e-ado-pat-escalation",
      run_id: runId,
      grant_id: "e2e-grant-1",
      kind: "tool_call",
      requested_scope: {
        lane: "azure_devops",
        provider_id: "e2e-row-1",
        org: "acme",
        grant_id: "e2e-grant-1",
        capability: "code_write",
        repo: "payments-api",
        ref_class: "",
        tool: "Azure DevOps",
        cmd: "Push commits (code_write) in acme/payments-api",
      },
      state: "PENDING",
      requested_at: new Date().toISOString(),
    };
    await page.route("**/api/v1/approvals*", async (route) => {
      if (route.request().method() !== "GET") return route.continue();
      await route.fulfill({ json: [row] });
    });
    clearRunTokens(runId);
    try {
      seedRunTokens(runId, [{ createdAt: new Date(), validTo: new Date(Date.now() + 3_600_000) }]);
      await gotoConsole(page);
      await navToRoute(page, `/runs/${runId}`);
      await page.getByRole("tab", { name: /Approvals/ }).click();
      const card = page.getByTestId("ado-capability-card");
      await expect(card).toBeVisible();
      await expect(card.getByText(ADO_PAT.APPROVAL_WIDENS)).toBeVisible();

      clearRunTokens(runId);
      await page.reload();
      await page.getByRole("tab", { name: /Approvals/ }).click();
      await expect(page.getByTestId("ado-capability-card")).toBeVisible();
      await expect(page.getByText(ADO_PAT.APPROVAL_WIDENS)).toHaveCount(0);
    } finally {
      clearRunTokens(runId);
    }
  });
});

test.describe("no horizontal scroll at 390px", () => {
  test.use({ viewport: { width: 390, height: 844 } });

  test("the person's card, the token dialog, New Run and a run page", async ({ page }) => {
    const runId = sql("SELECT id FROM agent_runs ORDER BY created_at LIMIT 1");
    let access: Record<string, unknown> = {
      token_mode: "own_pat",
      max_days: 30,
      token_scopes: ["Code (Read & write)", "Project and Team (Read)", "Work Items (Read)"],
      state: "expired_signin",
      cause: "token_expired",
    };
    await spliceAccess(page, () => ({ default_profile: ["code_read"], ...access }));
    clearRunTokens(runId);
    seedRunTokens(runId, [
      { createdAt: new Date(2000, 8, 29, 9, 2), validTo: new Date(2000, 8, 29, 17, 2), revokedAt: new Date(2000, 8, 29, 9, 41), reason: "run_end", lastError: "revoke refused" },
    ]);
    // The sidebar is collapsed at this width, so go straight to each page.
    await page.goto("/account");
    await expandCard(page, "Azure DevOps");
    expect(await noHorizontalScroll(page)).toBe(true);
    await page.getByRole("button", { name: ADO_PAT.OWN_ADD_CTA }).click();
    await expect(page.getByRole("dialog", { name: ADO_PAT.OWN_DIALOG_TITLE })).toBeVisible();
    expect(await noHorizontalScroll(page)).toBe(true);
    await page.getByRole("button", { name: ADO_PAT.OWN_DIALOG_CANCEL }).click();

    access = { token_mode: "minted_pat", state: "not_configured" };
    await page.goto("/runs/new");
    await page.getByRole("button", { name: /^Reuse a saved policy/ }).click();
    await expect(page.getByTestId("ado-launch-note")).toBeVisible();
    expect(await noHorizontalScroll(page)).toBe(true);

    await page.goto(`/runs/${runId}`);
    await expect(page.getByTestId("ado-run-tokens")).toBeVisible();
    expect(await noHorizontalScroll(page)).toBe(true);
    clearRunTokens(runId);
  });
});
