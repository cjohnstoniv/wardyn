/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, gotoConsole, ADMIN_TOKEN, sql } from "./fixtures";

const AUTH = { Authorization: `Bearer ${ADMIN_TOKEN}` };

// C-02 (#1062): a run's recording picker and egress tile must still surface
// rows written under the pre-0.8 action names — session.recording (now
// session.recording.write) and egress.pending (now egress.hold) —
// docs/AUDIT-ACTIONS.md's "Renamed in 0.8". This file seeds its OWN run
// ("e2e fixture recording-history") in `beforeAll` — not scripts/e2e-backend.sh's
// shared seed, which keeps the default backend under the 1000-row cap for
// every OTHER spec — carrying both a pre- and a post-0.8 recording row and
// one pre-0.8 held-egress row, buried behind 1000+ unrelated audit rows on
// that SAME run, so this also proves the recording picker's
// action_prefix=session.recording fetch has its own budget separate from the
// general (unscoped, 1000-row-capped) audit fetch a chatty run would
// otherwise exhaust. run-ui-e2e.sh boots a FRESH backend per spec file, so
// this fixture never leaks into another spec's run count.
test.describe("Run detail — historical recording and held-egress rows survive a 1000+-event trail (C-02)", () => {
  test.beforeAll(async ({ request }) => {
    const resp = await request.post("/api/v1/runs", {
      headers: AUTH,
      data: { agent: "claude-code", repo: "acme/widgets", title: "", task: "e2e fixture recording-history" },
    });
    expect(resp.ok(), `create run failed: ${resp.status()} ${await resp.text()}`).toBeTruthy();
    const { id: runId } = (await resp.json()) as { id: string };

    // Ordering mirrors the fixture this replaces (scripts/e2e-backend.sh's
    // former C-02 block): egress.pending BEFORE the 1001 filler rows, so it
    // stays inside this run's own oldest-first, 1000-row-capped general
    // fetch; the two recording rows LAST — fine, since attachSessions reads
    // them off the SEPARATE action_prefix=session.recording fetch, which
    // never competes with the filler for cap room at all.
    sql(
      `INSERT INTO audit_events (id, time, run_id, actor_type, actor, action, target, outcome, data)
       SELECT gen_random_uuid(), now(), '${runId}', 'system', 'proxy',
              'egress.pending', 'held.example.com:443', 'success', '{"domain":"held.example.com"}'::jsonb`,
    );
    sql(
      `INSERT INTO audit_events (id, time, run_id, actor_type, actor, action, target, outcome, data)
       SELECT gen_random_uuid(), now() - (n + 60 || ' seconds')::interval, '${runId}', 'agent', 'agent',
              'egress.allow', 'filler.example.com:443', 'success', '{}'::jsonb
       FROM generate_series(1, 1001) AS n`,
    );
    sql(
      `INSERT INTO audit_events (id, time, run_id, actor_type, actor, action, target, outcome, data)
       SELECT gen_random_uuid(), now() - interval '30 seconds', '${runId}', 'human', 'alice',
              'session.recording', '${runId}~e2e-session-old', 'success', '{}'::jsonb`,
    );
    sql(
      `INSERT INTO audit_events (id, time, run_id, actor_type, actor, action, target, outcome, data)
       SELECT gen_random_uuid(), now(), '${runId}', 'human', 'bob',
              'session.recording.write', '${runId}~e2e-session-new', 'success', '{}'::jsonb`,
    );
  });

  test("both the pre- and post-0.8 recording sessions appear in the picker, and the pre-0.8 held egress row still renders", async ({
    page,
  }) => {
    await gotoConsole(page);
    await page.getByText("e2e fixture recording-history").click();
    await expect(
      page.getByRole("heading", { name: "e2e fixture recording-history", level: 1 }),
    ).toBeVisible();

    // The Egress widget (on the Overview tab by default) is derived from the
    // general audit fetch — the historical egress.pending row was seeded to
    // clear that fetch's 1000-row, oldest-first cap despite 1000+ newer
    // filler rows on the same run, and still renders as held.
    await expect(page.getByRole("heading", { name: "Egress" })).toBeVisible();
    await expect(page.getByText("held.example.com")).toBeVisible();

    // The recording picker's index is a SEPARATE, action_prefix-scoped
    // fetch — both the old (session.recording) and new
    // (session.recording.write) named rows must appear regardless of the
    // same run's 1000+-row general trail.
    await page.getByRole("tab", { name: /recording/i }).click();
    await page.getByRole("combobox", { name: "Recorded session" }).click();
    await expect(page.getByText(/alice/)).toBeVisible();
    await expect(page.getByText(/bob/)).toBeVisible();
  });
});
