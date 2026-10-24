/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { Page } from "@playwright/test";
import { test, expect, sql, consoleAPI, gotoConsole, navToRoute, ADMIN_TOKEN } from "./fixtures";
import { DESKTOP } from "../src/app/components/wardyn/copy/desktop";
import { AGENTS, PROVIDERS } from "../src/app/lib/workspace-providers-copy";

// ============================================================================
// Sign-ins in a Linux VM's desktop (#2972, board R11-2972-DesktopSignIns). The `none` runner behind this
// backend has no VM and no guest agent, so the seeded RUNNING run is shown as a VM on the wire, its sign-in
// store is the row a VM's guest report would write, and the runner's half of pause and resume (the guest
// agent's SIGSTOP and SIGCONT) is played by the route below. Every GET and every refusal is the real server;
// the guest agent's half is proven by the Go guest-agent tests and the hosted VM conformance job.
// ============================================================================

const RUN_TASK = "e2e fixture 2";
const auth = { Authorization: `Bearer ${ADMIN_TOKEN}` };

function runId(): string {
  const id = sql(`SELECT id FROM agent_runs WHERE task = '${RUN_TASK}' LIMIT 1`);
  expect(id, `no seeded run with task ${RUN_TASK}`).toMatch(/^[0-9a-f-]{36}$/);
  return id;
}

function setStore(id: string, store: "memory" | "none" | null): void {
  sql(
    store === null
      ? `DELETE FROM desktop_vm_sign_ins WHERE run_id = '${id}'`
      : `INSERT INTO desktop_vm_sign_ins (run_id, store) VALUES ('${id}', '${store}')
         ON CONFLICT (run_id) DO UPDATE SET store = EXCLUDED.store, paused_by = NULL, paused_at = NULL`,
  );
}

/** The seeded RUNNING run has no sandbox ref (e2e-backend.sh gives it one only under WARDYN_E2E_TMUX=1), so
 * a real pause is refused as not running before the policy is read; the refusal tests give it one. */
function setSandboxRef(id: string, ref: string | null): void {
  sql(`UPDATE agent_runs SET sandbox_ref = ${ref === null ? "NULL" : `'${ref}'`} WHERE id = '${id}'`);
}

async function openVMRun(page: Page, id: string) {
  await page.route(`**/api/v1/runs/${id}`, async (route) => {
    const res = await route.fetch();
    const body = await res.json();
    await route.fulfill({ response: res, body: JSON.stringify({ ...body, machine_kind: "linux_vm" }) });
  });
  await page.goto(`/runs/${id}`);
  await expect(page.getByRole("heading", { name: RUN_TASK, level: 1 })).toBeVisible();
  const desk = page.getByRole("region", { name: DESKTOP.title });
  await desk.scrollIntoViewIfNeeded();
  return desk;
}

/** The guest agent's half: the server's own row write after a stop or continue that answered, then its view. */
async function playGuestAgent(page: Page, id: string) {
  const seen: string[] = [];
  await page.route(`**/api/v1/runs/${id}/desktop/sign-ins/*`, async (route) => {
    const verb = new URL(route.request().url()).pathname.split("/").pop()!;
    seen.push(verb);
    sql(
      verb === "pause"
        ? `UPDATE desktop_vm_sign_ins SET paused_by = 'admin-token', paused_at = now() WHERE run_id = '${id}'`
        : `UPDATE desktop_vm_sign_ins SET paused_by = NULL, paused_at = NULL WHERE run_id = '${id}'`,
    );
    const view = await consoleAPI(page, "GET", `/api/v1/runs/${id}/desktop/sign-ins`);
    await route.fulfill({ status: 200, contentType: "application/json", body: view.text });
  });
  return seen;
}

test.describe.configure({ mode: "serial" });

test.describe("Desktop sign-ins (#2972)", () => {
  test.afterEach(() => {
    const id = runId();
    setStore(id, null);
    setSandboxRef(id, null);
  });

  test("pause, the disclosure, and resume: the desktop says where sign-ins live and that the agent can use them", async ({ page }) => {
    const id = runId();
    setStore(id, "memory");
    await gotoConsole(page);
    const seen = await playGuestAgent(page, id);
    const desk = await openVMRun(page, id);

    // State 1: Ready, the pause beside Open desktop, the memory note last.
    await expect(desk.getByText(DESKTOP.ready, { exact: true })).toBeVisible();
    await expect(desk.getByRole("button", { name: "Open desktop" })).toBeVisible();
    await expect(desk.getByText(DESKTOP.signInsNote, { exact: true })).toBeVisible();
    await desk.getByRole("button", { name: "Pause the agent to sign in" }).click();

    // State 2: Agent paused, the disclosure verbatim, Resume in the pause's place. A reload reads the
    // server's own row: the pause is the run's state, not the tab's.
    await expect(desk.getByText("Agent paused", { exact: true })).toBeVisible();
    await expect(desk.getByText("The agent is paused while you sign in. Once you resume it, it can use what you signed into.", { exact: true })).toBeVisible();
    await page.reload();
    const again = page.getByRole("region", { name: DESKTOP.title });
    await again.scrollIntoViewIfNeeded();
    await expect(again.getByText(DESKTOP.agentPaused, { exact: true })).toBeVisible();
    await expect(again.getByRole("button", { name: DESKTOP.pauseToSignIn })).toHaveCount(0);
    await again.getByRole("button", { name: "Resume the agent" }).click();

    await expect(again.getByText(DESKTOP.ready, { exact: true })).toBeVisible();
    await expect(again.getByText(DESKTOP.pausedNote)).toHaveCount(0);
    await expect(again.getByRole("button", { name: DESKTOP.pauseToSignIn })).toBeVisible();
    expect(seen).toEqual(["pause", "resume"]);
  });

  test("a desktop that keeps none names the agent, offers no pause, and the server refuses one", async ({ page }) => {
    const id = runId();
    setStore(id, "none");
    setSandboxRef(id, "e2e-desk-signin");
    await gotoConsole(page);
    const view = JSON.parse((await consoleAPI(page, "GET", `/api/v1/runs/${id}/desktop/sign-ins`)).text);
    expect(view).toMatchObject({ store: "none", can_pause: false });
    const desk = await openVMRun(page, id);
    await expect(desk.getByText(DESKTOP.signInsOff(view.agent), { exact: true })).toBeVisible();
    await expect(desk.getByRole("button", { name: DESKTOP.pauseToSignIn })).toHaveCount(0);
    const refused = await consoleAPI(page, "POST", `/api/v1/runs/${id}/desktop/sign-ins/pause`);
    expect(refused.status, refused.text).toBe(403);
    expect(JSON.parse(refused.text).reason).toBe("desktop_sign_ins_off");
  });

  test("an admin sets Not allowed in the Agents tab: the pause leaves the run page and the server refuses it", async ({ page }) => {
    const id = runId();
    setStore(id, "memory");
    setSandboxRef(id, "e2e-desk-signin");
    const agent = sql(`SELECT agent FROM agent_runs WHERE id = '${id}'`);
    const before = await page.request.get("/api/v1/agent-providers", { headers: auth });
    const saved = await before.json();
    try {
      await gotoConsole(page);
      await navToRoute(page, "/admin/settings");
      await page.getByTestId("providers-card").getByRole("link").click();
      await page.getByRole("tab", { name: AGENTS.AGENTS_TITLE }).click();
      const row = page.getByTestId(`agent-row-${agent}`);
      await expect(row.getByText(AGENTS.DESKTOP_SIGN_INS_HINT, { exact: true })).toBeVisible();
      const field = row.getByRole("group", { name: new RegExp(`^${AGENTS.FIELD_DESKTOP_SIGN_INS} — `) });
      await expect(field.getByRole("button", { name: "Allowed", exact: true })).toHaveAttribute("aria-pressed", "true");
      await field.getByRole("button", { name: "Not allowed", exact: true }).click();
      await page.getByRole("button", { name: PROVIDERS.SAVE_CTA }).click();
      await expect(page.getByText(PROVIDERS.SAVED_TOAST)).toBeVisible();
      const roster = await (await page.request.get("/api/v1/agent-providers", { headers: auth })).json();
      expect(roster.agents.find((a: { id: string }) => a.id === agent)).toMatchObject({ desktop_sign_ins_off: true });

      const desk = await openVMRun(page, id);
      await expect(desk.getByText(DESKTOP.signInsNote, { exact: true })).toBeVisible();
      await expect(desk.getByRole("button", { name: DESKTOP.pauseToSignIn })).toHaveCount(0);
      const refused = await consoleAPI(page, "POST", `/api/v1/runs/${id}/desktop/sign-ins/pause`);
      expect(refused.status, refused.text).toBe(403);
      expect(JSON.parse(refused.text).reason).toBe("desktop_sign_ins_off");
    } finally {
      const cur = await page.request.get("/api/v1/agent-providers", { headers: auth });
      const headers: Record<string, string> = { ...auth };
      const etag = cur.headers()["etag"];
      if (etag) headers["If-Match"] = etag;
      await page.request.put("/api/v1/agent-providers", { headers, data: saved });
    }
  });
});
