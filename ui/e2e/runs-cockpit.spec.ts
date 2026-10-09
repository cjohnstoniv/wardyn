/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { randomUUID } from "node:crypto";
import { test, expect, ADMIN_TOKEN, gotoConsole, navTo, sql } from "./fixtures";
import { RUN_COCKPIT, UI_APPS_LANE } from "../src/app/components/wardyn/copy";
import { MODEL_ACCESS_BANNER, MODEL_ACCESS_RUN_DOOR } from "../src/app/components/wardyn/model-access-copy";
import { AGENTS } from "../src/app/lib/workspace-providers-copy";
import { CONNECTIONS } from "../src/app/components/wardyn/copy/door";
import type { Page } from "@playwright/test";

// Split out of runs.spec.ts (#209): the run cockpit's own widget behaviour —
// the Add-widget catalog, the failure block's sizing, the attach card's
// /healthz handling, focus mode's Escape key, and the model-credential
// refusal door — against the seeded 9-fixture backend runs.spec.ts's own top
// comment maps out (fixture N -> state). The focus-mode case forces the
// oldest seeded run to RUNNING and does not restore it — it deletes only the
// approval row it inserts; nothing else in this file mutates state at all.
test.describe.configure({ mode: "serial" });

// Open the Runs screen and wait for the seeded page to render. PENDING
// fixture 0 is always live, so it always renders (as "Queued" now — #1197 D2
// folds PENDING into the Running section rather than a "Pending" badge).
async function openRuns(page: Page): Promise<void> {
  await gotoConsole(page);
  await navTo(page, "Runs");
  await expect(page.getByRole("heading", { name: "Runs", level: 1 })).toBeVisible();
  await expect(page.getByText("e2e fixture 0")).toBeVisible();
}

// R4-F020: the Add-widget catalog enumerated the whole registry while the grid
// drew only what `available` admitted. On a FINISHED run the ssh widget can
// never render, yet the catalog still offered "Attach from your terminal" —
// one click ticked the row, ran addWidget and PUT the phantom placement to
// /api/v1/me/run-layout, and no tile came back. canvas.test.tsx pins the
// absence in jsdom; only a real browser proves nothing is PERSISTED.
test.describe("Run cockpit — the layout catalog offers no dead controls", () => {
  test("a finished run's catalog omits the ssh widget, and persists nothing", async ({ page }) => {
    const layoutWrites: string[] = [];
    page.on("request", (req) => {
      if (req.method() === "PUT" && new URL(req.url()).pathname === "/api/v1/me/run-layout") {
        layoutWrites.push(req.postData() ?? "");
      }
    });

    await openRuns(page);
    await page.getByText("e2e fixture 4").click(); // Completed
    await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{8,}/);
    await expect(page.getByRole("heading", { name: "e2e fixture 4", level: 1 })).toBeVisible();

    await page.getByRole("button", { name: "Edit layout" }).click();
    await page.getByRole("button", { name: "Add widget" }).click();
    const catalog = page.getByRole("dialog");
    await expect(catalog).toBeVisible();

    await expect(catalog.getByRole("button", { name: "Attach from your terminal" })).toHaveCount(0);
    // The entries that CAN render are still offered.
    await expect(catalog.getByRole("button", { name: "Sandbox" })).toBeVisible();
    // Opening the catalog wrote nothing, and no phantom widget can have been
    // stored because none was offered. X2-F7: the old assertion
    // (`.every(...)` over an always-empty array) was vacuous — it passed
    // whether or not the catalog ever fired a write at all.
    expect(layoutWrites).toEqual([]);
  });
});

// R4-F142: the canvas tile's FILL rule was written for a widget that renders ONE
// root card (`[&>section]:flex-1`), and the terminal widget renders a FRAGMENT —
// the M7(b) failure block, then the pane. The rule therefore caught the failure
// block, a `shrink-0` <section> built to size to its content, and gave it
// `flex: 1 1 0%`: measured in Chromium at 271px of a 518px tile, half the replay
// pane gone on every KILLED/FAILED run. jsdom computes no layout, so the vitest
// pin can only assert the selector — this is the one that reads real pixels.
test.describe("Run cockpit — the failure block sizes to its content, not to half the tile", () => {
  test("a killed run keeps its replay pane", async ({ page }) => {
    await openRuns(page);
    await page.getByText("e2e fixture 7").click(); // KILLED — always gets the block
    await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{8,}/);
    await expect(page.getByRole("heading", { name: "e2e fixture 7", level: 1 })).toBeVisible();

    const block = page.getByTestId("run-failure-block");
    await expect(block).toBeVisible();

    const measured = await block.evaluate((el) => {
      const cs = getComputedStyle(el);
      const parent = el.parentElement as HTMLElement;
      return {
        flexGrow: cs.flexGrow,
        blockH: el.getBoundingClientRect().height,
        scrollH: el.scrollHeight,
        parentH: parent.getBoundingClientRect().height,
      };
    });

    // Not stretched...
    expect(measured.flexGrow).toBe("0");
    // ...and not stretched in effect either: it is as tall as its own content
    // (within a rounding pixel), never a fixed share of the tile.
    // CI-flake (the second site of the 300.28px class): getBoundingClientRect
    // returns a FRACTIONAL height while scrollHeight is integral, so a
    // sub-pixel of layout jitter (300.28 against 298 + 2) failed a property
    // that tolerates a fractional pixel and would still catch a genuine
    // flex-stretch of tens of pixels. Rounded, like the above-the-fold bound.
    expect(Math.round(measured.blockH)).toBeLessThanOrEqual(measured.scrollH + 2);
    // The replay pane below it is still rendered. (Not a share of the hero:
    // the widget body is the viewport's height, and a long failure detail can
    // legitimately be most of a 720px tile — the F142 property is the two
    // checks above, content-sized and never flex-stretched, measured in real
    // pixels; the ratio this asserted before was the author's viewport, not
    // the fix.)
    const replay = block.locator("xpath=following-sibling::*[1]");
    await expect(replay).toBeVisible();
    expect(measured.parentH).toBeGreaterThan(measured.blockH);
  });
});

// #1487: a partial kill says more (teardown unconfirmed, a scratch caveat, and a
// line per credential Wardyn cannot take back), so in a narrow tile it is taller
// than the tile. It scrolls inside itself rather than clipping the evidence and
// the audit button. The kill outcome is spliced: the hermetic backend cannot
// produce a failed kill row or a minted credential.
test.describe("Run cockpit — a long kill outcome scrolls instead of clipping", () => {
  test("a partial kill with credential lines keeps Open audit trail reachable", async ({ page }) => {
    const auth = { Authorization: `Bearer ${ADMIN_TOKEN}` };
    const list = (await (await page.request.get("api/v1/runs?limit=1000", { headers: auth })).json()) as { id: string; task: string }[];
    const id = list.find((r) => r.task === "e2e fixture 7")!.id;
    const at = new Date().toISOString();
    await page.route(/\/api\/v1\/audit\?/, async (route) => {
      const action = new URL(route.request().url()).searchParams.get("action");
      if (action === "run.kill") {
        return route.fulfill({ json: [{ id: "k1", time: at, actor_type: "human", actor: "sam@acme.io", action: "run.kill", outcome: "failure", data: { error: "runner_error: connection refused" } }] });
      }
      if (action === "credential.mint") {
        return route.fulfill({ json: [{ id: "m1", time: at, actor_type: "agent", actor: "x", action: "credential.mint", outcome: "success", data: { grant_id: "g1" } }, { id: "m2", time: at, actor_type: "agent", actor: "x", action: "credential.mint", outcome: "success", data: { grant_id: "g2" } }] });
      }
      return route.fallback();
    });
    await page.route(`**/api/v1/runs/${id}/grants`, (route) =>
      route.fulfill({
        json: [
          { id: "g1", created_at: at, spec: { kind: "github_token", scope: {} } },
          { id: "g2", created_at: at, spec: { kind: "git_pat", scope: { host: "dev.azure.com" } } },
          { id: "g3", created_at: at, spec: { kind: "env_secret", scope: {} } },
        ],
      }),
    );
    await page.setViewportSize({ width: 1280, height: 720 });
    await page.goto(`runs/${id}`);
    const block = page.getByTestId("run-failure-block");
    await expect(block).toContainText("teardown is not confirmed");
    await expect(block).toContainText("it stays live until you rotate it on dev.azure.com");
    expect(await block.evaluate((el) => getComputedStyle(el).overflowY)).toBe("auto");
    // The block is bounded by its tile (a share of it), so it never spills out
    // of the tile where overflow-hidden would clip the audit button: its own
    // scroll holds the overflow, and the button is reachable inside it.
    const fits = await block.evaluate((el) => {
      const tile = (el.parentElement as HTMLElement).getBoundingClientRect();
      const b = el.getBoundingClientRect();
      return { insideTile: b.bottom <= tile.bottom + 1 && b.top >= tile.top - 1, scrolls: el.scrollHeight > el.clientHeight };
    });
    expect(fits.insideTile).toBe(true);
    const audit = block.getByRole("button", { name: "Open audit trail" });
    await audit.scrollIntoViewIfNeeded();
    await expect(audit).toBeInViewport();
  });
});

// R4-F037: the attach card's OFF paragraphs are claims about the DEPLOYMENT
// ("Off on this deployment... an operator turns it on by setting
// WARDYN_SSH_LISTEN"). lib/api/health.ts swallows a non-ok /healthz into a
// resolved `{}`, so the card used to print both claims after a 503 and never
// re-check (the effect's deps never change on this page). The vitest pin drives
// the same code path with a stubbed fetch; this one drives a real browser
// against a real daemon whose /healthz is failing.
test.describe("Attach card — a failing /healthz claims nothing about the deployment", () => {
  test("prints neither lane's OFF copy while /healthz 503s", async ({ page }) => {
    await openRuns(page);
    // Installed BEFORE the run opens: the card asks once, on mount.
    await page.route("**/healthz", (route) =>
      route.fulfill({ status: 503, contentType: "application/json", body: '{"error":"unavailable"}' }),
    );

    await page.getByText("e2e fixture 2").click(); // RUNNING — the card's gate
    await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{8,}/);
    // The card still renders: the CLI lane needs no gateway at all.
    await expect(page.getByText("Attach from your terminal")).toBeVisible();
    await expect(page.getByText("Wardyn CLI")).toBeVisible();
    // ...and asserts nothing about the two lanes it got no answer for.
    await expect(page.getByText(/Off on this deployment/)).toHaveCount(0);
    // Both headings stay — the lanes exist, their state is simply unknown.
    await expect(page.getByText("UI apps")).toBeVisible();

    await page.unroute("**/healthz");
  });
});

// The card lives in a fixed-height canvas tile that clips. Its content (every
// lane) is taller than the tile, so the BODY must be the scroll container or
// the bottom lanes are cut off with no way to reach them. Real layout is the
// only proof: jsdom has none.
test.describe("Attach card — its body scrolls inside its tile", () => {
  test("the last lane is reachable by scrolling the card body", async ({ page }) => {
    await openRuns(page);
    // SSH on, so the card carries its full set of lanes and command blocks.
    await page.route("**/healthz", (route) =>
      route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          status: "ok",
          ssh: { enabled: true, advertise_addr: "ssh.example.test:2222", host_key_fingerprint: "SHA256:e2eFixture" },
        }),
      }),
    );

    await page.getByText("e2e fixture 2").click(); // RUNNING — the card's gate
    await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{8,}/);
    const heading = page.getByRole("heading", { name: "Attach from your terminal" });
    await expect(heading).toBeVisible();

    const card = heading.locator("xpath=ancestor::section[1]");
    const body = card.locator("xpath=./*[last()]");
    // The last thing in the card: the UI-apps lane's closing line.
    const last = body.getByText(UI_APPS_LANE.offDocPath);
    await expect(last).toHaveCount(1);

    const m = await body.evaluate((el) => ({
      scrollHeight: el.scrollHeight,
      clientHeight: el.clientHeight,
      overflowY: getComputedStyle(el).overflowY,
    }));
    expect(m.overflowY).toBe("auto");
    expect(m.scrollHeight).toBeGreaterThan(m.clientHeight);

    // Off the tile's bottom edge until scrolled, inside it after.
    const inside = async () => {
      const [l, c] = await Promise.all([last.boundingBox(), card.boundingBox()]);
      return !!l && !!c && l.y + l.height <= c.y + c.height + 1;
    };
    expect(await inside()).toBe(false);
    await body.evaluate((el) => el.scrollTo({ top: el.scrollHeight }));
    await expect.poll(inside).toBe(true);

    await page.unroute("**/healthz");
  });
});

// F1-F3 repro (verdict N — NEEDS-REPRO; verified/fixed only if this goes red):
// focus-mode.tsx's Escape handler and Radix's own AlertDialog dismiss are BOTH
// capture-phase listeners on `document` — stopPropagation on one does not
// suppress the other (only stopImmediatePropagation would), so the question is
// registration order, not interception. A live browser is the only way to
// settle it: does denying a held approval from inside focus mode, then
// pressing Escape, close only the dialog (fine) or also exit focus mode
// (remounts the terminal pane, dropping the live attach socket on an
// interactive run — R1-F1's F1-F3 finding).
test.describe("Focus mode — Escape inside a Deny confirm", () => {
  // ticket: F1-F3
  test("Escape closes the Deny dialog without also exiting focus mode", async ({ page }) => {
    const runId = sql("SELECT id FROM agent_runs ORDER BY created_at LIMIT 1");
    sql(`UPDATE agent_runs SET state = 'RUNNING' WHERE id = '${runId}'`);
    // tool_call is unconditionally "held" (isHeld) — LiveApprovals renders its
    // decision strip, with a Deny button, in the terminal pane regardless of
    // wait_for_review timing/mode.
    const approvalId = randomUUID();
    sql(
      `INSERT INTO approvals (id, run_id, kind, requested_scope, state, requested_at)
       VALUES ('${approvalId}','${runId}','tool_call','{"tool":"f1f3-repro.exec"}'::jsonb,'PENDING',now())`,
    );

    try {
      await page.goto(`/runs/${runId}`);
      await expect(page.getByText("Running", { exact: true }).first()).toBeVisible();

      await page.getByRole("button", { name: RUN_COCKPIT.enterFocus }).click();
      await expect(page.getByRole("button", { name: RUN_COCKPIT.exitFocus })).toBeVisible();
      // The dock opens on the first widget (Egress) by default — its glass
      // panel overlaps the terminal pane's own LiveApprovals strip. Close it;
      // the repro is about Escape vs. the Deny dialog, not the dock.
      await page.getByRole("button", { name: RUN_COCKPIT.closeDock }).click();

      await page.getByRole("button", { name: "Deny", exact: true }).click();
      const dialog = page.getByRole("alertdialog");
      await expect(dialog).toBeVisible();

      await page.keyboard.press("Escape");

      // The dialog is gone (Radix's own Escape-dismiss did its job)...
      await expect(dialog).not.toBeVisible();
      // ...and focus mode is STILL the active surface — the SAME keypress
      // must not have ALSO fired focus mode's onExit.
      await expect(page.getByRole("button", { name: RUN_COCKPIT.exitFocus })).toBeVisible();
    } finally {
      sql(`DELETE FROM approvals WHERE id = '${approvalId}'`);
    }
  });
});

// ── 0.7.6 Finding 3 — "the failure names a destination instead of being one" ──
//
// The dispatch-time model-credential refusal stamps `reason` and the
// `provider` it is about on the run.create/failure row it already wrote; the
// console grades that ending `credential` and puts that provider's sign-in
// under the server's own sentence.
//
// Harness ceiling, the same one model-access-banner.spec.ts opens with: the
// seeded backend authenticates with a bare admin bearer token, has no per-user
// AWS session to grade and no run that reached dispatch with a dead credential —
// so the trail row, the provider access and the viewer's own subject are spliced.
// What only a browser proves is what is spliced here and asserted below: that
// this ending reaches the failure block as prose PLUS a door, on the run page,
// with no second "Sign in to AWS" beside it. The real refusal, from a real
// per-user AWS session that lapsed, is live case J (lane e2e-sso-path).
const CREDENTIAL_BEDROCK = {
  id: "bedrock-prod",
  name: "Bedrock (prod)",
  kind: "bedrock_sso",
  harnesses: ["claude-code"],
  default_for: ["claude-code"],
  host: "bedrock-runtime.us-east-1.amazonaws.com",
};
const CREDENTIAL_REFUSAL = `This run's model provider is ${CREDENTIAL_BEDROCK.name}, and your AWS sign-in for it can no longer be renewed — connect it from Getting started in the console, or from the banner the console shows on every page. Wardyn does not substitute a different model provider.`;
const CREDENTIAL_VIEWER = "alice@corp.example";

/** The viewer's own subject, so `created_by === principal` can be true of a
 *  seeded run: the door is the VIEWER's credential and an admin reading
 *  somebody else's failed run is not offered it. */
async function mockPrincipal(page: Page, principal: string): Promise<void> {
  await page.route("**/api/v1/me", async (route) => {
    const response = await route.fetch();
    const json = await response.json();
    json.principal = principal;
    await route.fulfill({ response, json });
  });
}

/** A lapsed AWS sign-in for the run's own Bedrock provider, cached and served
 *  (the landing redirect, the shell poll and the block's own refresh all hit
 *  this endpoint). */
async function mockActionableModelAccess(page: Page): Promise<void> {
  let cached: Record<string, unknown> | null = null;
  await page.route("**/api/v1/setup/status*", async (route) => {
    if (!cached) {
      const body = (await (await route.fetch()).json()) as Record<string, unknown>;
      body.model_providers = [CREDENTIAL_BEDROCK];
      body.provider_access = [{ provider: CREDENTIAL_BEDROCK.id, state: "expired_signin", action: AGENTS.SIGN_IN_AWS }];
      cached = body;
    }
    await route.fulfill({ json: cached! });
  });
}

test.describe("a run refused for a model credential carries the sign-in, not directions to it", () => {
  test("the failure block states the server's sentence and opens the AWS sign-in in place", async ({ page }) => {
    await mockPrincipal(page, CREDENTIAL_VIEWER);
    await mockActionableModelAccess(page);
    // The refused run: the server's sentence on the row (failure_hint), and the
    // viewer as its creator.
    await page.route("**/api/v1/runs/*", async (route) => {
      if (route.request().method() !== "GET") return route.fallback();
      const response = await route.fetch();
      const json = await response.json();
      if (json.task === "e2e fixture 6") {
        json.failure_hint = CREDENTIAL_REFUSAL;
        json.created_by = CREDENTIAL_VIEWER;
      }
      await route.fulfill({ response, json });
    });
    // …and the class, on the run.create/failure row the refusal writes.
    await page.route("**/api/v1/audit*", async (route) => {
      const response = await route.fetch();
      const rows = (await response.json()) as Record<string, unknown>[];
      if (!Array.isArray(rows) || rows.length === 0) return route.fulfill({ response, json: rows });
      const runID = rows[0].run_id;
      rows.unshift({
        id: randomUUID(),
        time: new Date().toISOString(),
        run_id: runID,
        actor_type: "system",
        actor: "wardynd",
        action: "run.create",
        target: String(runID),
        outcome: "failure",
        data: { error: CREDENTIAL_REFUSAL, reason: "model_credential", provider: CREDENTIAL_BEDROCK.id, kind: CREDENTIAL_BEDROCK.kind },
      });
      await route.fulfill({ response, json: rows });
    });

    await openRuns(page);
    await page.getByText("e2e fixture 6").click();
    await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{8,}/);

    const block = page.getByTestId("run-failure-block");
    await expect(block).toHaveAttribute("data-ending", "credential");
    // The SERVER's sentence, once, unchanged — no copy of ours restating it.
    await expect(block.getByText(CREDENTIAL_REFUSAL)).toHaveCount(1);
    await expect(block.getByText(MODEL_ACCESS_RUN_DOOR.NOTE)).toBeVisible();

    // ONE "Sign in to AWS" on the page: the block owns the door while it has
    // one, so the shell strip keeps its sentence and drops its button.
    await expect(page.getByText(CONNECTIONS.C6_LINE(CREDENTIAL_BEDROCK.name))).toBeVisible();
    await expect(page.getByRole("button", { name: AGENTS.SIGN_IN_AWS, exact: true })).toHaveCount(0);

    // A DOOR, not a signpost: the sign-in opens here, on the run's own page.
    const before = new URL(page.url()).pathname;
    // A click that lands while the block re-renders after its credential
    // refresh is lost, so click again until the door answers. Each pass clicks
    // only while the door is still CLOSED: an open one is a modal that
    // swallows the click, so re-clicking would throw inside the retry and the
    // visibility check would never run.
    const doorHeading = page.getByRole("heading", { name: MODEL_ACCESS_BANNER.DIALOG_TITLE });
    await expect(async () => {
      if ((await doorHeading.count()) === 0) {
        await block.getByRole("button", { name: MODEL_ACCESS_RUN_DOOR.SIGN_IN_ARIA }).click({ timeout: 2_000 });
      }
      await expect(doorHeading).toBeVisible({ timeout: 2_000 });
    }).toPass({ timeout: 20_000 });
    await expect(page.getByTestId("harness-login-pane")).toBeVisible();
    expect(new URL(page.url()).pathname).toBe(before);

    await page.keyboard.press("Escape");
    await expect(page.getByRole("heading", { name: MODEL_ACCESS_BANNER.DIALOG_TITLE })).toHaveCount(0);
  });
});
