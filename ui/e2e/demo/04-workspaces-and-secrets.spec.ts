/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * Video 4 of the 0.5 series — Workspaces & secrets.
 *
 * WHAT THIS FILMS. What a run may touch, stated as two lists: the directory it
 * attaches (a workspace — nothing above it is reachable) and the credential it
 * may use without ever holding it (a write-only secret, wired in by a
 * workspace REQUIREMENT and injected proxy-side). The take onboards a fresh
 * workspace on camera with "Allow writes" left unchecked (read-only by
 * default), looks at that workspace's own remembered egress, adds a secret
 * that can be set/rotated/deleted but never read back, launches a plain shell
 * run against it, and finishes on the audit trail showing both halves of the
 * grant — the workspace's requirement, and the run reading it — while a
 * canary value proves, on camera, that the secret never once rendered.
 *
 * WHY A SHELL RUN, NOT AN AGENT. This video is keyless (no model quota is
 * spent recording it) — the point is the workspace/secret plumbing, which
 * needs no LLM call to demonstrate. A plain "Shell command" run is governed
 * identically and, because initialWizardState() seeds `agent: "claude-code"`
 * regardless of run type (wizard-types.ts), it still resolves an LLM-provider
 * convention for the secret requirement to bind to (agentLLMProvider,
 * runs_create.go) — the credential mint fires at sandbox STARTUP either way,
 * never on traffic, so a run that makes no network call still earns its
 * "1 eligible · 1 minted" Credentials widget.
 *
 * STATE IT INHERITS. Shot inside the keyless V4 → V5 → V6 → V7 block: the
 * Getting Started funnel is already done (the console lands on /runs, not
 * /setup) and no model is connected. Neither matters here — Shell-command runs
 * carry no llmReady gate at all (new-run-screen.tsx only checks it for
 * `isAgent`). record-demo.sh only resets the stack for video 01, so this file
 * re-stages its OWN fixtures before filming rather than trusting what an
 * earlier take left behind.
 *
 * OWNERSHIP OF NOUNS. This video owns the workspace `secrets-lab` and the
 * secret name `anthropic-api-key`, and touches nothing else the series uses —
 * not slugify/example.com (V01/V02), not egress-lab (V05), not `record-demo`
 * (V07). api.anthropic.com is the one shared literal (task.ts's MODEL_HOST):
 * it is the real Anthropic host the seeded secret is FOR, so the workspace's
 * requirement contract names it too — that is not a borrowed noun, it is what
 * an anthropic-api-key requirement means.
 *
 * THE ARTIFACT THIS VIDEO IS ABOUT HAS NO CONSOLE SURFACE. A workspace
 * "requirement" (the contract that auto-attaches a secret/egress host to every
 * run against a workspace) is written by exactly one endpoint —
 * PUT /workspaces/{id}/requirements — and no screen in ui/src ever calls it.
 * Beat 2 creates `secrets-lab` on camera through the real "Add workspace"
 * dialog; the moment that dialog closes and hands back the new workspace's id,
 * this file issues that PUT itself, off camera, before Beat 3 ever looks at
 * the workspace again. It cannot run any earlier than that (the id does not
 * exist until the dialog creates it) — see seedRequirements()'s own comment.
 *
 * THE SENTINEL. Beat 4 pastes WARDYN-V04-CANARY-7Q4XZ into the Add-secret
 * dialog's Value field — the one moment its literal characters are legitimately
 * on screen, because an operator typing a credential into a write-only field is
 * the opposite of a leak. From that point on this file asserts, at every beat
 * that could plausibly leak it (after Save, after Launch, once the run
 * finishes, on the Audit tab), that the sentinel appears NOWHERE in the page.
 *
 * This is NOT a test. It asserts only enough to keep itself honest and to know
 * when to advance; a failure here means the recording is wrong, not that the
 * product is broken. It runs against the REAL compose stack on :8080 with a
 * real sandbox — the hermetic `-runner none` e2e backend cannot start one.
 *
 * Selectors are getByRole + accessible name, matching the rest of the suite:
 * every literal below exists in ui/src today. add-workspace-dialog.tsx and
 * secrets.tsx are the SAME dialogs walkthrough.spec.ts and ui/e2e/secrets.spec.ts
 * already drive; the interaction patterns here were proven working there first.
 *
 * Driven by `scripts/record-demo.sh --video 04`, which globs this exact
 * filename and names the take wardyn-04-workspaces-and-secrets-<stamp>.mp4
 * (docs/README.md already links that asset name — do not rename this file). It
 * self-skips without WARDYN_DEMO=1 so a bare `pnpm e2e` can never point a
 * headed browser at a developer's live stack and start deleting workspaces.
 */

import fs from "node:fs";
import { test, expect, type Page } from "@playwright/test";
import { MODEL_HOST } from "./task";
import { act, beat, caption, PACE, spotlight } from "./overlay";
// stage.ts is the rig: importing it registers this file's beforeAll/afterAll
// (one browser, one context, one recorded page), and every beat reads the page
// out of stage() inside a test body rather than closing over a module binding.
import { stage } from "./stage";

test.skip(!process.env.WARDYN_DEMO, "demo recording — run via `make record-demo` (exports WARDYN_DEMO=1)");

test.describe.configure({ mode: "serial" });

// ---------------------------------------------------------------------------
// This video's nouns. Deliberately LOCAL, not in task.ts: nothing else in the
// series touches secrets-lab or this secret name, and a shared constant is how
// two videos end up fighting over one workspace's requirements contract.
// ---------------------------------------------------------------------------

/** The workspace Beat 2 onboards on camera. Ours alone; recreated every take. */
const WORKSPACE_NAME = "secrets-lab";

/**
 * Its host directory. record-demo.sh only materializes the slugify fixture, so
 * this one is created here — an empty throwaway dir is all this video needs
 * (the shell run only greps its own environment, it never touches files), and
 * creating it in-process removes a staging step that would otherwise be prose
 * in a script nobody reads on shoot day.
 */
const WORKSPACE_PATH = process.env.WARDYN_DEMO_SECRETS_WORKSPACE || `${process.env.HOME}/wardyn-demo/secrets-lab`;

/** The secret name Beat 4 adds, and the one PROVIDER_NAME_CHIPS entry that matches it (secrets.tsx). */
const SECRET_NAME = "anthropic-api-key";

/**
 * The canary. Never a real credential — its only job is to be a string this
 * file can grep the whole page for. Video-scoped in the name on purpose: a
 * generic "test-secret-value" could plausibly collide with fixture data some
 * other video's take leaves lying around; this one cannot.
 */
const SENTINEL = "WARDYN-V04-CANARY-7Q4XZ";

/**
 * Beat 5's run title. Video-scoped in its own env var, not task.ts's shared
 * WARDYN_DEMO_TITLE (V02 already claims that name for its own run) — exporting
 * one to retake one video must never silently rename another's.
 */
const RUN_TITLE = process.env.WARDYN_DEMO_V04_TITLE || "V04 — wired to the secret by name";

/**
 * What Beat 5's shell run actually does. It never needs the credential's VALUE
 * — the whole point is that the sandbox is never given it — so this greps the
 * one place a leak would show up first: the sandbox's own environment.
 */
const PROOF_COMMAND = [
  "echo checking this sandbox for the secret it was wired to...",
  "env | grep -i canary || echo not present in this sandbox",
  "echo done",
].join("\n");

// Real containers, so this is minutes. A ceiling for waiting on the PRODUCT —
// the pacing the viewer sees comes from overlay.ts, never from here.
const RUN_FINISHES = 300_000;

/** Same bearer shape funnel.ts's clearWorkspace() and every sibling video use. */
function apiHeaders(): Record<string, string> | undefined {
  return process.env.WARDYN_DEMO_TOKEN ? { Authorization: `Bearer ${process.env.WARDYN_DEMO_TOKEN}` } : undefined;
}

/**
 * Per-take hygiene, off camera, before a frame of this video is worth filming:
 * delete any leftover secrets-lab workspace and anthropic-api-key secret a
 * PRIOR take left behind, and make sure the local directory the live
 * "Add workspace" dialog is about to point at actually exists on disk.
 *
 * Deliberately does NOT create the workspace itself — Beat 2 does that live,
 * on camera, through the real dialog. That is the one thing this function
 * cannot stage in advance.
 */
async function resetFixtures(page: Page): Promise<void> {
  fs.mkdirSync(WORKSPACE_PATH, { recursive: true });
  const headers = apiHeaders();

  const wsRes = await page.request.get("/api/v1/workspaces", { headers });
  expect(wsRes.ok(), `GET /api/v1/workspaces failed (${wsRes.status()}) — is the stack up on :8080?`).toBe(true);
  const wsBody = await wsRes.json();
  const wsItems: { id?: string; name?: string }[] = Array.isArray(wsBody) ? wsBody : (wsBody?.items ?? wsBody?.workspaces ?? []);
  for (const w of wsItems) {
    if (w?.id && w.name === WORKSPACE_NAME) {
      await page.request.delete(`/api/v1/workspaces/${w.id}`, { headers });
    }
  }

  const secRes = await page.request.get("/api/v1/secrets", { headers });
  expect(secRes.ok(), `GET /api/v1/secrets failed (${secRes.status()})`).toBe(true);
  const secBody = await secRes.json();
  const names: string[] = secBody?.names ?? [];
  if (names.includes(SECRET_NAME)) {
    await page.request.delete(`/api/v1/secrets/${encodeURIComponent(SECRET_NAME)}`, { headers });
  }
}

/**
 * Seed the workspace's requirements contract onto the row Beat 2 just created —
 * the ONLY thing in this video that has no console surface. Bundling the
 * secret AND its matching egress host in one PUT is not a shortcut standing in
 * for two separate steps: a workspace that requires the Anthropic key
 * legitimately also requires reaching api.anthropic.com to use it, so this is
 * one coherent contract. It is also what gives Beat 3 a real "Allowed hosts"
 * row — with honest provenance ("required by this workspace") — to point at,
 * and what lets Beat 5's shell run skip configuring Network entirely: the
 * workspace itself already vouches for the one host its secret needs
 * (applyWorkspaceRequirements, runs_create.go).
 */
async function seedRequirements(page: Page, workspaceId: string): Promise<void> {
  const res = await page.request.put(`/api/v1/workspaces/${workspaceId}/requirements`, {
    headers: apiHeaders(),
    data: {
      requirements: {
        [`secret:${SECRET_NAME}`]: { level: "required", provenance: "operator_set" },
        [`egress:${MODEL_HOST}`]: { level: "required", provenance: "operator_set" },
      },
    },
  });
  expect(
    res.ok(),
    `PUT /workspaces/${workspaceId}/requirements failed (${res.status()}): ${await res.text().catch(() => "")}`,
  ).toBe(true);
}

/**
 * The load-bearing negative assertion this whole video exists to make. Called
 * at every point after Beat 4's paste where the sentinel could plausibly leak
 * — a dialog that failed to clear its value, a toast that echoed it, a run's
 * environment, an audit event's data — so a take that narrates "the console
 * never prints it" over a screen that actually does print it fails here
 * instead of shipping.
 */
async function assertSentinelAbsent(page: Page, where: string): Promise<void> {
  await expect(page.locator("body"), `the sentinel secret rendered on screen at: ${where}`).not.toContainText(
    SENTINEL,
  );
}

// ---------------------------------------------------------------------------
// STAGING — the preconditions this video cannot survive without.
//
//   1. Record with `scripts/record-demo.sh --video 04` inside the keyless
//      V4 → V5 → V6 → V7 block — its default is already --no-reset for any
//      video but 01, so nothing extra is needed on the command line.
//   2. No model connection is required; a Shell-command run carries no
//      llmReady gate (new-run-screen.tsx only checks it for isAgent).
//   3. Everything else this file needs it stages itself, below.
// ---------------------------------------------------------------------------

// Registered AFTER stage.ts's own beforeAll (import order), so stage() is
// already assigned by the time this runs. Re-guards on WARDYN_DEMO because
// this hook DELETES a workspace and a secret: a file-level test.skip must
// never be the only thing standing between a developer's stack and that.
test.beforeAll(async () => {
  if (!process.env.WARDYN_DEMO) return;
  await resetFixtures(stage());
});

// ---------------------------------------------------------------------------
// Cold open + Beat 1 — the list is the blast radius
// ---------------------------------------------------------------------------

test("cold open + beat 1 — the list is the blast radius", async () => {
  test.setTimeout(120_000);
  const page = stage();
  await page.goto("/workspaces");
  await page.bringToFront();

  // Fail here rather than two minutes into a silent, caption-less take: every
  // narration call degrades to a no-op by design, so nothing downstream would
  // ever complain about an overlay that failed to install.
  await expect
    .poll(() => page.evaluate(() => typeof (window as unknown as Record<string, unknown>).__demo), { timeout: 15_000 })
    .toBe("object");

  const heading = page.getByRole("heading", { name: "Workspaces", level: 1 });
  await expect(heading).toBeVisible({ timeout: 30_000 });

  // NO CHAPTER CARD. chapter() SPEAKS its title and subtitle, and the V04 script
  // opens on two cold-open SAY lines with no card — a card here would put ~13
  // unbudgeted words into a narration measured at 1.46 w/s against a 2:30
  // ceiling. Same ruling V02 made for the same reason.
  await caption(page, "A run touches only what you handed it.");
  await beat(page, PACE.read);
  await caption(page, "Everything else on this machine does not exist to the sandbox.");
  await beat(page, PACE.read + 600);

  await spotlight(page, heading);
  await caption(page, "This list is the whole blast radius.");
  await beat(page, PACE.read);
  await spotlight(page, null);
  await caption(page, "A run attaches a workspace, or it gets nothing.");
  await beat(page, PACE.read + 400);
});

// ---------------------------------------------------------------------------
// Beat 2 — add a workspace, read-only by default
// ---------------------------------------------------------------------------

test("beat 2 — add workspace, read-only by default", async () => {
  test.setTimeout(120_000);
  const page = stage();

  await act(page, page.getByRole("button", { name: /Add your first workspace|Add workspace/ }).first());
  const dlg = page.getByRole("dialog");
  // By ROLE, not text: "Add workspace" is both the dialog title and its submit
  // button, so a bare getByText is a strict-mode violation (walkthrough.spec.ts
  // act 4 hit the same thing first).
  await expect(dlg.getByRole("heading", { name: "Add workspace" })).toBeVisible();

  await act(page, dlg.getByRole("button", { name: /Local directory/ }), "Source: repository, local directory, or empty.");

  const pathField = dlg.getByLabel("Path on this host");
  await spotlight(page, pathField);
  await pathField.fill(WORKSPACE_PATH);
  await caption(page, "One directory. Nothing above it is reachable from inside.");
  await beat(page, PACE.read);
  await spotlight(page, null);

  // Name auto-derives from the path's basename (add-workspace-dialog.tsx's
  // deriveName) — never typed here, so this is proof the claim is true, not
  // narration over a field this driver quietly filled itself.
  const nameField = dlg.getByLabel("Name", { exact: true });
  await expect(nameField).toHaveValue(WORKSPACE_NAME, { timeout: 10_000 });
  await spotlight(page, nameField);
  await beat(page, 700);
  await spotlight(page, null);

  const advanced = dlg.getByText("Advanced", { exact: true });
  await act(page, advanced, "Advanced holds the image, the mount path, and Allow writes to this directory.");
  await beat(page, PACE.read + 400);

  await spotlight(page, dlg.getByRole("radiogroup", { name: "Container image" }));
  await beat(page, 700);
  await spotlight(page, dlg.getByLabel("Mount path"));
  await beat(page, 700);
  await spotlight(page, null);

  // LOAD-BEARING, the opposite way round from every other video's onboarding
  // beat: `writable` defaults to FALSE (add-workspace-dialog.tsx) and this take
  // deliberately leaves it that way — the point of this beat is the default,
  // not a workaround for it.
  const writable = dlg.getByRole("checkbox", { name: /Allow writes/ });
  await expect(writable, "the checkbox must start UNCHECKED — this beat is about the default, not a click past it").not.toBeChecked();
  await spotlight(page, writable);
  await caption(page, "Off by default. Read-only unless you deliberately grant the write.");
  await beat(page, PACE.read + 400);
  await spotlight(page, null);

  await act(page, dlg.getByRole("button", { name: "Add workspace" }));
  await expect(dlg).toBeHidden({ timeout: 60_000 });

  // onCreated navigates straight to /workspaces/{id} (workspaces.tsx) — the id
  // in the URL IS the row this dialog just wrote, so pull it from there rather
  // than a second list fetch.
  await expect(page).toHaveURL(/\/workspaces\/[0-9a-f-]{8,}/i, { timeout: 30_000 });
  const workspaceId = page.url().split("/workspaces/")[1];

  // Off camera: the one write this video needs that has no console surface at
  // all. See seedRequirements()'s own comment for why it cannot run any
  // earlier than this. Reload so Beat 3's Allowed-hosts card reflects it —
  // workspace-detail.tsx does not poll unless a session is in flight.
  await seedRequirements(page, workspaceId);
  await page.reload();
});

// ---------------------------------------------------------------------------
// Beat 3 — the workspace remembers its egress
// ---------------------------------------------------------------------------

test("beat 3 — the workspace remembers its egress", async () => {
  test.setTimeout(60_000);
  const page = stage();

  await expect(page.getByRole("heading", { name: WORKSPACE_NAME, level: 1 })).toBeVisible({ timeout: 30_000 });

  await caption(page, "The workspace also remembers its egress: Allowed hosts.");
  await beat(page, PACE.read);

  const allowedHeading = page.getByRole("heading", { name: "Allowed hosts · 1", level: 2 });
  await expect(allowedHeading).toBeVisible({ timeout: 30_000 });
  await spotlight(page, allowedHeading);
  await beat(page, 700);

  const hostRow = page.getByText(MODEL_HOST, { exact: true }).first();
  await spotlight(page, hostRow);
  await caption(page, "Every row says why. Approved by you, or proved by a recording.");
  await beat(page, PACE.read + 400);
  // The claim the row actually earns: this one is here because the workspace
  // REQUIRES it (allowed-hosts-card.tsx's hostRows), not because it was
  // approved by hand or promoted from a recording.
  await expect(page.getByText("required by this workspace")).toBeVisible();
  await spotlight(page, null);

  // A glance, not a new line — the SCREEN direction, not a spoken beat.
  const deniedHeading = page.getByRole("heading", { name: /^Denied hosts ·/, level: 2 });
  await spotlight(page, deniedHeading);
  await beat(page, 900);
  await spotlight(page, null);
});

// ---------------------------------------------------------------------------
// Beat 4 — write-only secrets
// ---------------------------------------------------------------------------

test("beat 4 — write-only secrets", async () => {
  test.setTimeout(60_000);
  const page = stage();

  await page.goto("/secrets");
  await expect(page.getByRole("heading", { name: "Secrets", level: 1 })).toBeVisible({ timeout: 30_000 });

  await caption(page, "Secrets. Values go in and never come out.");
  await beat(page, PACE.read);

  await act(page, page.getByRole("button", { name: "Add secret", exact: true }));
  const dlg = page.getByRole("dialog");
  await expect(dlg.getByRole("heading", { name: "Add secret" })).toBeVisible();

  // The name CHIP, not typed — "anthropic-api-key" is one of secrets.tsx's
  // PROVIDER_NAME_CHIPS, the same name the workspace's requirement names.
  await act(page, dlg.getByRole("button", { name: SECRET_NAME, exact: true }));
  await expect(dlg.getByLabel("Name")).toHaveValue(SECRET_NAME);

  // The one moment the sentinel's literal characters are legitimately on
  // screen: an operator pasting a credential into a write-only field.
  const valueBox = dlg.getByLabel("Value");
  await spotlight(page, valueBox);
  await valueBox.fill(SENTINEL);
  await spotlight(page, null);
  await caption(page, "I can set it, rotate it, delete it. Never read it back.");
  await beat(page, PACE.read + 400);

  await act(page, dlg.getByRole("button", { name: "Save secret" }));
  await expect(dlg).toBeHidden({ timeout: 30_000 });
  // From here on the sentinel is checked at every beat that could leak it.
  await assertSentinelAbsent(page, "Secrets screen, right after Save secret");

  await caption(page, "A value nobody can display is a value nobody can shoulder-surf.");
  await beat(page, PACE.read + 400);

  // Open the row menu (Rotate/Delete), then close it without acting — the
  // SCREEN direction, never a click. (secrets.spec.ts notes the Radix menu can
  // open ABOVE the row and land outside the viewport for a positional click;
  // Escape sidesteps that entirely since nothing here is meant to be clicked.)
  const row = page.getByRole("row", { name: new RegExp(SECRET_NAME) });
  await spotlight(page, row);
  await act(page, row.getByRole("button", { name: "Secret actions" }));
  const menu = page.getByRole("menu");
  await expect(menu.getByRole("menuitem", { name: /Rotate/ })).toBeVisible();
  await expect(menu.getByRole("menuitem", { name: /Delete/ })).toBeVisible();
  await beat(page, 900);
  await page.keyboard.press("Escape");
  await spotlight(page, null);
  await assertSentinelAbsent(page, "Secrets screen, row menu open");
});

// ---------------------------------------------------------------------------
// Beat 5 — a run uses it without holding it
// ---------------------------------------------------------------------------

test("beat 5 — a run uses it without holding it", async () => {
  test.setTimeout(RUN_FINISHES + 120_000);
  const page = stage();

  await act(page, page.getByRole("button", { name: "New run" }), "A shell run on that workspace, wired to the secret by name.");
  await expect(page).toHaveURL(/\/runs\/new/, { timeout: 30_000 });

  // Title is required to enable Launch; the script narrates the MECHANISM, not
  // the mechanics of naming a run, so this is silent.
  await page.getByLabel("Title").fill(RUN_TITLE);

  await act(page, page.getByRole("radio", { name: "Shell command" }));
  await page.getByLabel("Command").fill(PROOF_COMMAND);

  // Same unnamed-combobox workaround walkthrough.spec.ts's act 5 documents:
  // this trigger has no accessible name (the Agent select beside it is the one
  // wired to a <label>), so filter on its placeholder text instead.
  await act(page, page.getByRole("combobox").filter({ hasText: "Ephemeral scratch" }));
  await act(page, page.getByRole("option", { name: new RegExp(WORKSPACE_NAME, "i") }).first());

  await act(page, page.getByRole("radio", { name: /^Confined/ }));
  await beat(page, PACE.read);

  // Network is left untouched on purpose — the workspace's own required egress
  // (seeded in Beat 2) already unions api.anthropic.com in, exactly like the
  // proof run at the end of act 5 leaves Network alone because "the workspace
  // already vouches for it".
  await act(
    page,
    page.getByRole("button", { name: "Launch run" }),
    "Wardyn resolves it outside the sandbox. The proxy attaches it on the way out.",
  );
  await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{8,}/i, { timeout: 60_000 });
  await assertSentinelAbsent(page, "run-detail, right after Launch");

  const finished = page.getByText(/^(Completed|Failed|Stopped|Killed)$/).first();
  await expect(finished).toBeVisible({ timeout: RUN_FINISHES });
  // usePoll pauses once the run is terminal (run-detail.tsx), so force one more
  // full fetch rather than trust whichever poll tick happened to observe it —
  // the Credentials widget below needs the grants + audit that came with it.
  await page.reload();
  await expect(page.getByText(/^(Completed|Failed|Stopped|Killed)$/).first()).toBeVisible({ timeout: 30_000 });
  await assertSentinelAbsent(page, "run-detail, once the run finished");

  // The finished-preset layout places Credentials by default (widget-registry.ts)
  // — no "Edit layout" detour needed to see it.
  const credentialsHeading = page.getByRole("heading", { name: "Credentials", level: 2 });
  await expect(credentialsHeading).toBeVisible({ timeout: 30_000 });
  await spotlight(page, credentialsHeading);
  await caption(page, "The sandbox never holds the value, and the console never prints it.");
  // The payoff: one grant, one mint — both fired at sandbox STARTUP
  // (handleInternalInjection's "startup mint"), not because the proof command
  // ever made a network call.
  await expect(page.getByText("1 eligible · 1 minted")).toBeVisible({ timeout: 60_000 });
  await beat(page, PACE.read + 600);
  await spotlight(page, null);
  await assertSentinelAbsent(page, "Credentials widget, run finished");
});

// ---------------------------------------------------------------------------
// Beat 6 — both halves on the record
// ---------------------------------------------------------------------------

test("beat 6 — both halves on the record", async () => {
  test.setTimeout(60_000);
  const page = stage();

  // The run's OWN Audit tab (already scoped to this run — run-detail.tsx's
  // AuditTab), not the global /audit page: it needs no search box because it
  // was never going to show any other run's rows.
  await act(page, page.getByRole("tab", { name: /Audit/ }), "Audit has both halves. The workspace granted it, Wardyn read it.");
  await beat(page, PACE.read + 400);

  // .first() on both: AuditTab renders the RAW action string per row, and a
  // second resolve of the same injection (a proxy restart, a renew) writes a
  // second secret.read row — a bare match would then be a strict-mode violation
  // that kills the take at 2:18 over an event we are glad to have twice.
  const grantRow = page.getByText("run.workspace.requirement.secret", { exact: true }).first();
  await expect(grantRow).toBeVisible({ timeout: 30_000 });
  await spotlight(page, grantRow);
  await beat(page, 800);

  const readRow = page.getByText("secret.read", { exact: true }).first();
  await expect(readRow).toBeVisible();
  await spotlight(page, readRow);
  await caption(page, "Read by the run's identity at start. Your browser never saw it.");
  await beat(page, PACE.read + 400);
  await spotlight(page, null);

  await assertSentinelAbsent(page, "Audit tab");

  await caption(page, "Scope, then credentials. Next: what happens when a run asks for more.");
  await beat(page, PACE.chapter);
});
