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
import { AGENTS, AGENTS_DRAFT, MODEL_ACCESS_CHIP_LABEL, PROVIDERS } from "../src/app/lib/workspace-providers-copy";
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

// Press Save and WAIT FOR THE WRITE — the console's own success toast, raised
// only once the PUT has resolved 200 (the handler writes inside the request, so
// a 200 IS a committed roster).
//
// This is a write BARRIER, and every happy-path save below needs one.
// `expect(...).toHaveCount(0)` on an error that has not happened yet is
// satisfied on its FIRST poll: it proves nothing about the in-flight PUT. The
// walk used it as if it did and then immediately POSTed /api/v1/runs — so when
// the PUT had not landed the dispatch was refused against the PREVIOUS test's
// roster, and the refusal named the AWS SSO lane instead of Amazon Bedrock
// (bearer key). It reads as "shared-Postgres contention" because load widens
// the window, but this file is serial: it is an ordering dependency plus a lost
// await. Mirrors providers.spec.ts's saveProviders, for the same race one
// screen over. NOT for the refusal tests — there is no success toast to wait
// for when the save is meant to be rejected.
async function saveAgents(page: Page): Promise<void> {
  await page.getByRole("button", { name: PROVIDERS.SAVE_CTA }).click();
  await expect(page.getByText(PROVIDERS.SAVED_TOAST)).toBeVisible();
  // Now meaningful — the save has SETTLED, so this says "it settled without a
  // refusal", not merely "no refusal has rendered yet".
  await expect(page.getByText(PROVIDERS.SAVE_ERROR)).toHaveCount(0);
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
    await row.getByRole("radio", { name: AGENTS.SOURCE_PER_USER }).click();
    await row.getByLabel(AGENTS.FIELD_SSO_START_URL).fill("https://acme.awsapps.com/start");
    await saveAgents(page);

    // Tab selection is local React state (providers-screen.tsx's `tab`), not
    // URL-carried — a reload always re-lands on the Git tab.
    await page.reload();
    await page.getByRole("button", { name: AGENTS.AGENTS_TITLE }).click();
    const reloaded = page.getByTestId("agent-row-claude-code");
    await expect(reloaded.getByRole("radio", { name: AGENTS.MECHANISM_BEDROCK_SSO })).toBeChecked();
    await expect(reloaded.getByLabel(AGENTS.FIELD_SSO_START_URL)).toHaveValue("https://acme.awsapps.com/start");

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
    await saveAgents(page);

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
    await saveAgents(page);

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

  // Appendix A finding 5: not_applicable carries NO label in
  // MODEL_ACCESS_CHIP_LABEL by design (the admin-token principal's own
  // answer — "this is a shared token, not a person") and NO action, so it is
  // outside the CASES table's shape above (every entry there has a real
  // chip). This harness's real llm_ready is TRUE deterministically, not
  // environmentally: this e2e daemon now declares a Bedrock lane (the
  // WARDYN_BEDROCK_REGION/_MODEL this file sets above), so `legacyIntegrations`
  // reports an `ai_provider` integration of kind bedrock regardless of the
  // host's own state — `computeLLMReady`'s AI-provider fallback returns true
  // on that alone, on a bare CI box too. So the correct render is the
  // deployment-wide fallback chip ("Model access · Provided by your admin")
  // — never one of the five server-driven AGENTS.MODEL_ACCESS_* labels, and
  // never the CTA a shared token cannot use.
  test("model_access.state=not_applicable falls back to the deployment chip, never the sign-in CTA", async ({
    page,
  }) => {
    await mockMemberRole(page);
    // Cache-and-serve rather than route.fetch()+refulfill per request: the
    // landing redirect (gotoConsole) and MemberGettingStarted's own mount can
    // both hit /setup/status, and a real round-trip PER match raced Playwright
    // disposing an in-flight route's response under load. One real fetch, then
    // every match (however many) is fulfilled from the cached body instead.
    let cached: Record<string, unknown> | null = null;
    await page.route("**/api/v1/setup/status*", async (route) => {
      if (!cached) {
        const response = await route.fetch();
        const body = await response.json();
        body.model_access = { state: "not_applicable" };
        cached = body;
      }
      // TS can't narrow a `let` captured by this closure across the `await`
      // above — the `if` guarantees it non-null by here.
      await route.fulfill({ json: cached! });
    });
    await gotoConsole(page);
    await navToRoute(page, "/setup");
    await expect(page.getByRole("heading", { name: "What's set up for you" })).toBeVisible();
    await expect(page.getByText("Model access · Provided by your admin")).toBeVisible();
    for (const label of Object.values(MODEL_ACCESS_CHIP_LABEL)) {
      await expect(page.getByText(label)).toHaveCount(0);
    }
    await expect(page.getByRole("button", { name: AGENTS.SIGN_IN_AWS })).toHaveCount(0);
  });
});

// Appendix A finding 4's other half — the roster pin. A real write/reload
// walk (like the per_user save test above) plus the server-400 refusal, both
// of which need a REAL model account on file to compare against
// (bedrockModelAccount/validateAgentSSOPin) — scripts/e2e-backend.sh sets
// WARDYN_BEDROCK_MODEL to a full ARN naming account 222222222222 for exactly
// this reason.
test.describe("agents — the roster pin (sso_account_id / sso_role_name)", () => {
  test.describe.configure({ mode: "serial" });

  // U2-04 (blind round 2, lens-U2): the SAME snapshot/restore the sibling
  // describe below carries, applied one describe earlier — where it was
  // missing. Both tests here save real rosters against the shared e2e daemon
  // (fullyParallel: every other spec FILE runs against it at the same time),
  // and without this the file ended with claude-code left declared
  // bedrock_sso + per_user + pinned. A persisted per_user claude-code row
  // makes the admin token's model_access `not_applicable` and can make
  // enforceCreateLLMMechanism refuse claude-code launches — i.e. it breaks
  // new-run.spec.ts, runs.spec.ts and the recording specs, from here.
  let rosterBefore: { agents?: unknown[] } | null = null;
  test.beforeEach(async ({ page }) => {
    rosterBefore = (await getAgentProviders(page)).providers;
  });
  test.afterEach(async ({ page }) => {
    if (rosterBefore) {
      const restore = await page.request.put("/api/v1/agent-providers", { headers: auth, data: rosterBefore });
      expect(restore.ok()).toBeTruthy();
      rosterBefore = null;
    }
  });

  test("a per_user row saves an account/role pin and re-renders it after a reload", async ({ page }) => {
    await gotoAgentsTab(page);
    const row = page.getByTestId("agent-row-claude-code");
    await expect(row).toBeVisible();

    await row.getByRole("radio", { name: AGENTS.MECHANISM_BEDROCK_SSO }).click();
    await row.getByRole("radio", { name: AGENTS.SOURCE_PER_USER }).click();
    await row.getByLabel(AGENTS.FIELD_SSO_START_URL).fill("https://acme.awsapps.com/start");
    await row.getByLabel(AGENTS_DRAFT.FIELD_SSO_ACCOUNT_ID).fill("222222222222");
    await row.getByLabel(AGENTS_DRAFT.FIELD_SSO_ROLE_NAME).fill("BedrockRunner");
    await saveAgents(page);
    await expect(page.getByText(PROVIDERS.SAVE_REFUSED_TITLE)).toHaveCount(0);

    await page.reload();
    await page.getByRole("button", { name: AGENTS.AGENTS_TITLE }).click();
    const reloaded = page.getByTestId("agent-row-claude-code");
    await expect(reloaded.getByLabel(AGENTS_DRAFT.FIELD_SSO_ACCOUNT_ID)).toHaveValue("222222222222");
    await expect(reloaded.getByLabel(AGENTS_DRAFT.FIELD_SSO_ROLE_NAME)).toHaveValue("BedrockRunner");

    const snap = await getAgentProviders(page);
    const claude = (snap.providers.agents as Array<Record<string, unknown>>).find((a) => a.id === "claude-code");
    expect(claude).toMatchObject({ sso_account_id: "222222222222", sso_role_name: "BedrockRunner" });
  });

  test("a pin whose account differs from the model ARN renders the SERVER's 400 body, writing nothing", async ({
    page,
  }) => {
    const before = await getAgentProviders(page);
    const beforeClaude = (before.providers.agents as Array<Record<string, unknown>>).find((a) => a.id === "claude-code");

    await gotoAgentsTab(page);
    const row = page.getByTestId("agent-row-claude-code");
    // U2-04: the afterEach above restores the roster this describe found on
    // arrival, so this test no longer inherits the previous one's SAVED
    // per_user row — it declares its own draft (unsaved: the pin fields only
    // render under a per_user credential source) and then asks the server to
    // refuse it. The before/after equality below is unaffected: a draft that
    // 400s writes nothing either way.
    await row.getByRole("radio", { name: AGENTS.MECHANISM_BEDROCK_SSO }).click();
    await row.getByRole("radio", { name: AGENTS.SOURCE_PER_USER }).click();
    await row.getByLabel(AGENTS.FIELD_SSO_START_URL).fill("https://acme.awsapps.com/start");
    await row.getByLabel(AGENTS_DRAFT.FIELD_SSO_ACCOUNT_ID).fill("111111111111");
    await row.getByLabel(AGENTS_DRAFT.FIELD_SSO_ROLE_NAME).fill("DevPower");
    await page.getByRole("button", { name: PROVIDERS.SAVE_CTA }).click();

    // The SERVER's own agent400SSOPinModelAccount sentence, verbatim.
    await expect(
      page.getByText(
        'agents: "claude-code": sso_account_id 111111111111 is not the account the configured Bedrock model lives in (222222222222) — a session for that account cannot invoke it',
      ),
    ).toBeVisible();

    const after = await getAgentProviders(page);
    const afterClaude = (after.providers.agents as Array<Record<string, unknown>>).find((a) => a.id === "claude-code");
    expect(afterClaude).toEqual(beforeClaude);
  });
});

// Appendix A finding 4 (prominence): the per_user sign-in affordance moves
// to the TOP of the expanded row, above the mechanism radio group, so it is
// the first thing an admin sees after declaring the lane — never scrolled
// past on the way to the legacy Settings door.
test.describe("agents — the per_user sign-in affordance renders before the mechanism field", () => {
  // U-09: this test's own PUT below writes a roster of ONE row
  // (claude-code), dropping codex-cli and none — real, against the shared
  // e2e daemon other spec FILES run against concurrently (fullyParallel).
  // Restore the roster this test found on arrival so it never leaks a
  // narrowed roster to anything running alongside or after it. R-05
  // (review): snapshotted in beforeEach (not the test body) so a second
  // test added to this describe restores its own arrival state too, and the
  // restore PUT's status is asserted so a failed restore is never silent.
  let rosterBefore: { agents?: unknown[] } | null = null;
  test.beforeEach(async ({ page }) => {
    rosterBefore = (await getAgentProviders(page)).providers;
  });
  test.afterEach(async ({ page }) => {
    if (rosterBefore) {
      const restore = await page.request.put("/api/v1/agent-providers", { headers: auth, data: rosterBefore });
      expect(restore.ok()).toBeTruthy();
      rosterBefore = null;
    }
  });

  test("the banner renders before the mechanism field under an actionable per_user row", async ({ page }) => {
    // Cache-and-serve (the same closure the not_applicable test above uses):
    // a per-request route.fetch()+refulfill raced Playwright disposing an
    // in-flight route's response under load (see that test's own comment).
    let cached: Record<string, unknown> | null = null;
    await page.route("**/api/v1/setup/status*", async (route) => {
      if (!cached) {
        const response = await route.fetch();
        const body = await response.json();
        body.model_access = { state: "not_configured", action: "Sign in to AWS" };
        cached = body;
      }
      // TS can't narrow a `let` captured by this closure across the `await`
      // above — the `if` guarantees it non-null by here.
      await route.fulfill({ json: cached! });
    });
    const put = await page.request.put("/api/v1/agent-providers", {
      headers: auth,
      data: {
        agents: [
          {
            id: "claude-code",
            mechanism: "bedrock_sso",
            credential_source: "per_user",
            sso_start_url: "https://acme.awsapps.com/start",
          },
        ],
      },
    });
    expect(put.status()).toBe(200);

    await gotoAgentsTab(page);
    const row = page.getByTestId("agent-row-claude-code");
    await expect(row.getByText(AGENTS_DRAFT.PER_USER_SIGN_IN_TITLE)).toBeVisible();
    const banner = row.getByTestId("per-user-sign-in-banner");
    const mechanismField = row.getByRole("radiogroup", { name: AGENTS.FIELD_MECHANISM });
    const bannerBox = await banner.boundingBox();
    const mechanismBox = await mechanismField.boundingBox();
    expect(bannerBox).not.toBeNull();
    expect(mechanismBox).not.toBeNull();
    expect(bannerBox!.y).toBeLessThan(mechanismBox!.y);
  });
});
