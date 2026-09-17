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
// capability gate (runs_create.go) is skipped entirely. There IS an
// ai_provider integration now (console-agents, 0.7.3: a Bedrock region+model
// are configured for the roster-pin e2e), so the model-provider warning
// below is unconditionally absent, not merely an environment fact.
import { test, expect, gotoConsole, ADMIN_TOKEN, launchRun } from "./fixtures";
import { RAIL_CREDENTIAL, RAIL_RECORDING_ON, RECORDING_DISABLED_TITLE, RUN } from "../src/app/components/wardyn/copy";
import { CC_META } from "../src/app/components/wardyn/cc-meta";
import { AGENTS, PROVIDERS } from "../src/app/lib/workspace-providers-copy";
import type { Page } from "@playwright/test";
import type { ConfinementClass } from "../src/app/lib/types";

// U-15: the rail's "recording is on" sentence is a shared constant now
// (RAIL_RECORDING_ON, imported above) instead of a literal re-typed here — its
// DISABLED twin always was one, so a reworded promise could move on screen while
// this copy went on passing. The unconditional credential line it replaced
// exists nowhere, so that one is still spelled out, to be asserted absent.
const OLD_UNCONDITIONAL_CREDENTIAL_LINE =
  "Minted at launch, injected by the proxy. Never written into the sandbox.";

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

// F2-F7/F3-F1: a minimal rail runs ~490-520px (fits easily at 650px tall);
// with a governance ceiling + a saved policy's tool_rules + 3 launch warnings
// all showing at once (the member/warnings path) it runs ~700-730px — below
// the fold at 1280x650 with no way to reach Launch/Open run. Spliced onto the
// real GET /policies/default, GET /policies and POST /runs responses (the
// same splice technique agents.spec.ts's own "201 carrying warnings" test
// uses, for the same reason: this harness's admin-token caller is never
// member-clamped for real) rather than a genuine member session — this pins
// the RAIL'S rendering of the combination, not the server-side clamping
// itself (Go-tested). ui/new-run-rail.tsx's primitive-level
// lg:max-h-[calc(100vh-5rem)] lg:overflow-y-auto (this lane) is what keeps
// Launch/Open run reachable here.
test.describe("New run rail — ceiling + tool rules + 3 warnings at 1280x650 (F2-F7/F3-F1)", () => {
  test("Launch, then Open run, stay in viewport with every rail section showing at once", async ({ page }) => {
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

    await gotoConsole(page);
    await page.getByRole("button", { name: "New run" }).click();
    await expect(page.getByRole("heading", { name: "New run" })).toBeVisible();

    // Ceiling section (GET /policies/default's governance_profile_name).
    await expect(page.getByText("Ceiling", { exact: true })).toBeVisible();
    await expect(page.getByText(/Bounded by "Contractor ceiling"/)).toBeVisible();

    // Switch to the Saved-policy lane and pick the tool_rules-bearing policy.
    await page.getByRole("button", { name: /^Reuse a saved policy/ }).click();
    await page.getByRole("combobox", { name: "Saved policy" }).click();
    await page.getByRole("option", { name: "e2e rail-height policy" }).click();
    await expect(page.getByText("Tool rules", { exact: true })).toBeVisible();
    // Launch is disabled with no title ("Give this run a title.") — fill one.
    await page.getByLabel("Title").fill("e2e rail-height");

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

    // Now all three warnings render too, and Open run replaces Launch — still
    // reachable, which is the actual defect this lane's fix addresses.
    await expect(page.getByText(AGENTS.LAUNCH_WARNING_TITLE)).toBeVisible();
    const openRun = page.getByRole("button", { name: AGENTS.OPEN_RUN_CTA });
    await expect(openRun).toBeVisible();
    await openRun.scrollIntoViewIfNeeded();
    box = await openRun.boundingBox();
    expect(box, "Open run boundingBox").not.toBeNull();
    expect(box!.y, "Open run top edge").toBeGreaterThanOrEqual(0);
    expect(box!.y + box!.height, "Open run bottom edge").toBeLessThanOrEqual(650);
  });
});

// ── Appendix A finding 1: the rail states what the server resolved ───────────
//
// Three cases, and the split between them is the design. /setup/status settles
// residency for ONE row shape (an enabled per_user + bedrock_sso row), because a
// roster cannot know which lane a run resolves; every other deployment reads
// "Resolved at launch." until Preflight answers for the exact body. So: case 1
// pins the no-click state on THIS daemon and asserts the row really is silent,
// case 2 pins the precise answer against the response the console itself got,
// and case 3 route-stubs the row-fixed shape at the height suite's viewport so
// the chip's extra line is MEASURED rather than assumed.

// railCredentialSentence is the rail's own mapping, spelled once more here so
// the assertion is "the console repeats the server", not "the console renders a
// string this spec also hardcodes". It reads the RESOLVED mechanism only — never
// the roster's declared one. Keep in step with new-run-rail.tsx.
function railCredentialSentence(cred?: {
  residency?: string;
  mechanism?: string;
  staged_placeholder?: boolean;
}): string {
  switch (cred?.residency) {
    case "proxy":
      return cred.staged_placeholder ? RAIL_CREDENTIAL.PROXY_STAGED : RAIL_CREDENTIAL.PROXY;
    case "sandbox":
      return cred.mechanism === "anthropic_subscription"
        ? RAIL_CREDENTIAL.SANDBOX_SUBSCRIPTION
        : RAIL_CREDENTIAL.SANDBOX_BEDROCK;
    case "image":
      return RAIL_CREDENTIAL.IMAGE;
    default:
      return RAIL_CREDENTIAL.RESOLVED_AT_LAUNCH;
  }
}

test.describe("New run rail — credentials and recording are read, not asserted", () => {
  test("with NO Preflight click the rail says exactly what /setup/status and /healthz say", async ({
    page,
  }, testInfo) => {
    const auth = { Authorization: `Bearer ${ADMIN_TOKEN}` };
    const status = await (await page.request.get("/api/v1/setup/status", { headers: auth })).json();
    const health = await (await page.request.get("/healthz", { headers: auth })).json();
    const row = (status.harnesses ?? []).find((h: { id: string }) => h.id === "claude-code");
    const recordingOff = health.components?.recording?.selected === "none";
    testInfo.annotations.push({
      type: "arm",
      description: `claude-code row: credential_residency=${row?.credential_residency ?? "(absent)"}, ` +
        `mechanism=${row?.mechanism ?? "(absent)"}; recording.selected=${health.components?.recording?.selected ?? "(absent)"}`,
    });
    // This daemon declares NO agent roster (scripts/e2e-backend.sh), so the row
    // settles nothing — which is the common deployment and the arm being pinned.
    // A future seeded roster would have to change this assertion deliberately
    // rather than silently re-point the case at a different arm.
    expect(row, "claude-code is in the harness catalog").toBeTruthy();
    expect(row.credential_residency, "no roster ⇒ the row settles no residency").toBeUndefined();

    await openNewRun(page);
    // Nothing is clicked: the state every person is in at the decision point, and
    // the state the old copy answered with "never written into the sandbox".
    await expect(page.getByTestId("preflight-result")).toHaveCount(0);
    await expect(page.getByText(RAIL_CREDENTIAL.RESOLVED_AT_LAUNCH, { exact: true })).toBeVisible();
    await expect(page.getByText(RAIL_CREDENTIAL.RUN_PREFLIGHT_HINT, { exact: true })).toBeVisible();
    await expect(
      page.getByText(recordingOff ? RECORDING_DISABLED_TITLE : RAIL_RECORDING_ON, { exact: true }),
    ).toBeVisible();
    // The old unconditional sentence is gone from the screen entirely.
    await expect(page.getByText(OLD_UNCONDITIONAL_CREDENTIAL_LINE)).toHaveCount(0);
  });

  test("after Preflight the rail states the verdict for the body it graded", async ({ page }, testInfo) => {
    const auth = { Authorization: `Bearer ${ADMIN_TOKEN}` };
    // Give the daemon a lane that actually RESOLVES, so this case pins the
    // precise answer rather than re-pinning "unknown": with a region and model
    // already configured (scripts/e2e-backend.sh), a stored bedrock-api-key makes
    // the bearer lane fire — the one Bedrock lane that is never resident.
    const put = await page.request.put("/api/v1/secrets/bedrock-api-key", {
      headers: auth,
      data: { value: "e2e-bedrock-bearer" },
    });
    expect(put.ok(), "PUT /secrets/bedrock-api-key").toBeTruthy();
    try {
      // Read the response the CONSOLE itself got, rather than reconstructing the
      // request body here — the override is defined as "the verdict for the body
      // this screen dry-ran", so that is what must be compared against.
      let graded: { residency?: string; mechanism?: string; staged_placeholder?: boolean } | undefined;
      await page.route("**/api/v1/runs/preflight", async (route) => {
        const response = await route.fetch();
        graded = (await response.json()).model_credential;
        await route.fulfill({ response });
      });

      await openNewRun(page);
      await page.getByRole("button", { name: "Preflight" }).click();
      await expect(page.getByTestId("preflight-result")).toBeVisible();
      expect(graded, "POST /runs/preflight carried a model_credential").toBeDefined();
      testInfo.annotations.push({
        type: "arm",
        description: `preflight model_credential: residency=${graded!.residency}, mechanism=${graded!.mechanism}`,
      });
      // A resolved verdict, not the unresolved one case 1 already covers.
      expect(graded!.residency, "preflight resolved a lane").not.toBe("unknown");
      expect(["proxy", "sandbox", "image"]).toContain(graded!.residency);
      await expect(page.getByText(railCredentialSentence(graded), { exact: true })).toBeVisible();
      await expect(page.getByText(RAIL_CREDENTIAL.RUN_PREFLIGHT_HINT)).toHaveCount(0);
    } finally {
      await page.request.delete("/api/v1/secrets/bedrock-api-key", { headers: auth });
    }
  });

  // U-4 (W6 blind lens): a CURRENT verdict that carries no `model_credential` —
  // what a 0.7.4 daemon always answers, and what 0.7.5 answers when the roster
  // read failed or there is no store. The rail still told the reader to press the
  // button whose result was on screen beside it: a promise that is false the
  // moment it is followed.
  test("a preflight verdict with no model_credential drops the Run Preflight hint", async ({ page }) => {
    await page.route("**/api/v1/runs/preflight", async (route) => {
      const response = await route.fetch();
      const body = await response.json();
      delete body.model_credential;
      await route.fulfill({ response, json: body });
    });

    await openNewRun(page);
    await expect(page.getByText(RAIL_CREDENTIAL.RUN_PREFLIGHT_HINT, { exact: true })).toBeVisible();
    await page.getByRole("button", { name: "Preflight" }).click();
    await expect(page.getByTestId("preflight-result")).toBeVisible();
    await expect(page.getByText(RAIL_CREDENTIAL.RESOLVED_AT_LAUNCH, { exact: true })).toBeVisible();
    await expect(page.getByText(RAIL_CREDENTIAL.RUN_PREFLIGHT_HINT)).toHaveCount(0);
  });

  // U-5 (W6 blind lens): a stock install before any key is added — llm_ready
  // false WITH a roster row. One section said this run's first model call fails
  // AND that its credential is resolved at launch AND to press Preflight to see
  // where. Nothing resolves at launch when nothing is connected.
  test("with no model provider connected the rail makes no residency promise at all", async ({ page }) => {
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

  // The ROW-FIXED shape — the field report's own estate, which this daemon has no
  // roster for. Stubbed at the height suite's own 1280x650 so the chip's extra
  // line is MEASURED: the sandbox arm renders a sentence AND a chip where every
  // other arm renders one line, and Launch must stay reachable.
  test("a per_user Bedrock SSO row states residency with no click, and Launch stays reachable at 1280x650", async ({
    page,
  }) => {
    await page.setViewportSize({ width: 1280, height: 650 });
    await page.route("**/api/v1/setup/status*", async (route) => {
      const response = await route.fetch();
      const body = await response.json();
      body.harnesses = (body.harnesses ?? []).map((h: { id: string }) =>
        h.id === "claude-code"
          ? {
              ...h,
              mechanism: "bedrock_sso",
              credential_source: "per_user",
              credential_residency: "sandbox",
            }
          : h,
      );
      await route.fulfill({ response, json: body });
    });

    await openNewRun(page);
    await expect(page.getByText(RAIL_CREDENTIAL.SANDBOX_BEDROCK, { exact: true })).toBeVisible();
    await expect(
      page.getByText(RAIL_CREDENTIAL.SANDBOX_BEDROCK_CHIP_PER_USER, { exact: true }),
    ).toBeVisible();
    await expect(page.getByText(RAIL_CREDENTIAL.RESOLVED_AT_LAUNCH)).toHaveCount(0);

    await page.getByLabel("Title").fill("e2e rail residency");
    await page.mouse.wheel(0, 400);
    const launch = page.getByRole("button", { name: "Launch run" });
    await launch.scrollIntoViewIfNeeded();
    const box = await launch.boundingBox();
    expect(box, "Launch run boundingBox").not.toBeNull();
    expect(box!.y, "Launch run top edge").toBeGreaterThanOrEqual(0);
    expect(box!.y + box!.height, "Launch run bottom edge").toBeLessThanOrEqual(650);
  });
});
