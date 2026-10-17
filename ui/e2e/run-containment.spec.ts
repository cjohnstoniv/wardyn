/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A run its Kubernetes runner deleted but cannot prove stopped (design K1 §3.1 Q2 and (b)): run detail
// shows the egress bound, then "termination unknown", names a reused node name, and gives a security
// operator the node record form with the machines it must name. The run and its containment are spliced
// in, since no seed script can put a runner in that state.
import { test, expect, gotoConsole, navTo } from "./fixtures";
import type { Page } from "@playwright/test";
import { CONTAINMENT as C } from "../src/app/components/wardyn/copy/run-containment";

const tuple = {
  run_id: "run",
  pods: [
    {
      component: "proxy",
      uid: "uid-proxy-1",
      node_name: "n1",
      causal_uid: "uid-n1",
      post_bind: { name: "n1", uid: "uid-n1", machine_id: "m1", system_uuid: "s1" },
    },
  ],
  reuses: [{ name: "n1", uid: "uid-n1-new", machine_id: "m7", system_uuid: "s7" }],
};

async function openHeldRun(page: Page, boundPassed: boolean): Promise<{ posted: () => unknown }> {
  let posted: unknown;
  await page.route("**/api/v1/runs/*", async (route) => {
    if (route.request().method() !== "GET") return route.fallback();
    const response = await route.fetch();
    const json = await response.json();
    if (json.task === "e2e fixture 2") Object.assign(json, { placement: "local", runner_name: "cluster-a", runner_online: true });
    await route.fulfill({ response, json });
  });
  await page.route("**/api/v1/runs/*/containment", (route) =>
    route.fulfill({
      json: {
        pending: {
          kind: "pending_delete",
          since: new Date(Date.now() - 40 * 60_000).toISOString(),
          bound_until: new Date(Date.now() + (boundPassed ? -5 : 20) * 60_000).toISOString(),
          bound_passed: boundPassed,
          name_reused: true,
          no_causal_association: false,
          two_people: true,
          tuple,
        },
        records: [],
      },
    }),
  );
  await page.route("**/api/v1/runs/*/node-record", async (route) => {
    posted = route.request().postDataJSON();
    await route.fulfill({
      status: 202,
      headers: { Location: "/api/v1/governance/changes/00000000-0000-0000-0000-000000000001" },
      json: { pending_change: { id: "00000000-0000-0000-0000-000000000001", state: "pending", target_kind: "run_node_record" } },
    });
  });
  await gotoConsole(page);
  await navTo(page, "Runs");
  await page.getByText("e2e fixture 2").click();
  await expect(page.getByRole("heading", { name: "e2e fixture 2", level: 1 })).toBeVisible();
  return { posted: () => posted };
}

test.describe("Run detail — a Kubernetes run held as containment pending", () => {
  test("shows the egress bound, the reused node name, and the record form with its machines", async ({ page }) => {
    await openHeldRun(page, false);
    await expect(page.getByTestId("run-containment-line")).toContainText("Containment pending; egress bounded until");
    await expect(page.getByTestId("run-containment-name-reused")).toContainText(C.NAME_REUSED);
    const form = page.getByTestId("run-node-record-form");
    await expect(form).toContainText(C.WARN_DELETED_NODE);
    await expect(page.getByTestId("run-node-record-machines")).toContainText(C.holder(C.HOLDER_CAUSAL, "uid-n1"));
    await expect(page.getByTestId("run-node-record-reuses")).toContainText(C.reuse("n1", "uid-n1-new", "m7", "s7"));
    await expect(form).toContainText("Pods marked for deletion: at or before");
    await expect(form.getByText(C.ATTEST_NO_PRIOR)).toHaveCount(0);
  });

  test("past the bound it says termination unknown, and a record waits for a second person", async ({ page }) => {
    const held = await openHeldRun(page, true);
    await expect(page.getByTestId("run-containment-line")).toHaveText(C.PASSED_TITLE);
    const form = page.getByTestId("run-node-record-form");
    await form.getByLabel(C.SOURCE_ID_LABEL, { exact: false }).fill("i-0abc123");
    await form.getByRole("button", { name: C.SUBMIT }).click();
    await expect(page.getByTestId("run-node-record-held")).toHaveText(C.HELD);
    expect(held.posted()).toMatchObject({ attestation: "node_powered_off", source: "provider_console", source_id: "i-0abc123", tuple });
  });
});
