/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import {
  test,
  expect,
  ADMIN_TOKEN,
  expandCard,
  gotoConsole,
  navToRoute,
} from "./fixtures";
import {
  AGENTS,
  PROVIDERS,
} from "../src/app/lib/workspace-providers-copy";
import { RUN_DETAIL } from "../src/app/components/wardyn/copy/run-cockpit";
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
//   * The Agents tab's own write walk (enable/default provider/save/reload)
//     is REAL — GET/PUT /agent-providers and /model-providers are operatorOnly
//     routes this harness's admin bearer reaches, so it writes to Postgres and
//     reads its own write back.
//   * The disabled-agent 422 (agentRosterRefusal) is real: no isOperator
//     exemption exists in that function.
//   * A roster row carries no model credential since #548; a body that still
//     declares one is refused at the PUT (strict decode), which is real too.
// ---------------------------------------------------------------------------

const auth = { Authorization: `Bearer ${ADMIN_TOKEN}` };

async function gotoAgentsTab(page: Page): Promise<void> {
  await gotoConsole(page);
  await navToRoute(page, "/admin/settings");
  await expandCard(page, "Workspace providers");
  await page.getByTestId("providers-card").getByText(PROVIDERS.CARD_OPEN).click();
  await expect(page).toHaveURL(/\/admin\/providers$/);
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

// GET-then-PUT-with-If-Match, the round trip the console itself performs.
async function putDoc(page: Page, path: string, body: unknown): Promise<void> {
  const cur = await page.request.get(path, { headers: auth });
  const etag = cur.headers()["etag"] ?? null;
  const headers: Record<string, string> = { ...auth };
  if (etag) headers["If-Match"] = etag;
  const res = await page.request.put(path, { headers, data: body });
  expect(res.status(), await res.text()).toBe(200);
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

  // Packet MP-C (design §5.3): the row's default model provider. The
  // providers are seeded through PUT /model-providers (Settings → Model
  // providers owns that document); the default is picked and saved here.
  test("G1: with no model provider set up, every managed agent says so", async ({ page }) => {
    await gotoAgentsTab(page);
    const row = page.getByTestId("agent-row-claude-code");
    await expect(row.getByText(AGENTS.NO_PROVIDER("Claude Code"))).toBeVisible();
    await expect(page.getByTestId("agent-row-none").getByText(AGENTS.MECHANISM_NONE)).toBeVisible();
  });

  test("G2 + G3: one provider is a static line; several are a select whose pick persists", async ({ page }) => {
    await putDoc(page, "/api/v1/model-providers", {
      providers: [
        {
          id: "bedrock-prod",
          name: "Bedrock (prod)",
          kind: "bedrock_sso",
          bedrock: { region: "us-east-1", sso_start_url: "https://acme.awsapps.com/start" },
          harnesses: [{ harness: "claude-code", model: "acme.claude-sonnet" }],
        },
        {
          id: "corp-gateway",
          name: "Corp gateway",
          kind: "custom_endpoint",
          base_url: "https://gateway.corp.example",
          harnesses: [
            { harness: "claude-code", path: "/anthropic" },
            { harness: "codex-cli", path: "/v1" },
          ],
        },
      ],
    });
    try {
      await gotoAgentsTab(page);
      await expect(page.getByTestId("agent-row-codex-cli").getByText(AGENTS.ONLY_PROVIDER("Corp gateway", "Codex CLI"))).toBeVisible();

      const row = page.getByTestId("agent-row-claude-code");
      await expect(row.getByText(AGENTS.DEFAULT_HINT)).toBeVisible();
      await row.getByRole("combobox", { name: `${AGENTS.FIELD_DEFAULT_PROVIDER} — Claude Code` }).click();
      await page.getByRole("option", { name: AGENTS.DEFAULT_OPTION("Bedrock (prod)", "Amazon Bedrock") }).click();
      await saveAgents(page);

      // Tab selection is local React state (providers-screen.tsx's `tab`), not
      // URL-carried — a reload always re-lands on the Git tab.
      await page.reload();
      await page.getByRole("button", { name: AGENTS.AGENTS_TITLE }).click();
      await expect(
        page.getByTestId("agent-row-claude-code").getByRole("combobox", { name: `${AGENTS.FIELD_DEFAULT_PROVIDER} — Claude Code` }),
      ).toHaveText(AGENTS.DEFAULT_OPTION("Bedrock (prod)", "Amazon Bedrock"));

      const snap = await getAgentProviders(page);
      const agents = snap.providers.agents as Array<Record<string, unknown>>;
      expect(agents.find((a) => a.id === "claude-code")).toMatchObject({ default_provider: "bedrock-prod" });
      // G2 writes nothing: the one candidate is not stored as a default.
      expect(agents.find((a) => a.id === "codex-cli")?.default_provider).toBeUndefined();
    } finally {
      // Roster first: a stored default naming a provider about to be removed
      // is refused. Both back to legacy open mode for the tests after this one.
      await putDoc(page, "/api/v1/agent-providers", {});
      await putDoc(page, "/api/v1/model-providers", {});
    }
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

  // #548: a roster row carries no model credential and there is no alias
  // window — a 0.7 body that still declares one is refused, not ignored.
  test("a roster body that still declares a model credential mechanism is refused", async ({ page }) => {
    const res = await page.request.put("/api/v1/agent-providers", {
      headers: auth,
      data: { agents: [{ id: "claude-code", mechanism: "bedrock_bearer" }] },
    });
    expect(res.status(), await res.text()).toBe(400);
  });
});

// A launch whose 201 carries warnings[] (#125): the run page renders them
// immediately — the console navigates there in the SAME TICK, no held screen,
// no toast, no timer — and Dismiss clears them client-side. Spliced on the
// create response (route.fetch() + patch + refulfill): this harness's
// admin-token caller is never member-clamped for real (isOperator
// short-circuits governance resolution, drives.spec.ts's own documented
// reason), so a genuine 201-with-warnings needs a member session this
// harness cannot mint. The CLIENT behavior this pins is C-UI's; the clamping
// itself is B-α/A2's, Go-tested.
test.describe("agents — a 201 carrying warnings navigates straight to the run", () => {
  test("navigates immediately, renders the warnings on the run page, and Dismiss clears them", async ({ page }) => {
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

    // Navigates in the same tick — no held /runs/new, no "Open run".
    await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{8,}/);
    await expect(page.getByText(AGENTS.LAUNCH_WARNING_TITLE)).toBeVisible();
    await expect(page.getByText("Egress narrowed to api.anthropic.com by member policy.")).toBeVisible();
    await expect(page.getByText(RUN_DETAIL.LAUNCH_WARNING_EPHEMERAL)).toBeVisible();

    await page.getByRole("button", { name: RUN_DETAIL.LAUNCH_WARNING_DISMISS }).click();
    await expect(page.getByText(AGENTS.LAUNCH_WARNING_TITLE)).toHaveCount(0);
  });
});

// The Agents tab's own roster-unknown state (the C-UI review fix pass): no
// `harnesses` in SetupStatus reads as FETCH_FAILED, never a wipeable empty
// roster — Save is withheld either way. Real GET /setup/status always
// returns the static catalog (setupHarnessTools has no failure mode this
// harness can trigger), so both arms are spliced.
test.describe("agents — the roster-unknown and empty-roster states withhold Save", () => {
  test("an absent `harnesses` field renders FETCH_FAILED_* with no Save", async ({ page }) => {
    // Cache-and-serve, not route.fetch()+refulfill per match: gotoAgentsTab's
    // walk (landing redirect, then settings-screen and providers-screen each
    // mounting) hits /setup/status more than once, and a real round trip PER
    // match raced Playwright disposing an in-flight route's response at
    // teardown ("apiResponse.json: Response has been disposed").
    let cached: Record<string, unknown> | null = null;
    await page.route("**/api/v1/setup/status*", async (route) => {
      if (!cached) {
        const response = await route.fetch();
        const json = await response.json();
        delete json.harnesses;
        cached = json;
      }
      await route.fulfill({ json: cached! });
    });
    await gotoAgentsTab(page);
    await expect(page.getByText(PROVIDERS.FETCH_FAILED_TITLE)).toBeVisible();
    await expect(page.getByRole("button", { name: PROVIDERS.SAVE_CTA })).toHaveCount(0);
  });

  test("a genuinely empty roster ([]) shows the lead, no rows, and no Save", async ({ page }) => {
    // Cache-and-serve (same reason as the FETCH_FAILED case above).
    let cached: Record<string, unknown> | null = null;
    await page.route("**/api/v1/setup/status*", async (route) => {
      if (!cached) {
        const response = await route.fetch();
        const json = await response.json();
        json.harnesses = [];
        cached = json;
      }
      await route.fulfill({ json: cached! });
    });
    await gotoAgentsTab(page);
    await expect(page.getByText(AGENTS.AGENTS_LEAD)).toBeVisible();
    await expect(page.getByTestId("agent-row-claude-code")).toHaveCount(0);
    await expect(page.getByRole("button", { name: PROVIDERS.SAVE_CTA })).toHaveCount(0);
  });
});
