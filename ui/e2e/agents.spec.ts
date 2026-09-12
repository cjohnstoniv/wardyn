/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import {
  test,
  expect,
  ADMIN_TOKEN,
  gotoConsole,
  mockMemberRole,
  navToRoute,
} from "./fixtures";
import { AGENTS, PROVIDERS } from "../src/app/lib/workspace-providers-copy";
import type { Page } from "@playwright/test";

// ---------------------------------------------------------------------------
// Agents tab / agent-enablement e2e (0.7.2, §5c.9) — lane: agents, port 8088,
// db wardyn_e2e. DEFERRED at E1's kickoff until C-UI's Agents tab
// (agents-tab.tsx, agent-picker.tsx, member-getting-started.tsx's Model access
// chip, run-detail's Effective policy widget) merged into feat/v0.7.2 —
// written after that merge (309a1929 + the 198b94dd review fix pass), against
// the tree those commits produced.
//
// BROWSER VS API, and why:
//   * The Agents tab's own write walk (enable/mechanism/credential-source/
//     save/reload) is REAL — GET/PUT /agent-providers are operatorOnly routes
//     this harness's admin bearer reaches, so it writes to Postgres and reads
//     its own write back.
//   * The DECLARED-MECHANISM 422 (enforceCreateLLMMechanism, runs.go:220) is
//     ALSO real: this harness has no Bedrock credential of any kind
//     configured, so declaring claude-code's mechanism `bedrock_bearer` and
//     launching genuinely resolves to "is not configured" — no stub involved.
//   * The disabled-agent 422 (agentRosterRefusal) is real for the same
//     reason: no isOperator exemption exists in that function.
//   * What CANNOT be real here: a genuine per-user AWS SSO sign-in (no IdP in
//     this harness — the file header repeated across drives.spec.ts/
//     governance.spec.ts's own documented ceiling) and the {ts} "expired"
//     variant of the declared-mechanism sentence, which fires only for a
//     CAPTURED-then-expired AWS SSO session (bedrockAuth.ssoRefreshFailure) —
//     there is no captured session to expire. Both are spliced (route.fetch()
//     + patch + refulfill, the mockMemberRole technique) to prove the
//     CLIENT's render only; the mechanics themselves are Go's, already unit-
//     tested (runs_dispatch_llm_mechanism_test.go, awssso_refresh_test.go per
//     a repo grep) and out of this lane's reach without a real IdP.
// ---------------------------------------------------------------------------

const auth = { Authorization: `Bearer ${ADMIN_TOKEN}` };

async function gotoAgentsTab(page: Page): Promise<void> {
  await gotoConsole(page);
  await navToRoute(page, "/settings");
  await page.getByTestId("providers-card").getByText(PROVIDERS.CARD_OPEN).click();
  await expect(page).toHaveURL(/\/providers$/);
  await page.getByRole("button", { name: AGENTS.AGENTS_TITLE }).click();
}

async function getAgentProviders(page: Page): Promise<{ providers: { agents?: unknown[] }; etag: string | null }> {
  const res = await page.request.get("/api/v1/agent-providers", { headers: auth });
  return { providers: await res.json(), etag: res.headers()["etag"] ?? null };
}

test.describe.configure({ mode: "serial" });

test.describe("agents — legacy open mode (no agent_providers row saved yet)", () => {
  test("both catalog agents render enabled, nothing marked unavailable in New Run's picker", async ({ page }) => {
    const before = await getAgentProviders(page);
    expect(before.providers.agents ?? []).toEqual([]);

    await gotoConsole(page);
    await page.getByRole("button", { name: "New run" }).click();
    await page.getByRole("radio", { name: /^Autonomous/ }).click();
    const agentSelect = page.getByRole("combobox", { name: "Agent" });
    await agentSelect.click();
    await expect(page.getByRole("option", { name: "Claude Code" })).toBeEnabled();
    await expect(page.getByRole("option", { name: "Codex CLI" })).toBeEnabled();
    await expect(page.getByText(AGENTS.UNAVAILABLE)).toHaveCount(0);
  });
});

test.describe("agents — the admin authoring walk (real writes, real reload)", () => {
  test.describe.configure({ mode: "serial" });

  test("enabling claude-code as bedrock_sso + per_user with a start URL persists", async ({ page }) => {
    await gotoAgentsTab(page);
    const row = page.getByTestId("agent-row-claude-code");
    await expect(row).toBeVisible();

    await row.getByRole("radio", { name: AGENTS.MECHANISM_BEDROCK_SSO }).click();
    await row.getByRole("button", { name: AGENTS.SOURCE_PER_USER }).click();
    // The Input under FIELD_SSO_START_URL carries no `id` (agents-tab.tsx),
    // so Field's label never associates with it — its accessible name falls
    // back to the placeholder. Located by that instead of getByLabel.
    await row.getByPlaceholder("https://my-org.awsapps.com/start").fill("https://acme.awsapps.com/start");
    await page.getByRole("button", { name: PROVIDERS.SAVE_CTA }).click();
    await expect(page.getByText(PROVIDERS.SAVE_ERROR)).toHaveCount(0);

    // Tab selection is local React state (providers-screen.tsx's `tab`), not
    // URL-carried — a reload always re-lands on the Git tab.
    await page.reload();
    await page.getByRole("button", { name: AGENTS.AGENTS_TITLE }).click();
    const reloaded = page.getByTestId("agent-row-claude-code");
    await expect(reloaded.getByRole("radio", { name: AGENTS.MECHANISM_BEDROCK_SSO })).toBeChecked();
    await expect(reloaded.getByPlaceholder("https://my-org.awsapps.com/start")).toHaveValue(
      "https://acme.awsapps.com/start",
    );

    const snap = await getAgentProviders(page);
    const claude = (snap.providers.agents as Array<Record<string, unknown>>).find((a) => a.id === "claude-code");
    expect(claude).toMatchObject({
      mechanism: "bedrock_sso",
      credential_source: "per_user",
      sso_start_url: "https://acme.awsapps.com/start",
    });
  });

  test("disabling codex-cli renders it unavailable in the New Run picker, and refuses a real launch naming it", async ({
    page,
  }) => {
    await gotoAgentsTab(page);
    const row = page.getByTestId("agent-row-codex-cli");
    await expect(row).toBeVisible();
    await row.getByRole("switch", { name: `${PROVIDERS.FIELD_ENABLED} — Codex CLI` }).click();
    await expect(row.getByText(AGENTS.AGENT_ROW_DISABLED_HINT)).toBeVisible();
    await page.getByRole("button", { name: PROVIDERS.SAVE_CTA }).click();
    await expect(page.getByText(PROVIDERS.SAVE_ERROR)).toHaveCount(0);

    // The picker: disabled, WITH its reason (never hidden).
    await gotoConsole(page);
    await page.getByRole("button", { name: "New run" }).click();
    await page.getByRole("radio", { name: /^Autonomous/ }).click();
    await page.getByRole("combobox", { name: "Agent" }).click();
    const codexOption = page.getByRole("option", { name: new RegExp(`Codex CLI.*${AGENTS.UNAVAILABLE}`) });
    await expect(codexOption).toBeVisible();
    await expect(codexOption).toHaveAttribute("aria-disabled", "true");
    await page.keyboard.press("Escape");

    // The real refusal — agentRosterRefusal, no operator exemption.
    const res = await page.request.post("/api/v1/runs", {
      headers: auth,
      data: { agent: "codex-cli", repo: "acme/widgets", title: "refused agent", task: "e2e agent refusal" },
    });
    expect(res.status()).toBe(422);
    const { error } = await res.json();
    expect(error).toBe('agent: "codex-cli" is not an enabled agent on this deployment — ask an admin');
  });

  test("a declared mechanism with nothing behind it refuses launch as 'not configured' (real, no Bedrock creds exist here)", async ({
    page,
  }) => {
    await gotoAgentsTab(page);
    const row = page.getByTestId("agent-row-claude-code");
    // Switch off per_user/SSO back to a lane this harness genuinely has zero
    // credential for: bedrock_bearer, with no bearer-key secret ever stored.
    await row.getByRole("radio", { name: AGENTS.MECHANISM_BEDROCK_BEARER }).click();
    await page.getByRole("button", { name: PROVIDERS.SAVE_CTA }).click();
    await expect(page.getByText(PROVIDERS.SAVE_ERROR)).toHaveCount(0);

    const res = await page.request.post("/api/v1/runs", {
      headers: auth,
      data: { agent: "claude-code", repo: "acme/widgets", title: "declared mechanism dead", task: "e2e mechanism gate" },
    });
    expect(res.status()).toBe(422);
    const { error } = await res.json();
    expect(error).toMatch(
      /^this run's model access is configured as Amazon Bedrock \(bearer key\), and that credential is not configured/,
    );
  });
});

// A launch whose 201 carries warnings[] (the C-UI review fix pass): the
// screen stays put, lists the warnings, and Launch becomes "Open run" — no
// toast, no timer, nothing navigates until that button is clicked. Spliced
// on the create response (route.fetch() + patch + refulfill): this harness's
// admin-token caller is never member-clamped for real (isOperator
// short-circuits governance resolution, drives.spec.ts's own documented
// reason), so a genuine 201-with-warnings needs a member session this
// harness cannot mint. The CLIENT behavior this pins is C-UI's; the clamping
// itself is B-α/A2's, Go-tested.
test.describe("agents — a 201 carrying warnings holds the screen (no timer)", () => {
  test("warnings render inline and Open run navigates only on click", async ({ page }) => {
    // The prior describe block left claude-code declared as bedrock_bearer
    // with nothing configured — a real launch would now itself 422 at the
    // declared-mechanism gate, before ever reaching the splice below. Back to
    // legacy open mode (no agent_providers row at all) so this test's own
    // launch succeeds for real, and the splice is the only thing standing in
    // for a member-clamped 201.
    await page.request.put("/api/v1/agent-providers", { headers: auth, data: { agents: [] } });

    await page.route("**/api/v1/runs", async (route) => {
      if (route.request().method() !== "POST") return route.fallback();
      const response = await route.fetch();
      const json = await response.json();
      json.warnings = ["Egress narrowed to api.anthropic.com by member policy."];
      await route.fulfill({ response, json });
    });

    await gotoConsole(page);
    await page.getByRole("button", { name: "New run" }).click();
    await page.getByLabel("Title").fill("e2e launch warnings");
    await page.getByRole("button", { name: "Launch run" }).click();

    await expect(page.getByText(AGENTS.LAUNCH_WARNING_TITLE)).toBeVisible();
    await expect(page.getByText("Egress narrowed to api.anthropic.com by member policy.")).toBeVisible();
    const openRun = page.getByRole("button", { name: AGENTS.OPEN_RUN_CTA });
    await expect(openRun).toBeVisible();
    // No navigation yet — still on /runs/new.
    await expect(page).toHaveURL(/\/runs\/new$/);
    await expect(page.getByRole("button", { name: "Launch run" })).toHaveCount(0);

    await openRun.click();
    await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{8,}/);
  });
});

// The Agents tab's own roster-unknown state (the C-UI review fix pass): no
// `harnesses` in SetupStatus reads as FETCH_FAILED, never a wipeable empty
// roster — Save is withheld either way. Real GET /setup/status always
// returns the static catalog (setupHarnessTools has no failure mode this
// harness can trigger), so both arms are spliced.
test.describe("agents — the roster-unknown and empty-roster states withhold Save", () => {
  test("an absent `harnesses` field renders FETCH_FAILED_* with no Save", async ({ page }) => {
    await page.route("**/api/v1/setup/status*", async (route) => {
      const response = await route.fetch();
      const json = await response.json();
      delete json.harnesses;
      await route.fulfill({ response, json });
    });
    await gotoAgentsTab(page);
    await expect(page.getByText(PROVIDERS.FETCH_FAILED_TITLE)).toBeVisible();
    await expect(page.getByRole("button", { name: PROVIDERS.SAVE_CTA })).toHaveCount(0);
  });

  test("a genuinely empty roster ([]) shows the lead, no rows, and no Save", async ({ page }) => {
    await page.route("**/api/v1/setup/status*", async (route) => {
      const response = await route.fetch();
      const json = await response.json();
      json.harnesses = [];
      await route.fulfill({ response, json });
    });
    await gotoAgentsTab(page);
    await expect(page.getByText(AGENTS.AGENTS_LEAD)).toBeVisible();
    await expect(page.getByTestId("agent-row-claude-code")).toHaveCount(0);
    await expect(page.getByRole("button", { name: PROVIDERS.SAVE_CTA })).toHaveCount(0);
  });
});

// The member's Getting Started "Model access" chip — six states
// (workspace-providers-prompt.md §7.7). Spliced onto GET /setup/status's
// model_access field (this harness has no per-user AWS session to produce
// any of these for real, per the file header). Only three states offer the
// member's own Sign in to AWS action.
test.describe("agents — member Getting Started's Model access chip (spliced states)", () => {
  const CASES: { state: string; chip: string; hasCta: boolean }[] = [
    { state: "live", chip: AGENTS.MODEL_ACCESS_LIVE, hasCta: false },
    { state: "expiring", chip: AGENTS.MODEL_ACCESS_EXPIRING, hasCta: true },
    { state: "expired_signin", chip: AGENTS.MODEL_ACCESS_EXPIRED, hasCta: true },
    { state: "not_configured", chip: AGENTS.MODEL_ACCESS_NOT_CONFIGURED, hasCta: true },
    { state: "shared_expired", chip: AGENTS.MODEL_ACCESS_SHARED_EXPIRED, hasCta: false },
  ];

  for (const c of CASES) {
    test(`model_access.state=${c.state} shows its chip${c.hasCta ? " and the Sign in to AWS action" : ", no action"}`, async ({
      page,
    }) => {
      await mockMemberRole(page);
      await page.route("**/api/v1/setup/status*", async (route) => {
        const response = await route.fetch();
        const json = await response.json();
        json.model_access = { state: c.state };
        await route.fulfill({ response, json });
      });
      await gotoConsole(page);
      await navToRoute(page, "/setup");
      await expect(page.getByRole("heading", { name: "What's set up for you" })).toBeVisible();
      await expect(page.getByText(c.chip)).toBeVisible();
      const cta = page.getByRole("button", { name: AGENTS.SIGN_IN_AWS });
      if (c.hasCta) {
        await expect(cta).toBeVisible();
      } else {
        await expect(cta).toHaveCount(0);
      }
    });
  }
});
