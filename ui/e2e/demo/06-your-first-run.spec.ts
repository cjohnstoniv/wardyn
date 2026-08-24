/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * Video 06 of the series — "Your first run".
 *
 * WHAT THIS FILMS. The simplest governed thing Wardyn can do — a plain shell
 * command, run unattended in the background against the workspace video 04
 * onboarded — governed by the POLICY WE SAVED IN EPISODE 05. That reuse is the
 * whole point: episode 05 wrote "first-policy" and named it; this run does not
 * author a new one, it REUSES that one BY REFERENCE. The video's job is to
 * teach the New Run form while nothing else is moving: what a run is, the three
 * kinds of task, how a saved policy attaches by name (nothing on the page is
 * merged into it), what Preflight promises before launch, and then the receipts
 * a finished run leaves behind — its exit code, the file it really wrote to the
 * host, and the audit trail from create to complete. Driving an agent is video
 * 07; autonomy with a human decision mid-run is video 08.
 *
 * THE 05 -> 06 PAYOFF. Episode 05 ends on the reuse-by-reference beat on this
 * same form; episode 06 opens by USING it. The narrative arc is "write the
 * rules once, reuse them for every run." So the OPENING is reworked from the
 * old inline-authoring choreography (template chips + a hand-edited spec doc)
 * into the "Reuse a saved policy" mode row (policy_id path) — and every old
 * inline-authoring line that went false under reuse is DIALOG-STALE-marked,
 * never reworded (see below).
 *
 * THE DIALOG. Owner lines that SURVIVE the rework are carried VERBATIM, in
 * order — do not reword; wording changes go through the owner. The NEW opening
 * (the reuse framing, "the policy from episode five") is drafted for the
 * owner's pen and marked [OWNER SLOT — drafted]. Lines the reuse made false
 * (the no-network framing, the inline first_use_approval mode-choice, the
 * "designed to fail" off-list beat) are DIALOG-STALE — pulled from the take,
 * flagged in local/episode-06-firstrun-proposal.md, never edited in place.
 * local/episode-06-stanza-check.py holds spec and proposal in lockstep. Short
 * stanzas ride BEAT_SHORT; full-length lines keep PACE.read.
 *
 * WHY AN OFFLINE COMMAND UNDER A 2-HOST POLICY. first-policy allows two hosts
 * (github.com, npmjs.org), holds everything else for review (deny_with_review),
 * and floors at CC2/Wall. This run's command needs NONE of that egress — it
 * takes stock of the workspace and writes the result to a file — so the policy
 * grants a modest envelope the command simply does not spend, and the receipts
 * show only Wardyn's own control channel. The command still produces a REAL
 * artifact (NOTES-INVENTORY.txt) in the writable workspace, so the take can
 * assert the byte actually landed on the host. Deliberately no off-list curl:
 * under deny_with_review an unlisted host would PARK a held approval into a
 * video that has not taught approvals yet (that is episode 10).
 *
 * STATE IT INHERITS. Video 04's slugify workspace, onboarded WRITABLE on camera
 * (the write grant is what lets this run's file reach the host), and video 05's
 * saved "first-policy". record-demo.sh --video 06 does not reset. The beforeAll
 * SEEDS first-policy off camera (POST /policies with 05's exact spec) so a take
 * shot independently of 05 still has a policy to reuse; if the workspace is
 * missing it fails loudly with the staging instruction instead of filming a
 * broken form. first-policy floors at CC2/Wall, so the filming host MUST be able
 * to build Wall — the launch beat 422s loudly on a host that cannot.
 *
 * OWNERSHIP OF NOUNS. This video launches one run titled RUN_TITLE (unique to
 * V06 — the board groups by title, and the verifier finds the run by it),
 * reuses the policy named POLICY_NAME ("first-policy", authored by video 05),
 * and writes one file, NOTES-INVENTORY.txt, inside the slugify workspace. It
 * touches no other series noun.
 *
 * This is NOT a test. It asserts only enough to keep itself honest and to know
 * when to advance; a failure here means the recording is wrong, not that the
 * product is broken. It runs against the REAL compose stack on :8080 with a
 * real sandbox.
 *
 * Driven by `scripts/record-demo.sh --video 06`, which globs this exact
 * filename and names the take wardyn-06-your-first-run-<stamp>.mp4
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

/** Unique title (DA5): the board groups by it and the verifier finds it.
 *  TWO-FILE CONTRACT: the default matches verify-demo-take.sh's
 *  WARDYN_DEMO_V06_TITLE fallback — change one, change both. */
const RUN_TITLE = process.env.WARDYN_DEMO_V06_TITLE || "Take inventory — first governed run";

/** The policy this run REUSES by reference — authored and saved by video 05.
 *  The reuse beat picks it by this exact name (matches 05's POLICY_NAME). */
const POLICY_NAME = "first-policy";

/**
 * 05's saved spec, verbatim (05-your-first-policy.spec.ts's POLICY_SPEC): CC2
 * floor (Wall), two allowed hosts, deny_with_review for everything else. The
 * beforeAll seeds a policy of this name+spec off camera so 06 is self-sufficient
 * when a take runs independently of 05's on-camera create.
 */
const POLICY_SPEC = {
  allowed_domains: ["github.com", "npmjs.org"],
  first_use_approval: "deny_with_review",
  min_confinement_class: "CC2",
  auto_stop_after_sec: 3600,
  eligible_grants: [],
} as const;

/** The artifact the run writes into the workspace — the proof a background
 *  run really touched the host. Deleted before the take so the diff is real. */
const ARTIFACT = "NOTES-INVENTORY.txt";

/**
 * The command. Offline on purpose — first-policy grants two hosts this command
 * never touches, and an off-list curl would PARK a held approval (the policy is
 * deny_with_review) into a video that has not taught approvals yet. It ends by
 * writing the artifact so the finished run has a diff. Multi-line: the console's
 * Shell-command field takes a script.
 */
const COMMAND = [
  "echo taking inventory of the workspace...",
  "ls -la",
  "wc -l src/*.js test/*.js",
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
 * Per-take hygiene + the preconditions. The slugify workspace must already
 * exist (video 04 creates it, writable); this video will not silently re-create
 * off camera what an earlier video teaches on camera. first-policy IS seeded off
 * camera (video 05 authors it on camera, but 06 must be self-sufficient for an
 * independent take). The artifact from a prior take is deleted so "Files
 * changed" shows a real new write.
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
    `no "${WORKSPACE_NAME}" workspace on this stack — video 06 runs against the one video 04 onboards. ` +
      `Shoot 04 first (scripts/record-demo.sh --video 04), or restage it off camera.`,
  ).toBe(true);
  fs.rmSync(path.join(WORKSPACE_PATH, ARTIFACT), { force: true });

  // Seed first-policy so the reuse beat has something to pick. Episode 05 saves
  // it ON CAMERA, but takes may run independently, so 06 must be self-sufficient:
  // clear any stale row of that name and POST a fresh one with 05's EXACT spec,
  // off camera, before the first beat. (Looked up by NAME in the reuse beat, so
  // a changed id across a re-seed is harmless.)
  const pol = await page.request.get("/api/v1/policies", { headers: apiHeaders() });
  expect(pol.ok(), `GET /api/v1/policies failed (${pol.status()}) — is the stack up on :8080?`).toBe(true);
  const polBody = await pol.json();
  const policies: { id?: string; name?: string }[] = Array.isArray(polBody) ? polBody : (polBody?.items ?? []);
  for (const p of policies) {
    if (p?.id && p.name === POLICY_NAME) {
      await page.request.delete(`/api/v1/policies/${p.id}`, { headers: apiHeaders() }).catch(() => {});
    }
  }
  const seeded = await page.request.post("/api/v1/policies", {
    headers: apiHeaders(),
    data: { name: POLICY_NAME, spec: POLICY_SPEC },
  });
  expect(
    seeded.ok(),
    `could not seed the "${POLICY_NAME}" policy (${seeded.status()}) — the reuse beat would have nothing to pick`,
  ).toBe(true);

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

test("V06 beat 1 — name it, aim it", async () => {
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
  // DIALOG-STALE: "No internet." — the reused first-policy grants two hosts
  // (github.com, npmjs.org), so the run's envelope is no longer no-network. The
  // COMMAND still touches nothing, but the line contradicts the policy the
  // viewer just watched being built in 05. Pulled, not reworded; see the
  // proposal. ("No agent. / Just a command." still lands as a pair.)
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
// Beat 2 — the workspace, the reused policy, the rail
// ---------------------------------------------------------------------------

test("V06 beat 2 — the envelope, by reference", async () => {
  test.setTimeout(180_000);
  const page = stage();

  // The workspace video 04 built. The combobox's option carries the name.
  const wsPicker = page.getByRole("combobox").filter({ hasText: /workspace|Ephemeral/i }).first();
  await act(page, wsPicker, "Attach the workspace we just created.");
  // DIALOG-STALE: "This is the blast radius we established in the last episode."
  // — the workspace is episode 04's; the LAST episode (05) established the
  // POLICY, not the blast radius. Off-by-one under the restructure. Pulled, not
  // reworded (see the proposal). "One directory. / Nothing else." still frame the
  // attached workspace.
  await caption(page, "One directory.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Nothing else.");
  await beat(page, BEAT_SHORT);
  await act(page, page.getByRole("option", { name: new RegExp(WORKSPACE_NAME) }).first(), "Choose the workspace.");

  // ── THE POLICY, BY REFERENCE (the 05 -> 06 payoff) ────────────────────
  //
  // Episode 05 authored and saved "first-policy"; this run REUSES it by
  // reference rather than authoring a fresh spec inline. "Reuse a saved policy"
  // is the Policy panel's mode row (policy-panel.tsx) — an aria-pressed <button>
  // whose accessible name is its title plus its hint, so prefix-match the title.
  // Switching to it HIDES the template chips and the spec-JSON textarea
  // (usingSaved renders only the picker), which is why every inline-authoring
  // locator the old beat drove — the spec box, the CI-baseline chip, the "No
  // egress" chip, the first_use_approval "Insert" rail row — is gone here, and
  // every inline-authoring line is DIALOG-STALE.
  //
  // DIALOG-STALE (the inline-authoring block, in original take order; all pulled,
  // none reworded — see local/episode-06-firstrun-proposal.md):
  //   · "Every rule for this run lives in one small spec — where it can go, what
  //     it can touch, and how much isolation it requires." (a drafted doc intro;
  //     no spec textarea on screen under reuse, and 05 is the spec episode)
  //   · "Short enough to read end to end before we launch." (same — no doc shown)
  //   · "And because this is a confined run, the network starts closed." and the
  //     "Network: none." CI-baseline chip act and "This command doesn't need the
  //     internet, so we're not giving it one." — FALSE: first-policy GRANTS two
  //     hosts (the command simply never uses them)
  //   · the four first_use_approval mode-choice lines ("…we decide what
  //     happens." / "We can hold it for approval." / "We can deny it, but let it
  //     ask." / "Or we can deny it silently.") — the stored policy already fixed
  //     the mode (deny_with_review); the run does not re-choose it
  await caption(page, "Last episode, we wrote our first policy and saved it as first-policy.");
  await beat(page, PACE.read);
  await caption(page, "This run doesn't need a new policy — it can reuse first-policy.");
  await beat(page, PACE.read);
  await act(page, page.getByRole("button", { name: /^Reuse a saved policy/ }), "Reuse a saved policy.");
  // The picker renders once this half is lit; the picks are silent (the rail
  // lines narrate the result). Same drive as 05's A4 reuse beat.
  await act(page, page.getByRole("combobox", { name: "Saved policy" }));
  await act(page, page.getByRole("option", { name: POLICY_NAME }));

  // The rail names the STORED policy, its floor and host count, and says outright
  // that nothing on this page is merged into it (new-run-screen.tsx). Scoped to
  // the rail (an <aside>) because the name also shows in the Select trigger.
  const rail = page.locator("aside").filter({ hasText: "What this run can do" });
  await expect(
    rail.getByText(POLICY_NAME, { exact: true }),
    `the rail does not name "${POLICY_NAME}" — the reuse pick never attached the saved policy`,
  ).toBeVisible();
  const railPolicy = rail.getByText(/^The stored spec governs this run/);
  await expect(railPolicy, "the rail isn't showing the reused policy by reference").toBeVisible();
  await centerInFrame(railPolicy);
  await spotlight(page, railPolicy);
  await caption(page, "There it is — first-policy, the one we just built.");
  await beat(page, PACE.read);
  await caption(page, "The run points to that saved policy. Its rules come from the policy itself.");
  await beat(page, PACE.read);
  await spotlight(page, null);

  // The barrier rides along as a SEPARATE per-run field (its own Seg, not part of
  // the stored spec). first-policy floors at CC2/Wall, so the form auto-clamps
  // the requested barrier UP to the floor and greys Fence beneath it — which is
  // exactly why "Same choices." is DIALOG-STALE: the original spec's OWN note
  // said a CC2 floor "would grey out Fence on the Barrier Seg and make 'Same
  // choices.' false." 05 already filmed the floor greying Fence, so this beat
  // only names the barrier riding along.
  await spotlight(page, page.getByRole("radiogroup", { name: "Barrier" }));
  await caption(page, "The sandbox barrier comes along with the run too.");
  await beat(page, PACE.read);
  await caption(page, "Same governance.");
  await beat(page, BEAT_SHORT);
  await spotlight(page, null);

  // PREFLIGHT — a dry run of the exact body Launch will send. It stays enabled
  // once a saved policy is picked (policy-panel.tsx: preflightDisabled only while
  // the saved lane has nothing selected). Clicked SILENTLY; the lines below
  // narrate the result, which renders beside Launch (new-run-screen.tsx) as the
  // enforced-barrier chip plus either clamp warnings or "No adjustments." — one
  // panel, NOT two columns (so the reuse re-draft names the chip, not a
  // side-by-side the UI never renders).
  await act(page, page.getByRole("button", { name: "Preflight" }));
  const preflight = page.getByTestId("preflight-result");
  await expect(
    preflight,
    "preflight returned nothing — the control plane refused the dry run, so the reused policy is not launchable here",
  ).toBeVisible({ timeout: 30_000 });
  await spotlight(page, preflight);
  await caption(page, "That's the important part of this screen.");
  await beat(page, PACE.read);
  await caption(page, "Before launch, we can see what this run is allowed to do.");
  await beat(page, PACE.read);
  // [OWNER SLOT — drafted] re-draft for reuse: under policy_id the result box
  // resolves the STORED spec and shows the enforced-barrier chip. Replaces the
  // old drafted "two things side by side…" line, which described a two-column UI
  // the panel never rendered.
  await caption(page, "It resolves that policy and shows the barrier this run will actually use.");
  await beat(page, PACE.read);
  // An operator's own saved spec is not clamped, so this reads "No adjustments."
  // Asserted BEFORE it is spoken — a stack that clamped something must not be
  // narrated as if it hadn't.
  await expect(
    preflight.getByText("No adjustments."),
    "preflight came back with adjustments — the policy that runs is not the stored policy",
  ).toBeVisible();
  // [OWNER SLOT — drafted] names what "No adjustments." says (carried from the
  // old drafted "They match. Nothing was added or widened when the policy was applied.", trimmed to
  // drop the "they match" two-column framing).
  await caption(page, "Nothing was added or widened when the policy was applied.");
  await beat(page, PACE.read);

  // DIALOG-STALE (the "designed to fail" off-list beat, in original take order;
  // all pulled, none reworded): "So let's run it — and have it reach for a host
  // that was never on the list." / "This one is designed to fail." / "The
  // request is outside the contract, so it gets denied." — the COMMAND no longer
  // curls an unlisted host (under deny_with_review that would park a held
  // approval, and 06 has not taught approvals), and deny_with_review is not a
  // silent deny anyway.

  // The envelope recap — re-pointed at the reused policy in the rail (its old
  // targets, the "No egress" chip and the first_use_approval rail row, exist only
  // in inline mode).
  await spotlight(page, page.getByRole("heading", { name: "Workspace", level: 3 }).locator("xpath=ancestor::section[1]"));
  await caption(page, "The workspace defines what it can touch.");
  await beat(page, PACE.read);
  await spotlight(page, railPolicy);
  await caption(page, "The network defines where it can go.");
  await beat(page, PACE.read);
  await caption(page, "And the policy defines what happens when it tries something else.");
  await beat(page, PACE.read);
  await spotlight(page, null);
  await caption(page, "That's the envelope.");
  await beat(page, PACE.read);
});

// ---------------------------------------------------------------------------
// Beat 3 — launch, and let it be a background run
// ---------------------------------------------------------------------------

test("V06 beat 3 — launch, walk away", async () => {
  test.setTimeout(RUN_FINISHES + 120_000);
  const page = stage();

  await act(page, page.getByRole("button", { name: "Launch run" }), "Launch.");

  // The cockpit opens on the new run.
  await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{8,}/i, { timeout: 60_000 });
  await caption(page, "Once it's launched, the envelope is set.");
  await beat(page, PACE.read);
  await caption(page, "Only a decision at the boundary can change what this run is allowed to do.");
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
  // 2x speed FIRST, silently: speed is a creation-time option in
  // terminal-player.tsx, so changing it REBUILDS the player unstarted.
  // Clicking it after playback began wiped a playing terminal back to the
  // black poster mid-beat (owner report, 2026-08-23: "plays instantly and
  // then goes away") — the order is the fix. The rebuilt player then plays
  // to the end and HOLDS its final frame through the closing captions,
  // which is the beat's whole point.
  await page.getByRole("radio", { name: "2x speed" }).click().catch(() => {});
  await beat(page, 600);
  await caption(page, "Open the terminal record.");
  await spotlight(page, player);
  await player.click().catch(() => {});
  await spotlight(page, null);
  await beat(page, PACE.afterClick);
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

test("V06 beat 4 — the receipts", async () => {
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
  // 04 doing its job. Asserted against the disk, not narrated over a widget:
  // a take where the write silently failed must die here, not ship. (The
  // replay itself already played in beat 3, in the Overview's hero pane —
  // the Files-changed widget stays out of this video: a 7-second sandbox is
  // torn down before it ever polls, so it is honestly empty here, and its
  // moment is video 07 where an agent works long enough to watch live.)
  expect(
    fs.existsSync(path.join(WORKSPACE_PATH, ARTIFACT)),
    `the run's ${ARTIFACT} never landed in ${WORKSPACE_PATH} — was the workspace onboarded writable?`,
  ).toBe(true);
  await caption(page, "And here's the file it created.");
  await beat(page, PACE.read);
  await caption(page, "That file is inside the workspace we mounted — not somewhere else on the host.");
  await beat(page, PACE.read);

  // 3. wardynd's own allow row — the run's ONE egress line even though the
  // command reached the internet not at all: the sandbox's control channel back
  // to Wardyn, not the open internet. first-policy allows two hosts, but the
  // command touches neither, so the panel shows only this internal entry — the
  // teaching that "allowed" and "reached" are different things.
  const egressPanel = page.getByRole("heading", { name: "Egress" }).locator("xpath=ancestor::section[1]");
  await spotlight(page, egressPanel);
  await caption(page, "And there's one internal network entry here.");
  await beat(page, PACE.read);
  await caption(page, "That's Wardyn's own control channel.");
  await beat(page, PACE.read);
  await caption(page, "Internal control traffic — not internet access.");
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
  // (series ruling S4: don't outrun what the screen shows). NOTE: first-policy
  // floors at CC2/Wall, so this run enforces AT LEAST Wall (higher than the
  // old CI-baseline CC1) — re-confirm the sensor is blind at that tier.
  await caption(page, "On this barrier, the kernel sensor can't give us a complete picture.");
  await beat(page, PACE.read);
  await caption(page, "We'll come back to that in episode twelve.");
  await beat(page, PACE.read);
});

// ---------------------------------------------------------------------------
// Conclusion
// ---------------------------------------------------------------------------

test("V06 conclusion", async () => {
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
  // DIALOG-STALE: "No internet was granted." and "An off-list request was
  // denied." — the reused first-policy GRANTS two hosts, and the command reaches
  // for none (no off-list curl), so neither claim is true of this run. Pulled,
  // not reworded; see the proposal.
  await caption(page, "The command still completed its work.");
  await beat(page, PACE.read);
  await caption(page, "And we have the receipts to prove what happened.");
  await beat(page, PACE.read);
  await caption(page, "Next, we'll put an actual agent inside one.");
  await beat(page, PACE.read);
  await caption(page, "And this time, we'll drive it ourselves.");
  await beat(page, PACE.read);
  await caption(page, "");
  await silentCard(page, "Next — 07: Interactive runs");
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
