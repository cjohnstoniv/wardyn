/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { randomBytes } from "node:crypto";
import { test, expect, asRealMember, gotoConsole, navTo, sql, MEMBER_PRINCIPAL } from "./fixtures";

// A person's own Runs list, the User view the console always asks for, carries the runs of the service
// identities they currently sponsor (design section 8; the API half is the runs-list filter in
// internal/api/runs_list_filter.go). A run of a service identity somebody else sponsors is not in it.
test.describe("Runs: the service identities a member sponsors (#1508)", () => {
  test("the member's list carries their service identity's run, and no other identity's", async ({ page }) => {
    const tag = randomBytes(4).toString("hex");
    const mine = `e2e-mine-${tag}`;
    const theirs = `e2e-theirs-${tag}`;
    sql(`INSERT INTO service_identities (name, sponsor, created_by) VALUES ('${mine}', '${MEMBER_PRINCIPAL}', 'e2e-ops')`);
    sql(`INSERT INTO service_identities (name, sponsor, created_by) VALUES ('${theirs}', 'e2e-someone-else', 'e2e-ops')`);
    sql(`UPDATE agent_runs SET created_by = 'service:${mine}' WHERE task = 'e2e fixture 2'`);
    sql(`UPDATE agent_runs SET created_by = 'service:${theirs}' WHERE task = 'e2e fixture 3'`);

    await asRealMember(page);
    await gotoConsole(page, "user");
    await navTo(page, "Runs");

    await expect(page.getByText("e2e fixture 2")).toBeVisible();
    await expect(page.getByText("e2e fixture 3")).toHaveCount(0);
  });
});
