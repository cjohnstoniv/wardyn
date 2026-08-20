/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * Video 00 of the series — "Why govern agents" (the primer).
 *
 * WHAT THIS FILMS. A slide deck, not the console: the problem statement the
 * whole series answers — strangers' code on your machines, agents raising the
 * stakes, one real caught-on-camera exhibit from later in the series, the
 * sandbox/egress/proxy vocabulary in plain language, observe-then-decide, and
 * the series map. Deliberately NOT Wardyn-specific until the last slide
 * (owner sign-off 2026-08-20: security-first, agentic AND non-agentic
 * workloads, the Datadog catch one exhibit among several).
 *
 * THE LANE. ui/e2e/demo/assets/primer.html over file://, driven by the same
 * demo project and overlay as every console take — the recording is the page,
 * the caption bar is spoken by the narrator, the ring points at slide
 * elements. The compose stack is NOT touched: no run, no workspace, no model.
 * Take with the default no-reset (record-demo.sh only resets for --video 01).
 *
 * NUMBERING. Interim 00 — the approved 12-episode renumber lands as one
 * mechanical commit later; this spec becomes 01 there and every old spec
 * shifts. 00 keeps today's take collision-free against the shipped 01–10.
 *
 * EXHIBIT PROVENANCE. assets/primer-datadog.png is a crop of the shipped V04
 * take (the EGRESS panel: allow rows for the model host, one pending row for
 * Claude Code's own telemetry endpoint) — unedited product pixels, which is
 * what the narration claims. If V04 is ever re-shot and the panel changes
 * shape, recrop rather than letting the exhibit drift from what later
 * episodes show.
 */

import { test, expect, type Page } from "@playwright/test";
import { beat, caption, chapter, PACE, spotlight } from "./overlay";
import { stage } from "./stage";

test.skip(!process.env.WARDYN_DEMO, "demo recording — run via `make record-demo` (exports WARDYN_DEMO=1)");

test.describe.configure({ mode: "serial" });

const DECK = new URL("./assets/primer.html", import.meta.url).href;

/** Switch the deck to one slide and let the fade settle before speaking. */
async function show(page: Page, id: string): Promise<void> {
  await page.evaluate((slide: string) => {
    (window as unknown as { __slide: (s: string) => void }).__slide(slide);
  }, id);
  await page.waitForTimeout(450);
}

/** Reveal a staged punchline at its spoken beat (deck starts them hidden). */
async function unhide(page: Page, id: string): Promise<void> {
  await page.evaluate((el: string) => {
    (window as unknown as { __unhide: (s: string) => void }).__unhide(el);
  }, id);
}

test("V00 — why govern agents (the primer)", async () => {
  test.setTimeout(10 * 60_000);
  const page = stage();
  await page.goto(DECK);
  await page.bringToFront();

  // Fail here rather than three minutes into a caption-less take (same guard
  // as V01): narration degrades to a no-op by design, so nothing downstream
  // would complain about an overlay that never installed.
  await expect
    .poll(() => page.evaluate(() => typeof (window as unknown as Record<string, unknown>).__demo), { timeout: 15_000 })
    .toBe("object");

  // The exhibit is the one external asset — a deck that renders without it
  // films a claim with a hole where the receipt goes.
  await expect
    .poll(() => page.locator("#exhibit").evaluate((el) => (el as HTMLImageElement).naturalWidth), {
      timeout: 15_000,
    })
    .toBeGreaterThan(0);
  await expect(page.locator("#roadmap div")).toHaveCount(11);

  await chapter(page, "Why govern agents", "Episode one — the problem, before any product");
  // The deck starts with every slide hidden so the chapter card opens on
  // black, not on a ghost of S1's text (persona round 1, Dana).
  await show(page, "s1");

  // --- S1 · strangers' code, already ----------------------------------------
  await caption(page, "Start before agents — with code your machines already run every day.");
  await beat(page, PACE.read);
  await spotlight(page, page.locator("#t-dep"));
  await caption(page, "Every dependency install executes code you never read — beside your credentials.");
  await beat(page, PACE.read);
  await spotlight(page, page.locator("#t-script"));
  await caption(page, "Build scripts and install hooks run with everything your shell can reach — the command line every program on your machine answers to.");
  await beat(page, PACE.read);
  await spotlight(page, page.locator("#t-ci"));
  await caption(page, "And CI — the robot that runs your team's checks — runs it all unattended, with the deploy keys nearby.");
  await beat(page, PACE.read);
  await unhide(page, "s1-supply");
  await spotlight(page, page.locator("#s1-supply"));
  await caption(page, "Supply-chain attacks live exactly here. event-stream — a package millions depended on, handed to a stranger who slipped in a wallet stealer.");
  await beat(page, PACE.read);
  await unhide(page, "s1-predates");
  await spotlight(page, page.locator("#s1-predates"));
  await caption(page, "That risk predates AI. Most teams simply never watch the traffic.");
  await beat(page, PACE.read);
  await spotlight(page, null);

  // --- S2 · agents raise the stakes ------------------------------------------
  await show(page, "s2");
  await caption(page, "An AI coding agent is that same exposure — with hands.");
  await beat(page, PACE.read);
  await spotlight(page, page.locator("#agent-caps"));
  await caption(page, "It runs commands, edits files, installs packages, calls APIs. Anything your shell can do, it can do.");
  await beat(page, PACE.read);
  await spotlight(page, page.locator("#s2-eager"));
  await caption(page, "It is not malicious. It is eager, fast, and working unsupervised at two in the morning.");
  await beat(page, PACE.read);
  await spotlight(page, null);

  // --- S3 · the exhibit -------------------------------------------------------
  await show(page, "s3");
  await spotlight(page, page.locator("#exhibit"));
  await caption(page, "This is a real frame from episode six of this series — a coding agent's egress panel: everything trying to LEAVE its box.");
  await beat(page, PACE.read);
  await spotlight(page, page.locator("#exhibit-pending"));
  await caption(page, "Its own harness — the CLI wrapped around the model — quietly reported usage back to its vendor. Nobody asked; it is just what the tool does.");
  await beat(page, PACE.read);
  await caption(page, "Harmless, today. A theft looks identical at this door — same row, but the cargo is your customer list. Episode five stops those.");
  await beat(page, PACE.read);
  await spotlight(page, page.locator("#exhibit-quote"));
  await caption(page, "This one was caught and held — parked until a human decides. Most setups never even see it leave.");
  await beat(page, PACE.read);
  // The forward-reference gets something to look at (Dana r3: the ring parked
  // stale on the callout while the voice left for episodes ten and twelve).
  await unhide(page, "exhibit-refs");
  await spotlight(page, page.locator("#exhibit-refs"));
  await caption(page, "Who decides, how far a yes reaches, and the record it all leaves — episodes ten and twelve.");
  await beat(page, PACE.read);
  // The truth line closes S3 (it summarizes the exhibit) and gets its own
  // band — every other hinge line earned one, and this was the last spot
  // where the screen went dead under an important sentence (Priya+Sam r4).
  await unhide(page, "s3-truth");
  await spotlight(page, page.locator("#s3-truth"));
  await caption(page, "That is the uncomfortable truth: you cannot list what a tool will need up front — and you rarely know everything it does.");
  await beat(page, PACE.read);
  await spotlight(page, null);

  // --- S4 · trust it all, or block it all -------------------------------------
  await show(page, "s4");
  await spotlight(page, page.locator("#card-trust"));
  await caption(page, "Trust it all — every script and agent works beside your keys, and the traffic goes unwatched.");
  await beat(page, PACE.read);
  await spotlight(page, page.locator("#card-block"));
  await caption(page, "Or block it all — the security team wins the argument, and the productivity never arrives.");
  await beat(page, PACE.read);
  await spotlight(page, null);

  // --- S5 · the vocabulary -----------------------------------------------------
  await show(page, "s5");
  await caption(page, "There is a third answer, and it starts with three old words.");
  await beat(page, PACE.read);
  await spotlight(page, page.locator("#d-room"));
  await caption(page, "A sandbox is a locked room: the work happens inside, and no keys live in the room — nothing for a thief to reuse.");
  await beat(page, PACE.read);
  await spotlight(page, page.locator("#d-door"));
  await caption(page, "Egress is anything trying to leave — and it gets exactly one door.");
  await beat(page, PACE.read);
  await spotlight(page, page.locator("#d-proxy"));
  await caption(page, "A proxy is the checkpoint at that door. It stands outside the room, it holds the keys — and it attaches them on the way out.");
  await beat(page, PACE.read);
  await caption(page, "So a thief can't take the key. What a run may do with it — that is the policy's job, episode eight.");
  await beat(page, PACE.read);
  await unhide(page, "d-rule");
  await spotlight(page, page.locator("#d-rule"));
  await caption(page, "Deny by default. Decide the exceptions. Write every attempt down.");
  await beat(page, PACE.read);
  await spotlight(page, null);

  // --- S6 · observe, then decide ------------------------------------------------
  // Three numbered cards, three beats — one blob ring over a numbered sequence
  // was Sam's "missed beat"; the loop is the product idea and earns its rhythm.
  await show(page, "s6");
  await spotlight(page, page.locator("#steps > .card").nth(0));
  await caption(page, "So don't guess. Run the job once, watched — the door deliberately opened for that one supervised run, on work you trust.");
  await beat(page, PACE.read);
  await spotlight(page, page.locator("#steps > .card").nth(1));
  await caption(page, "See every host it actually reached — evidence, not a wishlist.");
  await beat(page, PACE.read);
  await spotlight(page, page.locator("#steps > .card").nth(2));
  await caption(page, "Then decide: yes, no, or ask me — and it lands on the record.");
  await beat(page, PACE.read);
  await unhide(page, "s6-ep9");
  await spotlight(page, page.locator("#s6-ep9"));
  await caption(page, "Turning one watched run into a policy you keep — and keeping that first run safe — is episode nine.");
  await beat(page, PACE.read);
  await unhide(page, "s6-thesis");
  await spotlight(page, page.locator("#s6-thesis"));
  await caption(page, "One boundary, the same discipline — for your builds, your pipelines, and your agents.");
  await beat(page, PACE.read);
  await spotlight(page, null);

  // --- S7 · the series map --------------------------------------------------------
  await show(page, "s7");
  // Ring the footer that carries these exact words — the styled 02 tile read
  // as a ring while the voice said open-source/one-command (Dana r4).
  await spotlight(page, page.locator("#oss-line"));
  await caption(page, "The rest of this series stands that answer up for real — Wardyn: open source, one command on your own machine, every claim proved on camera.");
  await beat(page, PACE.read);
  await spotlight(page, page.locator("#roadmap"));
  await caption(page, "Eleven more episodes, about three minutes each. Here is the map.");
  await beat(page, PACE.read + 800);
  await spotlight(page, page.locator("#ep05"));
  await caption(page, "In a hurry? Episode five — what it stops — is the payoff to jump to.");
  await beat(page, PACE.read);
  await spotlight(page, page.locator("#next"));
  await caption(page, "Next: set up the host — one command to a governed machine, and the guardrails proved before an agent touches your code.");
  await beat(page, PACE.read + 400);
  await spotlight(page, null);
});
