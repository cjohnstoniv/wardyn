/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The file_download card and the kubectl change card on the standalone /approvals list. As approvals-mcp-call.spec.ts
// does, this spec intercepts **/api/v1/approvals* with synthetic rows (the seeded backend has no desktop file or held
// kubectl write) and the approve route, and lets the rest hit the real backend.
import { test, expect, gotoConsole, navTo, sql } from "./fixtures";
import { DECIDERS_INBOX } from "../src/app/components/wardyn/copy/deciders-inbox";
import { APPROVAL_MCP_CALL } from "../src/app/components/wardyn/copy/approvals";
import { FILE_DOWNLOAD_CARD, KUBE_CALL_CARD } from "../src/app/components/wardyn/copy/files-review";
import type { Page } from "@playwright/test";

const SHA = "9f2c6b0e4d8a17f35c2e9b1a0d7f64c3e85b2a19f0c4d6e7a8b9c0d1e2f341ab";
const ASKER = "dana.reyes@acme.example";
const BODY_TEXT = '{\n  "spec": {\n    "replicas": 3\n  }\n}';
const KUBE = { cluster: "k8s.staging.acme.example", namespace: "team-a", path: "/apis/apps/v1/namespaces/team-a/deployments/web" };

type Row = Record<string, unknown>;

const base = (id: string, runId: string, over: Row): Row => ({
  id,
  run_id: runId,
  state: "PENDING",
  requested_at: new Date().toISOString(),
  decidable: true,
  ...over,
});

const download = (id: string, runId: string, over: Row = {}): Row =>
  base(id, runId, {
    kind: "file_download",
    requested_scope: { name: "build-logs.tar.gz", bytes: 19293184, sha256: SHA, requested_by: ASKER },
    ...over,
  });

const kube = (id: string, runId: string, method: string, over: Row = {}): Row =>
  base(id, runId, {
    kind: "tool_call",
    requested_scope: { ...KUBE, method, call_sha256: id.padEnd(64, "a").slice(0, 64), hold_seconds: 90, ...(method === "PATCH" ? { body: { spec: { replicas: 3 } } } : {}) },
    held: true,
    ...over,
  });

/** Serves `rows` by state, and records the approve calls. */
async function serve(page: Page, rows: Row[]) {
  const approved: string[] = [];
  await page.route("**/api/v1/approvals*", async (route) => {
    const req = route.request();
    if (req.method() !== "GET") return route.continue();
    const state = new URL(req.url()).searchParams.get("state");
    const out = rows.filter((r) => (state === "" || state === null ? true : r.state === state) && !approved.includes(String(r.id)));
    await route.fulfill({ body: JSON.stringify(out), contentType: "application/json" });
  });
  await page.route("**/api/v1/approvals/*/approve", async (route) => {
    const id = route.request().url().split("/approvals/")[1].split("/")[0];
    approved.push(id);
    await route.fulfill({ body: JSON.stringify({ ...rows.find((r) => r.id === id), state: "APPROVED" }), contentType: "application/json" });
  });
  return approved;
}

async function open(page: Page) {
  await gotoConsole(page);
  await navTo(page, "Approvals");
}

test.describe("Approvals — a file download and a kubectl change", () => {
  test("a decider's file_download card shows the facts and approves once", async ({ page }) => {
    const runId = sql("SELECT id FROM agent_runs ORDER BY created_at LIMIT 1");
    const approved = await serve(page, [download("e2e-dl-1", runId, { requested_by: ASKER, deciders: ["unit_admins", "security"] })]);
    await open(page);
    const card = page.getByTestId("file-download-card");
    await expect(card).toBeVisible();
    await expect(card).toContainText(FILE_DOWNLOAD_CARD.TITLE("build-logs.tar.gz"));
    await expect(card).toContainText("18.4 MB (19,293,184 bytes)");
    await expect(card).toContainText(SHA);
    await expect(page.getByText(`Requested by ${ASKER}`)).toBeVisible();
    await expect(card).toContainText(DECIDERS_INBOX.SECOND_PERSON);
    await card.getByRole("button", { name: APPROVAL_MCP_CALL.APPROVE }).click();
    await expect(card).toHaveCount(0);
    expect(approved).toEqual(["e2e-dl-1"]);
  });

  test("the asker's own file_download card has its pair disabled", async ({ page }) => {
    const runId = sql("SELECT id FROM agent_runs ORDER BY created_at LIMIT 1");
    await page.route("**/api/v1/me", async (route) => {
      const response = await route.fetch();
      const json = await response.json();
      json.principal = ASKER;
      await route.fulfill({ response, json });
    });
    await serve(page, [download("e2e-dl-2", runId)]);
    await open(page);
    const card = page.getByTestId("file-download-card");
    await expect(card).toBeVisible();
    await expect(card.getByRole("button", { name: APPROVAL_MCP_CALL.APPROVE })).toBeDisabled();
    await expect(card.getByRole("button", { name: APPROVAL_MCP_CALL.DENY })).toBeDisabled();
    await expect(card).toContainText(DECIDERS_INBOX.SOMEONE_ELSE);
    await expect(card).toContainText(FILE_DOWNLOAD_CARD.ASKER_NOTE);
  });

  test("a kubectl write with a body shows the server's text and approves once", async ({ page }) => {
    const runId = sql("SELECT id FROM agent_runs ORDER BY created_at LIMIT 1");
    const approved = await serve(page, [kube("e2e-kube-1", runId, "PATCH", { call_text: { arguments: BODY_TEXT } })]);
    await open(page);
    const card = page.getByTestId("kube-call-card");
    await expect(card).toBeVisible();
    await expect(card.getByRole("region", { name: KUBE_CALL_CARD.FIELD_BODY })).toHaveText(BODY_TEXT);
    await card.getByRole("button", { name: APPROVAL_MCP_CALL.APPROVE }).click();
    await expect(card).toHaveCount(0);
    expect(approved).toEqual(["e2e-kube-1"]);
  });

  test("a held DELETE with empty text reads No body and can be approved", async ({ page }) => {
    const runId = sql("SELECT id FROM agent_runs ORDER BY created_at LIMIT 1");
    await serve(page, [kube("e2e-kube-2", runId, "DELETE", { call_text: { arguments: "" } })]);
    await open(page);
    const card = page.getByTestId("kube-call-card");
    await expect(card).toContainText(KUBE_CALL_CARD.NO_BODY);
    await expect(card.getByRole("button", { name: APPROVAL_MCP_CALL.APPROVE })).toBeEnabled();
  });

  test("a kubectl write with no call_text turns Approve off and keeps Deny", async ({ page }) => {
    const runId = sql("SELECT id FROM agent_runs ORDER BY created_at LIMIT 1");
    await serve(page, [kube("e2e-kube-3", runId, "PATCH")]);
    await open(page);
    const card = page.getByTestId("kube-call-card");
    await expect(card).toContainText(KUBE_CALL_CARD.NO_TEXT);
    await expect(card.getByRole("button", { name: APPROVAL_MCP_CALL.APPROVE })).toBeDisabled();
    await expect(card.getByRole("button", { name: APPROVAL_MCP_CALL.DENY })).toBeEnabled();
  });

  test("the Decided tab titles both kinds with their basis lines", async ({ page }) => {
    const runId = sql("SELECT id FROM agent_runs ORDER BY created_at LIMIT 1");
    await serve(page, [
      download("e2e-dl-3", runId, { state: "APPROVED", decided_by: "lee.chen@acme.example", decided_at: new Date().toISOString(), decision_basis: "security" }),
      kube("e2e-kube-4", runId, "PATCH", { state: "APPROVED", held: false, decided_by: "you", decided_at: new Date().toISOString(), decision_basis: "owner" }),
    ]);
    await open(page);
    await page.getByRole("tab", { name: /^Decided/ }).click();
    await expect(page.getByText(FILE_DOWNLOAD_CARD.LIST_TITLE("build-logs.tar.gz"))).toBeVisible();
    await expect(page.getByText(KUBE_CALL_CARD.LIST_TITLE("PATCH", "team-a"))).toBeVisible();
    await expect(page.getByText(DECIDERS_INBOX.BASIS_SECURITY)).toBeVisible();
    await expect(page.getByText(DECIDERS_INBOX.BASIS_OWNER)).toBeVisible();
  });
});
