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
// -runner none, so /setup/status reports runner.driver:"none" — no runner AT
// ALL, which new-run-screen.tsx treats as unknown availability, not
// confirmed-absent (0.7.8), so all three barrier tiers stay selectable and the
// runner capability gate (runs_create.go) is skipped entirely. There IS an
// ai_provider integration now (console-agents, 0.7.3: a Bedrock region+model
// are configured for the roster-pin e2e), so the model-provider warning
// below is unconditionally absent, not merely an environment fact.
import { test, expect, gotoConsole, ADMIN_TOKEN, launchRun } from "./fixtures";
import { NO_BARRIER, RAIL, RAIL_CREDENTIAL, RAIL_PROVIDER, RAIL_RECORDING_ON, RECORDING_DISABLED_TITLE, RUN } from "../src/app/components/wardyn/copy";
import { MODEL_ACCESS_BANNER } from "../src/app/components/wardyn/model-access-copy";
import { CC_META } from "../src/app/components/wardyn/cc-meta";
import { AUTONOMY_META } from "../src/app/components/wardyn/autonomy-meta";
import { AUTONOMY_RAIL, autonomyBoundSentence } from "../src/app/lib/governance-copy";
import { AGENTS, PROVIDERS } from "../src/app/lib/workspace-providers-copy";
import type { Page } from "@playwright/test";
import { goToNewRunPanel } from "./fixtures";
import type { ConfinementClass } from "../src/app/lib/types";

// U-15: the rail's "recording is on" sentence is a shared constant now
// (RAIL_RECORDING_ON, imported above) instead of a literal re-typed here — its
// DISABLED twin always was one, so a reworded promise could move on screen while
// this copy went on passing. The unconditional credential line it replaced
// exists nowhere, so that one is still spelled out, to be asserted absent.
const OLD_UNCONDITIONAL_CREDENTIAL_LINE =
  "Minted at launch, injected by the proxy. Never written into the sandbox.";

// The New Run rail: the one complementary region named for what it answers.
const rail = (page: Page) => page.getByRole("complementary", { name: "What this run can do" });

// Launch is on screen from every panel at this viewport height, scrolled to if
// the rail is taller than what is left of it. `lg:sticky lg:top-6` only
// settles the rail once the PAGE has scrolled past that offset, so the page is
// scrolled first, as a person filling in a long panel already would have.
async function expectLaunchReachable(page: Page, height: number) {
  const launch = page.getByRole("button", { name: "Launch run" });
  for (const panel of ["run", "workspace", "access", "policy"] as const) {
    await goToNewRunPanel(page, panel);
    // Over the page's own scroller, so the wheel scrolls the page.
    await page.getByRole("heading", { name: "New run" }).hover();
    await page.mouse.wheel(0, 400);
    await expect(launch).toBeVisible();
    await launch.scrollIntoViewIfNeeded();
    const box = await launch.boundingBox();
    expect(box, `Launch run boundingBox on ${panel}`).not.toBeNull();
    expect(box!.y, `Launch run top edge on ${panel}`).toBeGreaterThanOrEqual(0);
    expect(box!.y + box!.height, `Launch run bottom edge on ${panel}`).toBeLessThanOrEqual(height);
  }
  await goToNewRunPanel(page, "run");
}

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
    // It opens on the Run panel, the first of the four.
    await expect(page.getByRole("heading", { name: "Run", exact: true, level: 2 })).toBeVisible();
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
    await goToNewRunPanel(page, "policy");
    await expect(spec).toHaveValue(/"api\.anthropic\.com"/);
    await expect(page.getByText("Valid JSON")).toBeVisible();
    await expect(page.getByText("1 domain allowed")).toBeVisible();

    await page.getByRole("button", { name: "Package registries" }).click();
    await expect(spec).toHaveValue(/"pypi\.org"/);
    await expect(page.getByText(/1[0-9] domains allowed/)).toBeVisible();

    // A broken document says so instead of deriving from nothing, and Launch
    // stops rather than posting a body nobody can read.
    await goToNewRunPanel(page, "run");
    await page.getByLabel("Title").fill("e2e smoke");
    await expect(page.getByRole("button", { name: "Launch run" })).toBeEnabled();
    await goToNewRunPanel(page, "policy");
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
    await goToNewRunPanel(page, "policy");
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

    await goToNewRunPanel(page, "policy");
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
    // The required title first: the line above Launch names one issue at a time.
    await page.getByLabel("Title").fill("e2e saved mode");
    await goToNewRunPanel(page, "policy");
    await expect(page.getByLabel("Spec (JSON)")).toBeVisible();

    await page.getByRole("button", { name: /Reuse a saved policy/ }).click();
    await expect(page.getByLabel("Spec (JSON)")).toHaveCount(0);
    await expect(page.getByRole("combobox", { name: "Saved policy" })).toBeVisible();
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

  // #1922: a title is required. A fresh form (interactive, no prompt) has
  // nothing to derive one from, so Launch waits and says so once, as a link
  // that puts focus on the field from whichever panel is on screen.
  test("Launch requires a title, and says so with a link to the field", async ({ page }) => {
    await openNewRun(page);
    const launch = page.getByRole("button", { name: "Launch run" });
    const title = page.getByLabel("Title");
    await expect(title).toBeFocused();
    await expect(launch).toBeDisabled();
    await expect(title).toHaveAttribute("aria-invalid", "true");
    await expect(page.getByRole("navigation", { name: "New run" }).getByRole("button", { name: "Run 1 issue" })).toBeVisible();

    await goToNewRunPanel(page, "policy");
    await page.getByRole("button", { name: "Give this run a title." }).click();
    await expect(title).toBeFocused();

    await title.fill("e2e required title");
    await expect(launch).toBeEnabled();
    await expect(page.getByText("Give this run a title.")).toHaveCount(0);
  });

  // The title is prefilled, visibly and editably, from the first line of the
  // Command, and Command is one line: Enter neither launches nor adds a line.
  test("Command is a single-line input whose first line prefills the title", async ({ page }) => {
    await openNewRun(page);
    await page.getByRole("radio", { name: "Shell command" }).click();
    const command = page.getByLabel("Command");
    await expect(command).toHaveJSProperty("tagName", "INPUT");
    await command.fill("make test");
    await command.press("Enter");
    await expect(command).toHaveValue("make test");
    await expect(page).toHaveURL(/\/runs\/new$/);
    await expect(page.getByLabel("Title")).toHaveValue("make test");
    await expect(page.getByRole("button", { name: "Launch run" })).toBeEnabled();

    // A Task typed for an agent is its own value, never the command.
    await page.getByRole("radio", { name: "Agent task" }).click();
    await page.getByRole("radio", { name: /^Autonomous/ }).click();
    await expect(page.getByLabel("Task")).toHaveValue("");
  });

  // #1920: Esc leaves an untouched form at once and asks before leaving a
  // dirty one; the ghost Runs button goes through the same guard.
  test("Esc and Runs ask before leaving a dirty form, and leave an untouched one", async ({ page }) => {
    await openNewRun(page);
    await page.getByLabel("Title").fill("e2e dirty leave");
    await page.keyboard.press("Escape");
    const dialog = page.getByRole("alertdialog", { name: "Leave without saving?" });
    await expect(dialog).toBeVisible();
    await dialog.getByRole("button", { name: "Keep editing" }).click();
    await expect(dialog).toBeHidden();
    await expect(page).toHaveURL(/\/runs\/new$/);
    await expect(page.getByLabel("Title")).toHaveValue("e2e dirty leave");

    await page.getByRole("button", { name: "Runs", exact: true }).click();
    await expect(dialog).toBeVisible();
    await dialog.getByRole("button", { name: "Discard changes" }).click();
    await expect(page).toHaveURL(/\/runs$/);

    // Untouched — moving between panels is not a change.
    await page.getByRole("button", { name: "New run" }).click();
    await expect(page.getByRole("heading", { name: "New run" })).toBeVisible();
    await goToNewRunPanel(page, "workspace");
    await page.keyboard.press("Escape");
    await expect(page).toHaveURL(/\/runs$/);
  });

  test("launching creates a run and lands on its detail page", async ({ page }) => {
    await openNewRun(page);
    await page.getByLabel("Title").fill("e2e smoke");
    await launchRun(page);
  });
});

// #214 — the Barrier control out of the Policy card into its own section, and
// Launch disabled (with its reason and a route to the Environment step) on a
// host that genuinely cannot build a barrier. The seeded backend runs with
// `-runner none` (this file's own header), which reads as UNKNOWN
// availability, not confirmed-absent — so the settled-empty case this issue
// is about is spliced onto a real /setup/status response, the same technique
// agents.spec.ts and model-access-banner.spec.ts already use.
test.describe("New run — no barrier can be built (#214)", () => {
  async function spliceNoBarrier(page: Page) {
    // Cache-and-serve, not route.fetch()+refulfill per match: the Environment
    // step this test clicks through to re-reads /setup/status, and a real round
    // trip PER match raced Playwright disposing an in-flight route's response
    // at teardown ("apiResponse.json: Response has been disposed").
    let cached: Record<string, unknown> | null = null;
    await page.route("**/api/v1/setup/status*", async (route) => {
      if (!cached) {
        const json = await (await route.fetch()).json();
        json.runner = { ...json.runner, driver: "docker", confinement_classes: [] };
        cached = json;
      }
      await route.fulfill({ json: cached! });
    });
  }

  test("the Barrier control leads the Policy panel, above the policy modes", async ({ page }) => {
    await openNewRun(page);
    await goToNewRunPanel(page, "policy");
    const barrier = page.getByRole("radiogroup", { name: "Barrier tier" });
    await expect(barrier).toBeVisible();
    const modes = page.getByRole("button", { name: /^Custom policy/ });
    expect((await barrier.boundingBox())!.y).toBeLessThan((await modes.boundingBox())!.y);
  });

  test("disables Launch with its reason beside it and a route to the Environment step", async ({ page }) => {
    // The Admin Setup funnel shows the welcome hero first until this is set
    // (confinement-posture.spec.ts's own precedent) — this test clicks
    // through into the funnel itself, not just to its URL.
    await page.addInitScript(() => {
      try {
        localStorage.setItem("wardyn-onboarding-seen", "1");
      } catch {
        /* private mode — ignore */
      }
    });
    await spliceNoBarrier(page);
    await openNewRun(page);
    await page.getByLabel("Title").fill("e2e no barrier");

    const launchBtn = page.getByRole("button", { name: "Launch run" });
    await expect(launchBtn).toBeDisabled();
    await expect(page.getByText(NO_BARRIER.LAUNCH_REASON, { exact: false })).toBeVisible();
    // Scoped to the rail: the shell banner and the top bar carry the SAME
    // link text elsewhere on this page (#214's other two routes), so an
    // unscoped query is a Playwright strict-mode violation, not a bug.
    const rail = page.locator("aside");
    const route = rail.getByRole("link", { name: NO_BARRIER.CTA });
    await expect(route).toHaveAttribute("href", NO_BARRIER.ADMIN_ROUTE);

    // #1328 review round 2, R2-1 — the seeded backend is a single-operator
    // install (a bare admin bearer, "url" access): console-view.tsx's
    // viewVerdict `pass`es /admin/setup straight through for it, whichever
    // view the click came from. So this proves the actual destination, not
    // just the URL: the Environment step itself, not the read-only member
    // recap plain /setup used to strand this caller on.
    // The typed title makes the draft dirty, and this screen's own links ask
    // before they leave it (#1920).
    await route.click();
    await page.getByRole("alertdialog", { name: "Leave without saving?" }).getByRole("button", { name: "Discard changes" }).click();
    await expect(page).toHaveURL(/\/admin\/setup\?step=environment/);
    await expect(page.getByRole("heading", { name: "Pick your barrier", level: 2 })).toBeVisible();
  });

  test("a host with at least one barrier leaves Launch alone", async ({ page }) => {
    await openNewRun(page); // the seeded backend's default (-runner none): unknown, never disabled for this reason
    await page.getByLabel("Title").fill("e2e unknown host");
    await expect(page.getByText(NO_BARRIER.LAUNCH_REASON, { exact: false })).toHaveCount(0);
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

    // The hermetic backend runs no barrier runtime, so its real preflight answers
    // a missing `backend` row, which (correctly) holds Launch for up to a minute.
    // This spec is about the BYTES of the two bodies, so the checklist is
    // answered clear here; the body the daemon would have received is recorded
    // first. Auto-preflight fires on its own, so the last body seen is compared.
    await page.route("**/api/v1/runs/preflight", (route) =>
      route.fulfill({ json: { enforced_confinement_class: "CC1", setup_items: [] } }),
    );
    await openNewRun(page);
    await page.getByLabel("Title").fill("e2e preflight parity");

    await goToNewRunPanel(page, "policy");
    await page.getByRole("button", { name: /^Check again$/ }).click();
    await expect(page.getByTestId("preflight-result")).toBeVisible();

    // Nothing is touched between the two clicks, so Review answered for exactly
    // this launch — or it lied.
    await launchRun(page);

    expect(bodies.preflight).toBeTruthy();
    expect(bodies.create).toBeTruthy();
    expect(JSON.parse(bodies.preflight)).toEqual(JSON.parse(bodies.create));
  });
});

// C2 — "Use the default policy": the launch carries no policy of its own, and the
// run's recorded policy origin is the default the server resolved.
test.describe("New run — Use the default policy", () => {
  test("POST /runs carries neither policy_id nor inline_policy and the run's source is the default", async ({ page }) => {
    const bodies: Record<string, string> = {};
    page.on("request", (req) => {
      if (req.method() === "POST" && new URL(req.url()).pathname === "/api/v1/runs") {
        bodies.create = req.postData() ?? "";
      }
    });
    await openNewRun(page);
    await goToNewRunPanel(page, "policy");
    await page.getByRole("button", { name: /^Use the default policy/ }).click();
    await expect(page.getByLabel("Spec (JSON)")).toHaveCount(0);
    await goToNewRunPanel(page, "run");
    await page.getByLabel("Title").fill("e2e default policy");
    await launchRun(page);

    const sent = JSON.parse(bodies.create);
    expect(sent).not.toHaveProperty("policy_id");
    expect(sent).not.toHaveProperty("inline_policy");

    const id = new URL(page.url()).pathname.split("/").pop();
    const auth = { Authorization: `Bearer ${ADMIN_TOKEN}` };
    const view = await (await page.request.get(`/api/v1/runs/${id}/policy`, { headers: auth })).json();
    expect(view.source.kind).toBe("default");
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
test.describe("New run — clone from a killed run", () => {
  // ticket: B4b
  test("clones task/agent/barrier from the killed run; Title tracks the cloned task", async ({ page }) => {
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
    await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{8,}/);
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
    await goToNewRunPanel(page, "policy");
    await expect(
      page.getByRole("radiogroup", { name: "Barrier" }).getByRole("radio", { name: barrierLabel }),
    ).toHaveAttribute("aria-checked", "true");

    // Title does NOT clone verbatim (fixture 7 was seeded untitled) — but
    // #1197 L2's prefill fills it from the cloned task, so the required title
    // is already there and Launch is enabled with none typed by hand.
    await goToNewRunPanel(page, "run");
    const launch = page.getByRole("button", { name: "Launch run" });
    await expect(page.getByLabel("Title")).toHaveValue(source.task);
    await expect(launch).toBeEnabled();
    // Still editable — the operator's own title replaces the derived one.
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

    await goToNewRunPanel(page, "workspace");
    await page.getByRole("combobox").filter({ hasText: /Ephemeral scratch/ }).click();
    await page.getByRole("option", { name: "payments" }).click();

    // An issue (#1922): error tone beside the Select, counted on the panel's
    // nav button, and it holds Launch.
    const note = page.getByText(PROVIDERS.CARD_NOT_ADMITTED);
    await expect(note).toBeVisible();
    await expect(note).toHaveClass(/text-danger/);
    await expect(page.locator("#nr-workspace")).toHaveAttribute("aria-invalid", "true");
    await expect(page.getByRole("navigation", { name: "New run" }).getByRole("button", { name: /^Workspace 1 issue/ })).toBeVisible();
    await expect(page.getByRole("button", { name: "Launch run" })).toBeDisabled();

    // From another panel it is named above Launch instead — once — and the
    // link leads back to the Select.
    await goToNewRunPanel(page, "run");
    await page.getByLabel("Title").fill("e2e not admitted");
    await expect(page.getByText(PROVIDERS.CARD_NOT_ADMITTED)).toHaveCount(1);
    await page.getByRole("button", { name: PROVIDERS.CARD_NOT_ADMITTED }).click();
    await expect(page.locator("#nr-workspace")).toBeFocused();
  });
});

// F2-F7/F3-F1: a minimal rail runs ~490-520px (fits easily at 650px tall);
// with a governance ceiling + a saved policy's tool_rules + 3 launch warnings
// all showing at once (the member/warnings path) it runs ~700-730px — below
// the fold at 1280x650 with no way to reach Launch. Spliced onto the
// real GET /policies/default, GET /policies and POST /runs responses (the
// same splice technique agents.spec.ts's own "201 carrying warnings" test
// uses, for the same reason: this harness's admin-token caller is never
// member-clamped for real) rather than a genuine member session — this pins
// the RAIL'S rendering of the combination, not the server-side clamping
// itself (Go-tested). ui/new-run-rail.tsx's primitive-level
// lg:max-h-[calc(100vh-5rem)] lg:overflow-y-auto (this lane) is what keeps
// Launch reachable here; #125 dropped the post-launch "Open run" hold this
// used to also pin — a launch now navigates away in the same tick.
test.describe("New run rail — ceiling + tool rules + 3 warnings at 1280x650 (F2-F7/F3-F1)", () => {
  test("Launch stays reachable with every rail section showing at once, and navigates straight to the run with its warnings", async ({ page }) => {
    await page.setViewportSize({ width: 1280, height: 650 });

    const baseSpec = {
      allowed_domains: ["api.anthropic.com"],
      first_use_approval: "deny_with_review" as const,
      min_confinement_class: "CC2" as ConfinementClass,
    };

    // A REAL policy, not a mocked list entry: POST /runs validates policy_id
    // against the store, so a fabricated id 404s the launch itself (the
    // splice below only patches the RESPONSE of a request that must first
    // succeed for real — same reason agents.spec.ts's "201 carrying
    // warnings" test launches a real run rather than mocking the whole
    // create path).
    const auth = { Authorization: `Bearer ${ADMIN_TOKEN}` };
    const created = await page.request.post("/api/v1/policies", {
      headers: auth,
      data: {
        name: "e2e rail-height policy",
        spec: {
          ...baseSpec,
          tool_rules: [
            { tool: "Read", effect: "allow" },
            { tool: "Bash", effect: "hold" },
          ],
        },
      },
    });
    expect(created.ok(), "POST /policies").toBeTruthy();

    await page.route("**/api/v1/policies/default*", async (route) => {
      if (route.request().method() !== "GET") return route.fallback();
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ ...baseSpec, governance_profile_name: "Contractor ceiling" }),
      });
    });

    await page.route("**/api/v1/runs", async (route) => {
      if (route.request().method() !== "POST") return route.fallback();
      const response = await route.fetch();
      const json = await response.json();
      json.warnings = [
        "Egress narrowed to api.anthropic.com by member policy.",
        "Confinement floor raised to CC2 by member policy.",
        "Grant kind git_pat removed by member policy.",
      ];
      await route.fulfill({ response, json });
    });

    // This test is about the rail's HEIGHT budget at 1280x650, not about
    // barrier availability — but the seeded backend's own `-runner none`
    // (this file's header) reads as no-barrier for #214's shell banner
    // (deriveReadiness counts confinement_classes, empty either way), which
    // would otherwise push <main> down and eat into the rail's own
    // `calc(100vh-5rem)` budget for a reason this test isn't about. Spliced
    // to a real barrier so that banner stays off, same as every other
    // pre-#214 assumption here.
    await page.route("**/api/v1/setup/status*", async (route) => {
      const response = await route.fetch();
      const json = await response.json();
      json.runner = { ...json.runner, driver: "docker", confinement_classes: ["CC1"] };
      await route.fulfill({ response, json });
    });

    await gotoConsole(page);
    await page.getByRole("button", { name: "New run" }).click();
    await expect(page.getByRole("heading", { name: "New run" })).toBeVisible();

    // Ceiling section (GET /policies/default's governance_profile_name).
    await expect(page.getByText("Ceiling", { exact: true })).toBeVisible();
    await expect(page.getByText(/Bounded by "Contractor ceiling"/)).toBeVisible();

    // Switch to the Saved-policy lane and pick the tool_rules-bearing policy.
    await goToNewRunPanel(page, "policy");
    await page.getByRole("button", { name: /^Reuse a saved policy/ }).click();
    await page.getByRole("combobox", { name: "Saved policy" }).click();
    await page.getByRole("option", { name: "e2e rail-height policy" }).click();
    await expect(page.getByText("Tool rules", { exact: true })).toBeVisible();
    await goToNewRunPanel(page, "run");
    await page.getByLabel("Title").fill("e2e rail-height");
    // Re-derived for the four-panel layout (#1922): Launch is reachable from
    // every panel, with the whole rail showing.
    await expectLaunchReachable(page, 650);

    // Reachable via scroll — not "fits with no scroll needed" (the rail is
    // legitimately taller than the viewport here; that's what
    // lg:overflow-y-auto is for). `lg:sticky lg:top-6` only settles the rail
    // to its top-6 offset once the PAGE itself has scrolled past that
    // point — at the page's natural (unscrolled) rest position the rail
    // starts lower, under the header, so scroll the page down first (a real
    // user filling in Workspace/Policy/Barrier above the rail already would
    // have) THEN scroll to the button inside the now-settled rail.
    await page.mouse.wheel(0, 400);
    const launch = page.getByRole("button", { name: "Launch run" });
    await expect(launch).toBeVisible();
    await launch.scrollIntoViewIfNeeded();
    let box = await launch.boundingBox();
    expect(box, "Launch run boundingBox (pre-launch)").not.toBeNull();
    expect(box!.y, "Launch run top edge (pre-launch)").toBeGreaterThanOrEqual(0);
    expect(box!.y + box!.height, "Launch run bottom edge (pre-launch)").toBeLessThanOrEqual(650);

    await launch.click();

    // #125: a 2xx launch navigates straight to the run, in the same tick — no
    // held rail to stay reachable in any more. All three warnings ride along
    // as router state and render on the run page itself.
    await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{8,}/);
    await expect(page.getByText(AGENTS.LAUNCH_WARNING_TITLE)).toBeVisible();
    for (const w of [
      "Egress narrowed to api.anthropic.com by member policy.",
      "Confinement floor raised to CC2 by member policy.",
      "Grant kind git_pat removed by member policy.",
    ]) {
      await expect(page.getByText(w)).toBeVisible();
    }
  });
});

// ── Appendix A finding 1: the rail states what the server resolved ───────────
//
// Since #548 a run's model credential comes only from its model provider, so
// the rail reads where it lives off the provider this run would use — before
// any click — and says only "Resolved at launch." where no provider serves the
// agent. Case 1 pins a REAL provider on this daemon; the rest splice
// /setup/status (route.fetch() + patch + refulfill) for the shapes this
// harness cannot hold for real: its admin bearer is no person, so it can never
// hold a per-person provider credential.

const E2E_PROVIDER = { id: "e2e-anthropic", name: "E2E Anthropic" };
const E2E_BEDROCK = {
  id: "bedrock-e2e",
  name: "Bedrock (e2e)",
  kind: "bedrock_sso",
  harnesses: ["claude-code"],
  host: "bedrock-runtime.us-east-1.amazonaws.com",
};

/** A real key provider serving claude-code, written through the admin API;
 *  the returned function clears the block again. */
async function seedKeyProvider(page: Page): Promise<() => Promise<void>> {
  const auth = { Authorization: `Bearer ${ADMIN_TOKEN}` };
  const put = await page.request.put("/api/v1/model-providers", {
    headers: auth,
    data: {
      providers: [
        { ...E2E_PROVIDER, kind: "anthropic_api_key", harnesses: [{ harness: "claude-code" }] },
      ],
    },
  });
  expect(put.status(), `PUT /model-providers: ${await put.text()}`).toBe(200);
  return async () => {
    const clear = await page.request.put("/api/v1/model-providers", { headers: auth, data: {} });
    expect(clear.status(), `clearing /model-providers: ${await clear.text()}`).toBe(200);
  };
}

/** Splices one Bedrock SSO provider onto /setup/status: claude-code's
 *  default, and this person not signed in to it. */
async function bedrockProviderStatus(page: Page): Promise<void> {
  await page.route("**/api/v1/setup/status*", async (route) => {
    const response = await route.fetch();
    const body = await response.json();
    body.llm_ready = true;
    body.model_providers = [{ ...E2E_BEDROCK, default_for: ["claude-code"] }];
    body.provider_access = [{ provider: E2E_BEDROCK.id, state: "not_configured", action: AGENTS.SIGN_IN_AWS }];
    await route.fulfill({ response, json: body });
  });
}

test.describe("New run rail — credentials and recording are read, not asserted", () => {
  test.afterEach(async ({ page }) => {
    // The console polls setup/status; a poll in flight at teardown otherwise
    // surfaces as an orphan "apiResponse.json: Response has been disposed" error.
    await page.unrouteAll({ behavior: "ignoreErrors" });
  });

  test("with NO Preflight click the rail states the provider's residency and what /healthz says", async ({
    page,
  }, testInfo) => {
    const auth = { Authorization: `Bearer ${ADMIN_TOKEN}` };
    const clear = await seedKeyProvider(page);
    try {
      const health = await (await page.request.get("/healthz", { headers: auth })).json();
      const recordingOff = health.components?.recording?.selected === "none";
      testInfo.annotations.push({
        type: "arm",
        description: `recording.selected=${health.components?.recording?.selected ?? "(absent)"}`,
      });

      await openNewRun(page);
      // Nothing is clicked: the state every person is in at the decision point,
      // and the state the old copy answered with "never written into the sandbox".
      await expect(page.getByTestId("preflight-result")).toHaveCount(0);
      await expect(rail(page).getByText(RAIL_PROVIDER.STATIC(E2E_PROVIDER.name), { exact: true })).toBeVisible();
      await expect(rail(page).getByText(RAIL_CREDENTIAL.PROXY, { exact: true })).toBeVisible();
      await expect(page.getByText(RAIL_CREDENTIAL.RUN_PREFLIGHT_HINT)).toHaveCount(0);
      await expect(
        page.getByText(recordingOff ? RECORDING_DISABLED_TITLE : RAIL_RECORDING_ON, { exact: true }),
      ).toBeVisible();
      // The old unconditional sentence is gone from the screen entirely.
      await expect(page.getByText(OLD_UNCONDITIONAL_CREDENTIAL_LINE)).toHaveCount(0);
    } finally {
      await clear();
    }
  });

  // U-4 (W6 blind lens): where no provider serves the agent the rail says only
  // "Resolved at launch." and how to find out — and a CURRENT verdict that
  // carries no `model_credential` drops that hint: pressing the button whose
  // result is on screen beside it is a promise that is false the moment it is
  // followed.
  test("a preflight verdict with no model_credential drops the Run Preflight hint", async ({ page }) => {
    await page.route("**/api/v1/setup/status*", async (route) => {
      const response = await route.fetch();
      const body = await response.json();
      body.llm_ready = true;
      await route.fulfill({ response, json: body });
    });
    await page.route("**/api/v1/runs/preflight", async (route) => {
      const response = await route.fetch();
      const body = await response.json();
      delete body.model_credential;
      await route.fulfill({ response, json: body });
    });

    await openNewRun(page);
    await expect(page.getByText(RAIL_CREDENTIAL.RUN_PREFLIGHT_HINT, { exact: true })).toBeVisible();
    await goToNewRunPanel(page, "policy");
    await page.getByRole("button", { name: "Check again" }).click();
    await expect(page.getByTestId("preflight-result")).toBeVisible();
    await expect(page.getByText(RAIL_CREDENTIAL.RESOLVED_AT_LAUNCH, { exact: true })).toBeVisible();
    await expect(page.getByText(RAIL_CREDENTIAL.RUN_PREFLIGHT_HINT)).toHaveCount(0);
  });

  // U-5 (W6 blind lens): a stock install before any provider is added. One
  // section said this run's first model call fails AND that its credential is
  // resolved at launch AND to press Check again to see where. Nothing resolves at
  // launch when nothing is connected.
  test("with no model provider connected the rail makes no residency promise at all", async ({ page }) => {
    // The warning reads the server's llm_ready (hasLlmPath, lib/readiness.ts).
    await page.route("**/api/v1/setup/status*", async (route) => {
      const response = await route.fetch();
      const body = await response.json();
      body.llm_ready = false;
      await route.fulfill({ response, json: body });
    });

    await openNewRun(page);
    await expect(page.getByText(/No model provider is connected/)).toBeVisible();
    await expect(page.getByText(RAIL_CREDENTIAL.RESOLVED_AT_LAUNCH)).toHaveCount(0);
    await expect(page.getByText(RAIL_CREDENTIAL.RUN_PREFLIGHT_HINT)).toHaveCount(0);
  });

  // The field report's own estate: a Bedrock SSO provider. The sandbox arm
  // renders the provider line, a sentence AND a chip where the proxy arm
  // renders two lines.
  test("a Bedrock SSO provider states residency with no click", async ({ page }) => {
    await bedrockProviderStatus(page);

    await openNewRun(page);
    // The rail's Credentials summary; the Run panel's picker states the same
    // two facts beside the control that chooses the provider.
    await expect(rail(page).getByText(RAIL_PROVIDER.STATIC(E2E_BEDROCK.name), { exact: true })).toBeVisible();
    await expect(rail(page).getByText(RAIL_CREDENTIAL.SANDBOX_BEDROCK, { exact: true })).toBeVisible();
    // #1922: the "Per-person AWS sign-in" chip is removed from both.
    await expect(page.getByText("Per-person AWS sign-in", { exact: true })).toHaveCount(0);
    await expect(page.getByText(RAIL_CREDENTIAL.RESOLVED_AT_LAUNCH)).toHaveCount(0);
  });

  // Under a Bedrock SSO provider the rail's provider section (the provider
  // line above the residency sentence) once pushed Launch about 25px below a
  // 1280x650 viewport, out of scroll reach. Re-derived for the four-panel
  // layout (#1922): reachable from every panel.
  test("under a Bedrock SSO provider, Launch stays reachable at 1280x650", async ({ page }) => {
    await page.setViewportSize({ width: 1280, height: 650 });
    await bedrockProviderStatus(page);

    await openNewRun(page);
    await expect(rail(page).getByText(RAIL_CREDENTIAL.SANDBOX_BEDROCK, { exact: true })).toBeVisible();
    await page.getByLabel("Title").fill("e2e rail residency");
    await expectLaunchReachable(page, 650);
  });

  // Below lg the rail is the page's footer: one rail and one Launch, on screen
  // from every panel without horizontal scroll, the sections behind a toggle.
  for (const size of [
    { width: 390, height: 844 },
    { width: 320, height: 568 },
  ]) {
    test(`at ${size.width}x${size.height} the footer keeps Launch on screen from every panel`, async ({ page }) => {
      await bedrockProviderStatus(page);
      // Opened at the default size (the shell's own nav needs it), then narrowed.
      await openNewRun(page);
      await page.setViewportSize(size);
      await expect(rail(page)).toHaveCount(1);
      await expect(page.getByRole("button", { name: "Launch run" })).toHaveCount(1);

      const toggle = rail(page).getByRole("button", { name: "What this run can do" });
      await expect(toggle).toHaveAttribute("aria-expanded", "false");
      await expect(rail(page).getByText("Barrier", { exact: true })).toBeHidden();
      for (const panel of ["run", "workspace", "access", "policy"] as const) {
        await goToNewRunPanel(page, panel);
        // Reachable, as on the desktop pins: at 320px the console shell is
        // itself a little taller than the viewport, so the document scrolls.
        await page.getByRole("button", { name: "Launch run" }).scrollIntoViewIfNeeded();
        const box = (await page.getByRole("button", { name: "Launch run" }).boundingBox())!;
        expect(box.y, `Launch top edge on ${panel}`).toBeGreaterThanOrEqual(0);
        expect(box.y + box.height, `Launch bottom edge on ${panel}`).toBeLessThanOrEqual(size.height);
        const fits = await page.locator("#main-content").evaluate((main) => main.scrollWidth <= main.clientWidth);
        expect(fits, `no horizontal scroll on ${panel}`).toBe(true);
      }

      // Expanding shows the summary above the decision block, keeps focus on
      // the toggle, and leaves Launch on screen.
      await toggle.click();
      await expect(toggle).toHaveAttribute("aria-expanded", "true");
      await expect(toggle).toBeFocused();
      await expect(rail(page).getByText("Barrier", { exact: true })).toBeVisible();
      // Reachable, as on the desktop pins: an open summary may leave the
      // footer taller than its bound, and then the footer itself scrolls.
      await page.getByRole("button", { name: "Launch run" }).scrollIntoViewIfNeeded();
      const open = (await page.getByRole("button", { name: "Launch run" }).boundingBox())!;
      expect(open.y, "Launch top edge, summary open").toBeGreaterThanOrEqual(0);
      expect(open.y + open.height, "Launch bottom edge, summary open").toBeLessThanOrEqual(size.height);
    });
  }

  // The launch door: create's 422 lands untruncated in the rail (launch.error),
  // and one carrying reason model_credential opens the door of the provider it
  // names (#543). These pin the WIRING, not the server's wording.
  const refusal = `This run's model provider is ${E2E_BEDROCK.name}, and you are not signed in to AWS for it.`;
  async function refuseLaunch(page: Page, body: Record<string, string>) {
    await page.route("**/api/v1/runs", async (route) => {
      if (route.request().method() !== "POST") return route.fallback();
      await route.fulfill({ status: 422, contentType: "application/json", body: JSON.stringify(body) });
    });
  }

  test("a Launch 422 carrying reason model_credential and its provider opens that provider's sign-in itself", async ({
    page,
  }) => {
    await bedrockProviderStatus(page);
    await refuseLaunch(page, { error: refusal, reason: "model_credential", provider: E2E_BEDROCK.id });

    await openNewRun(page);
    await page.getByLabel("Title").fill("e2e model access refusal");
    await page.getByRole("button", { name: "Launch run" }).click();
    await expect(page.getByText(refusal)).toBeVisible();
    // The door opened itself: the dialog, with the real pane in it.
    await expect(page.getByRole("heading", { name: MODEL_ACCESS_BANNER.DIALOG_TITLE })).toBeVisible();
    await expect(page.getByTestId("harness-login-pane")).toBeVisible();
    // Escape: nothing launched, the sentence stays, and the page never left.
    await page.keyboard.press("Escape");
    await expect(page.getByTestId("harness-login-pane")).toHaveCount(0);
    await expect(page.getByText(refusal)).toBeVisible();
    await expect(page).toHaveURL(/\/runs\/new$/);
  });

  test("a Launch 422 with no reason — a policy error — shows the sentence and opens nothing", async ({ page }) => {
    await bedrockProviderStatus(page);
    await refuseLaunch(page, { error: refusal });
    await openNewRun(page);
    await page.getByLabel("Title").fill("e2e plain refusal");
    await page.getByRole("button", { name: "Launch run" }).click();
    await expect(page.getByText(refusal)).toBeVisible();
    // #459: the refusal is an announced alert region, sr-only prefix + the
    // server's own sentence, unchanged. No dialog opens here to aria-hide it.
    await expect(page.getByRole("alert")).toContainText(RAIL.LAUNCH_ERROR_LABEL);
    await expect(page.getByRole("alert")).toContainText(refusal);
    await expect(page.getByTestId("harness-login-pane")).toHaveCount(0);
    await expect(page.getByRole("heading", { name: MODEL_ACCESS_BANNER.DIALOG_TITLE })).toHaveCount(0);
  });

  // #725/T-65 — a credential refusal that names no provider opens no door: an
  // AWS sign-in repairs nothing the server did not name, whatever the
  // deployment's claude-code default is.
  test("a model_credential refusal naming no provider opens no sign-in, on a deployment with a Bedrock default", async ({
    page,
  }) => {
    await bedrockProviderStatus(page);
    await refuseLaunch(page, { error: refusal, reason: "model_credential" });
    await openNewRun(page);
    await page.getByLabel("Title").fill("e2e codex refusal");
    await page.getByRole("combobox", { name: "Agent" }).click();
    await page.getByRole("option", { name: "Codex CLI" }).click();
    await page.getByRole("button", { name: "Launch run" }).click();
    await expect(page.getByText(refusal)).toBeVisible();
    await expect(page.getByTestId("harness-login-pane")).toHaveCount(0);
    await expect(page.getByRole("heading", { name: MODEL_ACCESS_BANNER.DIALOG_TITLE })).toHaveCount(0);
  });
});

// #93/#96 — the New Run rail's Autonomy section. The seeded backend's bearer
// is an operator, so a real preflight never resolves an autonomy cap
// (effectiveCeiling's own operator short-circuit — see governance.spec.ts's
// header note) — spliced onto POST /runs/preflight's real response, the same
// route.fetch()+patch+refulfill technique this file already uses above for
// model_credential, so the shape around the spliced field stays genuine.
test.describe("New run rail — the Autonomy section (#93/#96)", () => {
  async function mockPreflightAutonomy(page: Page, boundBy: string[]): Promise<void> {
    await page.route("**/api/v1/runs/preflight", async (route) => {
      const response = await route.fetch();
      const json = await response.json();
      json.autonomy = {
        level: "L1",
        posture: { egress: "sealed", secrets: "powerful", confinement: "CC1" },
        bound_by: boundBy,
      };
      await route.fulfill({ response, json });
    });
  }

  test("shows the resolved level and, for a tie, EVERY bound_by cause — not just the first", async ({ page }) => {
    // Ruling 1 (#96 review): bound_by is a LIST, and a tie at the resolved
    // level names every cause. This is the regression the ruling exists to
    // prevent: reading bound_by[0] alone would drop confinement_cc1 here.
    await mockPreflightAutonomy(page, ["secrets_powerful", "confinement_cc1"]);
    await openNewRun(page);
    await goToNewRunPanel(page, "policy");
    await page.getByRole("button", { name: "Check again" }).click();
    await expect(page.getByTestId("preflight-result")).toBeVisible();

    await expect(page.getByText(AUTONOMY_RAIL.HEADING, { exact: true })).toBeVisible();
    await expect(page.getByText(AUTONOMY_META.L1.label, { exact: true })).toBeVisible();
    const sentence = autonomyBoundSentence(["secrets_powerful", "confinement_cc1"]);
    await expect(page.getByText(sentence, { exact: true })).toBeVisible();
  });

  test("with no autonomy on the wire (the default): no cap on this deployment's operator bearer", async ({
    page,
  }) => {
    await openNewRun(page);
    await goToNewRunPanel(page, "policy");
    await page.getByRole("button", { name: "Check again" }).click();
    await expect(page.getByTestId("preflight-result")).toBeVisible();
    await expect(page.getByText(AUTONOMY_RAIL.HEADING, { exact: true })).toBeVisible();
    await expect(page.getByText(AUTONOMY_RAIL.NO_PROFILE, { exact: true })).toBeVisible();
  });
});

// P7 — the quota sentences are the server's (internal/api/run_fit.go) and the console adds none, so
// these pin the wiring: a 422 from preflight or from Launch is the rail's alert, verbatim, and the
// advisories ride the warnings list. The refusal is spliced as a response, the same technique as the
// sections above; the server's own gate is pinned in Go (TestCreateRun_QuotaBreachRefusesBeforeDispatch).
test.describe("New run rail — namespace quota sentences (P7)", () => {
  const breach =
    "this run needs 2 CPU, 4Gi memory, more than quota runs-quota has left (1 CPU, 2Gi memory). Stop a run, or ask your admin to raise the quota.";
  const nearFull =
    "this run would fill quota runs-quota to 94% (0.5 CPU, 1Gi memory left after it) — later runs may be refused.";
  const nodeFit =
    "no node this run may be placed on is large enough for 2 CPU, 4Gi memory. It may wait unscheduled until one is.";

  test("a preflight 422 for a breached quota shows the sentence in the alert", async ({ page }) => {
    await page.route("**/api/v1/runs/preflight", (route) =>
      route.fulfill({ status: 422, contentType: "application/json", body: JSON.stringify({ error: breach, reason: "namespace_quota_exceeded" }) }),
    );
    await openNewRun(page);
    await goToNewRunPanel(page, "policy");
    await page.getByRole("button", { name: "Check again" }).click();
    await expect(page.getByRole("alert")).toContainText(breach);
    await expect(page.getByTestId("preflight-result")).toHaveCount(0);
  });

  test("a Launch 422 for a breached quota shows the sentence and stays on the page", async ({ page }) => {
    await page.route("**/api/v1/runs", async (route) => {
      if (route.request().method() !== "POST") return route.fallback();
      await route.fulfill({ status: 422, contentType: "application/json", body: JSON.stringify({ error: breach, reason: "namespace_quota_exceeded" }) });
    });
    await openNewRun(page);
    await page.getByLabel("Title").fill("e2e quota refusal");
    await page.getByRole("button", { name: "Launch run" }).click();
    await expect(page.getByRole("alert")).toContainText(breach);
    await expect(page).toHaveURL(/\/runs\/new$/);
  });

  test("the near-full and node-fit advisories are listed in the preflight warnings", async ({ page }) => {
    await page.route("**/api/v1/runs/preflight", async (route) => {
      const response = await route.fetch();
      const json = await response.json();
      json.warnings = [...(json.warnings ?? []), nearFull, nodeFit];
      await route.fulfill({ response, json });
    });
    await openNewRun(page);
    await goToNewRunPanel(page, "policy");
    await page.getByRole("button", { name: "Check again" }).click();
    const result = page.getByTestId("preflight-result");
    await expect(result.getByRole("listitem").filter({ hasText: nearFull })).toHaveCount(1);
    await expect(result.getByRole("listitem").filter({ hasText: nodeFit })).toHaveCount(1);
  });
});
