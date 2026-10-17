/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { randomUUID } from "node:crypto";
import { test, expect, ADMIN_TOKEN, gotoConsole, mockMemberRole, navTo, navToRoute, sql } from "./fixtures";
import { CHANGE_HEADING, POLICY_TAB, SUMMARY } from "../src/app/components/screens/run-detail/policy-tab-copy";
import { GIT_PAT_SCOPE } from "../src/app/components/wardyn/copy";
import type { Page } from "@playwright/test";

// E2E coverage for "Make a policy from this run" (X2-F6) — run-detail.tsx's
// Audit tab button opens profile-review.tsx's ProfileReview sheet
// (POST /runs/{id}/profile/synthesize), and its Save dialog persists the synthesized
// inline_policy via POST /policies (profile-review.tsx's SavePolicyDialog).
// Had zero e2e — this proves the real round trip: the saved policy is a REAL
// row the /policies screen lists, not just a client-side success toast.
const POLICY_NAME = `e2e-run-profile-${Date.now()}`;
const auth = { Authorization: `Bearer ${ADMIN_TOKEN}` };

test.describe("Run detail — Make a policy from this run", () => {
  // ticket: X2-F6
  // Fix pass (review F4): this file's own backend is fresh per run-ui-e2e.sh
  // invocation, but policies.spec.ts's file-wide invariant (a clean policy
  // table, so its empty-state specs hold) only survives a plain
  // `pnpm playwright test` over ONE shared backend if every mutating spec
  // cleans up after itself — this one didn't. `request`, not `page`: an
  // afterAll hook runs at worker scope and cannot use the test-scoped `page`
  // fixture.
  test.afterAll(async ({ request }) => {
    const res = await request.get("/api/v1/policies", { headers: auth });
    const policies: Array<{ id: string; name: string }> = await res.json();
    const created = policies.find((p) => p.name === POLICY_NAME);
    // Review N2: the delete is asserted, not fire-and-forget — a 403/404 here
    // would otherwise be the silent leak this hook exists to prevent.
    if (created) expect((await request.delete(`/api/v1/policies/${created.id}`, { headers: auth })).ok()).toBe(true);
  });

  test("saves a real policy that appears on /policies", async ({ page }) => {
    await gotoConsole(page, "admin");
    await navTo(page, "Runs");
    await expect(page.getByText("e2e fixture 4")).toBeVisible();
    await page.getByText("e2e fixture 4").click();
    await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{8,}/);

    await page.getByRole("tab", { name: "Audit" }).click();
    await page.getByRole("button", { name: "Make a policy from this run" }).click();

    const sheet = page.getByRole("dialog").filter({ hasText: "Synthesized profile" });
    await expect(sheet).toBeVisible();
    await expect(sheet.getByText("Overall risk")).toBeVisible();

    await sheet.getByRole("button", { name: /^Save as policy$/ }).click();
    // Scoped by the dialog's DESCRIPTION, not its "Save as policy" title —
    // the sheet behind it stays mounted and its own trigger button carries
    // that exact same string, which would make a title-scoped locator
    // ambiguous (two dialogs open at once).
    const saveDialog = page.getByRole("dialog").filter({ hasText: "Persist the synthesized inline_policy" });
    await expect(saveDialog).toBeVisible();
    await saveDialog.getByLabel("Policy name").fill(POLICY_NAME);
    await saveDialog.getByRole("button", { name: "Save policy" }).click();

    // Both the name dialog and the sheet behind it close on a successful save
    // (profile-review.tsx's onSaved calls onClose too).
    await expect(saveDialog).toHaveCount(0);
    await expect(sheet).toHaveCount(0);

    // A run row links its User-view path; the Admin view's own run links
    // arrive with the admin run monitor (M-7), so reach Policies by its path.
    await navToRoute(page, "/admin/policies");
    await expect(page.getByRole("heading", { name: "Policies", level: 1 })).toBeVisible();
    await expect(
      page.getByRole("table").getByRole("row").filter({ hasText: POLICY_NAME }),
    ).toBeVisible();
  });
});

// ---------------------------------------------------------------------------
// The run page's Policy tab (#1425): the policy a run actually got, where it
// started from, and what launch changed. GET /runs/{id}/policy is spliced whole
// (route.fulfill): the seeded backend's runs never dispatch, so none carries the
// run.policy.resolve row the real read is built from, and the harness's operator
// token cannot reach the member-tier redaction either — the same reason
// governance-member.spec.ts splices its member views. The server side (source,
// changes, redaction, stored_policy_now) is pinned in Go.
// ---------------------------------------------------------------------------
const SAVED = "e2e-run-policy";

const RECORDED = {
  state: "recorded",
  source: { kind: "stored", policy_id: "3f1c9a2e-7d44-4b1e-9a0c-52e8b6d1f0a7", name: SAVED },
  spec: {
    allowed_domains: ["api.anthropic.com", "e2e-policy.example", "registry.npmjs.org"],
    first_use_approval: "deny_with_review",
    min_confinement_class: "CC1",
  },
  redacted: false,
  changes: [{ cause: "workspace", field: "allowed_domains", added: ["registry.npmjs.org"] }],
  complete: true,
  stored_policy_now: { state: "changed", name: SAVED },
};

// Answers the run's policy read with `body`, and counts the reads so a test can
// assert none was made before the tab opened.
async function spliceRunPolicy(page: Page, body: Record<string, unknown>): Promise<{ reads: () => number }> {
  let reads = 0;
  await page.route("**/api/v1/runs/*/policy", async (route) => {
    if (route.request().method() !== "GET") return route.fallback();
    reads++;
    const runId = route.request().url().match(/\/runs\/([^/]+)\/policy/)?.[1] ?? "";
    await route.fulfill({ json: { run_id: runId, ...body } });
  });
  return { reads: () => reads };
}

async function openFixture(page: Page, task: string): Promise<void> {
  await gotoConsole(page);
  await navTo(page, "Runs");
  await expect(page.getByText(task)).toBeVisible();
  await page.getByText(task).click();
  await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{8,}/);
}

test.describe("Run detail — the Policy tab (#1425)", () => {
  // The real flow, with no splice on the policy read: a saved policy, a run
  // launched from it (its run.create row carries a real policy_source), the
  // envelope dispatch would write (the `none` runner never dispatches, so
  // it goes in with sql() exactly as scripts/e2e-backend.sh seeds ui_apps),
  // and then an edit of the saved policy. The tab must still show what the run
  // got, with the server's own "changed" verdict.
  test("shows what the run got after its saved policy was edited, and Copy YAML puts exactly that on the clipboard", async ({
    page,
    context,
    request,
  }) => {
    // Chromium refuses navigator.clipboard.writeText without this (providers.spec.ts).
    await context.grantPermissions(["clipboard-read", "clipboard-write"]);
    const stamp = randomUUID().slice(0, 8);
    const name = `e2e-run-policy-${stamp}`;
    const task = `run policy view ${stamp}`;
    const spec = {
      allowed_domains: ["api.anthropic.com", "e2e-policy.example"],
      first_use_approval: "deny_with_review",
      min_confinement_class: "CC1",
    };
    const created = await request.post("/api/v1/policies", { headers: auth, data: { name, spec } });
    expect(created.status(), await created.text()).toBe(201);
    const policyId = ((await created.json()) as { id: string }).id;
    try {
      const launched = await request.post("/api/v1/runs", {
        headers: auth,
        data: { agent: "claude-code", repo: "acme/widgets", task, policy_id: policyId },
      });
      expect(launched.status(), await launched.text()).toBe(201);
      const runId = sql(`SELECT id FROM agent_runs WHERE task = '${task}' ORDER BY created_at DESC LIMIT 1`);

      const resolved = JSON.stringify({
        ...spec,
        allowed_domains: [...spec.allowed_domains, "registry.npmjs.org"],
      });
      const workspaceAdd = JSON.stringify({ kind: "workspace", added_domains: ["registry.npmjs.org"] });
      sql(
        `SELECT audit_append(gen_random_uuid(), now(), '${runId}', 'system', 'wardynd', 'run.egress.add', '${runId}', 'success', '', '${workspaceAdd}'::jsonb), ` +
          `audit_append(gen_random_uuid(), now(), '${runId}', 'system', 'wardynd', 'run.policy.resolve', '${runId}', 'success', '', '${resolved}'::jsonb)`,
      );
      const edited = await request.put(`/api/v1/policies/${policyId}`, {
        headers: auth,
        data: { name, spec: { ...spec, allowed_domains: ["api.anthropic.com"] } },
      });
      expect(edited.status(), await edited.text()).toBe(200);

      await gotoConsole(page);
      await navToRoute(page, `/runs/${runId}`);

      // The tab is between Approvals and Audit, and nothing was read yet.
      await expect(page.getByRole("tab", { name: POLICY_TAB.tab })).toBeVisible();
      const tabs = await page.getByRole("tab").allTextContents();
      expect(tabs.map((t) => t.trim()).slice(1, 4)).toEqual(["Approvals", POLICY_TAB.tab, "Audit"]);
      let reads = 0;
      page.on("request", (r) => {
        if (/\/runs\/[^/]+\/policy$/.test(r.url())) reads++;
      });
      expect(reads).toBe(0);

      // The identity rail's Policy row is a View link that opens the tab.
      await page.getByRole("button", { name: "View", exact: true }).click();
      await expect(page.getByRole("tab", { name: POLICY_TAB.tab })).toHaveAttribute("data-state", "active");
      const tab = page.getByTestId("run-policy-tab");
      await expect(tab).toBeVisible();

      await expect(tab.getByText(POLICY_TAB.sourceStored(name))).toBeVisible();
      await expect(page.getByTestId("policy-since-banner")).toHaveText(POLICY_TAB.changedSince(name));

      // The host the workspace added sits under its own heading, marked as
      // added; the host the saved policy has since lost is still what this run got.
      const group = page.getByRole("heading", { name: CHANGE_HEADING.workspace }).locator("..");
      await expect(group.getByText("registry.npmjs.org")).toBeVisible();
      await expect(group).toContainText(POLICY_TAB.chipAdded);
      await expect(tab.getByText("e2e-policy.example")).toBeVisible();
      await expect(tab.getByText(POLICY_TAB.scope)).toBeVisible();
      expect(reads).toBe(1);

      await tab.getByRole("button", { name: POLICY_TAB.viewYaml, exact: true }).click();
      const block = tab.locator("pre");
      await expect(block).toContainText("e2e-policy.example");
      await expect(block).toContainText("registry.npmjs.org");

      await tab.getByRole("button", { name: POLICY_TAB.copyYaml, exact: true }).click();
      await expect(tab.getByText("Copied")).toBeAttached();
      const copied = await page.evaluate(() => navigator.clipboard.readText());
      const shown = (await block.locator("code > div").allTextContents()).join("\n");
      expect(copied).toBe(shown);
      expect(copied).toMatch(/^allowed_domains:\n {2}- api\.anthropic\.com\n {2}- e2e-policy\.example\n {2}- registry\.npmjs\.org\n/);
    } finally {
      expect((await request.delete(`/api/v1/policies/${policyId}`, { headers: auth })).ok()).toBe(true);
    }
  });

  // The narrowing the run got on its stored token: chips, the repositories, and
  // the honesty sentence on the chip. The resolved spec is spliced like the
  // member read below (the seeded runs never dispatch).
  test("shows a stored token's narrowing, and says every repository when it has none", async ({ page }) => {
    await spliceRunPolicy(page, {
      ...RECORDED,
      changes: [],
      stored_policy_now: undefined,
      spec: {
        ...RECORDED.spec,
        eligible_grants: [
          {
            kind: "git_pat",
            requires_approval: false,
            scope: {
              host: "gitlab.example.com",
              secret_name: "git-pat-gitlab-example-com",
              forge: "gitlab",
              repos: ["group/app", "group/libs/*"],
              access: "read",
              api: true,
            },
          },
          { kind: "git_pat", requires_approval: false, scope: { host: "git.example.com", secret_name: "git-pat-git-example-com" } },
        ],
      },
    });
    await openFixture(page, "e2e fixture 2");
    await page.getByRole("tab", { name: POLICY_TAB.tab }).click();
    const tab = page.getByTestId("run-policy-tab");

    // One "Git access token" title; each grant is its host line, then what it is narrowed to.
    await expect(tab.getByText(SUMMARY.grantKinds.git_pat, { exact: true })).toHaveCount(1);
    const narrowed = tab.locator("li").filter({ hasText: "gitlab.example.com" });
    await expect(narrowed.getByText(SUMMARY.readOnly, { exact: true })).toBeVisible();
    await expect(narrowed.getByText(GIT_PAT_SCOPE.RUN_API, { exact: true })).toBeVisible();
    await expect(tab.getByText("group/app, group/libs/*", { exact: true })).toBeVisible();
    await expect(narrowed.getByText(SUMMARY.readOnly, { exact: true }).locator("xpath=ancestor::span[@title][1]")).toHaveAttribute(
      "title",
      GIT_PAT_SCOPE.HONESTY_TOKEN,
    );

    const open = tab.locator("li").filter({ hasText: "git.example.com" });
    await expect(tab.getByText(GIT_PAT_SCOPE.RUN_REPOS_ALL, { exact: true })).toBeVisible();
    await expect(open.getByText(SUMMARY.readOnly, { exact: true })).toHaveCount(0);
  });

  test("a run that stopped before its sandbox was set up says no policy was applied", async ({ page }) => {
    await spliceRunPolicy(page, { state: "never", source: { kind: "inline" }, redacted: false, changes: [], complete: true });
    await openFixture(page, "e2e fixture 6");
    await page.getByRole("tab", { name: POLICY_TAB.tab }).click();
    const tab = page.getByTestId("run-policy-tab");
    await expect(tab.getByText(POLICY_TAB.never)).toBeVisible();
    await expect(tab.getByText(POLICY_TAB.sourceInline)).toBeVisible();
    await expect(tab.getByRole("button", { name: POLICY_TAB.copyYaml })).toHaveCount(0);
  });

  // The admin-token harness cannot reach the member path (the redaction is
  // server-side on !isSecurityOperator), so the member's read is spliced, as
  // governance-member.spec.ts does for the ceiling: the served body carries
  // `redacted` and a `<redacted>` folder source, exactly as the server writes them.
  test("a member sees Hidden for a folder source, and S-33 above the YAML", async ({ page }) => {
    await mockMemberRole(page);
    await spliceRunPolicy(page, {
      ...RECORDED,
      redacted: true,
      changes: [],
      stored_policy_now: undefined,
      spec: {
        ...RECORDED.spec,
        workspace_mounts: [{ source: "<redacted>", target: "/home/agent/work/shared", read_only: true }],
      },
    });
    await openFixture(page, "e2e fixture 2");
    await page.getByRole("tab", { name: POLICY_TAB.tab }).click();
    const tab = page.getByTestId("run-policy-tab");

    await expect(tab.getByRole("note", { name: `${POLICY_TAB.hidden}. ${POLICY_TAB.hiddenTip}` })).toHaveAttribute(
      "title",
      POLICY_TAB.hiddenTip,
    );
    await expect(tab.getByText("/home/agent/work/shared")).toBeVisible();
    await expect(tab).not.toContainText("<redacted>");
    await expect(tab.getByText(POLICY_TAB.redacted)).toHaveCount(0);

    await tab.getByRole("button", { name: POLICY_TAB.viewYaml, exact: true }).click();
    await expect(tab.getByText(POLICY_TAB.redacted)).toBeVisible();
    await expect(tab.locator("pre")).toContainText("<redacted>");
  });
});
