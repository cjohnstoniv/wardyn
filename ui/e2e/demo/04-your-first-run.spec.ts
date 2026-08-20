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

  // Two ring moves across the one cold-open line (no text changes) — the
  // static form otherwise sits still for ~16s with nothing pointing at it.
  const formCard = page.getByRole("heading", { name: "This run", level: 2 }).locator("xpath=ancestor::section[1]");
  const rail = page.getByText("What this run can do");
  await spotlight(page, formCard);
  await caption(page, "Everything Wardyn does is a run: a sandbox, a policy around it,");
  await beat(page, PACE.read);
  await spotlight(page, rail);
  await caption(page, "and a record after it.");
  await beat(page, PACE.read);
  await spotlight(page, null);
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

  // Description too, filmed silently — no caption; the field being on camera
  // at all is the teaching (Dana: "that field is what my auditor reads first").
  await page.getByLabel("Description").fill("Inventory only — nothing should leave the box.");

  // The three kinds of task. Shell command is the radio this video picks; the
  // other two get one honest line each so the viewer knows the map — ringed in
  // turn, since both are visible while Agent task is still the default pick.
  await caption(page, "Three kinds of task.");
  await beat(page, PACE.read);
  await spotlight(page, page.getByRole("radio", { name: /Agent task/ }));
  await caption(page, "An agent works on its own;");
  await beat(page, PACE.read);
  await spotlight(page, page.getByRole("radio", { name: /Terminal/ }));
  await caption(page, "a terminal is you, live inside the box.");
  await beat(page, PACE.read);
  await act(page, page.getByRole("radio", { name: /Shell command/ }), "And a shell command just runs — unattended, start to finish.");

  const cmd = page.getByLabel("Command");
  await cmd.fill(COMMAND);
  await spotlight(page, cmd);
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
  const wsPicker = page.getByRole("combobox").filter({ hasText: /workspace|Ephemeral/i }).first();
  await act(page, wsPicker, "Attach the workspace from last video — the one directory this run may touch.");
  await act(
    page,
    page.getByRole("option", { name: new RegExp(WORKSPACE_NAME) }).first(),
    "slugify — the folder video two onboarded, writes granted.",
  );

  // Confined, then the NONE preset — not the default. The form seeds
  // "Just the model provider" (initialWizardState allows api.anthropic.com,
  // because most runs are agent runs), so "we allow nothing" is only true
  // after this click. Which is itself the teaching: the presets are how a
  // run's network access is tuned, and this video picks the tightest one on
  // camera rather than narrating a default it did not choose.
  await act(page, page.getByRole("radio", { name: /^Confined/ }), "Confined means default-deny: nothing reaches the network unless we allow it.");
  await act(page, page.getByRole("radio", { name: /^None/ }), "These presets are the tuning knob — and this command needs no internet, so: none.");

  // The barrier rides along unremarked otherwise — name it once, on camera,
  // wording deliberately tier-agnostic so it stays true at whichever barrier
  // this host defaults to at take time (series ruling S4).
  await spotlight(page, page.getByRole("radiogroup", { name: "Barrier" }));
  await caption(page, "The barrier row rides along from video two — any tier, exactly the same governance.");
  await beat(page, PACE.read);
  await spotlight(page, null);

  // The unlisted-hosts rule — the OTHER half of the envelope (owner note:
  // whether requests are even expected must be controllable, with always-deny
  // as the fallback). The three modes are the card's own Seg now, and this
  // run picks the strictest one on camera because it is the honest choice: a
  // command that expects zero requests should not park approvals on a human.
  const rules = page.getByRole("radiogroup", { name: "Unlisted hosts" });
  await centerInFrame(rules); // S2: scrollIntoViewIfNeeded leaves it under the caption bar
  await spotlight(page, rules);
  await caption(page, "And you decide what happens if it reaches for anything anyway.");
  await beat(page, PACE.read);
  await caption(page, "Hold it for a live decision, deny but raise it for review — or deny silently.");
  await beat(page, PACE.read);
  await act(page, rules.getByRole("radio", { name: "Deny silently" }), "It pokes one off-list host on purpose — deny silently means it fails fast, on the record.");
  await spotlight(page, null);

  // The rail is the contract, and with zero hosts it is one sentence long.
  // "0 hosts allowed" is the rail's own line (new-run-screen.tsx's
  // hostCount === 0 branch) — assert it so the narration can't outrun the form.
  const rail = page.getByText("What this run can do");
  await rail.scrollIntoViewIfNeeded().catch(() => {});
  await spotlight(page, rail);
  await expect(page.getByText("0 hosts allowed")).toBeVisible();
  await caption(page, "This panel — what this run can do — is the contract, settled before launch.");
  await beat(page, PACE.read);
  await spotlight(page, wsPicker);
  await caption(page, "Plus the workspace above: that directory, and nothing else.");
  await beat(page, PACE.read);
  await spotlight(page, null);
});

// ---------------------------------------------------------------------------
// Beat 3 — launch, and let it be a background run
// ---------------------------------------------------------------------------

test("V03 beat 3 — launch, walk away", async () => {
  test.setTimeout(RUN_FINISHES + 120_000);
  const page = stage();

  await act(
    page,
    page.getByRole("button", { name: "Launch run" }),
    "Launch. From here the envelope is set — only an approval could widen it.",
  );

  // The cockpit opens on the new run.
  await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{8,}/i, { timeout: 60_000 });
  await caption(page, "Launched. Unattended doesn't mean invisible — this page can watch its terminal the whole way.");
  await beat(page, PACE.read);

  // Running OR Completed — never Running alone. Take 1 died here: a
  // five-command script finishes in ~7 seconds, faster than the first poll of
  // a Running-only assertion, so the header said Completed while the driver
  // waited three minutes for a state the run had already left. The narration
  // is written to be true at either speed.
  await expect(page.getByText(/Running|Completed/).first()).toBeVisible({ timeout: SANDBOX_UP });
  await expect(page.getByText("Completed", { exact: true }).first()).toBeVisible({ timeout: RUN_FINISHES });
  await caption(page, "And this one is already done — a five-line script barely outlives its own sandbox.");
  await beat(page, PACE.read);

  // THE LOGS, on camera, at speed (owner note from take 1: the run must not
  // just sit there completed — show the work). The Overview's hero pane for a
  // finished background run IS the session replay; play it at 2× so the video
  // never waits on real time. Both clicks are the real product controls the
  // viewer has: the speed radio TerminalPlayer just grew, and asciinema's own
  // start overlay.
  const pane = page.getByText("run finished · replay").locator("..").locator("..");
  await pane.scrollIntoViewIfNeeded().catch(() => {});
  // Start playback FIRST — the player opens on a black, unstarted frame, and a
  // caption spoken before the click narrates emptiness (persona round 1).
  const player = page.locator(".ap-player, .ap-wrapper, .ap-terminal").first();
  await expect(player).toBeVisible({ timeout: 60_000 });
  await player.click().catch(() => {});
  await act(page, page.getByRole("radio", { name: "2x speed" }), "Here is everything it did — the run records its own terminal.");
  // Film mitigation for product finding #9 (exec runs speak "agent" elsewhere
  // on this page): the replay's own first line is the honest label — point at
  // it and say so. agent-run-lib.sh prints it before the command runs.
  await caption(page, "The first line is the honest label — a shell command, no agent harness. Same governance either way.");
  await beat(page, PACE.read);
  await caption(page, "Replayed at double speed: the listing, the line counts, the inventory being written.");
  // Hold for the cast itself (~7s at 1×, ~4s at 2×) plus a breath — the
  // whole point of the beat is that the WORK is on screen.
  await beat(page, PACE.read + 5000);
  await caption(page, "You could watch this live during a long run. After it, the replay is the log.");
  await beat(page, PACE.read + 400);
});

// ---------------------------------------------------------------------------
// Beat 4 — the receipts
// ---------------------------------------------------------------------------

test("V03 beat 4 — the receipts", async () => {
  test.setTimeout(180_000);
  const page = stage();

  // 1. The exit code, on the run's own header. Required, not best-effort:
  // take 1 proved the chip renders ("agent exit 0") and it is the headline
  // receipt of a background run.
  const exitChip = page.getByText(/exit 0/i).first();
  await expect(exitChip).toBeVisible({ timeout: 30_000 });
  await spotlight(page, exitChip);
  await caption(page, "Exit zero — the command's own verdict, promoted to the run's headline.");
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
  await caption(page, "You watched it print the file it wrote — and that path is the host's own disk, through the mount.");
  await beat(page, PACE.read);

  // 3. wardynd's own allow row — the run's ONE egress line despite "no
  // network": the sandbox's control channel back to Wardyn, not the internet.
  // The deny-silently curl above now gives the panel a deny row to sit beside
  // it, so the contrast (deny + allow, side by side) is the teaching.
  const egressPanel = page.getByRole("heading", { name: "Egress" }).locator("xpath=ancestor::section[1]");
  await spotlight(page, egressPanel);
  await caption(page, "One allow in the ledger — wardynd, the sandbox's control channel back to Wardyn. Not the internet.");
  await beat(page, PACE.read);
  await spotlight(page, null);

  // 4. The audit trail: create → exec → complete, nobody watching.
  await act(page, page.getByRole("tab", { name: "Audit" }));
  await caption(page, "The trail: run create, exec, complete — written as it happened, append-only.");
  await beat(page, PACE.read);
  await expect(page.getByText(/run\.create|Created the run/i).first()).toBeVisible({ timeout: 30_000 });
  await beat(page, PACE.read);
  // VERIFY at rehearsal: kernel.sensor.blind must actually read true for the
  // barrier this take's run used — reword or drop the line if it doesn't
  // (series ruling S4: don't outrun what the screen shows).
  await caption(page, "kernel.sensor.blind is Wardyn noting its kernel eye can't see this barrier — video twelve's subject.");
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
  await caption(page, "No internet was granted, one poke was denied silently — and it still left a file and a trail.");
  await beat(page, PACE.read);
  await caption(page, "Everything else in this series is the same shape with more inside the envelope.");
  await beat(page, PACE.read + 400);
  await caption(page, "Next: before an agent gets a real job — watch this boundary stop things, on purpose.");
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
