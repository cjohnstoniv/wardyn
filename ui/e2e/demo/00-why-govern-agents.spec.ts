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

  // --- S1 · strangers' code, already ----------------------------------------
  await caption(page, "Start before agents — with code your machines already run every day.");
  await beat(page, PACE.read);
  await spotlight(page, page.locator("#t-dep"));
  await caption(page, "Every dependency install executes code strangers wrote — beside your credentials.");
  await beat(page, PACE.read);
  await spotlight(page, page.locator("#t-script"));
  await caption(page, "Build scripts and postinstall hooks run with whatever your shell can reach.");
  await beat(page, PACE.read);
  await spotlight(page, page.locator("#t-ci"));
  await caption(page, "And CI runs all of it unattended, with the deploy keys nearby.");
  await beat(page, PACE.read);
  await spotlight(page, null);
  await caption(page, "Supply-chain attacks live exactly there — one compromised package, reading your environment and shipping it out.");
  await beat(page, PACE.read);
  await caption(page, "That risk predates AI. Most teams simply never watch the traffic.");
  await beat(page, PACE.read);

  // --- S2 · agents raise the stakes ------------------------------------------
  await show(page, "s2");
  await caption(page, "An AI coding agent is that same exposure — with hands.");
  await beat(page, PACE.read);
  await spotlight(page, page.locator("#agent-caps"));
  await caption(page, "It runs commands, edits files, installs packages, calls APIs. Anything your shell can do, it can do.");
  await beat(page, PACE.read);
  await spotlight(page, null);
  await caption(page, "It is not malicious. It is eager, fast, and working unsupervised at two in the morning.");
  await beat(page, PACE.read);

  // --- S3 · the exhibit -------------------------------------------------------
  await show(page, "s3");
  await spotlight(page, page.locator("#exhibit"));
  await caption(page, "This is a real frame from later in this series — a coding agent, mid ordinary job.");
  await beat(page, PACE.read);
  await caption(page, "Its own tooling reached for a telemetry endpoint. Nobody asked for that — it is just what the tool does.");
  await beat(page, PACE.read);
  await spotlight(page, page.locator("#exhibit-quote"));
  await caption(page, "Caught at the door, held, and a human decided. Most setups never even see it leave.");
  await beat(page, PACE.read);
  await spotlight(page, null);
  await caption(page, "That is the uncomfortable truth: you cannot list what a tool will need up front — and you rarely know everything it does.");
  await beat(page, PACE.read);

  // --- S4 · trust it all, or block it all -------------------------------------
  await show(page, "s4");
  await caption(page, "So teams get offered two bad answers.");
  await beat(page, PACE.read);
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
  await caption(page, "A sandbox is a locked room: the work happens inside, and the room holds nothing worth stealing.");
  await beat(page, PACE.read);
  await spotlight(page, page.locator("#d-door"));
  await caption(page, "Egress is anything trying to leave — and it gets exactly one door.");
  await beat(page, PACE.read);
  await spotlight(page, page.locator("#d-proxy"));
  await caption(page, "A proxy is the checkpoint at that door. It stands outside the room — and it holds the keys, not the room.");
  await beat(page, PACE.read);
  await spotlight(page, page.locator("#d-rule"));
  await caption(page, "Deny by default. Decide the exceptions. Write every attempt down.");
  await beat(page, PACE.read);
  await spotlight(page, null);

  // --- S6 · observe, then decide ------------------------------------------------
  await show(page, "s6");
  await caption(page, "And because nobody can predict a tool's needs, you do not guess.");
  await beat(page, PACE.read);
  await spotlight(page, page.locator("#steps"));
  await caption(page, "Run the job once, watched. See every host it actually reached. Then decide — on evidence.");
  await beat(page, PACE.read);
  await spotlight(page, null);
  await caption(page, "One boundary, the same discipline — for your builds, your pipelines, and your agents.");
  await beat(page, PACE.read);

  // --- S7 · the series map --------------------------------------------------------
  await show(page, "s7");
  await caption(page, "The rest of this series stands that answer up for real — a tool called Wardyn, every claim proved on camera.");
  await beat(page, PACE.read);
  await spotlight(page, page.locator("#roadmap"));
  await caption(page, "Eleven more short episodes. Here is the map.");
  await beat(page, PACE.read + 800);
  await spotlight(page, page.locator("#next"));
  await caption(page, "Next: set up the host — one command to a governed machine, and the guardrails proved before an agent touches your code.");
  await beat(page, PACE.read + 400);
  await spotlight(page, null);
});
