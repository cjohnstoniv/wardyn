/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect } from "./fixtures";

// 0.8.6 fleet-fl4 (M6) — the admin Fleet capacity card on /admin/runs. The
// endpoint is route-mocked (the technique admin-run-monitor.spec.ts uses for
// proxy-window): the capacity figures need a populated cluster, and the card's
// job here is to render exactly what the response says.
const STUCK = "3f9c1a2b-0000-4000-8000-000000000001";

const k8s = {
  holding: 14,
  unknown: 3,
  agent_cpu_request_millis: 30000,
  agent_cpu_limit_millis: 30000,
  agent_memory_request_mib: 53248,
  agent_memory_limit_mib: 53248,
  proxy_cpu_millis: 6000,
  proxy_memory_mib: 9216,
  proxy_cpu_uncapped: 0,
  held_cpu_millis: 36000,
  held_memory_mib: 62464,
};

test.describe("the admin Fleet capacity card (fleet-fl4)", () => {
  test("shows configured reservations, the unknown count, a basis per runner and links a waiting run", async ({ page }) => {
    await page.route("**/api/v1/admin/runs/capacity", (route) =>
      route.fulfill({
        json: {
          generated_at: new Date().toISOString(),
          basis: "configured_reservations",
          states: { RUNNING: 9, STARTING: 2, WAITING_FOR_CONFIRMATION: 1, PENDING: 3 },
          paused: 2,
          kept: 1,
          totals: { basis: "requests", ...k8s },
          age_buckets: [
            { bucket: "under_1h", count: 4 },
            { bucket: "1h_to_8h", count: 6 },
            { bucket: "8h_to_24h", count: 2 },
            { bucket: "1d_to_7d", count: 2 },
            { bucket: "over_7d", count: 0 },
          ],
          by_runner: { k8s: { basis: "requests", ...k8s } },
          by_owner: [{ owner: "ana@example.com", holding: 3, by_runner: { k8s: { ...k8s, holding: 3 } } }],
          by_owner_truncated: false,
          unschedulable: [
            {
              id: STUCK,
              owner: "ana@example.com",
              waited_seconds: 7200,
              reason: "Unschedulable",
              runner_kind: "k8s",
              agent_cpu_request_millis: 2000,
              agent_memory_request_mib: 4096,
              proxy_cpu_millis: 500,
              proxy_memory_mib: 256,
            },
          ],
          unschedulable_total: 1,
        },
      }),
    );

    await page.goto("/admin/runs");
    const card = page.getByTestId("fleet-capacity-card");
    await expect(card).toContainText("14 runs · 36 CPU · 61 GiB configured reservations · 3 unknown");
    await expect(card).toContainText("Waiting for room · 1");

    await card.getByRole("button", { name: /Fleet capacity/ }).click();
    const block = card.getByTestId("fleet-capacity-runner-k8s");
    await expect(block).toContainText("Kubernetes · requests");
    await expect(block).toContainText("3 unknown — not in these totals.");

    await card.getByTestId("fleet-capacity-waiting").getByRole("link", { name: "3f9c1a2b" }).click();
    await expect(page).toHaveURL(new RegExp(`/admin/runs/${STUCK}$`));
  });
});
