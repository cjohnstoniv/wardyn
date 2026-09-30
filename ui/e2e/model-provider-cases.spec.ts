/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #545 (MP-26) — a pinning spec per fixture case in the multi-provider design's
// §5 "Cases" table (docs/design's multi-provider-design.md, mirrored into
// model-providers-prompt.md's own §2/§5.1-5.2): (a) one provider, (b) Claude
// Code offering the subscription, the key and the gateway, (c) a gateway on
// both harnesses, (d) one harness offering a one-click sign-in and a typed
// key, (e) nothing connected.
//
// Every case is read through Settings → Model providers (#536, on main) —
// the one shipped surface that renders the fixtures table's own facts (kind,
// what each person provides, Used by, the default, the connected count). Two
// screens the design also names for these cases are NOT here: Agents tab
// default-provider picker (#539) and Getting started → Your model
// connections (#541) are both still open — neither is on main, so a case
// that needed either stays unwritten (see the issue body / PR description
// for what's waiting).
//
// (a), (c), (d), (e) go through the REAL backend, no stubbed route: real
// PUT /model-providers + PUT /agent-providers documents, and real per-person
// credentials via seedUserToken below — a genuine api_tokens row (the #698
// pattern; never a spliced /me, which would leave the write itself hitting
// the real backend as the admin bearer, not as a distinct person). (b) is
// the one exception, and it stubs for a documented, pre-existing reason (see
// its own describe block) — the same reason model-provider-editor.spec.ts's
// own Claude-subscription test already stubs for.
import type { Page } from "@playwright/test";
import { createHash, randomBytes } from "node:crypto";
import { test, expect, ADMIN_TOKEN, expandCard, gotoConsole, navToRoute, sql } from "./fixtures";
import { MODEL_LEDE, MODEL_PROVIDERS as M, PROVIDER_EDITOR } from "../src/app/lib/model-providers-copy";

const auth = { Authorization: `Bearer ${ADMIN_TOKEN}` };

async function gotoSettings(page: Page): Promise<void> {
  await gotoConsole(page);
  await navToRoute(page, "/admin/settings");
  await expandCard(page, M.TITLE);
  await expect(page.getByTestId("model-providers-list").getByText(MODEL_LEDE)).toBeVisible();
}

// GET-then-PUT-with-If-Match against one of the two singleton documents,
// exactly the round trip the console itself performs (model-providers.ts /
// agent-providers.ts) — real writes, real optimistic concurrency, never a
// last-writer-wins guess.
async function putDoc(page: Page, path: string, body: unknown): Promise<void> {
  const cur = await page.request.get(path, { headers: auth });
  const etag = cur.headers()["etag"] ?? null;
  const headers: Record<string, string> = { ...auth };
  if (etag) headers["If-Match"] = etag;
  const res = await page.request.put(path, { headers, data: body });
  expect(res.status(), await res.text()).toBe(200);
}

const putModelProviders = (page: Page, providers: unknown[]) =>
  putDoc(page, "/api/v1/model-providers", { providers });
const putAgentProviders = (page: Page, agents: unknown[]) => putDoc(page, "/api/v1/agent-providers", { agents });

// Clears both documents back to legacy-open mode ({} on each) between cases,
// so one case's fixture never leaks into the next in this serial file.
// Agent providers FIRST: a stored default_provider naming a model provider
// this call is about to remove would otherwise 400 (stillDefaultRefusal).
async function clearFixtures(page: Page): Promise<void> {
  await putAgentProviders(page, []);
  await putModelProviders(page, []);
}

// A genuine per-person caller, seeded the way the server's own mint stores
// one — the same api_tokens row shape available-to.spec.ts's seedUserToken
// uses (#698, T-38's own pattern), copied here rather than imported: a
// Playwright spec file cannot import another spec file (zero tests collected
// otherwise). Every model-provider credential door reads THIS caller's own
// namespace, never the admin bearer's — a spliced /me would leave the write
// landing on the harness's real admin principal instead, proving nothing
// about a second person's own credential.
function seedUserToken(userType: string): { Authorization: string } {
  const raw = `wdn_${randomBytes(32).toString("hex")}`;
  const hash = createHash("sha256").update(raw).digest("hex");
  const who = `${userType}-${hash.slice(0, 8)}@e2e.test`;
  sql(
    `INSERT INTO api_tokens (id, principal, email, role, user_type, groups, groups_truncated, name, token_sha256, created_at)
     VALUES (gen_random_uuid(), '${who}', '${who}', 'user', '${userType}', '[]'::jsonb, false, 'e2e', '${hash}', now())`,
  );
  return { Authorization: `Bearer ${raw}` };
}

async function putCredential(page: Page, id: string, headers: { Authorization: string }, value: string) {
  return page.request.put(`/api/v1/model-providers/${id}/credential`, { headers, data: { value } });
}

const row = (page: Page, id: string) => page.getByTestId(`model-provider-${id}`);

test.describe.configure({ mode: "serial" });

test.describe("Model provider cases (design §5) — real backend, real per-person credentials", () => {
  test("(a) one provider stays as simple as today — no default chip even once one is set (A2)", async ({ page }) => {
    await clearFixtures(page);
    await putModelProviders(page, [
      {
        id: "bedrock-prod",
        name: "Bedrock (prod)",
        kind: "bedrock_sso",
        bedrock: { region: "us-east-1", sso_start_url: "https://acme.awsapps.com/start" },
        harnesses: [{ harness: "claude-code", model: "acme.claude-sonnet" }],
      },
    ]);
    // The design's own rule for this case: "with one candidate there is no
    // picker" (§5, case a). Marking it the default anyway must NOT paint a
    // "Default for…" chip — model-providers-list.tsx's chipDefaults keeps
    // only a harness with >=2 candidates, so this is the mutation this test
    // pins: drop that filter and the chip appears here where design says it
    // must not.
    await putAgentProviders(page, [{ id: "claude-code", default_provider: "bedrock-prod" }]);

    await gotoSettings(page);
    const bedrock = row(page, "bedrock-prod");
    await expect(bedrock.getByText("Bedrock (prod)")).toBeVisible();
    await expect(bedrock.getByText("Amazon Bedrock")).toBeVisible();
    await expect(bedrock.getByText(M.PROVIDES.SSO)).toBeVisible();
    await expect(bedrock.getByText(M.USED_BY(["Claude Code"]))).toBeVisible();
    await expect(bedrock.getByText(M.CONNECTED(0))).toBeVisible();
    await expect(bedrock.getByText(/^Default for /)).toHaveCount(0);

    const wire = await (await page.request.get("/api/v1/model-providers", { headers: auth })).json();
    expect(wire.connected_people).toEqual({ "bedrock-prod": 0 });
  });

  test("(c) a gateway on both harnesses — one provider, two real people, counted once each (A4)", async ({ page }) => {
    await clearFixtures(page);
    await putModelProviders(page, [
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
    ]);

    // Two real people store their own token, for real — the count this
    // pins is PEOPLE, not requests or harnesses: the same provider serving
    // both harnesses must still count each person once (connectedPeople,
    // internal/api/model_provider_credentials.go). Mutation this proves:
    // break that de-duplication (or the credential write itself) and this
    // count stops matching the wire.
    const alice = seedUserToken("standard");
    const bob = seedUserToken("standard");
    expect((await putCredential(page, "corp-gateway", alice, "e2e-alice-corp-gateway-token")).status()).toBe(204);
    expect((await putCredential(page, "corp-gateway", bob, "e2e-bob-corp-gateway-token")).status()).toBe(204);

    await gotoSettings(page);
    const gateway = row(page, "corp-gateway");
    await expect(gateway.getByText("Corp gateway")).toBeVisible();
    await expect(gateway.getByText(M.PROVIDES.TOKEN)).toBeVisible();
    await expect(gateway.getByText(M.USED_BY(["Claude Code", "Codex CLI"]))).toBeVisible();
    await expect(gateway.getByText(M.CONNECTED(2))).toBeVisible();

    const wire = await (await page.request.get("/api/v1/model-providers", { headers: auth })).json();
    expect(wire.connected_people).toEqual({ "corp-gateway": 2 });
  });

  test("(d) one harness, a one-click sign-in and a typed key — Bedrock stays the default, the key row is not, and each row's count is its own", async ({
    page,
  }) => {
    await clearFixtures(page);
    await putModelProviders(page, [
      {
        id: "bedrock-prod",
        name: "Bedrock (prod)",
        kind: "bedrock_sso",
        bedrock: { region: "us-east-1", sso_start_url: "https://acme.awsapps.com/start" },
        harnesses: [{ harness: "claude-code", model: "acme.claude-sonnet" }],
      },
      { id: "anthropic-key", kind: "anthropic_api_key", harnesses: [{ harness: "claude-code" }] },
    ]);
    await putAgentProviders(page, [{ id: "claude-code", default_provider: "bedrock-prod" }]);

    // One real person connects only the key row — the brief's rejected
    // "shared plus per-person" shape cannot occur (design §5, case d note);
    // what IS real is that a sign-in-kind row (Bedrock) can hold no typed
    // credential of its own and a typed-key row's count never borrows the
    // sign-in row's.
    const carol = seedUserToken("standard");
    expect((await putCredential(page, "anthropic-key", carol, "sk-ant-e2e-carol-000000000000")).status()).toBe(204);

    await gotoSettings(page);
    const bedrock = row(page, "bedrock-prod");
    await expect(bedrock.getByText(M.CHIP_DEFAULT_FOR(["Claude Code"]))).toBeVisible();
    await expect(bedrock.getByText(M.CONNECTED(0))).toBeVisible();

    const key = row(page, "anthropic-key");
    await expect(key.getByText("Anthropic API key")).toHaveCount(1);
    await expect(key.getByText(M.PROVIDES.KEY)).toBeVisible();
    await expect(key.getByText(M.CONNECTED(1))).toBeVisible();
    await expect(key.getByText(/^Default for /)).toHaveCount(0);

    const wire = await (await page.request.get("/api/v1/model-providers", { headers: auth })).json();
    expect(wire.connected_people).toEqual({ "bedrock-prod": 0, "anthropic-key": 1 });
  });

  // (e)'s OTHER half — "Bedrock expired while the gateway token is stored" —
  // is not pinned here: a real expiring/dead AWS SSO session needs a live
  // sign-in this hermetic `-runner none` harness cannot produce, and the ONE
  // surface that would render a per-person expiry state (Getting started →
  // Your model connections, #541) is not on main yet. This half — nothing
  // connected at all, for two real un-credentialed providers — IS real and
  // IS on main today.
  test("(e) nothing connected — every row reads the real zero-count state before anyone has stored anything", async ({
    page,
  }) => {
    await clearFixtures(page);
    await putModelProviders(page, [
      {
        id: "bedrock-prod",
        name: "Bedrock (prod)",
        kind: "bedrock_sso",
        bedrock: { region: "us-east-1", sso_start_url: "https://acme.awsapps.com/start" },
        harnesses: [{ harness: "claude-code", model: "acme.claude-sonnet" }],
      },
      { id: "anthropic-key", kind: "anthropic_api_key", harnesses: [{ harness: "claude-code" }] },
    ]);

    await gotoSettings(page);
    await expect(row(page, "bedrock-prod").getByText(M.CONNECTED(0))).toBeVisible();
    await expect(row(page, "anthropic-key").getByText(M.CONNECTED(0))).toBeVisible();

    const wire = await (await page.request.get("/api/v1/model-providers", { headers: auth })).json();
    expect(wire.connected_people).toEqual({ "bedrock-prod": 0, "anthropic-key": 0 });
  });
});

// (b) — Claude Code offering the subscription, the key and the gateway, the
// default being the gateway. Stubbed, unlike every case above: introducing an
// anthropic_subscription provider for REAL 400s in this harness (E4 —
// WARDYN_AGENT_IMAGES carries no "claude-code" pin here, so the Claude
// sign-in image never resolves; model_providers.go's mp400SignInImage). That
// is exactly the gate model-provider-editor.spec.ts's own Claude-subscription
// test already works around by stubbing the same route, for the same reason
// — this is that precedent, not a new one. What a real write already proves
// end to end — a person's own credential landing in connected_people — is
// (c) and (d) above; this test's own job is the three-row rendering and its
// one default chip (packet MP-A, A3).
test.describe("(b) Claude Code offers the subscription, the key and the gateway; the default is Corp gateway", () => {
  test("three rows, each with its own provides-line and count, and only the gateway carries the default chip", async ({
    page,
  }) => {
    await page.route("**/api/v1/model-providers", (route) =>
      route.request().method() === "GET"
        ? route.fulfill({
            contentType: "application/json",
            body: JSON.stringify({
              providers: [
                { id: "claude-subscription", kind: "anthropic_subscription", harnesses: [{ harness: "claude-code" }] },
                { id: "anthropic-key", kind: "anthropic_api_key", harnesses: [{ harness: "claude-code" }] },
                {
                  id: "corp-gateway",
                  name: "Corp gateway",
                  kind: "custom_endpoint",
                  base_url: "https://gateway.corp.example",
                  harnesses: [{ harness: "claude-code", path: "/anthropic" }],
                },
              ],
              connected_people: { "claude-subscription": 4, "anthropic-key": 0, "corp-gateway": 9 },
            }),
          })
        : route.fallback(),
    );
    await page.route("**/api/v1/agent-providers", (route) =>
      route.request().method() === "GET"
        ? route.fulfill({
            contentType: "application/json",
            body: JSON.stringify({ agents: [{ id: "claude-code", default_provider: "corp-gateway" }] }),
          })
        : route.fallback(),
    );

    await gotoSettings(page);

    const sub = row(page, "claude-subscription");
    await expect(sub.getByText("Claude subscription", { exact: true })).toHaveCount(1);
    await expect(sub.getByText(PROVIDER_EDITOR.PROVIDES_CLAUDE)).toBeVisible();
    await expect(sub.getByText(M.CONNECTED(4))).toBeVisible();
    await expect(sub.getByText(/^Default for /)).toHaveCount(0);

    const key = row(page, "anthropic-key");
    await expect(key.getByText("Anthropic API key")).toHaveCount(1);
    await expect(key.getByText(M.PROVIDES.KEY)).toBeVisible();
    await expect(key.getByText(M.CONNECTED(0))).toBeVisible();
    await expect(key.getByText(/^Default for /)).toHaveCount(0);

    const gateway = row(page, "corp-gateway");
    await expect(gateway.getByText(M.PROVIDES.TOKEN)).toBeVisible();
    await expect(gateway.getByText(M.CONNECTED(9))).toBeVisible();
    await expect(gateway.getByText(M.CHIP_DEFAULT_FOR(["Claude Code"]))).toBeVisible();
  });
});
