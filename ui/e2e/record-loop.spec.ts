/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, gotoConsole, navToRoute } from "./fixtures";

// E2E coverage for the Record Mode loop's guided close (Workstream B, B4):
// caught state -> guided "Approve selected + replay again" -> the
// requirements PUT + confined re-POST it fires -> the clean chip -> the B3
// roll-up line. Intercept-driven (page.route), like recording.spec.ts: no
// real sandbox exists to drive an actual confined replay, so a single
// mutable `state` object stands in for the workspace row. The requirements
// PUT and the confined re-POST each mutate it in place before responding —
// the UI's own next fetch (workspace-detail.tsx's load(false), fired right
// after doRecord resolves) already reflects the outcome, so the spec needs
// no timers, no polling, no attach-terminal machinery. Fully synthetic
// (WS_ID is never a real workspace), so there's nothing to clean up.

const WS_ID = "e2e-record-loop-ws";
const WS_GLOB = `**/api/v1/workspaces/${WS_ID}`;
const REQUIREMENTS_GLOB = `**/api/v1/workspaces/${WS_ID}/requirements`;
const RECORD_GLOB = `**/api/v1/workspaces/${WS_ID}/record`;

// The starting state: session "build & test" recorded open, and its confined
// replay already settled CAUGHT — one host held (pending), not denied, so
// it's the case B2 defaults to CHECKED in the guided selector.
function initialWorkspace() {
  return {
    id: WS_ID,
    name: "record-loop-e2e",
    kind: "repo",
    source: "acme/record-loop",
    status: "scanned",
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    requirements: {},
    record_results: {
      "build-test": {
        run_id: "run-a",
        label: "build & test",
        mode: "interactive",
        status: "recorded",
        observations: { domains: [], minted_grant_ids: [], exec_argv0s: [], file_writes: [], connects: [], anomalies: [] },
      },
      "verify:build-test": {
        run_id: "run-b",
        label: "build & test",
        mode: "interactive",
        confined: true,
        status: "recorded",
        clean: false,
        caught: 1,
        finished_at: "2026-01-01T00:05:00Z",
        observations: {
          domains: [{ host: "files.pythonhosted.org", methods: ["GET"], allow_count: 0, deny_count: 0, pending_count: 1 }],
        },
      },
    },
  };
}

test("caught -> guided approve -> requirements PUT + confined re-POST -> clean chip + roll-up line", async ({ page }) => {
  const state = initialWorkspace();
  const putBodies: unknown[] = [];
  const recordBodies: unknown[] = [];

  // GET always answers with the current mutable state; the PUT and POST
  // handlers below mutate it before their own response, so the very next GET
  // (load(false), fired by the app itself right after each write settles)
  // already reflects the outcome.
  await page.route(WS_GLOB, (route) =>
    route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(state) }),
  );

  await page.route(REQUIREMENTS_GLOB, async (route) => {
    const body = route.request().postDataJSON();
    putBodies.push(body);
    state.requirements = body.requirements;
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(state) });
  });

  await page.route(RECORD_GLOB, async (route) => {
    const body = route.request().postDataJSON();
    recordBodies.push(body);
    // Simulate the confined replay landing clean, instantly — this spec
    // proves the loop's WIRING (what gets posted, what renders after), not
    // the sandbox itself.
    state.record_results["verify:build-test"] = {
      run_id: "run-c",
      label: "build & test",
      mode: "interactive",
      confined: true,
      status: "recorded",
      clean: true,
      caught: 0,
      finished_at: "2026-01-01T00:10:00Z",
      observations: {
        domains: [{ host: "files.pythonhosted.org", methods: ["GET"], allow_count: 1, deny_count: 0, pending_count: 0 }],
      },
    };
    await route.fulfill({
      status: 202,
      contentType: "application/json",
      body: JSON.stringify({ record_run_id: "run-c", confinement_class: "CC2", warnings: [] }),
    });
  });

  await gotoConsole(page);
  await navToRoute(page, `/workspaces/${WS_ID}`);
  await expect(page.getByRole("heading", { name: "record-loop-e2e" })).toBeVisible();

  const session = page.getByTestId("session-build-test");
  await expect(session).toBeVisible();

  // Starts caught: the held (not denied) host defaults CHECKED (B2's rule —
  // only a live-denied host defaults out).
  await expect(session).toContainText("Replayed — caught 1");
  await expect(session.getByRole("checkbox", { name: "Approve files.pythonhosted.org" })).toBeChecked();

  await session.getByRole("button", { name: /^approve 1 selected host and replay again$/i }).click();

  // The shared untrusted-content confirm — single host, non-selectable.
  const confirm = page.getByRole("alertdialog");
  await expect(confirm).toContainText("Approve egress to files.pythonhosted.org?");
  await confirm.getByRole("button", { name: "Approve host" }).click();

  // The requirements PUT landed the checked host as an egress: required row...
  await expect(session).toContainText("Replayed clean");
  expect(putBodies).toEqual([
    { requirements: { "egress:files.pythonhosted.org": { level: "required", provenance: "operator_set" } } },
  ]);
  // ...and the guided loop's second half — the confined re-POST — fired for
  // the SAME named session, confined.
  expect(recordBodies).toEqual([{ name: "build & test", confined: true }]);

  // The clean chip renders, and so does B3's workspace-wide roll-up line.
  await expect(session.getByText("Replayed clean")).toBeVisible();
  await expect(page.getByTestId("record-last-clean-replay")).toContainText("Last clean confined replay: build & test");
});
