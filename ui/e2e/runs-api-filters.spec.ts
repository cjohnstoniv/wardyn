/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #1197: the "Your runs" scoping pin belongs at the API level, not the UI —
// the console page sends no `view` yet (ui/src/app/components/screens/
// runs.tsx calls the plain listRuns(), unchanged by this lane), so a
// UI-level assertion on the runs page would be unreachable here. This
// mirrors member-console.spec.ts's own `page.request.get` pattern for the
// admin read.

import { test, expect, ADMIN_TOKEN, asRealMember, consoleAPI } from "./fixtures";

test("GET /runs: view=user forces owner=me for an admin token, view=admin still sees everyone", async ({ page }) => {
  // Seed a run owned by someone other than the admin token.
  await asRealMember(page);
  const foreignCreated = await consoleAPI(page, "POST", "/api/v1/runs", { agent: "claude-code", task: "foreign run for owner-scope test" });
  expect(foreignCreated.status, foreignCreated.text).toBe(201);
  const foreignID = JSON.parse(foreignCreated.text).id as string;

  const getAsAdmin = (query: string) =>
    page.request.get(`/api/v1/runs${query}`, { headers: { Authorization: `Bearer ${ADMIN_TOKEN}` } });

  // Today's admin scope (no view=): sees everyone, foreign run included.
  const unscoped = await getAsAdmin("");
  expect(unscoped.status()).toBe(200);
  const unscopedIDs = (await unscoped.json()).map((r: { id: string }) => r.id);
  expect(unscopedIDs).toContain(foreignID);

  // The owner-forcing fix: view=user forces owner=me for the admin token
  // too, even though owner=all on its own would otherwise mean everyone for
  // an operator.
  const scoped = await getAsAdmin("?view=user&owner=all");
  expect(scoped.status()).toBe(200);
  const scopedRuns = (await scoped.json()) as Array<{ id: string; created_by: string }>;
  expect(scopedRuns.map((r) => r.id)).not.toContain(foreignID);
  for (const r of scopedRuns) {
    expect(r.created_by).toBe("admin-token");
  }

  // view=admin (explicit) keeps the org-wide scope — the owner force is
  // view=user-specific, not a blanket narrowing of every operator read.
  const admin = await getAsAdmin("?view=admin");
  expect(admin.status()).toBe(200);
  const adminIDs = (await admin.json()).map((r: { id: string }) => r.id);
  expect(adminIDs).toContain(foreignID);
});
