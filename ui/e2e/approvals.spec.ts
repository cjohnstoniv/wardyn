/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { execFileSync } from "node:child_process";
import { randomUUID } from "node:crypto";
import type { Page } from "@playwright/test";
import { test, expect, gotoConsole } from "./fixtures";

// ---------------------------------------------------------------------------
// Approvals screen e2e (lane: approvals, port 8104, db wardyn_e2e_4).
//
// The shared seed (scripts/e2e-backend.sh) does NOT create approvals, so this
// lane seeds its own approval fixtures directly via SQL (the backend exposes no
// admin-token endpoint to *create* an approval — `POST /internal/approvals`
// requires a run-scoped token). We talk to the same Postgres the backend uses
// via `docker exec <container> psql`, mirroring the script's own SQL-seeding
// pattern, and we hold the container/db names in env-overridable constants so
// the verifier (which re-runs this spec against the same backend) stays portable.
//
// Redesign invariants covered (see approvals.tsx + wardyn/copy.ts):
//   * every pending card gets a per-kind BLAST-RADIUS banner ("What you're
//     approving:" / "Blast radius:") derived from the real requested_scope, plus
//     a RunContextRow that fetches the gating run and links to /runs/:id.
//   * credential blast copy is the HONEST broker line (the agent works through a
//     short-lived scoped credential; the stored key stays in Wardyn) — approval
//     only AUTHORIZES a later mint, never mints on the spot.
//   * the decision dialog/button says "Approve" — NOT "Approve & mint".
//   * the Decided view surfaces EXPIRED approvals, and a DENIED verdict renders
//     in the danger/red tone, never green.
//   * deny requires a reason; approve does not.
// ---------------------------------------------------------------------------

const PG_CONTAINER = process.env.WARDYN_E2E_PG_CONTAINER || "wardyn-test-pg";
const PG_DBNAME = process.env.WARDYN_E2E_PG_DBNAME || "wardyn_e2e_4";

// Every test in this file mutates / reads the SAME shared backend (one wardynd +
// one Postgres). Force the whole file to run sequentially in a single worker so
// backend state is deterministic (a mutating test can't yank the pending queue
// out from under a read-only assertion).
test.describe.configure({ mode: "serial" });

// Run one SQL statement against the lane's Postgres. -tA gives tuple-only,
// unaligned output so the caller can parse a single scalar trivially.
function sql(statement: string): string {
  return execFileSync(
    "docker",
    ["exec", "-i", PG_CONTAINER, "psql", "-U", "wardyn", "-d", PG_DBNAME, "-tAc", statement],
    { encoding: "utf8" },
  ).trim();
}

// A run id to bind seeded approvals to (FK approvals.run_id -> agent_runs.id).
function anyRunId(): string {
  const id = sql("SELECT id FROM agent_runs ORDER BY created_at LIMIT 1");
  if (!id) throw new Error("no seeded runs found — is the backend up and seeded?");
  return id;
}

// Insert a PENDING approval and return its id. The unique marker travels in the
// requested_scope JSON so the card it produces is unambiguously locatable on
// screen (the derived title + banner render the marker as visible text).
function seedPending(opts: {
  kind: "credential" | "egress_domain" | "tool_call";
  scope: Record<string, unknown>;
}): string {
  const id = randomUUID();
  const runId = anyRunId();
  const scopeJson = JSON.stringify(opts.scope).replace(/'/g, "''");
  sql(
    `INSERT INTO approvals (id, run_id, kind, requested_scope, state, requested_at)
     VALUES ('${id}', '${runId}', '${opts.kind}', '${scopeJson}'::jsonb, 'PENDING', now())`,
  );
  return id;
}

// Delete every PENDING approval so a mutating test starts from a known-empty
// pending queue. Idempotent and re-seed tolerant.
function clearPending(): void {
  sql("DELETE FROM approvals WHERE state = 'PENDING'");
}

function deleteApproval(id: string): void {
  sql(`DELETE FROM approvals WHERE id = '${id}'`);
}

// A per-test-run-unique domain marker. Decided approvals accumulate across runs,
// so a fixed marker would eventually match multiple cards; a unique one keeps
// each flow test's card unambiguous regardless of leftover history.
function uniqueMarker(prefix: string): string {
  return `${prefix}-${randomUUID().slice(0, 8)}.example.test`;
}

// Build a literal-dot regex that matches a marker string anywhere on screen.
function markerRe(marker: string): RegExp {
  return new RegExp(marker.replace(/[.]/g, "\\."));
}

// The sidebar entry is a react-router <NavLink> (role="link"). Once the
// pending-count badge appears its accessible name becomes "Approvals 2", so
// match it tolerantly, then wait for the screen to land by its page heading.
async function gotoApprovals(page: Page): Promise<void> {
  await page.getByRole("link", { name: /^Approvals(\s+\d+)?$/ }).click();
  await expect(page.getByRole("heading", { name: "Approvals" })).toBeVisible();
}

// Seed the standing read-only fixtures the assertion specs rely on: two pending
// (one credential, one egress) plus one each of APPROVED / DENIED / EXPIRED in
// the decided view. Tolerant of pre-existing rows (ON CONFLICT DO NOTHING).
function seedReadOnlyFixtures(): void {
  const runId = anyRunId();
  const rows = [
    `('a0000000-0000-0000-0000-000000000001','${runId}','credential','{"audience":"github.com","scopes":["repo:read"],"ttl":"15m"}'::jsonb,'PENDING',now(),NULL,'','',''),`,
    `('a0000000-0000-0000-0000-000000000002','${runId}','egress_domain','{"domain":"ro-pending.example.com","port":443}'::jsonb,'PENDING',now(),NULL,'','',''),`,
    `('a0000000-0000-0000-0000-000000000003','${runId}','credential','{"audience":"ro-approved.example"}'::jsonb,'APPROVED',now()-interval '1 hour',now()-interval '50 min','admin@wardyn','jti-ro-approved','Verified scope is minimal'),`,
    `('a0000000-0000-0000-0000-000000000004','${runId}','egress_domain','{"domain":"ro-denied.example.org"}'::jsonb,'DENIED',now()-interval '2 hour',now()-interval '110 min','admin@wardyn','','Domain not on allowlist'),`,
    `('a0000000-0000-0000-0000-000000000005','${runId}','tool_call','{"tool":"ro-expired.exec"}'::jsonb,'EXPIRED',now()-interval '3 hour',now()-interval '2 hour','','','')`,
  ].join("\n");
  sql(
    `INSERT INTO approvals (id, run_id, kind, requested_scope, state, requested_at, decided_at, decided_by, minted_jti, reason)
     VALUES ${rows}
     ON CONFLICT (id) DO NOTHING`,
  );
}

test.describe("Approvals screen", () => {
  test.beforeEach(async ({ page }) => {
    seedReadOnlyFixtures();
    await gotoConsole(page);
    await gotoApprovals(page);
  });

  test("renders the approvals page header and HITL copy", async ({ page }) => {
    await expect(page.getByRole("heading", { name: "Approvals" })).toBeVisible();
    // The page description explains an approval only AUTHORIZES a later broker mint.
    await expect(
      page.getByText(/Approving a credential authorizes the broker to mint/i),
    ).toBeVisible();
    // Both tabs exist.
    await expect(page.getByRole("tab", { name: /Pending/ })).toBeVisible();
    await expect(page.getByRole("tab", { name: "Decided" })).toBeVisible();
  });

  test("Pending tab lists cards with kind, blast-radius banner, run context and actions", async ({ page }) => {
    // Default tab is Pending. The credential kind chip renders.
    await expect(page.getByText("credential", { exact: true }).first()).toBeVisible();
    // The PENDING state badge renders capitalized ("Pending").
    await expect(page.getByText("Pending", { exact: true }).first()).toBeVisible();
    // The egress pending card's derived title carries its unique scope domain.
    await expect(page.getByText(markerRe("ro-pending.example.com")).first()).toBeVisible();

    // Every pending card gets a two-line blast-radius banner (D1).
    await expect(page.getByText("What you're approving:").first()).toBeVisible();
    await expect(page.getByText("Blast radius:").first()).toBeVisible();

    // The RunContextRow links each card to the gating run.
    await expect(page.getByText("Open run").first()).toBeVisible();

    // Per-card Approve / Deny actions.
    await expect(page.getByRole("button", { name: "Approve" }).first()).toBeVisible();
    await expect(page.getByRole("button", { name: "Deny" }).first()).toBeVisible();
  });

  test("credential pending card carries the honest broker blast copy (mint is authorized, not immediate)", async ({ page }) => {
    // The credential card's blast banner uses the broker line verbatim — the
    // run works through a short-lived scoped credential; the stored key stays
    // in Wardyn — and never claims a token is minted on approval. ("The run",
    // not "The agent", since the platform-first reframe — see wardyn/copy.ts.)
    await expect(
      page.getByText(/The run works through a short-lived, scoped credential/i),
    ).toBeVisible();
    await expect(
      page.getByText(/The broker mints a short-lived, scoped credential bound to this run's identity/i),
    ).toBeVisible();
  });

  test("Approve dialog says \"Approve\" — NOT \"Approve & mint\" (over-claim fix)", async ({ page }) => {
    await page.getByRole("button", { name: "Approve" }).first().click();

    const dialog = page.getByRole("dialog");
    await expect(dialog).toBeVisible();
    await expect(dialog.getByRole("heading", { name: "Approve request" })).toBeVisible();

    // The confirm button reads exactly "Approve" — it must NOT over-claim with
    // "Approve & mint" / any "mint" verb.
    await expect(dialog.getByRole("button", { name: "Approve", exact: true })).toBeVisible();
    await expect(dialog.getByRole("button", { name: /mint/i })).toHaveCount(0);
    // Reason is optional for approvals.
    await expect(dialog.getByText(/\(optional\)/i)).toBeVisible();

    // Cancel without deciding — no mutation.
    await dialog.getByRole("button", { name: "Cancel" }).click();
    await expect(page.getByRole("dialog")).toHaveCount(0);
  });

  test("Deny dialog requires a reason and is labelled \"Confirm deny\"", async ({ page }) => {
    await page.getByRole("button", { name: "Deny" }).first().click();

    const dialog = page.getByRole("dialog");
    await expect(dialog).toBeVisible();
    await expect(dialog.getByRole("heading", { name: "Deny request" })).toBeVisible();

    // The confirm button reads "Confirm deny" and is disabled until a reason is
    // supplied (deny requires an audit reason; approve does not).
    const confirm = dialog.getByRole("button", { name: "Confirm deny" });
    await expect(confirm).toBeVisible();
    await expect(confirm).toBeDisabled();

    await dialog.getByLabel(/Reason/).fill("Domain not on allowlist");
    await expect(confirm).toBeEnabled();

    await dialog.getByRole("button", { name: "Cancel" }).click();
    await expect(page.getByRole("dialog")).toHaveCount(0);
  });

  test("Decided tab surfaces APPROVED, DENIED and EXPIRED, with DENIED in the danger tone", async ({ page }) => {
    await page.getByRole("tab", { name: "Decided" }).click();

    // State badges render capitalized. EXPIRED being present is the fix: a
    // timed-out request must still appear in the decided view.
    await expect(page.getByText("Approved", { exact: true }).first()).toBeVisible();
    await expect(page.getByText("Denied", { exact: true }).first()).toBeVisible();
    await expect(page.getByText("Expired", { exact: true }).first()).toBeVisible();

    // The decided rows carry their derived titles.
    await expect(page.getByText(markerRe("ro-denied.example.org")).first()).toBeVisible();
    await expect(page.getByText(markerRe("ro-expired.exec")).first()).toBeVisible();

    // Honesty: a DENIED verdict renders in the danger/red tone, never a green ✓.
    await expect(page.getByText("Denied", { exact: true }).first()).toHaveClass(/text-danger/);

    // Decided cards are terminal — no Approve/Deny actions.
    await expect(page.getByRole("button", { name: "Approve" })).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Deny" })).toHaveCount(0);
  });
});

test.describe("Approvals — decision flows (mutating, self-seeded)", () => {
  test("the credential Approve dialog frames approval as authorizing a later broker mint", async ({ page }) => {
    clearPending();
    const id = seedPending({ kind: "credential", scope: { audience: "flow.example.test" } });
    try {
      await gotoConsole(page);
      await gotoApprovals(page);

      // Exactly one (credential) pending card — its Approve opens the dialog with
      // the credential-specific description.
      await page.getByRole("button", { name: "Approve" }).first().click();
      const dialog = page.getByRole("dialog");
      await expect(dialog).toBeVisible();
      await expect(
        dialog.getByText(/authorizes the broker to mint a short-lived scoped token/i),
      ).toBeVisible();
      // Still "Approve" (not "& mint"), reason still optional.
      await expect(dialog.getByRole("button", { name: "Approve", exact: true })).toBeVisible();
      await expect(dialog.getByRole("button", { name: /mint/i })).toHaveCount(0);

      await dialog.getByRole("button", { name: "Cancel" }).click();
      await expect(page.getByRole("dialog")).toHaveCount(0);
    } finally {
      deleteApproval(id);
    }
  });

  test("approve commits and shows a success toast", async ({ page }) => {
    clearPending();
    const marker = uniqueMarker("approve-flow");
    const id = seedPending({ kind: "egress_domain", scope: { domain: marker, port: 443 } });

    try {
      await gotoConsole(page);
      await gotoApprovals(page);

      // The one pending card's unique scope marker is on screen.
      await expect(page.getByText(markerRe(marker)).first()).toBeVisible();

      await page.getByRole("button", { name: "Approve" }).first().click();
      const dialog = page.getByRole("dialog");
      await dialog.getByLabel(/Reason/).fill("Verified scope is minimal and time-boxed");
      await dialog.getByRole("button", { name: "Approve", exact: true }).click();

      // Success toast, dialog closes, and the now-decided request leaves Pending.
      await expect(page.getByText("Request approved")).toBeVisible();
      await expect(page.getByRole("dialog")).toHaveCount(0);
      await expect(page.getByText(markerRe(marker))).toHaveCount(0);

      // It re-surfaces in the Decided view as Approved (exactly once — the decided
      // row shows only the derived title, so the marker appears a single time).
      await page.getByRole("tab", { name: "Decided" }).click();
      await expect(page.getByText(markerRe(marker))).toHaveCount(1);
      await expect(page.getByText("Approved", { exact: true }).first()).toBeVisible();
    } finally {
      deleteApproval(id);
    }
  });

  test("deny commits with a reason and shows a success toast", async ({ page }) => {
    clearPending();
    const marker = uniqueMarker("deny-flow");
    const id = seedPending({ kind: "egress_domain", scope: { domain: marker } });

    try {
      await gotoConsole(page);
      await gotoApprovals(page);
      await expect(page.getByText(markerRe(marker)).first()).toBeVisible();

      await page.getByRole("button", { name: "Deny" }).first().click();
      const dialog = page.getByRole("dialog");
      await dialog.getByLabel(/Reason/).fill("Domain not on the egress allowlist");
      await dialog.getByRole("button", { name: "Confirm deny" }).click();

      await expect(page.getByText("Request denied")).toBeVisible();
      await expect(page.getByRole("dialog")).toHaveCount(0);
      await expect(page.getByText(markerRe(marker))).toHaveCount(0);

      // It re-surfaces in the Decided view as Denied (exactly once).
      await page.getByRole("tab", { name: "Decided" }).click();
      await expect(page.getByText(markerRe(marker))).toHaveCount(1);
      await expect(page.getByText("Denied", { exact: true }).first()).toBeVisible();
    } finally {
      deleteApproval(id);
    }
  });

  test("a decision that can no longer commit surfaces an error toast (no infinite spinner)", async ({ page }) => {
    clearPending();
    const marker = uniqueMarker("vanishing-flow");
    const id = seedPending({ kind: "egress_domain", scope: { domain: marker } });

    try {
      await gotoConsole(page);
      await gotoApprovals(page);
      await expect(page.getByText(markerRe(marker)).first()).toBeVisible();

      // Open the approve dialog, then delete the row out from under the UI so the
      // POST fails. The dialog must report the failure via a toast and reset, not
      // hang on a spinner.
      await page.getByRole("button", { name: "Approve" }).first().click();
      const dialog = page.getByRole("dialog");
      await expect(dialog).toBeVisible();

      deleteApproval(id);

      await dialog.getByRole("button", { name: "Approve", exact: true }).click();

      await expect(page.getByText("Failed to approve request")).toBeVisible();
      // Dialog stays open and the confirm button is interactive again (not a
      // permanently-disabled spinner) so the operator can cancel/retry.
      await expect(dialog).toBeVisible();
      await expect(dialog.getByRole("button", { name: "Approve", exact: true })).toBeEnabled();

      await dialog.getByRole("button", { name: "Cancel" }).click();
      await expect(page.getByRole("dialog")).toHaveCount(0);
    } finally {
      deleteApproval(id);
    }
  });

  test("empty pending queue renders the caught-up empty state", async ({ page }) => {
    clearPending();

    await gotoConsole(page);
    await gotoApprovals(page);

    await expect(page.getByText("You're all caught up")).toBeVisible();
    await expect(
      page.getByText(/New credential, egress, and tool-call requests appear here the moment an agent needs you/i),
    ).toBeVisible();

    // Restore standing fixtures so subsequent runs of the read-only specs pass.
    seedReadOnlyFixtures();
  });
});
