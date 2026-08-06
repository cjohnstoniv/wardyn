/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The "Add workspace" wizard — the single front door onto the Workspace
// Composition model (migration 0029), replacing the old 6-step import dialog.
//
// The rail is SEVEN steps (wizard-types.ts's WIZARD_STEPS):
//   Sources · Base image · Integrations · Build · Requirements · Verify · Done
// Integrations and Build sit between the image and the contract, and Verify
// between the contract and Done — so "one Continue past the image" lands on
// Integrations, not Requirements.
//
// Source of truth read before writing selectors:
//   src/app/components/screens/workspace-wizard/{wizard,step-sources,
//     step-base-image,step-integrations,integration-requirements,step-build,
//     step-requirements,verify-session,step-done,wizard-types}.tsx
//   src/app/components/screens/workspaces.tsx (the "Add workspace" entry
//     points + the empty-state CTA + the list's status chip)
//   src/app/components/screens/workspace-detail/{workspace-detail,
//     requirements-card}.tsx (the Done step's "Open <name> ->" landing page)
//   src/app/lib/workspace-copy.ts (V2C/C copy constants), wizard.test.tsx (the
//     component's own Vitest suite — ported its baseWorkspace() fixture shape
//     and driveToBaseImage() convention below)
//
// The seeded backend runs `-runner none`, which resolves to a literal nil
// Runner (cmd/wardynd/boot_deps.go's buildRunnerFromFlags) — NOT a stub
// driver. A REPO source's scan therefore 503s immediately server-side
// (handleScanWorkspace's `if s.cfg.Store == nil` … `Runner == nil` guard),
// not a slow/hanging one — so the "mid-scan" scenario below stubs the scan
// call itself to get a sustained (not instantly-failed) scanning state,
// deterministically, rather than racing real backend timing.
//
// A LOCAL_DIR source's scan is different: handleScanWorkspace's local_dir
// branch never touches the runner (host-side, inline `os.Stat` + scan), so it
// works for real against any real, existing directory on the SAME machine
// wardynd runs on. scripts/run-ui-e2e.sh always launches Playwright with cwd
// `ui/` (`cd ui && pnpm exec playwright test …`), so `process.cwd()` here is a
// real Node/pnpm project directory wardynd can actually scan — the same
// Node-detection -> `registry.npmjs.org` auto-allowed-egress shape
// wizard.test.tsx's own baseWorkspace() fixture uses.
import * as path from "node:path";
import { test, expect, gotoConsole } from "./fixtures";
import type { Page, Locator } from "@playwright/test";

function dialog(page: Page): Locator {
  return page.getByRole("dialog");
}

// Open /workspaces and click the header "Add workspace" button — the entry
// point used once at least one workspace already exists (every scenario here
// except the empty-list one runs against the shared seeded backend, which
// always carries the "payments" fixture). Lands on step ① Sources.
async function openAddWorkspaceWizard(page: Page): Promise<Locator> {
  await gotoConsole(page);
  await page.goto("/workspaces");
  await page.getByRole("button", { name: "Add workspace" }).click();
  const dlg = dialog(page);
  await expect(dlg.getByRole("heading", { name: "Add workspace" })).toBeVisible();
  return dlg;
}

// A unique-per-test workspace name — the shared seeded backend never resets
// between the tests in this file (or this file and its siblings), so distinct
// names keep each test's row unambiguous in a list that only ever grows.
function uniqueName(tag: string): string {
  return `wiz-${tag}-${Date.now()}-${Math.floor(Math.random() * 1000)}`;
}

test.describe("Add workspace wizard", () => {
  test("empty list shows the onboarding CTA, which opens the wizard with the ephemeral floor row present", async ({
    page,
  }) => {
    // Stub the list endpoint only (not a workspace sub-resource — Playwright's
    // `*` excludes `/`, so `/workspaces/{id}` calls are untouched) so this one
    // test sees the true empty state regardless of what earlier specs seeded.
    await page.route("**/api/v1/workspaces*", (route) => route.fulfill({ json: [] }));

    await gotoConsole(page);
    await page.goto("/workspaces");
    await expect(page.getByText("No workspaces onboarded yet.")).toBeVisible();

    await page.getByRole("button", { name: "Onboard your first workspace" }).click();
    const dlg = dialog(page);
    await expect(dlg.getByRole("heading", { name: "Add workspace" })).toBeVisible();

    // The composition floor: exactly one ephemeral source, its floor-specific
    // copy (never the generic ephemeral description), and its remove button
    // disabled (it's the only source — every workspace has at least one).
    const row = dlg.locator('[data-testid="source-row"]');
    await expect(row).toHaveCount(1);
    await expect(row.getByText("Ephemeral directory")).toBeVisible();
    await expect(
      row.getByText("Every workspace has at least one source; this scratch directory is the floor."),
    ).toBeVisible();
    await expect(row.getByRole("button", { name: "Remove ephemeral directory" })).toBeDisabled();

    // No workspace was created — Cancel, not Close.
    await expect(dlg.getByRole("button", { name: "Cancel" })).toBeVisible();
    await dlg.getByRole("button", { name: "Cancel" }).click();
    await expect(dialog(page)).not.toBeVisible();
  });

  test("adding a local directory source auto-derives a distinct mount target", async ({ page }) => {
    const dlg = await openAddWorkspaceWizard(page);
    await dlg.getByLabel("Name").fill(uniqueName("target-derive"));

    await dlg.getByRole("button", { name: "Add Local directory" }).click();
    const row = dlg.locator('[data-testid="source-row"]').filter({ hasText: "Local directory" });
    await row.getByPlaceholder("/home/me/projects/payments").fill("/home/me/projects/reports");

    // Two sources now exist (the ephemeral floor + this one), so the target
    // placeholder is NOT the bare default — it's namespaced by the source's own
    // base name (wizard-types.ts's defaultTargetFor/baseNameOf).
    const target = row.getByLabel("Mount target");
    await expect(target).toHaveAttribute("placeholder", "/home/agent/work/reports");

    // Typing an explicit target always wins over the derived suggestion — and
    // defaultTargetFor returns the TYPED value once there is one, so the
    // placeholder becomes the typed target too. Hold the field by its label:
    // re-finding it by the original placeholder finds nothing after the fill.
    await target.fill("/home/agent/custom-mount");
    await expect(target).toHaveValue("/home/agent/custom-mount");

    // Nothing was created — leave cleanly.
    await dlg.getByRole("button", { name: "Cancel" }).click();
  });

  test("an SSH repo source with no stored key hard-blocks that row only", async ({ page }) => {
    const dlg = await openAddWorkspaceWizard(page);
    await dlg.getByLabel("Name").fill(uniqueName("ssh-gate"));

    await dlg.getByRole("button", { name: "Add Repository" }).click();
    const row = dlg.locator('[data-testid="source-row"]').filter({ hasText: "Repository" });
    await row.getByPlaceholder("acme/payments-service").fill("git@github.com:acme/private-repo.git");

    // The row itself hard-blocks: no ssh-key-github-com secret is seeded.
    const sshGate = row.locator('[data-testid="ssh-gate"]');
    await expect(sshGate).toBeVisible();
    await expect(sshGate.getByText("SSH key needed first")).toBeVisible();
    await expect(sshGate.getByRole("button", { name: "Add SSH key" })).toBeVisible();

    // "Hard-blocks THAT ROW only": the wizard's own Continue is gated on the
    // Name field alone (wizard.tsx's footer), never on a per-row credential
    // gap — the operator can still create the workspace and fix the row later
    // from its own page.
    await expect(dlg.getByRole("button", { name: "Continue →" })).toBeEnabled();

    await dlg.getByRole("button", { name: "Cancel" }).click();
  });

  test("step 2 shows four base-image cards; Customize accepts a Dockerfile step and a credential-shaped line warns without blocking Continue", async ({
    page,
  }) => {
    const dlg = await openAddWorkspaceWizard(page);
    // Ephemeral-only (the floor, untouched): handleScanWorkspace's
    // ephemeral-only branch needs no runner and resolves instantly, so this
    // reaches step 2 with no scan latency at all — the fastest path there.
    await dlg.getByLabel("Name").fill(uniqueName("cred-warn"));
    await dlg.getByRole("button", { name: "Continue →" }).click();

    await expect(dlg.getByRole("radiogroup", { name: "Base image" })).toBeVisible();
    for (const id of ["recommended", "registry", "custom", "byo"]) {
      await expect(dlg.locator(`[data-testid="image-card-${id}"]`)).toBeVisible();
    }

    await dlg.locator('[data-testid="image-card-custom"]').click();
    const steps = dlg.getByLabel("Custom build steps");
    await expect(steps).toBeVisible();
    await steps.fill("RUN echo hi\nENV AWS_ACCESS_KEY=AKIAABCDEFGHIJKL1234");

    const warning = dlg.locator('[data-testid="cred-warning"]');
    await expect(warning).toBeVisible();
    await expect(warning.getByText(/looks like a credential/i)).toBeVisible();
    await expect(warning.getByText(/AWS access key/i)).toBeVisible();

    // WARNS, never blocks: Continue stays enabled with the flagged line still
    // in the editor.
    await expect(dlg.getByRole("button", { name: "Continue →" })).toBeEnabled();

    await dlg.getByRole("button", { name: "Close" }).click();
  });

  test("'Continue without waiting' escapes a slow-scanning source mid-flight", async ({ page }) => {
    // The seeded backend's nil runner would 503 a real repo scan immediately
    // (an instant FAILURE, not a sustained one) — stub the scan call itself so
    // the wizard sits in a genuine "still scanning" state long enough to
    // interact with, independent of that.
    // Never released: the test only needs the sustained "scanning" window, and
    // an unresolved route is simply dropped when Playwright tears the page
    // down at test end — letting it resolve would tip startScan into its
    // real 40x1.5s poll-for-completion loop against a workspace that (this
    // stub aside) never actually left pending_scan server-side.
    const scanGate = new Promise<void>(() => {});
    await page.route("**/api/v1/workspaces/*/scan", async (route) => {
      await scanGate;
      await route.fulfill({ status: 202, json: { scan_run_id: "e2e-fake-scan", state: "PENDING" } });
    });

    const dlg = await openAddWorkspaceWizard(page);
    await dlg.getByLabel("Name").fill(uniqueName("partial-scan"));
    await dlg.getByRole("button", { name: "Add Repository" }).click();
    const row = dlg.locator('[data-testid="source-row"]').filter({ hasText: "Repository" });
    await row.getByPlaceholder("acme/payments-service").fill("acme/e2e-scan-target");

    await dlg.getByRole("button", { name: "Continue →" }).click();

    // Still gated on the stubbed response: a real (non-ephemeral) source is
    // queued/scanning, never failed, so the footer offers the "without
    // waiting" escape rather than "Continue anyway".
    const wait = dlg.getByRole("button", { name: "Continue without waiting" });
    await expect(wait).toBeVisible();
    await expect(
      dlg.getByText("Suggestions improve when the scan lands — you can change the image on the workspace's page any time."),
    ).toBeVisible();

    await wait.click();

    // Landed on step 2, Phase B, with every card honestly marked as guessed
    // from an incomplete scan.
    await expect(dlg.getByRole("radiogroup", { name: "Base image" })).toBeVisible();
    await expect(dlg.getByText("based on a partial scan").first()).toBeVisible();

    await dlg.getByRole("button", { name: "Close" }).click();
  });

  test("flipping a seeded egress row Required to Optional and finishing lands on the workspace's page with the accepted contract", async ({
    page,
  }) => {
    const name = uniqueName("egress-flip");

    // Step 3 (Integrations) needs a real, nameable row to prove
    // continueFromIntegrations (wizard.tsx) actually PERSISTS a pick — the
    // seeded backend stores no integration of its own. Inject one generic row
    // into every /setup/status response for THIS page only (the real payload,
    // augmented — never a real PUT /integrations/{id}): the shared backend
    // never resets between specs (see the file header), so a real write here
    // would accumulate forever across CI runs. Unique per run, so it can never
    // collide with a row an earlier run left behind.
    const feedName = uniqueName("feed");
    await page.route("**/api/v1/setup/status", async (route) => {
      const res = await route.fetch();
      const body = await res.json();
      body.integrations = [
        ...(body.integrations ?? []),
        { id: feedName, name: feedName, category: "package_feed", type: "package_feed", hosts: ["registry.example.com"] },
      ];
      await route.fulfill({ response: res, json: body });
    });

    const dlg = await openAddWorkspaceWizard(page);
    await dlg.getByLabel("Name").fill(name);

    // A real, existing directory wardynd can actually scan (see the file
    // header) — a Node/pnpm project, so the profile lands with
    // registry.npmjs.org auto-allowed, exactly wizard.test.tsx's own fixture
    // shape.
    await dlg.getByRole("button", { name: "Add Local directory" }).click();
    const sourceRow = dlg.locator('[data-testid="source-row"]').filter({ hasText: "Local directory" });
    await sourceRow.getByPlaceholder("/home/me/projects/payments").fill(path.resolve(process.cwd()));

    await dlg.getByRole("button", { name: "Continue →" }).click();
    // Local-dir scans are synchronous server-side; wait for Phase B rather
    // than any specific timing.
    await expect(dlg.getByRole("radiogroup", { name: "Base image" })).toBeVisible({ timeout: 30_000 });

    // Step 3 — Integrations (INTEGRATIONS_BLURB). Pick the injected feed row
    // for real: "Add to this workspace" defaults a generic row to Required
    // (IntegrationRow, integration-requirements.tsx), so the Continue below
    // has an actual pick to persist, not just a blurb to read past.
    await dlg.getByRole("button", { name: "Continue →" }).click();
    await expect(dlg.getByText(/Pick what this workspace connects through/)).toBeVisible();
    const integrationsSection = dlg.locator('[data-testid="integration-requirements"]');
    await expect(integrationsSection.getByText(feedName)).toBeVisible();
    await integrationsSection.getByRole("button", { name: "Add to this workspace" }).click();
    const feedLane = dlg.getByRole("radiogroup", { name: `${feedName} lane` });
    await expect(feedLane.getByRole("radio", { name: "Required" })).toBeChecked();

    // Step 4 — Build (BUILD_BLURB). It auto-kicks a build on mount.
    // scripts/e2e-backend.sh builds wardynd without -tags docker and passes no
    // -envbuild flag, so ImageBuilder is nil server-side and resolveBuildView
    // (workspace_build.go) deterministically settles on state "none" — the
    // honest reachable outcome here, never "building"/"done" (those need the
    // real docker driver this binary doesn't have).
    await dlg.getByRole("button", { name: "Continue →" }).click();
    await expect(dlg.getByText(/This builds it now — visibly/)).toBeVisible();
    await expect(
      dlg.getByText("devcontainer builds are not enabled on this host — sessions boot the stock agent image"),
    ).toBeVisible();
    await dlg.getByRole("button", { name: /^Continue/ }).click();

    // Step 5 — Requirements (C.S3_BLURB).
    await expect(
      dlg.getByText("Set what this workspace always carries, and what a run has to ask for."),
    ).toBeVisible();

    // The seeded egress row: required by default (deriveInitialRequirements),
    // flip it to Optional.
    const egressGroup = dlg.getByRole("radiogroup", { name: "registry.npmjs.org lane" });
    await expect(egressGroup).toBeVisible();
    await expect(egressGroup.getByRole("radio", { name: "Required" })).toBeChecked();
    await egressGroup.getByRole("radio", { name: "Optional" }).click();
    await expect(egressGroup.getByRole("radio", { name: "Optional" })).toBeChecked();

    // Requirements PUTs the map on its own primary — "Save & continue →",
    // not the retired "Accept & finish" — and lands on Verify, which is its
    // own step before Done.
    await dlg.getByRole("button", { name: /^Save & continue/ }).click();
    await expect(dlg.getByText(/Drive the workspace for real/)).toBeVisible();

    // Step 6 — Verify. -runner none hard-503s a record/verify launch before
    // any session exists (handleRecordWorkspace's Runner==nil gate,
    // internal/api/record.go:265) — no live session is reachable under this
    // e2e binary. "Verify with a session" is hidden too (nothingResolves: no
    // AI integration was named), leaving only "Verify in a terminal" — click
    // it and assert the honest failure the launch actually reports, rather
    // than asserting nothing about what the button does.
    const verifyLaunch = dlg.locator('[data-testid="verify-session-launch"]');
    await verifyLaunch.getByRole("button", { name: "Verify in a terminal" }).click();
    await expect(verifyLaunch.getByText(/record needs a configured runner/)).toBeVisible();

    await dlg.getByRole("button", { name: "Finish" }).click();

    await expect(dlg.getByText(`${name} is usable.`)).toBeVisible();

    await dlg.getByRole("button", { name: new RegExp(`^Open ${name}`) }).click();

    // Landed on the workspace's own page, showing the SAME accepted contract.
    await expect(page).toHaveURL(/\/workspaces\/[^/]+$/);
    await expect(page.getByRole("heading", { name })).toBeVisible();
    const detailEgressGroup = page.getByRole("radiogroup", { name: "registry.npmjs.org lane" });
    await expect(detailEgressGroup).toBeVisible();
    await expect(detailEgressGroup.getByRole("radio", { name: "Optional" })).toBeChecked();

    // ...and the Integrations-step pick survived the round trip too — a fresh
    // GET on a freshly mounted page, not the wizard's own optimistic state.
    const detailFeedLane = page.getByRole("radiogroup", { name: `${feedName} lane` });
    await expect(detailFeedLane).toBeVisible();
    await expect(detailFeedLane.getByRole("radio", { name: "Required" })).toBeChecked();
  });

  test("closing mid-flow keeps the workspace — it survives in the list, not silently dropped", async ({ page }) => {
    const name = uniqueName("close-midflow");
    const dlg = await openAddWorkspaceWizard(page);
    await dlg.getByLabel("Name").fill(name);
    await dlg.getByRole("button", { name: "Continue →" }).click();

    // Once the workspace exists, the footer swaps Cancel for Close and states
    // the honest consequence up front.
    await expect(dlg.getByRole("radiogroup", { name: "Base image" })).toBeVisible();
    await expect(
      dlg.getByText("Closing keeps this workspace — you can pick up from its page any time."),
    ).toBeVisible();
    await dlg.getByRole("button", { name: "Close" }).click();
    await expect(dialog(page)).not.toBeVisible();

    // It survives, incomplete (no Requirements step reached) — the point of
    // the test. Its status word is "Usable", NOT "Setting up": this workspace
    // is ephemeral-only, and handleScanWorkspace's own contract is that "an
    // ephemeral-only composition reads scanned with an empty profile straight
    // from the hydrate pass" — scanned maps to Usable (lib/workspace-status.ts
    // :: statusWord). There is nothing left to scan, so claiming otherwise
    // would be the dishonest reading.
    await page.goto("/workspaces");
    const row = page.getByRole("row", { name: new RegExp(name) });
    await expect(row).toBeVisible();
    await expect(row.getByText("Usable")).toBeVisible();
  });
});
