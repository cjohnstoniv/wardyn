/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, gotoConsole } from "./fixtures";

// C-02 (#1062): a run's recording picker and egress tile must still surface
// rows written under the pre-0.8 action names — session.recording (now
// session.recording.write) and egress.pending (now egress.hold) —
// docs/AUDIT-ACTIONS.md's "Renamed in 0.8". scripts/e2e-backend.sh seeds one
// run ("e2e fixture recording-history") carrying both a pre- and a post-0.8
// recording row and one pre-0.8 held-egress row, buried behind 1000+
// unrelated audit rows on that SAME run, so this also proves the recording
// picker's action_prefix=session.recording fetch has its own budget separate
// from the general (unscoped, 1000-row-capped) audit fetch a chatty run would
// otherwise exhaust.
test.describe("Run detail — historical recording and held-egress rows survive a 1000+-event trail (C-02)", () => {
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
