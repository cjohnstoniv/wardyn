/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { randomUUID } from "node:crypto";
import { test, expect, ADMIN_TOKEN, gotoConsole, navTo } from "./fixtures";
import { GOVERNANCE as GOV, RUN_LIMITS as RL, runLimitsChip } from "../src/app/lib/governance-copy";
import type { Page } from "@playwright/test";

// Split out of governance.spec.ts (#209): the profile-limits round trips —
// the run-quota chip, the two storage ceilings, and the seven run limits.
// Each case here creates its OWN uniquely-named profile (randomUUID
// suffixed) rather than reading governance.spec.ts's "walled" fixture, so
// none of it depends on that file's authoring-walk state or its serial
// ordering.
test.describe.configure({ mode: "serial" });

// The §E north-star ceiling — the same literal governance.spec.ts's own
// YOLO_CEILING const carries; duplicated here (rather than shared across
// files, which AGENTS.md's "a spec must never import another spec" forbids)
// because the quota-only test below needs a ceiling the server accepts and
// asserts nothing about its contents.
const YOLO_CEILING = {
  allowed_domains: ["api.anthropic.com"],
  denied_domains: ["github.example.com", "artifactory.corp.example.com"],
  first_use_approval: "deny_with_review",
  min_confinement_class: "CC3",
  auto_stop_after_sec: 3600,
  eligible_grants: [],
  tool_rules: [{ tool: "*", effect: "allow" }],
};

// The screen draws TWO tables, and a profile NAME is a cell in both — see
// governance.spec.ts's own copy of this helper for the full rationale.
const profilesTable = (page: Page) => page.getByRole("table").first();

// R4/F032 — the Limits cell tested only the three BOOLEAN doors, so a profile
// whose one limit is a run quota read GOV.LIMITS_NONE ("None") while
// denyUserRunQuota (internal/api/runs_create_validate.go) was refusing that
// member's next run with a 422. Real profile, real row: only the rendered table
// proves the cell, and only a stored max_concurrent_runs proves it round-trips
// the wire.
test.describe("governance — a quota-only profile is not 'None'", () => {
  // ticket: R4/F032
  test("names the cap in the Limits column, and leaves an unlimited profile reading None", async ({
    page,
  }) => {
    const auth = { Authorization: `Bearer ${ADMIN_TOKEN}` };
    const capped = `quota-only-${randomUUID().slice(0, 8)}`;
    const free = `unlimited-${randomUUID().slice(0, 8)}`;
    for (const [name, limits] of [
      [capped, { max_concurrent_runs: 3 }],
      [free, {}],
    ] as const) {
      const res = await page.request.post("/api/v1/governance/profiles", {
        headers: auth,
        data: { name, ceiling: YOLO_CEILING, limits },
      });
      expect(res.status()).toBe(201);
    }

    await gotoConsole(page, "admin");
    await navTo(page, "Governance");

    const cappedRow = page.getByRole("row").filter({ hasText: capped });
    await expect(cappedRow.getByText(GOV.LIMIT_QUOTA_LABEL(3))).toBeVisible();
    await expect(cappedRow.getByText(GOV.LIMITS_NONE, { exact: true })).toHaveCount(0);
    // ...and the genuinely unlimited one still says None.
    await expect(
      page.getByRole("row").filter({ hasText: free }).getByText(GOV.LIMITS_NONE, { exact: true }),
    ).toBeVisible();
  });
});

// The 0.7.2 storage ceilings: two more integer limits share LimitNumberRow's
// shape with max_concurrent_runs (F032, above) — max_ephemeral_disk_mib and
// max_drive_size_mib. Neither earns a Limits-column chip (only the run quota
// does — governance-screen.tsx's derivation of GOV.LIMITS_NONE reads all
// three doors plus the quota, deliberately not these two, which surface on
// /providers' Storage tab as the ceiling an admin sets and on /drives at
// write-time instead), so the round-trip through the EDITOR — real fields,
// real save, real reload — is the only e2e proof either exists on the wire.
test.describe("governance — the two storage ceilings round-trip through the editor", () => {
  // ticket: 0.7.2
  test("both LimitNumberRows write real integers, and 0 means unlimited on both", async ({ page }) => {
    const name = `storage-ceilings-${randomUUID().slice(0, 8)}`;
    await gotoConsole(page, "admin");
    await navTo(page, "Governance");
    await page.getByRole("button", { name: GOV.NEW_CTA, exact: true }).click();

    const editor = page.getByTestId("governance-profile-editor");
    await page.locator("#governance-profile-name").fill(name);

    // Both rows render their own hint, and both say 0 is unlimited — read
    // straight off the frozen copy, not retyped.
    await expect(editor.getByText(GOV.LIMIT_EPHEMERAL_HINT)).toBeVisible();
    await expect(editor.getByText(GOV.LIMIT_DRIVE_SIZE_HINT)).toBeVisible();

    const ephemeral = page.locator("#governance-limit-ephemeral");
    const driveSize = page.locator("#governance-limit-drive-size");
    // Unset reads as the LimitNumberRow's own empty value, never a literal 0 —
    // the same numberField convention the Storage tab's disk fields use, so an
    // admin never mistakes "nothing set" for "explicitly zero".
    await expect(ephemeral).toHaveValue("");
    await expect(driveSize).toHaveValue("");

    await ephemeral.fill("4096");
    await driveSize.fill("102400");
    await page.getByRole("button", { name: GOV.SAVE, exact: true }).click();
    await expect(editor).toHaveCount(0);

    // The stored row, read back from the server — not local component state.
    const auth = { Authorization: `Bearer ${ADMIN_TOKEN}` };
    const snap = await (await page.request.get("/api/v1/governance", { headers: auth })).json();
    const stored = snap.profiles.find((p: { name: string }) => p.name === name);
    expect(stored, `profile ${name} missing — did the save land?`).toBeTruthy();
    expect(stored.limits.max_ephemeral_disk_mib).toBe(4096);
    expect(stored.limits.max_drive_size_mib).toBe(102400);

    // Reopening the SAME profile shows the SAME two numbers — the editor
    // reads the stored row, not a value it remembers from the form it just
    // closed.
    await page.getByRole("button", { name: `${GOV.EDIT} ${name}` }).click();
    await expect(page.locator("#governance-limit-ephemeral")).toHaveValue("4096");
    await expect(page.locator("#governance-limit-drive-size")).toHaveValue("102400");

    // Clearing both back to empty and saving persists them as unlimited
    // (0/absent), never as a refused write — 0 is a valid ceiling, not an
    // error.
    await page.locator("#governance-limit-ephemeral").fill("");
    await page.locator("#governance-limit-drive-size").fill("");
    await page.getByRole("button", { name: GOV.SAVE, exact: true }).click();
    await expect(page.getByTestId("governance-profile-editor")).toHaveCount(0);

    const snap2 = await (await page.request.get("/api/v1/governance", { headers: auth })).json();
    const cleared = snap2.profiles.find((p: { name: string }) => p.name === name);
    expect(cleared.limits.max_ephemeral_disk_mib ?? 0).toBe(0);
    expect(cleared.limits.max_drive_size_mib ?? 0).toBe(0);
  });
});

// RL-14 (0.8, #579): the seven run limits. The editor shows days, hours and
// minutes and the wire is seconds, so only a real save and a real read-back
// prove the conversion — and the default-past-max refusal is the SERVER's
// (runLimitsRefusal), so only this backend proves an admin is shown it.
test.describe("governance — the run limits round-trip through the editor (RL-14)", () => {
  test("durations save as seconds, reopen in their units, and a default past its max is refused", async ({
    page,
  }) => {
    const name = `run-limits-${randomUUID().slice(0, 8)}`;
    const auth = { Authorization: `Bearer ${ADMIN_TOKEN}` };
    const stored = async () => {
      const snap = await (await page.request.get("/api/v1/governance", { headers: auth })).json();
      const p = snap.profiles.find((x: { name: string }) => x.name === name);
      expect(p, `profile ${name} missing — did the save land?`).toBeTruthy();
      return p.limits;
    };
    const unitPicker = (label: string) =>
      page.getByRole("combobox", { name: RL.UNIT_PICKER_LABEL(label), exact: true });

    await gotoConsole(page, "admin");
    await navTo(page, "Governance");
    await page.getByRole("button", { name: GOV.NEW_CTA, exact: true }).click();
    const editor = page.getByTestId("governance-profile-editor");
    await page.locator("#governance-profile-name").fill(name);

    await page.locator("#governance-limit-max-end").fill("14");
    await page.locator("#governance-limit-default-end").fill("1");
    await page.locator("#governance-limit-max-wait").fill("8");
    // A sub-hour wait in the hours row: the number, then the unit.
    await page.locator("#governance-limit-default-wait").fill("30");
    await unitPicker(RL.DEFAULT_WAIT_LABEL).click();
    await page.getByRole("option", { name: "minutes", exact: true }).click();
    await page.locator("#governance-limit-pause-idle").fill("30");
    await editor.getByRole("switch", { name: RL.ALLOW_NO_END_LABEL, exact: true }).click();
    await editor.getByRole("switch", { name: RL.USER_CHANGES_LABEL, exact: true }).click();
    await page.getByRole("button", { name: GOV.SAVE, exact: true }).click();
    await expect(editor).toHaveCount(0);

    const limits = await stored();
    expect(limits.max_end_ahead_sec).toBe(1209600);
    expect(limits.default_end_sec).toBe(86400);
    expect(limits.max_wait_sec).toBe(28800);
    expect(limits.default_wait_sec).toBe(1800);
    expect(limits.pause_idle_after_sec).toBe(1800);
    expect(limits.allow_no_end).toBe(true);
    expect(limits.user_changes_limits).toBe(true);

    // The list names what was stored, and no longer reads None.
    const row = profilesTable(page).getByRole("row").filter({ hasText: name });
    await expect(row.getByText(runLimitsChip(limits)!, { exact: true })).toBeVisible();
    await expect(row.getByText(GOV.LIMITS_NONE, { exact: true })).toHaveCount(0);

    // Reopened from the stored row: each value in its unit — 1800s in the
    // hours row reads 30 minutes, never 1 hour or blank.
    await page.getByRole("button", { name: `${GOV.EDIT} ${name}` }).click();
    for (const [id, label, value, unit] of [
      ["#governance-limit-max-end", RL.MAX_END_LABEL, "14", "days"],
      ["#governance-limit-default-end", RL.DEFAULT_END_LABEL, "1", "days"],
      ["#governance-limit-max-wait", RL.MAX_WAIT_LABEL, "8", "hours"],
      ["#governance-limit-default-wait", RL.DEFAULT_WAIT_LABEL, "30", "minutes"],
      ["#governance-limit-pause-idle", RL.PAUSE_IDLE_LABEL, "30", "minutes"],
    ] as const) {
      await expect(page.locator(id)).toHaveValue(value);
      await expect(unitPicker(label)).toHaveText(unit);
    }
    await expect(editor.getByRole("switch", { name: RL.ALLOW_NO_END_LABEL, exact: true })).toHaveAttribute(
      "aria-checked",
      "true",
    );
    await expect(editor.getByRole("switch", { name: RL.USER_CHANGES_LABEL, exact: true })).toHaveAttribute(
      "aria-checked",
      "true",
    );

    // A default end past the longest end: the server refuses the write, the
    // editor stays open showing its message, and the stored row is unchanged.
    await page.locator("#governance-limit-default-end").fill("30");
    await page.getByRole("button", { name: GOV.SAVE, exact: true }).click();
    await expect(editor.getByRole("alert")).toContainText(
      "limits.default_end_sec: 2592000 is past limits.max_end_ahead_sec (1209600)",
    );
    await expect(editor).toBeVisible();
    expect((await stored()).default_end_sec).toBe(86400);
  });
});
