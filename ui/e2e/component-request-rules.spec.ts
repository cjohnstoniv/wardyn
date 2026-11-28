/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Request rules on custom components, against a real backend (no page.route): the switch is
// `features.component_request_rules.enabled`, written through PUT /site-config with If-Match.
//   (a) Switch on, the organisation's editor: a rule added and saved is still there after a reload, and a
//       path the server refuses shows the server's sentence under the rules, with that rule marked.
//   (b) Switch off, the organisation's editor: a component that already has rules shows only the
//       remove row; Remove rules then Save stores none.
//   (c) Switch off, a person's own component on Your account: the same remove row, and Remove rules then
//       Save stores none.
// Serial, one backend: the switch is put back as it was found, and every component made here is deleted,
// whatever failed.
import { randomUUID } from "node:crypto";
import type { APIRequestContext } from "@playwright/test";
import { test, expect, ADMIN_TOKEN, MEMBER_TOKEN, asRealMember, expandCard, gotoConsole, navToRoute } from "./fixtures";
import { SERVER_OWNED_SITE_CONFIG_KEYS } from "../src/app/lib/types/site";
import { COMPONENTS_ADMIN } from "../src/app/components/wardyn/copy/components-admin";
import { CUSTOM_COMPONENT, MY_COMPONENTS } from "../src/app/components/wardyn/copy/components";
import { REQUEST_RULES } from "../src/app/components/screens/components/request-rules-field";

test.describe.configure({ mode: "serial" });

const admin = { Authorization: `Bearer ${ADMIN_TOKEN}` };
const member = { Authorization: `Bearer ${MEMBER_TOKEN}` };
const HOST = "api.rules-e2e.example";
const RULE = { host: HOST, methods: ["GET"], paths: ["/v1"], effect: "allow" };
const suffix = randomUUID().slice(0, 8);
const NEW_NAME = `e2e rules new ${suffix}`;
const ORG_NAME = `e2e rules org ${suffix}`;
const MINE_NAME = `e2e rules mine ${suffix}`;
const ORG_ID = randomUUID();

interface StoredComponent {
  id: string;
  name: string;
  definition: { request_rules?: unknown[] };
}

// The switch as this file found it, put back in afterAll.
let wasOn = false;
let mineId = "";

async function readSwitch(request: APIRequestContext): Promise<{ on: boolean; body: Record<string, unknown>; etag: string }> {
  const res = await request.get("/api/v1/site-config", { headers: admin });
  expect(res.status(), await res.text()).toBe(200);
  const body = (await res.json()) as Record<string, unknown>;
  const features = body.features as { component_request_rules?: { enabled?: boolean } } | undefined;
  return { on: features?.component_request_rules?.enabled === true, body, etag: res.headers()["etag"] ?? "" };
}

// The stored document, minus what the server owns, sent back with only this key in `features`: the other keys
// carry forward as stored.
async function setSwitch(request: APIRequestContext, enabled: boolean): Promise<void> {
  const { body, etag } = await readSwitch(request);
  for (const k of SERVER_OWNED_SITE_CONFIG_KEYS) delete body[k];
  const put = await request.put("/api/v1/site-config", {
    headers: { ...admin, "If-Match": etag },
    data: { ...body, features: { component_request_rules: { enabled } } },
  });
  expect(put.status(), await put.text()).toBe(200);
  expect((await readSwitch(request)).on).toBe(enabled);
}

async function orgComponent(request: APIRequestContext, name: string): Promise<StoredComponent | undefined> {
  const res = await request.get("/api/v1/components", { headers: admin });
  expect(res.status(), await res.text()).toBe(200);
  return ((await res.json()) as StoredComponent[]).find((c) => c.name === name);
}

async function myComponent(request: APIRequestContext, id: string): Promise<StoredComponent | undefined> {
  const res = await request.get("/api/v1/me/components", { headers: member });
  expect(res.status(), await res.text()).toBe(200);
  return ((await res.json()) as { mine: StoredComponent[] }).mine.find((c) => c.id === id);
}

test.beforeAll(async ({ request }) => {
  wasOn = (await readSwitch(request)).on;
  await setSwitch(request, true);
  // (b)'s and (c)'s components, stored with a rule while the switch is on.
  const org = await request.put(`/api/v1/components/${ORG_ID}`, {
    headers: admin,
    data: { name: ORG_NAME, definition: { hosts: [HOST], request_rules: [RULE] } },
  });
  expect([200, 201], await org.text()).toContain(org.status());
  const mine = await request.post("/api/v1/me/components", {
    headers: member,
    data: { name: MINE_NAME, definition: { hosts: [HOST], request_rules: [RULE] } },
  });
  expect(mine.status(), await mine.text()).toBe(201);
  mineId = ((await mine.json()) as StoredComponent).id;
});

test.afterAll(async ({ request }) => {
  try {
    const made = await orgComponent(request, NEW_NAME);
    for (const id of [made?.id, ORG_ID]) {
      if (id) await request.delete(`/api/v1/components/${id}`, { headers: admin });
    }
    if (mineId) await request.delete(`/api/v1/me/components/${mineId}`, { headers: member });
  } finally {
    await setSwitch(request, wasOn);
  }
});

test("switch on: the organisation's editor keeps a saved rule, and shows a refused path under the rules", async ({ page, request }) => {
  await gotoConsole(page, "admin");
  await navToRoute(page, "/admin/components");
  await page.getByRole("button", { name: COMPONENTS_ADMIN.ADD }).click();
  const create = page.getByRole("dialog", { name: COMPONENTS_ADMIN.EDITOR_NEW });
  await create.locator("#component-name").fill(NEW_NAME);
  await create.locator("#component-hosts").fill(HOST);
  await create.getByRole("button", { name: REQUEST_RULES.ADD }).click();
  await create.getByRole("combobox", { name: REQUEST_RULES.HOST_OF(1) }).click();
  await page.getByRole("option", { name: HOST, exact: true }).click();
  await create.getByRole("textbox", { name: REQUEST_RULES.METHODS_OF(1) }).fill("GET");
  await create.getByRole("textbox", { name: REQUEST_RULES.PATHS_OF(1) }).fill("/v1");
  await create.getByRole("button", { name: COMPONENTS_ADMIN.SAVE, exact: true }).click();
  await expect(page.getByText(COMPONENTS_ADMIN.SAVED_TOAST)).toBeVisible();
  await expect.poll(async () => (await orgComponent(request, NEW_NAME))?.definition.request_rules).toMatchObject([RULE]);

  await page.reload();
  await page.getByRole("button", { name: COMPONENTS_ADMIN.EDIT_ARIA(NEW_NAME) }).click();
  const edit = page.getByRole("dialog", { name: COMPONENTS_ADMIN.EDITOR_EDIT });
  await expect(edit.getByTestId("request-rule")).toHaveCount(1);
  await expect(edit.getByRole("combobox", { name: REQUEST_RULES.HOST_OF(1) })).toHaveText(HOST);
  await expect(edit.getByRole("textbox", { name: REQUEST_RULES.METHODS_OF(1) })).toHaveValue("GET");
  const paths = edit.getByRole("textbox", { name: REQUEST_RULES.PATHS_OF(1) });
  await expect(paths).toHaveValue("/v1");

  // A path with no leading slash: the server's sentence shows under the rules, and the rule and its paths are marked.
  await paths.fill("v1/x");
  await edit.getByRole("button", { name: COMPONENTS_ADMIN.SAVE, exact: true }).click();
  await expect(edit.locator("#component-rules-error")).toContainText('"v1/x" must start with /');
  const rule = edit.getByTestId("request-rule");
  await expect(rule).toContainText(REQUEST_RULES.REFUSED);
  await expect(paths).toHaveAttribute("aria-invalid", "true");
  expect((await orgComponent(request, NEW_NAME))?.definition.request_rules).toMatchObject([RULE]);
});

test("switch off: the organisation's editor offers only to remove stored rules, and saving stores none", async ({ page, request }) => {
  await setSwitch(request, false);
  await gotoConsole(page, "admin");
  await navToRoute(page, "/admin/components");
  await page.getByRole("button", { name: COMPONENTS_ADMIN.EDIT_ARIA(ORG_NAME) }).click();
  const edit = page.getByRole("dialog", { name: COMPONENTS_ADMIN.EDITOR_EDIT });
  await expect(edit.getByText(REQUEST_RULES.OFF_COUNT(1), { exact: true })).toBeVisible();
  await expect(edit.getByText(REQUEST_RULES.OFF_SENTENCE)).toBeVisible();
  await expect(edit.getByRole("button", { name: REQUEST_RULES.ADD })).toHaveCount(0);
  await expect(edit.getByTestId("request-rule")).toHaveCount(0);
  await edit.getByRole("button", { name: REQUEST_RULES.REMOVE_ALL }).click();
  await expect(edit.getByText(REQUEST_RULES.REMOVED)).toBeVisible();
  await edit.getByRole("button", { name: COMPONENTS_ADMIN.SAVE, exact: true }).click();
  await expect(page.getByText(COMPONENTS_ADMIN.SAVED_TOAST)).toBeVisible();
  await expect.poll(async () => (await orgComponent(request, ORG_NAME))?.definition.request_rules ?? []).toEqual([]);
});

test("switch off: a person's own component offers only to remove its rules, and saving stores none", async ({ page, request }) => {
  expect((await readSwitch(request)).on).toBe(false);
  await asRealMember(page);
  await gotoConsole(page);
  await navToRoute(page, "/account");
  await expandCard(page, MY_COMPONENTS.TITLE);
  await page.getByRole("button", { name: `${MY_COMPONENTS.EDIT} ${MINE_NAME}` }).click();
  const edit = page.getByRole("dialog", { name: CUSTOM_COMPONENT.TITLE_EDIT });
  await expect(edit.getByText(REQUEST_RULES.OFF_COUNT(1), { exact: true })).toBeVisible();
  await expect(edit.getByText(REQUEST_RULES.OFF_SENTENCE)).toBeVisible();
  await expect(edit.getByRole("button", { name: REQUEST_RULES.ADD })).toHaveCount(0);
  await edit.getByRole("button", { name: REQUEST_RULES.REMOVE_ALL }).click();
  await expect(edit.getByText(REQUEST_RULES.REMOVED)).toBeVisible();
  await edit.getByRole("button", { name: CUSTOM_COMPONENT.SAVE, exact: true }).click();
  await expect(edit).toBeHidden();
  await expect.poll(async () => (await myComponent(request, mineId))?.definition.request_rules ?? []).toEqual([]);
});
