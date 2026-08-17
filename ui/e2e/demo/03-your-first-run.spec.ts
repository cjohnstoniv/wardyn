/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * Video 03 of the 0.5 series — "Your first run".
 *
 * WHAT THIS FILMS. The simplest governed thing Wardyn can do: a plain shell
 * command, run unattended in the background, against the workspace video 02
 * onboarded. No agent, no model, no network — deliberately. The video's whole
 * job is to teach the New Run form itself while nothing else is moving: what a
 * run is, the three kinds of task, why this one needs zero egress, what the
 * rail promises before launch, and then the receipts a finished run leaves
 * behind — its exit code, the file it really wrote to the host, and the audit
 * trail from create to complete. Driving an agent is video 04; autonomy with
 * a human decision mid-run is video 05.
 *
 * WHY A SHELL COMMAND WITH NO NETWORK. Three reasons, each load-bearing:
 * keyless (no model quota spent on the form-teaching video); the tightest
 * possible envelope makes the rail's promise legible ("this run can touch the
 * workspace, and nothing else" is provable at a glance when the allowlist is
 * empty); and the command still produces a REAL artifact — it writes an
 * inventory file into the writable workspace, so "Files changed" has an honest
 * row and the take can assert the byte actually landed on the host.
 *
 * STATE IT INHERITS. Video 02's finished stack: the slugify workspace exists,
 * onboarded WRITABLE on camera (the write grant is what lets this run's file
 * reach the host). record-demo.sh --video 03 does not reset. If the workspace
 * is missing (a fresh stack, or 02 was never shot here) the beforeAll fails
 * loudly with the staging instruction instead of filming a broken form.
 *
 * OWNERSHIP OF NOUNS. This video launches one run titled RUN_TITLE (unique to
 * V03 — the board groups by title, and the verifier finds the run by it) and
 * writes one file, NOTES-INVENTORY.txt, inside the slugify workspace. It
 * touches no other series noun.
 *
 * This is NOT a test. It asserts only enough to keep itself honest and to know
 * when to advance; a failure here means the recording is wrong, not that the
 * product is broken. It runs against the REAL compose stack on :8080 with a
 * real sandbox.
 *
 * Driven by `scripts/record-demo.sh --video 03`, which globs this exact
 * filename and names the take wardyn-03-your-first-run-<stamp>.mp4
 * (docs/README.md links that asset name — do not rename this file). It
 * self-skips without WARDYN_DEMO=1.
 */

import fs from "node:fs";
import path from "node:path";
import { test, expect, type Page } from "@playwright/test";
import { WORKSPACE_NAME, WORKSPACE_PATH } from "./task";
import { act, beat, caption, chapter, PACE, spotlight } from "./overlay";
// stage.ts is the rig: importing it registers this file's beforeAll/afterAll
// (one browser, one context, one recorded page).
import { stage } from "./stage";

test.skip(!process.env.WARDYN_DEMO, "demo recording — run via `make record-demo` (exports WARDYN_DEMO=1)");

test.describe.configure({ mode: "serial" });

// ---------------------------------------------------------------------------
// This video's nouns.
// ---------------------------------------------------------------------------

/** Unique title (DA5): the board groups by it and the verifier finds it. */
const RUN_TITLE = process.env.WARDYN_DEMO_V03_TITLE || "Take inventory — first governed run";

/** The artifact the run writes into the workspace — the proof a background
 *  run really touched the host. Deleted before the take so the diff is real. */
const ARTIFACT = "NOTES-INVENTORY.txt";

/**
 * The command. Offline on purpose (the run's allowlist is EMPTY — an npm
 * install here would hang a held approval into a video that has not taught
 * approvals yet), and it ends by writing the artifact so the finished run has
 * a diff. Multi-line: the console's Shell-command field takes a script.
 */
const COMMAND = [
  "echo taking inventory of the workspace...",
  "ls -la",
  "wc -l src/*.js test/*.js",
  `wc -l src/*.js test/*.js > ${ARTIFACT}`,
  "echo inventory written",
].join("\n");

// Real containers: minutes, not seconds. Ceilings for the PRODUCT — pacing
// comes from overlay.ts.
const SANDBOX_UP = 180_000;
const RUN_FINISHES = 300_000;

function apiHeaders(): Record<string, string> | undefined {
  return process.env.WARDYN_DEMO_TOKEN ? { Authorization: `Bearer ${process.env.WARDYN_DEMO_TOKEN}` } : undefined;
}

/**
 * Per-take hygiene + the one hard precondition. The slugify workspace must
 * already exist (video 02 creates it, writable); this video will not silently
 * re-create off camera what an earlier video teaches on camera. The artifact
 * from a prior take is deleted so "Files changed" shows a real new write.
 */
async function preflight(page: Page): Promise<void> {
  const res = await page.request.get("/api/v1/workspaces", { headers: apiHeaders() });
  expect(res.ok(), `GET /api/v1/workspaces failed (${res.status()}) — is the stack up on :8080?`).toBe(true);
  const body = await res.json();
  const items: { id?: string; name?: string }[] = Array.isArray(body)
    ? body
    : (body?.items ?? body?.workspaces ?? []);
  expect(
    items.some((w) => w.name === WORKSPACE_NAME),
    `no "${WORKSPACE_NAME}" workspace on this stack — video 03 runs against the one video 02 onboards. ` +
      `Shoot 02 first (scripts/record-demo.sh --video 02), or restage it off camera.`,
  ).toBe(true);
  fs.rmSync(path.join(WORKSPACE_PATH, ARTIFACT), { force: true });
}

test.beforeAll(async () => {
  if (!process.env.WARDYN_DEMO) return;
  await preflight(stage());
});

// ---------------------------------------------------------------------------
// Cold open + Beat 1 — the form, taught while nothing is moving
// ---------------------------------------------------------------------------

test("V03 beat 1 — name it, aim it", async () => {
  test.setTimeout(180_000);
  const page = stage();
  await page.goto("/runs/new");
  await page.bringToFront();

  await expect
    .poll(() => page.evaluate(() => typeof (window as unknown as Record<string, unknown>).__demo), { timeout: 15_000 })
    .toBe("object");
  await expect(page.getByRole("heading", { name: "New run", level: 1 })).toBeVisible({ timeout: 30_000 });

  await chapter(page, "Your first run", "A governed command, in the background");
  await caption(page, "Everything Wardyn does is a run: a sandbox, a policy around it, and a record after it.");
  await beat(page, PACE.read);
  await caption(page, "The first one should be boring on purpose — a plain command, no agent, no network.");
  await beat(page, PACE.read);
  await caption(page, "That way the form itself gets to be the lesson.");
  await beat(page, PACE.read);

  // Title first — the board groups by it, and an untitled run is a loose card.
  const title = page.getByLabel("Title");
  await spotlight(page, title);
  await title.fill(RUN_TITLE);
  await caption(page, "The title is how you find this later — same title, same work, one group on the board.");
  await beat(page, PACE.read);
  await spotlight(page, null);

  // The three kinds of task. Shell command is the radio this video picks; the
  // other two get one honest line each so the viewer knows the map.
  await caption(page, "Three kinds of task. An agent works on its own; a terminal is you, live inside the box.");
  await beat(page, PACE.read);
  await act(page, page.getByRole("radio", { name: /Shell command/ }), "And a shell command just runs — unattended, start to finish.");

  const cmd = page.getByLabel("Command");
  await spotlight(page, cmd);
  await cmd.fill(COMMAND);
  await caption(page, "This one takes stock of the project and writes the tally to a file.");
  await beat(page, PACE.read);
  await spotlight(page, null);
});

// ---------------------------------------------------------------------------
// Beat 2 — the workspace, the envelope, the rail
// ---------------------------------------------------------------------------

test("V03 beat 2 — the envelope", async () => {
  test.setTimeout(120_000);
  const page = stage();

  // The workspace video 02 built. The combobox's option carries the name.
  await caption(page, "Attach the workspace from last video — the one directory this run may touch.");
  await beat(page, PACE.read);
  const wsPicker = page.getByRole("combobox").filter({ hasText: /workspace|Ephemeral/i }).first();
  await act(page, wsPicker);
  await act(page, page.getByRole("option", { name: new RegExp(WORKSPACE_NAME) }).first());

  // Confined, then the NONE preset — not the default. The form seeds
  // "Just the model provider" (initialWizardState allows api.anthropic.com,
  // because most runs are agent runs), so "we allow nothing" is only true
  // after this click. Which is itself the teaching: the presets are how a
  // run's network access is tuned, and this video picks the tightest one on
  // camera rather than narrating a default it did not choose.
  await act(page, page.getByRole("radio", { name: /^Confined/ }), "Confined means default-deny: nothing reaches the network unless we allow it.");
  await act(page, page.getByRole("radio", { name: /^None/ }), "These presets are the tuning knob — and this command needs no internet, so: none.");
  await caption(page, "Anything it reaches for anyway would stop and ask a human. Nothing slips out quietly.");
  await beat(page, PACE.read);

  // The rail is the contract, and with zero hosts it is one sentence long.
  // "0 hosts allowed" is the rail's own line (new-run-screen.tsx's
  // hostCount === 0 branch) — assert it so the narration can't outrun the form.
  const rail = page.getByText("What this run can do");
  await rail.scrollIntoViewIfNeeded().catch(() => {});
  await spotlight(page, rail);
  await expect(page.getByText("0 hosts allowed")).toBeVisible();
  await caption(page, "The rail is the contract, settled before launch: this workspace, and nothing else.");
  await beat(page, PACE.read + 400);
  await spotlight(page, null);
});

// ---------------------------------------------------------------------------
// Beat 3 — launch, and let it be a background run
// ---------------------------------------------------------------------------

test("V03 beat 3 — launch, walk away", async () => {
  test.setTimeout(RUN_FINISHES + 120_000);
  const page = stage();

  await act(page, page.getByRole("button", { name: "Launch run" }));

  // The cockpit opens on the new run.
  await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{8,}/i, { timeout: 60_000 });
  await caption(page, "Launched. This run is unattended — there is nothing to type and nothing to babysit.");
  await beat(page, PACE.read);

  // RUNNING first (a real sandbox boots), then COMPLETED. Both chips are the
  // run header's own state; asserting the sequence keeps the narration honest.
  await expect(page.getByText(/Running/i).first()).toBeVisible({ timeout: SANDBOX_UP });
  await caption(page, "A sandbox boots, the command runs, and we could close this tab and come back.");
  await beat(page, PACE.read);

  await expect(page.getByText("Completed", { exact: true }).first()).toBeVisible({ timeout: RUN_FINISHES });
  await caption(page, "Done. Now the part that outlives the sandbox: the receipts.");
  await beat(page, PACE.read + 400);
});

// ---------------------------------------------------------------------------
// Beat 4 — the receipts
// ---------------------------------------------------------------------------

test("V03 beat 4 — the receipts", async () => {
  test.setTimeout(180_000);
  const page = stage();

  // 1. The exit code, on the run's own header.
  const exitChip = page.getByText(/exit 0/i).first();
  if (await exitChip.isVisible().catch(() => false)) {
    await spotlight(page, exitChip);
    await caption(page, "Exit zero — the command's own verdict, promoted to the run's headline.");
    await beat(page, PACE.read);
    await spotlight(page, null);
  }

  // 2. Files changed: the inventory file, and ONLY the inventory file.
  const files = page.getByRole("heading", { name: "Files changed" }).first();
  await files.scrollIntoViewIfNeeded().catch(() => {});
  await spotlight(page, files);
  await caption(page, "Files changed lists exactly what it touched: one new file, the inventory.");
  await beat(page, PACE.read);
  await expect(page.getByText(ARTIFACT).first()).toBeVisible({ timeout: 30_000 });
  await spotlight(page, null);

  // ...and the file is REALLY on the host — the writable grant from video 02
  // doing its job. Asserted, not narrated: a take where the write silently
  // failed must die here, not ship.
  expect(
    fs.existsSync(path.join(WORKSPACE_PATH, ARTIFACT)),
    `the run's ${ARTIFACT} never landed in ${WORKSPACE_PATH} — was the workspace onboarded writable?`,
  ).toBe(true);
  await caption(page, "And it is really on this machine's disk — the write grant from last video, doing its job.");
  await beat(page, PACE.read);

  // 3. The audit trail: create → dispatch → complete, nobody watching.
  await act(page, page.getByRole("tab", { name: "Audit" }));
  await caption(page, "The audit trail: created, dispatched, completed — written as it happened.");
  await beat(page, PACE.read);
  await expect(page.getByText(/run\.create|Created the run/i).first()).toBeVisible({ timeout: 30_000 });
  await beat(page, PACE.read);
});

// ---------------------------------------------------------------------------
// Conclusion
// ---------------------------------------------------------------------------

test("V03 conclusion", async () => {
  test.setTimeout(60_000);
  const page = stage();

  await chapter(page, "What you just saw", "One command, fully governed, receipts kept");
  await caption(page, "That is the whole shape of a run: an envelope you set, work inside it, a record after.");
  await beat(page, PACE.read);
  await caption(page, "This one was a shell command with no network at all — and it still left a diff and a trail.");
  await beat(page, PACE.read);
  await caption(page, "Everything else in this series is the same shape with more inside the envelope.");
  await beat(page, PACE.read + 400);
  await caption(page, "Next: put an agent in the box, and drive it yourself.");
  await beat(page, PACE.read);
  await caption(page, "");
  await silentCard(page, "Next — 04: Interactive runs");
});

/** The unspoken outro card, per the series convention video 01 set. */
async function silentCard(page: Page, text: string): Promise<void> {
  const set = (t: string) =>
    page
      .evaluate((s: string) => {
        const d = (window as unknown as Record<string, Record<string, (...x: unknown[]) => void>>).__demo;
        d?.chapter?.(s, "");
      }, t)
      .catch(() => {
        /* overlay absent — cosmetic, never fatal */
      });
  await set(text);
  await page.waitForTimeout(PACE.chapter);
  await set("");
}
