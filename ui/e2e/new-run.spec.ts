/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New run e2e — the ONE-PAGE screen at /runs/new that replaced the 5-step
// PermissionWizard dialog (Basics → Access → Egress → Confinement → Review).
//
// What is worth proving against a live backend, rather than in jsdom: that the
// form's fields follow the run mode, and that the shared Policy panel — the
// same component /policies authors through — really governs what this run
// ships. The Confinement + Network cards it replaced put the envelope behind a
// preset stack and a dialog; the spec JSON is the envelope now.
//
// Notes on the seeded backend (scripts/e2e-backend.sh): wardynd runs with
// -runner none, so /healthz advertises NO confinement_classes — unknown, not
// confirmed-absent, so all three barrier tiers stay selectable and the runner
// capability gate (runs_create.go) is skipped entirely. There is no ai_provider
// integration at all, so an agent run honestly reports that no model provider
// is connected.
import { test, expect, gotoConsole, ADMIN_TOKEN, launchRun } from "./fixtures";
import { RUN } from "../src/app/components/wardyn/copy";
import { CC_META } from "../src/app/components/wardyn/cc-meta";
import { PROVIDERS } from "../src/app/lib/workspace-providers-copy";
import type { Page } from "@playwright/test";
import type { ConfinementClass } from "../src/app/lib/types";

async function openNewRun(page: Page) {
  await gotoConsole(page);
  await page.getByRole("button", { name: "New run" }).click();
  await expect(page).toHaveURL(/\/runs\/new$/);
  await expect(page.getByRole("heading", { name: "New run" })).toBeVisible();
}

test.describe("New run — one page", () => {
  test("the top bar's New run navigates to the page, not a dialog", async ({ page }) => {
    await openNewRun(page);
    // The wizard rendered inside a Dialog; nothing modal should be present.
    await expect(page.getByRole("dialog")).toHaveCount(0);
    await expect(page.getByRole("heading", { name: "What to run" })).toBeVisible();
  });

  // The form's fields follow the run mode. Interactive is the default, and an
  // interactive run has NO task — the server ignores one, so the screen asks
  // what to start with instead of for a prompt nothing will read.
  test("the fields follow the run mode", async ({ page }) => {
    await openNewRun(page);
    await expect(page.getByRole("radiogroup", { name: "Start with" })).toBeVisible();
    await expect(page.getByLabel("Task")).toHaveCount(0);

    await page.getByRole("radio", { name: /^Autonomous/ }).click();
    await expect(page.getByLabel("Task")).toBeVisible();
    await expect(page.getByRole("radiogroup", { name: "Start with" })).toHaveCount(0);
  });

  // The choice that proves a run needn't involve AI. It re-labels the field and
  // swaps the help text, because a shell command is run verbatim — and it is
  // unattended by definition, so the run mode disappears with the agent picker.
  test("Shell command drops the agent picker and relabels the field", async ({ page }) => {
    await openNewRun(page);
    await page.getByRole("radio", { name: "Shell command" }).click();
    await expect(page.getByLabel("Command")).toBeVisible();
    await expect(page.getByText(/Run verbatim in the sandbox/)).toBeVisible();
    await expect(page.getByRole("combobox", { name: "Agent" })).toHaveCount(0);
    await expect(page.getByRole("radiogroup", { name: "Run mode" })).toHaveCount(0);
  });

  // The Policy panel replaced the Confinement + Network cards: the run's policy
  // IS the spec JSON, authored through the same component /policies uses. Its
  // live derivations are what the rail's Network section used to be — the
  // consequences of the envelope, while you build it rather than after.
  test("the panel's derivations track the spec as it changes", async ({ page }) => {
    await openNewRun(page);
    const spec = page.getByLabel("Spec (JSON)");

    // Opens on the Minimal template: one host, a review rule, a CC2 floor.
    await expect(spec).toHaveValue(/"api\.anthropic\.com"/);
    await expect(page.getByText("Valid JSON")).toBeVisible();
    await expect(page.getByText("1 domain allowed")).toBeVisible();

    await page.getByRole("button", { name: "Package registries" }).click();
    await expect(spec).toHaveValue(/"pypi\.org"/);
    await expect(page.getByText(/1[0-9] domains allowed/)).toBeVisible();

    // A broken document says so instead of deriving from nothing, and Launch
    // stops rather than posting a body nobody can read. The title is filled
    // first so the disable is the SPEC's doing, not the title rule's.
    await page.getByLabel("Title").fill("e2e smoke");
    await expect(page.getByRole("button", { name: "Launch run" })).toBeEnabled();
    await spec.fill("{ not json");
    await expect(page.getByText(/Invalid JSON/)).toBeVisible();
    await expect(page.getByRole("button", { name: "Launch run" })).toBeDisabled();
    await expect(page.getByText("The policy spec isn't valid JSON.")).toBeVisible();
  });

  // The Record radio only ever set allow_all_egress — a promise this screen
  // could not keep, since real Record Mode is workspace-level. It is a template
  // now, named for what it actually does.
  test("the allow-all template says block-list only, never 'unrestricted'", async ({ page }) => {
    await openNewRun(page);
    await page.getByRole("button", { name: "Allow-all — observe first" }).click();

    await expect(page.getByLabel("Spec (JSON)")).toHaveValue(/"allow_all_egress": true/);
    await expect(page.getByText("Allow-all egress (block-list only)")).toBeVisible();
  });

  // The Edit-hosts dialog's job — pick hosts, set the unlisted rule, block hosts
  // outright — is the JSON itself now, with the Fields rail documenting each key
  // and writing a starting value for it.
  test("the Fields rail inserts a key into the spec, and the derivations follow", async ({ page }) => {
    await openNewRun(page);
    const spec = page.getByLabel("Spec (JSON)");

    await page.getByRole("button", { name: "Insert denied_domains" }).click();
    await expect(spec).toHaveValue(/"denied_domains"/);
    // A deny beats an allow in both egress modes, so it is counted separately.
    await expect(page.getByText("1 domain allowed, 1 denied")).toBeVisible();

    // The unlisted-host rule is a documented key, not a buried dropdown.
    await expect(page.getByTitle("docs/POLICIES.md#first_use_approval-modes")).toBeVisible();
  });

  // Two lanes, one panel: reuse a stored policy by reference, or author one for
  // this run. Switching lanes swaps the editor for the picker, and nothing on
  // the page is merged into a stored spec.
  test("the mode row swaps the editor for the saved-policy picker", async ({ page }) => {
    await openNewRun(page);
    await expect(page.getByLabel("Spec (JSON)")).toBeVisible();

    await page.getByRole("button", { name: /Reuse a saved policy/ }).click();
    await expect(page.getByLabel("Spec (JSON)")).toHaveCount(0);
    await expect(page.getByRole("combobox", { name: "Saved policy" })).toBeVisible();
    // The launch gate surfaces ONE problem at a time, earliest first — give
    // the run a title so the policy problem is the displayed message.
    await page.getByLabel("Title").fill("mode row e2e");
    // Nothing is picked yet, so Launch says what it is waiting for.
    await expect(page.getByText("Pick a saved policy, or write a custom one.")).toBeVisible();

    await page.getByRole("button", { name: /Custom policy/ }).click();
    await expect(page.getByLabel("Spec (JSON)")).toBeVisible();
  });

  // The "no model provider is connected" rail warning is NOT asserted here on
  // purpose. Whether one exists is an environment fact: this backend runs
  // wardynd on the host, and setupProviders() detects the host's own logged-in
  // Claude CLI — so the warning is correctly absent on a developer machine and
  // present on a bare CI box. Asserting either way would make this suite pass
  // or fail on who ran it. It is pinned in new-run-screen.test.tsx instead,
  // where the SetupStatus is controlled.

  // Every run is named: the title is the grouping key on the Runs board, so
  // Launch stays disabled — and says why — until there is one.
  test("Launch waits for a title, and says what it is waiting for", async ({ page }) => {
    await openNewRun(page);
    const launch = page.getByRole("button", { name: "Launch run" });
    await expect(launch).toBeDisabled();
    await expect(page.getByText("Give this run a title.")).toBeVisible();

    await page.getByLabel("Title").fill("e2e smoke");
    await expect(launch).toBeEnabled();
  });

  test("launching creates a run and lands on its detail page", async ({ page }) => {
    await openNewRun(page);
    await page.getByLabel("Title").fill("e2e smoke");
    await launchRun(page);
  });
});

// R4-F118 — "Review predicts launch", proved on the wire.
//
// runs.wire.fields.test.ts pins runWireBody; new-run-screen.test.tsx pins that
// the SCREEN hands both doors the same input. Neither watches the bytes, and
// `grep -rn preflight e2e/*.spec.ts e2e/fixtures.ts` matched nothing at all
// before this — preflight had no browser coverage in either tier. This asserts
// the two request BODIES the daemon actually receives are the same object.
test.describe("New run — Preflight sends the body Launch sends", () => {
  test("POST /runs/preflight and POST /runs carry byte-identical bodies", async ({ page }) => {
    const bodies: Record<string, string> = {};
    page.on("request", (req) => {
      if (req.method() !== "POST") return;
      const path = new URL(req.url()).pathname;
      if (path === "/api/v1/runs/preflight") bodies.preflight = req.postData() ?? "";
      // The bare create route, not the preflight one under it.
      if (path === "/api/v1/runs") bodies.create = req.postData() ?? "";
    });

    await openNewRun(page);
    await page.getByLabel("Title").fill("e2e preflight parity");

    await page.getByRole("button", { name: /^Preflight$/ }).click();
    await expect(page.getByTestId("preflight-result")).toBeVisible();

    // Nothing is touched between the two clicks, so Review answered for exactly
    // this launch — or it lied.
    await launchRun(page);

    expect(bodies.preflight).toBeTruthy();
    expect(bodies.create).toBeTruthy();
    expect(JSON.parse(bodies.preflight)).toEqual(JSON.parse(bodies.create));
  });
});

// B4b — "Start a run like this one". 0.7.3 F7 moved this off the failure
// block (which only rendered for a run that ended badly) onto the run
// HEADER, which offers it for every terminal state — the header's onClone
// hands the wizard a RunPrefill via react-router navigation state
// (run-detail.tsx's onClone -> navigate("/runs/new", { state: { prefill } }))
// — real navigation, real state, so this has to be driven through the UI
// click rather than a bare page.goto (which would carry no location state at
// all). The failure block no longer has its own clone button, so
// `getByRole("button", { name: RUN.CLONE_CTA })` below resolves to exactly
// one element (a second door would be a Playwright strict-mode violation).
test.describe("New run — B4b clone from a killed run", () => {
  test("clones task/agent/barrier from the killed run, and Launch enables once titled", async ({ page }) => {
    const auth = { Authorization: `Bearer ${ADMIN_TOKEN}` };
    // "e2e fixture 7" is the seeded backend's KILLED run (scripts/e2e-backend.sh)
    // — read its real facts rather than hardcode them, so this test tracks the
    // seed instead of duplicating it.
    const runs = await (await page.request.get("/api/v1/runs?limit=1000", { headers: auth })).json();
    const source = runs.find((r: { task: string }) => r.task === "e2e fixture 7");
    expect(source, "seeded KILLED fixture 7 not found").toBeTruthy();
    expect(source.state).toBe("KILLED");

    await gotoConsole(page);
    await page.getByText("e2e fixture 7").click();
    await expect(page).toHaveURL(/\/runs\/.+/);
    await expect(page.getByText("Killed", { exact: true })).toBeVisible();

    await page.getByRole("button", { name: RUN.CLONE_CTA }).click();
    await expect(page).toHaveURL(/\/runs\/new$/);
    await expect(page.getByRole("heading", { name: "New run" })).toBeVisible();

    // The prefill banner — both standing sentences, and never the inline-
    // policy ceiling (this run launched against a stored/default policy, not
    // an inline one).
    const banner = page.getByRole("status").filter({ hasText: RUN.CLONE_NOTE });
    await expect(banner).toBeVisible();
    await expect(banner.getByText(RUN.CLONE_CEILING_NOTE)).toBeVisible();

    // Task carried verbatim.
    await expect(page.getByLabel("Task")).toHaveValue(source.task);
    // Agent carried (fixture 7 is claude-code, per the seed's agents array).
    await expect(page.getByRole("combobox", { name: "Agent" })).toHaveText(/Claude Code/);
    // Barrier carried — the run's own confinement_class, whatever it is.
    const barrierLabel = CC_META[source.confinement_class as ConfinementClass].label;
    await expect(
      page.getByRole("radiogroup", { name: "Barrier" }).getByRole("radio", { name: barrierLabel }),
    ).toHaveAttribute("aria-checked", "true");

    // Title does NOT clone (fixture 7 was seeded untitled) — Launch is
    // withheld until one is given, exactly the fresh-wizard rule.
    const launch = page.getByRole("button", { name: "Launch run" });
    await expect(launch).toBeDisabled();
    await page.getByLabel("Title").fill("cloned from fixture 7");
    await expect(launch).toBeEnabled();
  });
});

// The workspace-row "not an enabled provider" state (A3's per-repo-source
// `admitted` flag). Spliced onto the real GET /workspaces response — no
// provider row exists in this harness to genuinely produce admitted:false
// (legacy open mode admits everything), so this proves the CLIENT's render of
// a wire fact the server can compose; the admission RULE itself is Go's
// (user-drives-copy.ts's DRIVE_MEMBER precedent for the same technique).
test.describe("New run — workspace-card 'not an enabled provider' state", () => {
  test("a repo source with admitted:false shows PROVIDERS.CARD_NOT_ADMITTED under the picker", async ({
    page,
  }) => {
    // listWorkspaces() calls withLimit("/workspaces"), which appends
    // "?limit=..." — a bare "**/api/v1/workspaces" glob anchors past the end
    // of the path and never matches the query-string form (the same trap
    // drives.spec.ts's own RUNS_LIST_GLOB comment names for /runs).
    await page.route("**/api/v1/workspaces*", async (route) => {
      if (route.request().method() !== "GET") return route.fallback();
      const response = await route.fetch();
      const json = await response.json();
      const list = Array.isArray(json) ? json : (json.workspaces ?? []);
      const target = list[0];
      if (target) {
        target.sources = [
          ...(target.sources ?? []),
          { type: "repo", source: "https://gitlab.example/acme/refused.git", admitted: false },
        ];
      }
      await route.fulfill({ response, json });
    });

    await gotoConsole(page);
    await page.getByRole("button", { name: "New run" }).click();
    await expect(page.getByRole("heading", { name: "New run" })).toBeVisible();

    await page.getByRole("combobox").filter({ hasText: /Ephemeral scratch/ }).click();
    await page.getByRole("option", { name: "payments" }).click();

    await expect(page.getByText(PROVIDERS.CARD_NOT_ADMITTED)).toBeVisible();
  });
});
