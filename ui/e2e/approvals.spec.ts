/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { randomUUID } from "node:crypto";
import type { Page } from "@playwright/test";
import { test, expect, gotoConsole, mockMemberRole, sql } from "./fixtures";

// ---------------------------------------------------------------------------
// Approvals screen e2e (lane: approvals, port 8088, db wardyn_e2e).
//
// The shared seed (scripts/e2e-backend.sh) does NOT create approvals, so this
// lane seeds its own approval fixtures directly via SQL (the backend exposes no
// admin-token endpoint to *create* an approval — `POST /internal/approvals`
// requires a run-scoped token), through the shared `sql` helper in fixtures.ts.
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
//   * decision-scope (once/this run/until/always — copy.ts's APPROVAL_SCOPE_*):
//     the ReasonDialog segmented control on this page, the decided-row scope
//     badge (approvalScopeBadge), and — via the run-cockpit describe block
//     below — the LiveApprovals split button's caret menu. The confirm button
//     stays exactly "Approve" no matter which scope is picked.
// ---------------------------------------------------------------------------

// Every test in this file mutates / reads the SAME shared backend (one wardynd +
// one Postgres). Force the whole file to run sequentially in a single worker so
// backend state is deterministic (a mutating test can't yank the pending queue
// out from under a read-only assertion).
test.describe.configure({ mode: "serial" });

// A run id to bind seeded approvals to (FK approvals.run_id -> agent_runs.id).
function anyRunId(): string {
  const id = sql("SELECT id FROM agent_runs ORDER BY created_at LIMIT 1");
  if (!id) throw new Error("no seeded runs found — is the backend up and seeded?");
  return id;
}

// Same run as anyRunId(), forced into RUNNING. LiveApprovals — the split-button
// decision surface — only mounts when run.state === "RUNNING"
// (run-detail.tsx:442, inside Cockpit's terminalPane), so the split-button spec
// needs a run pinned there. The shared seed's non-terminal states aren't stable
// enough to rely on as-is: runs.spec.ts notes the `none` runner's reconciler
// can legitimately advance a run away from its seeded state before a spec's
// tests reach it. Force it directly instead — the same one-column SQL UPDATE
// technique e2e-backend.sh's own seeder uses to produce its RUNNING fixture.
function runningRunId(): string {
  const id = anyRunId();
  sql(`UPDATE agent_runs SET state = 'RUNNING' WHERE id = '${id}'`);
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
    `('a0000000-0000-0000-0000-000000000001','${runId}','credential','{"audience":"github.com","scopes":["repo:read"],"ttl":"15m"}'::jsonb,'PENDING',now(),NULL,'','','',''),`,
    `('a0000000-0000-0000-0000-000000000002','${runId}','egress_domain','{"domain":"ro-pending.example.com","port":443}'::jsonb,'PENDING',now(),NULL,'','','',''),`,
    `('a0000000-0000-0000-0000-000000000003','${runId}','credential','{"audience":"ro-approved.example"}'::jsonb,'APPROVED',now()-interval '1 hour',now()-interval '50 min','admin@wardyn','jti-ro-approved','Verified scope is minimal','run'),`,
    `('a0000000-0000-0000-0000-000000000004','${runId}','egress_domain','{"domain":"ro-denied.example.org"}'::jsonb,'DENIED',now()-interval '2 hour',now()-interval '110 min','admin@wardyn','','Domain not on allowlist','run'),`,
    `('a0000000-0000-0000-0000-000000000005','${runId}','tool_call','{"tool":"ro-expired.exec"}'::jsonb,'EXPIRED',now()-interval '3 hour',now()-interval '2 hour','','','','')`,
  ].join("\n");
  sql(
    // decision_scope (migration 0039) MUST be listed explicitly: an omitted
    // column defaults to '' for every row (NOT NULL DEFAULT ''), which makes
    // copy.ts's approvalScopeBadge() return undefined for every decided row —
    // any scope-badge assertion would then pass vacuously against a blank.
    // Row 4 (ro-denied.example.org — the only DECIDED egress_domain fixture
    // here) carries a real 'run' scope so the badge has something to render.
    // Row 3 (credential) also carries a realistic 'run' even though its kind
    // means the badge never shows it (approvalScopeBadge is egress_domain-only
    // by design). Row 5 (EXPIRED) stays '' on purpose, matching ExpireStale's
    // own deliberate zero-scope write — an expiry is a sweep nobody decided.
    `INSERT INTO approvals (id, run_id, kind, requested_scope, state, requested_at, decided_at, decided_by, minted_jti, reason, decision_scope)
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
    // Default tab is Pending. The credential kind chip renders its human copy
    // ("Credential"), not the raw wire token (ui-approvals-2, copy.ts's
    // APPROVAL_KIND_LABEL via WIRE_TO_COPY).
    await expect(page.getByText("Credential", { exact: true }).first()).toBeVisible();
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

    // Decided-row scope badge (Phase 0 §6, copy.ts's approvalScopeBadge) — only
    // rendered for a DECIDED egress_domain row that carries a decision_scope.
    // ro-denied.example.org is seeded DENIED + decision_scope 'run' above; the
    // badge renders "this run" (APPROVAL_SCOPE_LABEL.run, lowercased) beside
    // the state chip. This is the assertion the column-list omission bug made
    // impossible: before decision_scope was added to the seed, every decided
    // row's scope was '', so approvalScopeBadge always returned undefined and
    // no badge ever rendered — this would have passed vacuously either way.
    await expect(page.getByText("this run", { exact: true })).toBeVisible();

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

  test("picking a non-default decision scope keeps the confirm button honestly labelled \"Approve\", and the decided row picks up that scope's badge", async ({ page }) => {
    clearPending();
    const marker = uniqueMarker("scope-flow");
    const id = seedPending({ kind: "egress_domain", scope: { domain: marker, port: 443 } });

    try {
      await gotoConsole(page);
      await gotoApprovals(page);
      await expect(page.getByText(markerRe(marker)).first()).toBeVisible();

      await page.getByRole("button", { name: "Approve" }).first().click();
      const dialog = page.getByRole("dialog");
      await expect(dialog).toBeVisible();

      // The decision-scope segmented control (reason-dialog.tsx) — rendered
      // for egress_domain only, all four options present together.
      const scopeGroup = dialog.getByRole("radiogroup", { name: "Decision scope" });
      await expect(scopeGroup).toBeVisible();
      await expect(scopeGroup.getByRole("radio", { name: /^Once\b/ })).toBeVisible();
      await expect(scopeGroup.getByRole("radio", { name: /^This run\b/ })).toBeVisible();
      await expect(scopeGroup.getByRole("radio", { name: /^Until…/ })).toBeVisible();
      await expect(scopeGroup.getByRole("radio", { name: /^Always\b/ })).toBeVisible();

      // Pick "Until…" + a preset (not the default "This run") — this is also
      // the one scope whose badge depends on decision_expires_at reaching the
      // row, so picking it proves that field round-trips too, not just
      // decision_scope.
      const untilOption = scopeGroup.getByRole("radio", { name: /^Until…/ });
      await untilOption.click();
      await expect(untilOption).toHaveAttribute("aria-checked", "true");
      await dialog.getByRole("button", { name: "15 minutes" }).click();

      // Honesty guard (pre-existing, unrelated to this feature, and NOT to be
      // weakened by it): no matter which scope is selected, the confirm button
      // stays exactly "Approve" — reason-dialog.tsx never folds the scope into
      // the label.
      await expect(dialog.getByRole("button", { name: "Approve", exact: true })).toBeVisible();
      await expect(dialog.getByRole("button", { name: /mint/i })).toHaveCount(0);

      await dialog.getByRole("button", { name: "Approve", exact: true }).click();

      await expect(page.getByText("Request approved")).toBeVisible();
      await expect(page.getByRole("dialog")).toHaveCount(0);

      // It re-surfaces in Decided as Approved, carrying an "until <time>"
      // badge — the live round-trip proof that decision_scope AND
      // decision_expires_at reach the row (copy.ts's approvalScopeBadge), not
      // just that the SQL-seeded fixture in the test above can render one.
      // Not asserting the exact clock text: shortTime() formats via
      // toLocaleTimeString with no fixed locale/timezone, which is exactly the
      // kind of thing that is stable on one machine and flaky on another.
      await page.getByRole("tab", { name: "Decided" }).click();
      await expect(page.getByText(markerRe(marker))).toHaveCount(1);
      await expect(page.getByText("Approved", { exact: true }).first()).toBeVisible();
      await expect(page.getByText(/^until\s/i).first()).toBeVisible();
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

// ---------------------------------------------------------------------------
// The OTHER decision surface: LiveApprovals' split button, mounted on the run
// cockpit (/runs/:id), not the standalone /approvals page above. Unreachable
// from that page (it uses PendingCard + ReasonDialog's segmented control, not
// LiveApprovals) — this is the surface the queue tests above cannot exercise,
// so it gets its own describe block and its own navigation.
// ---------------------------------------------------------------------------
test.describe("Approvals — decision-scope split button (run cockpit)", () => {
  test("the LiveApprovals split button offers all four scopes via its caret, with Always disabled for a workspace-less run", async ({ page }) => {
    clearPending();
    const runId = runningRunId();
    // live-approvals.tsx reads requested_scope.host (NOT .domain, unlike the
    // standalone page's deriveTitle, which checks both) — the key must be
    // "host" or the row renders "unknown host" instead of this marker.
    const marker = uniqueMarker("split-button");
    const id = seedPending({ kind: "egress_domain", scope: { host: marker, port: 443 } });

    try {
      await page.goto(`/runs/${runId}`);
      // Sanity: runningRunId()'s forced SQL state actually took, and the
      // cockpit rendered (LiveApprovals only mounts when run.state ===
      // "RUNNING" — run-detail.tsx:442).
      await expect(page.getByText("Running", { exact: true }).first()).toBeVisible();

      const row = page.getByTestId("live-approval-row");
      await expect(row).toBeVisible();
      await expect(row.getByText(markerRe(marker))).toBeVisible();

      // The split button's bare halves stay named "Approve"/"Deny" — same
      // unanchored match the card buttons above use (not exact:true: that's
      // reserved in this file for disambiguating a dialog's CONFIRM button
      // from an "Approve & mint" over-claim, not for these plain row buttons).
      const approveBtn = row.getByRole("button", { name: "Approve" });
      const denyBtn = row.getByRole("button", { name: "Deny" });
      await expect(approveBtn).toBeVisible();
      await expect(approveBtn).toBeEnabled();
      await expect(denyBtn).toBeVisible();
      await expect(denyBtn).toBeEnabled();

      // Two carets on the row (approve's and deny's ScopeMenu instances) share
      // the same aria-label by design — the component comment is explicit
      // that neither may say "approve"/"deny" in its accessible name, or an
      // unanchored /approve/i query upstream would match two buttons. DOM
      // order is deterministic (approve button, approve caret, deny button,
      // deny caret), so .first()/.last() disambiguate them here.
      const approveCaret = row.getByRole("button", { name: "More options" }).first();
      const denyCaret = row.getByRole("button", { name: "More options" }).last();

      await approveCaret.click();
      const approveMenu = page.getByRole("menu");
      await expect(approveMenu).toBeVisible();
      // All four approve-flavor options render together (ScopeMenu's default,
      // pre-"Until…"-picked view).
      await expect(approveMenu.getByRole("button", { name: /^Once\b/ })).toBeVisible();
      await expect(approveMenu.getByRole("button", { name: /^This run\b/ })).toBeVisible();
      await expect(approveMenu.getByRole("button", { name: /^Until…/ })).toBeVisible();
      const alwaysBtn = approveMenu.getByRole("button", { name: /^Always\b/ });
      await expect(alwaysBtn).toBeVisible();
      // hasWorkspace is derived from run.workspace_ids, which this seeded run
      // has none of — Always must be a REAL disabled attribute (not
      // aria-disabled; the component comment is explicit Playwright would
      // happily "click" that), with the no-workspace reason shown in its place.
      await expect(alwaysBtn).toBeDisabled();
      await expect(
        approveMenu.getByText("Always needs a workspace — this run isn't attached to one."),
      ).toBeVisible();
      // Close without picking — this approval stays undecided for cleanup.
      await page.keyboard.press("Escape");
      await expect(page.getByRole("menu")).toHaveCount(0);

      // The deny-flavor menu carries its OWN label set (DENY_SCOPE_LABEL) —
      // spot-check one to prove the verb swap actually renders different copy
      // rather than reusing the approve labels.
      await denyCaret.click();
      const denyMenu = page.getByRole("menu");
      await expect(denyMenu).toBeVisible();
      await expect(denyMenu.getByRole("button", { name: /^Deny always\b/ })).toBeVisible();
      await page.keyboard.press("Escape");
      await expect(page.getByRole("menu")).toHaveCount(0);
    } finally {
      deleteApproval(id);
    }
  });
});

// ---------------------------------------------------------------------------
// F-12 pinning — LiveApprovals' row-level gate follows canDecideApproval
// (server truth: authorizeMemberDecision, internal/api/approvals.go), not a
// blanket !operator disable. A member may decide an egress_domain approval on
// a run they own; credential and tool_call stay admin-only regardless.
//
// mockMemberRole (fixtures.ts) only flips what the CLIENT believes about its
// own role — this harness always authenticates every request with the seeded
// admin bearer token server-side (isOperator has no per-human session to
// demote), so the decide() call below rides that real admin token and
// genuinely succeeds. That is fine and explicitly documented as fine: the
// RENDER decision (is the button enabled) is what F-12 pins, not server-side
// ownership scoping — that is proven in Go (see this file's own report / the
// TestDecide_MemberKindRestriction and TestAuthzMatrix coverage in
// internal/api/authz_test.go).
//
// A true "foreign run" negative is NOT meaningfully testable at this
// component: LiveApprovals only ever polls approvals already filtered to
// `a.run_id === runId` (live-approvals.tsx's refresh()), so a row from a
// different run can never even reach this strip to be rendered disabled or
// enabled — there is no client-side ownership check for a render test to
// exercise (canDecideApproval mirrors decide() on KIND alone; ownership is a
// precondition of the row existing at all, per canDecideApproval's own doc).
// The non-egress-kind negative below is the real, honestly-automatable
// negative case.
test.describe("F-12 — LiveApprovals row gate mirrors canDecideApproval, not a blanket operator check", () => {
  test("a member's Approve/Deny are ENABLED on their own run's egress_domain approval, and a real decide round-trips", async ({
    page,
  }) => {
    clearPending();
    const runId = runningRunId();
    const marker = uniqueMarker("f12-egress");
    const id = seedPending({ kind: "egress_domain", scope: { host: marker, port: 443 } });

    try {
      await mockMemberRole(page);
      await page.goto(`/runs/${runId}`);
      await expect(page.getByText("Running", { exact: true }).first()).toBeVisible();

      const row = page.getByTestId("live-approval-row").filter({ hasText: markerRe(marker) });
      await expect(row).toBeVisible();
      const approveBtn = row.getByRole("button", { name: "Approve" });
      const denyBtn = row.getByRole("button", { name: "Deny" });
      // F-12: was blanket-disabled for any non-operator; now enabled for the
      // one kind a member may decide.
      await expect(approveBtn).toBeEnabled();
      await expect(denyBtn).toBeEnabled();
      // The strip must not claim "admin only" over a row the viewer can, in
      // fact, act on (live-approvals.tsx's OperatorOnlyHint gate). Scoped to
      // the strip itself — the page can carry an unrelated "blocked until an
      // admin decides it" banner elsewhere that also mentions "admin".
      const panel = page.getByTestId("live-approvals");
      await expect(panel.getByText("Requires the admin role.", { exact: true })).toHaveCount(0);

      await approveBtn.click();
      // A real decide() round trip (rides the harness's real admin bearer
      // token) — the row leaves PENDING and the strip goes idle.
      await expect(row).toHaveCount(0, { timeout: 10_000 });
      await expect(page.getByTestId("live-approvals-idle")).toBeVisible();
    } finally {
      deleteApproval(id);
    }
  });

  test("negative: a non-egress kind (tool_call) stays disabled for a member on the SAME owned run", async ({ page }) => {
    clearPending();
    const runId = runningRunId();
    const id = seedPending({ kind: "tool_call", scope: { tool: "bash", cmd: "rm -rf /" } });

    try {
      await mockMemberRole(page);
      await page.goto(`/runs/${runId}`);
      await expect(page.getByText("Running", { exact: true }).first()).toBeVisible();

      const row = page.getByTestId("live-approval-row");
      await expect(row).toBeVisible();
      await expect(row.getByRole("button", { name: "Approve" })).toBeDisabled();
      await expect(row.getByRole("button", { name: "Deny" })).toBeDisabled();
      // credential/tool_call stay admin-only regardless of ownership — the
      // hint IS shown here, unlike the all-egress case above. Scoped to the
      // strip itself (see the positive test's comment for why).
      const panel = page.getByTestId("live-approvals");
      await expect(panel.getByText("Requires the admin role.", { exact: true })).toBeVisible();
    } finally {
      deleteApproval(id);
    }
  });
});
