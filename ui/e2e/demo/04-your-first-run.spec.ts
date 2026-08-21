/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * Video 04 of the series — "Your first run".
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
 * THE DIALOG IS THE OWNER'S, VERBATIM (rewrite of 2026-08-21, from
 * local/episodes-03-12-scripts-current.md's "Episode 04 — Your first run"
 * section, as edited). Every SAY / SAY-ON-CLICK stanza in that section is one
 * caption() (or one act() caption) here, in order — do not reword; wording
 * changes go through the script file and the owner. Short stanzas ride
 * BEAT_SHORT; full-length lines keep PACE.read.
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
 * reach the host). record-demo.sh --video 04 does not reset. If the workspace
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
 * Driven by `scripts/record-demo.sh --video 04`, which globs this exact
 * filename and names the take wardyn-03-your-first-run-<stamp>.mp4
 * (docs/README.md links that asset name — do not rename this file). It
 * self-skips without WARDYN_DEMO=1.
 */

import fs from "node:fs";
import path from "node:path";
import { test, expect, type Page } from "@playwright/test";
import { WORKSPACE_NAME, WORKSPACE_PATH } from "./task";
import { sweepStaleState } from "./sweep";
import { act, beat, caption, centerInFrame, chapter, PACE, spotlight } from "./overlay";
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
  `curl -sS --max-time 5 https://example.com || echo "example.com: refused, as configured"`,
  `wc -l src/*.js test/*.js > ${ARTIFACT}`,
  "echo inventory written",
  `cat ${ARTIFACT}`,
].join("\n");

// Real containers: minutes, not seconds. Ceilings for the PRODUCT — pacing
// comes from overlay.ts.
const SANDBOX_UP = 180_000;
const RUN_FINISHES = 300_000;

/** Hold for a SHORT stanza — the owner's staccato lines drag on PACE.read. */
const BEAT_SHORT = 1400;

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
      `Shoot 03 first (scripts/record-demo.sh --video 03), or restage it off camera.`,
  ).toBe(true);
  fs.rmSync(path.join(WORKSPACE_PATH, ARTIFACT), { force: true });

  // Clear any PENDING approval an earlier video deliberately left undecided —
  // video 01's "once, or for good" demo ends on a fresh re-raise it never
  // answers, and that row keeps a "1" badge on the Approvals nav item for
  // every later video's sidebar. Denied off camera, with a reason that says
  // why, so this take's chrome is quiet. (Its own run is long dead; denying
  // is a no-op beyond the bookkeeping.)
  const pending = await page.request.get("/api/v1/approvals?state=PENDING", { headers: apiHeaders() });
  if (pending.ok()) {
    const rows: { id?: string }[] = (await pending.json().catch(() => [])) ?? [];
    for (const ap of Array.isArray(rows) ? rows : []) {
      if (ap?.id) {
        await page.request
          .post(`/api/v1/approvals/${ap.id}/deny`, {
            headers: apiHeaders(),
            data: { reason: "stale demo approval from an earlier take — cleared off camera" },
          })
          .catch(() => {});
      }
    }
  }

  // S6: also kill any run still squatting on slugify from an earlier take.
  await sweepStaleState(["slugify"]);
}

test.beforeAll(async () => {
  if (!process.env.WARDYN_DEMO) return;
  await preflight(stage());
});

// ---------------------------------------------------------------------------
// Cold open + Beat 1 — the form, taught while nothing is moving
// ---------------------------------------------------------------------------

test("V04 beat 1 — name it, aim it", async () => {
  test.setTimeout(180_000);
  const page = stage();
  await page.goto("/runs/new");
  await page.bringToFront();

  await expect
    .poll(() => page.evaluate(() => typeof (window as unknown as Record<string, unknown>).__demo), { timeout: 15_000 })
    .toBe("object");
  await expect(page.getByRole("heading", { name: "New run", level: 1 })).toBeVisible({ timeout: 30_000 });

  await chapter(page, "Your first run", "A governed command, in the background");

  // Two ring moves across the cold-open lines (no text changes) — the static
  // form otherwise sits still with nothing pointing at it.
  const formCard = page.getByRole("heading", { name: "This run", level: 3 }).locator("xpath=ancestor::section[1]");
  const rail = page.getByText("What this run can do");
  await spotlight(page, formCard);
  await caption(page, "Everything Wardyn does starts with a run.");
  await beat(page, PACE.read);
  await caption(page, "A sandbox.");
  await beat(page, BEAT_SHORT);
  await caption(page, "A set of rules.");
  await beat(page, BEAT_SHORT);
  await spotlight(page, rail);
  await caption(page, "And a record of what happened.");
  await beat(page, BEAT_SHORT);
  await spotlight(page, null);
  await caption(page, "For the first one, we're going to keep it deliberately boring.");
  await beat(page, PACE.read);
  await caption(page, "No agent.");
  await beat(page, BEAT_SHORT);
  await caption(page, "No internet.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Just a command.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Because before we make this complicated, it's worth seeing what the basic contract looks like.");
  await beat(page, PACE.read);

  // Title first — the board groups by it, and an untitled run is a loose card.
  const title = page.getByLabel("Title");
  await spotlight(page, title);
  await caption(page, "Give the run a name.");
  await beat(page, BEAT_SHORT);
  await title.fill(RUN_TITLE);
  await caption(page, "That's how we'll find it later.");
  await beat(page, BEAT_SHORT);
  await spotlight(page, null);
  await caption(page, "Then choose what kind of work it should do.");
  await beat(page, PACE.read);

  // Description too, filmed silently — no owner line covers it; the field
  // being on camera at all is the teaching (Dana: "that field is what my
  // auditor reads first").
  await page.getByLabel("Description").fill("Inventory only — nothing should leave the box.");

  // The three kinds of task. Shell command is the radio this video picks; the
  // other two get one owner line each so the viewer knows the map — ringed in
  // turn, since both are visible while Agent task is still the default pick.
  await spotlight(page, page.getByRole("radio", { name: /Agent task/ }));
  await caption(page, "An agent can work on its own.");
  await beat(page, PACE.read);
  await spotlight(page, page.getByRole("radio", { name: /Terminal/ }));
  await caption(page, "A terminal puts you inside the sandbox.");
  await beat(page, PACE.read);
  await spotlight(page, page.getByRole("radio", { name: /Shell command/ }));
  await caption(page, "And a shell command simply runs from beginning to end.");
  await beat(page, PACE.read);
  await act(page, page.getByRole("radio", { name: /Shell command/ }), "Shell command.");

  const cmd = page.getByLabel("Command");
  await cmd.fill(COMMAND);
  await spotlight(page, cmd);
  await caption(page, "This one will take stock of the project and write the results to a file.");
  await beat(page, PACE.read);
  await spotlight(page, null);
});

// ---------------------------------------------------------------------------
// Beat 2 — the workspace, the envelope, the rail
// ---------------------------------------------------------------------------

test("V04 beat 2 — the envelope", async () => {
  test.setTimeout(180_000);
  const page = stage();

  // The workspace video 02 built. The combobox's option carries the name.
  const wsPicker = page.getByRole("combobox").filter({ hasText: /workspace|Ephemeral/i }).first();
  await act(page, wsPicker, "Attach the workspace we just created.");
  await caption(page, "This is the blast radius we established in the last episode.");
  await beat(page, PACE.read);
  await caption(page, "One directory.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Nothing else.");
  await beat(page, BEAT_SHORT);
  await act(page, page.getByRole("option", { name: new RegExp(WORKSPACE_NAME) }).first(), "Choose the workspace.");

  // Confined has no click line of its own in the script — it rides the
  // network-starts-closed stanza that follows it. The form seeds "Just the
  // model provider" by default (initialWizardState allows api.anthropic.com,
  // because most runs are agent runs), so "the network starts closed" is only
  // true after the None click below.
  await act(page, page.getByRole("radio", { name: /^Confined/ }), "And because this is a confined run, the network starts closed.");
  await act(page, page.getByRole("radio", { name: /^None/ }), "Network: none.");
  await caption(page, "This command doesn't need the internet, so we're not giving it one.");
  await beat(page, PACE.read);

  // The barrier rides along unremarked otherwise — name it once, on camera,
  // wording deliberately tier-agnostic so it stays true at whichever barrier
  // this host defaults to at take time (series ruling S4).
  await spotlight(page, page.getByRole("radiogroup", { name: "Barrier" }));
  await caption(page, "The sandbox barrier comes along with the run too.");
  await beat(page, PACE.read);
  await caption(page, "Same choices.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Same governance.");
  await beat(page, BEAT_SHORT);
  await spotlight(page, null);

  // The unlisted-hosts rule — the OTHER half of the envelope (owner note:
  // whether requests are even expected must be controllable, with always-deny
  // as the fallback). The three modes are the card's own Seg now, and this
  // run picks the strictest one on camera because it is the honest choice: a
  // command that expects zero requests should not park approvals on a human.
  const rules = page.getByRole("radiogroup", { name: "Unlisted hosts" });
  await centerInFrame(rules); // S2: scrollIntoViewIfNeeded leaves it under the caption bar
  await spotlight(page, rules);
  await caption(page, "And if the command tries to reach somewhere it shouldn't, we decide what happens.");
  await beat(page, PACE.read);
  await caption(page, "We can stop it.");
  await beat(page, BEAT_SHORT);
  await caption(page, "We can ask for a decision.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Or we can deny it silently.");
  await beat(page, BEAT_SHORT);
  // Owner's "Launch the command that deliberately reaches an unlisted host"
  // has no launch click of its own here (Launch itself is beat 3) — the
  // COMMAND already carries the off-list curl, so it rides this pick.
  await act(
    page,
    rules.getByRole("radio", { name: "Deny silently" }),
    "Launch the command that deliberately reaches an unlisted host.",
  );
  await spotlight(page, null);
  await caption(page, "This one is designed to fail.");
  await beat(page, PACE.read);
  await caption(page, "The request is outside the contract, so it gets denied.");
  await beat(page, PACE.read);

  // The rail is the contract, and with zero hosts it is one sentence long.
  // "0 hosts allowed" is the rail's own line (new-run-screen.tsx's
  // hostCount === 0 branch) — assert it so the narration can't outrun the form.
  const rail = page.getByText("What this run can do");
  await rail.scrollIntoViewIfNeeded().catch(() => {});
  await spotlight(page, rail);
  await expect(page.getByText("0 hosts allowed")).toBeVisible();
  await caption(page, "That's the important part of this screen.");
  await beat(page, PACE.read);
  await caption(page, "Before launch, we can see what this run is allowed to do.");
  await beat(page, PACE.read);
  // NOT wsPicker: its hasText filter matched the pre-selection placeholder,
  // and once slugify is chosen no combobox carries that text — ring the
  // Workspace card itself, which is the better frame for the line anyway.
  await spotlight(page, page.getByRole("heading", { name: "Workspace", level: 3 }).locator("xpath=ancestor::section[1]"));
  await caption(page, "The workspace defines what it can touch.");
  await beat(page, PACE.read);
  await spotlight(page, rail);
  await caption(page, "The network defines where it can go.");
  await beat(page, PACE.read);
  await spotlight(page, rules);
  await caption(page, "And the policy defines what happens when it tries something else.");
  await beat(page, PACE.read);
  await spotlight(page, null);
  await caption(page, "That's the envelope.");
  await beat(page, PACE.read);
});

// ---------------------------------------------------------------------------
// Beat 3 — launch, and let it be a background run
// ---------------------------------------------------------------------------

test("V04 beat 3 — launch, walk away", async () => {
  test.setTimeout(RUN_FINISHES + 120_000);
  const page = stage();

  await act(page, page.getByRole("button", { name: "Launch run" }), "Launch.");

  // The cockpit opens on the new run.
  await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{8,}/i, { timeout: 60_000 });
  await caption(page, "Once it's launched, the envelope is set.");
  await beat(page, PACE.read);
  await caption(page, "Only a decision at the boundary can change it.");
  await beat(page, PACE.read);

  // SCREEN: Run terminal/replay.
  await caption(page, "And unattended doesn't mean invisible.");
  await beat(page, PACE.read);
  await caption(page, "We can still watch what the run is doing.");
  await beat(page, PACE.read);

  // Running OR Completed — never Running alone. Take 1 died here: a
  // five-command script finishes in ~7 seconds, faster than the first poll of
  // a Running-only assertion, so the header said Completed while the driver
  // waited three minutes for a state the run had already left. The narration
  // is written to be true at either speed.
  await expect(page.getByText(/Running|Completed/).first()).toBeVisible({ timeout: SANDBOX_UP });
  await expect(page.getByText("Completed", { exact: true }).first()).toBeVisible({ timeout: RUN_FINISHES });
  await caption(page, "This one is short.");
  await beat(page, BEAT_SHORT);
  await caption(page, "A few commands, a few lines of output, and it's finished.");
  await beat(page, PACE.read);

  // THE LOGS, on camera, at speed (owner note from take 1: the run must not
  // just sit there completed — show the work). The Overview's hero pane for a
  // finished background run IS the session replay; play it at 2× so the video
  // never waits on real time. Both clicks are the real product controls the
  // viewer has: the speed radio TerminalPlayer just grew, and asciinema's own
  // start overlay.
  const pane = page.getByText("run finished · replay").locator("..").locator("..");
  await pane.scrollIntoViewIfNeeded().catch(() => {});
  // Start playback FIRST — the player opens on a black, unstarted frame. The
  // owner's "Open the terminal record" narrates the CLICK itself, so it reads
  // fine over the still-black frame (persona round 1's concern was narrating
  // the REPLAY'S CONTENT too early, not the click that opens it).
  const player = page.locator(".ap-player, .ap-wrapper, .ap-terminal").first();
  await expect(player).toBeVisible({ timeout: 60_000 });
  await caption(page, "Open the terminal record.");
  await spotlight(page, player);
  await player.click().catch(() => {});
  await spotlight(page, null);
  await beat(page, PACE.afterClick);
  // 2x speed is a production/pacing mechanic only — the owner's script never
  // narrates it, so it rides silently between captions.
  await page.getByRole("radio", { name: "2x speed" }).click().catch(() => {});
  await caption(page, "Wardyn keeps the terminal output as part of the run.");
  // Hold for the cast itself (~7s at 1×, ~4s at 2×) plus a breath — the
  // whole point of the beat is that the WORK is on screen.
  await beat(page, PACE.read + 5000);
  await caption(page, "So you don't have to sit here watching it finish.");
  await beat(page, PACE.read);
  await caption(page, "The run keeps the tape for you.");
  await beat(page, PACE.read);
});

// ---------------------------------------------------------------------------
// Beat 4 — the receipts
// ---------------------------------------------------------------------------

test("V04 beat 4 — the receipts", async () => {
  test.setTimeout(180_000);
  const page = stage();

  // 1. The exit code, on the run's own header. Required, not best-effort:
  // take 1 proved the chip renders ("agent exit 0") and it is the headline
  // receipt of a background run.
  const exitChip = page.getByText(/exit 0/i).first();
  await expect(exitChip).toBeVisible({ timeout: 30_000 });
  await spotlight(page, exitChip);
  await caption(page, "Exit zero.");
  await beat(page, BEAT_SHORT);
  await caption(page, "The command completed successfully.");
  await beat(page, PACE.read);
  await spotlight(page, null);

  // 2. ...and the file is REALLY on the host — the writable grant from video
  // 02 doing its job. Asserted against the disk, not narrated over a widget:
  // a take where the write silently failed must die here, not ship. (The
  // replay itself already played in beat 3, in the Overview's hero pane —
  // the Files-changed widget stays out of this video: a 7-second sandbox is
  // torn down before it ever polls, so it is honestly empty here, and its
  // moment is video 05 where an agent works long enough to watch live.)
  expect(
    fs.existsSync(path.join(WORKSPACE_PATH, ARTIFACT)),
    `the run's ${ARTIFACT} never landed in ${WORKSPACE_PATH} — was the workspace onboarded writable?`,
  ).toBe(true);
  await caption(page, "And here's the file it created.");
  await beat(page, PACE.read);
  await caption(page, "That file is inside the workspace we mounted — not somewhere else on the host.");
  await beat(page, PACE.read);

  // 3. wardynd's own allow row — the run's ONE egress line despite "no
  // network": the sandbox's control channel back to Wardyn, not the internet.
  // The deny-silently curl above now gives the panel a deny row to sit beside
  // it, so the contrast (deny + allow, side by side) is the teaching.
  const egressPanel = page.getByRole("heading", { name: "Egress" }).locator("xpath=ancestor::section[1]");
  await spotlight(page, egressPanel);
  await caption(page, "There's also one network entry here.");
  await beat(page, PACE.read);
  await caption(page, "That's Wardyn's own control channel.");
  await beat(page, PACE.read);
  await caption(page, "It's not the open internet.");
  await beat(page, PACE.read);
  await spotlight(page, null);

  // 4. The audit trail: create → exec → complete, nobody watching.
  await act(page, page.getByRole("tab", { name: "Audit" }), "Open Audit.");
  await caption(page, "Now we get the receipts.");
  await beat(page, PACE.read);
  await expect(page.getByText(/run\.create|Created the run/i).first()).toBeVisible({ timeout: 30_000 });
  await caption(page, "The run was created.");
  await beat(page, BEAT_SHORT);
  await caption(page, "The command executed.");
  await beat(page, BEAT_SHORT);
  await caption(page, "The run completed.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Those events are recorded as they happen.");
  await beat(page, PACE.read);
  await caption(page, "And you'll notice another detail here.");
  await beat(page, PACE.read);
  await caption(page, "Wardyn is honest about what its sensors can see.");
  await beat(page, PACE.read);
  // VERIFY at rehearsal: kernel.sensor.blind must actually read true for the
  // barrier this take's run used — reword or drop the line if it doesn't
  // (series ruling S4: don't outrun what the screen shows).
  await caption(page, "On this barrier, the kernel sensor can't give us a complete picture.");
  await beat(page, PACE.read);
  await caption(page, "We'll come back to that in episode twelve.");
  await beat(page, PACE.read);
});

// ---------------------------------------------------------------------------
// Conclusion
// ---------------------------------------------------------------------------

test("V04 conclusion", async () => {
  test.setTimeout(60_000);
  const page = stage();

  await chapter(page, "What you just saw", "One command, fully governed, receipts kept");
  await caption(page, "That's the basic shape of a Wardyn run.");
  await beat(page, PACE.read);
  await caption(page, "You define the envelope.");
  await beat(page, BEAT_SHORT);
  await caption(page, "The work happens inside it.");
  await beat(page, BEAT_SHORT);
  await caption(page, "And the run leaves a record behind.");
  await beat(page, PACE.read);
  await caption(page, "No internet was granted.");
  await beat(page, BEAT_SHORT);
  await caption(page, "An off-list request was denied.");
  await beat(page, BEAT_SHORT);
  await caption(page, "The command still completed its work.");
  await beat(page, PACE.read);
  await caption(page, "And we have the receipts to prove what happened.");
  await beat(page, PACE.read);
  await caption(page, "Next, we're going to stop being polite.");
  await beat(page, BEAT_SHORT);
  await caption(page, "We're going to see what the boundary actually stops.");
  await beat(page, PACE.read);
  await caption(page, "");
  await silentCard(page, "Next — 05: What it stops");
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
