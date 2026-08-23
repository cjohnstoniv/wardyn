/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * Video 01 of the series — "Why govern agents" (the primer).
 *
 * WHAT THIS FILMS. A slide deck, not the console: the problem statement the
 * whole series answers. Deliberately not Wardyn-specific until the last slide.
 *
 * THE DIALOG IS THE OWNER'S, VERBATIM (rewrite of 2026-08-21, from
 * local/episode-01-script-current.md as edited). Every SAY stanza in that
 * script is one caption here — do not reword lines; wording changes go
 * through the script file and the owner. The staccato rhythm (many short
 * lines) is deliberate: short beats ride a tightened floor (BEAT_SHORT),
 * full-length lines keep PACE.read.
 *
 * THE LANE. ui/e2e/demo/assets/primer.html over file://, driven by the demo
 * project and overlay like any console take. The compose stack is NOT
 * touched. record-demo.sh treats --video 01 as STACKLESS (no reset, no
 * token, no workspace).
 *
 * EXHIBIT PROVENANCE. assets/primer-datadog.png is a crop of the episode-06
 * footage (the EGRESS panel) — unedited product pixels. If that episode is
 * re-shot and the panel changes, recrop rather than letting the exhibit
 * drift from what later episodes show.
 */

import { test, expect, type Page } from "@playwright/test";
import { beat, caption, PACE, spotlight } from "./overlay";
import { stage } from "./stage";

test.skip(!process.env.WARDYN_DEMO, "demo recording — run via `make record-demo` (exports WARDYN_DEMO=1)");

test.describe.configure({ mode: "serial" });

const DECK = new URL("./assets/primer.html", import.meta.url).href;

/** Hold for a SHORT stanza — the owner's staccato lines drag on PACE.read. */
const BEAT_SHORT = 1400;

/** Switch the deck to one slide and let the fade settle before speaking. */
async function show(page: Page, id: string): Promise<void> {
  await page.evaluate((slide: string) => {
    (window as unknown as { __slide: (s: string) => void }).__slide(slide);
  }, id);
  await page.waitForTimeout(450);
}

/** Reveal a staged element at its spoken beat (deck starts them hidden). */
async function unhide(page: Page, id: string): Promise<void> {
  await page.evaluate((el: string) => {
    (window as unknown as { __unhide: (s: string) => void }).__unhide(el);
  }, id);
}

/**
 * The chapter card WITHOUT overlay.chapter()'s spoken title — the owner's
 * script speaks its own two opening lines over the card instead.
 */
async function silentChapter(page: Page, title: string, sub: string): Promise<void> {
  await page
    .evaluate(
      ([t, s]) => {
        const d = (window as unknown as Record<string, Record<string, (...x: unknown[]) => void>>).__demo;
        d?.chapter?.(t, s);
      },
      [title, sub],
    )
    .catch(() => {});
}
async function clearChapter(page: Page): Promise<void> {
  await silentChapter(page, "", "");
  await page.waitForTimeout(400);
}

test("V01 — why govern agents (the primer)", async () => {
  test.setTimeout(12 * 60_000);
  const page = stage();
  await page.goto(DECK);
  await page.bringToFront();

  // Fail here rather than four minutes into a caption-less take: narration
  // degrades to a no-op by design, so nothing downstream would complain.
  await expect
    .poll(() => page.evaluate(() => typeof (window as unknown as Record<string, unknown>).__demo), { timeout: 15_000 })
    .toBe("object");
  await expect
    .poll(() => page.locator("#exhibit").evaluate((el) => (el as HTMLImageElement).naturalWidth), { timeout: 15_000 })
    .toBeGreaterThan(0);
  await expect(page.locator("#roadmap div")).toHaveCount(11);

  // --- B0 · chapter ----------------------------------------------------------
  await silentChapter(page, "Why govern agents", "Episode one — understanding the problem, before the solution");
  await caption(page, "Why govern agents?");
  await beat(page, BEAT_SHORT);
  await caption(page, "Before we get into the solution, let's start with the problem.");
  await beat(page, PACE.read);
  await clearChapter(page);
  await show(page, "s1");

  // --- S1 · the floor we all stand on ---------------------------------------
  await caption(page, "Let's start before AI.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Every day, our machines run code we didn't write.");
  await beat(page, PACE.read);
  await spotlight(page, page.locator("#t-dep"));
  await caption(page, "You install a package. An install script runs.");
  await beat(page, PACE.read);
  await spotlight(page, page.locator("#t-script"));
  await caption(page, "You build something. More scripts run.");
  await beat(page, PACE.read);
  await spotlight(page, page.locator("#t-ci"));
  await caption(page, "Your CI pipeline runs tests, builds releases, and sometimes deploys straight to production.");
  await beat(page, PACE.read);
  await spotlight(page, null);
  await caption(page, "And depending on how things are set up, that code can have access to the same machine, files, credentials, and network that your own software can reach.");
  await beat(page, PACE.read);
  await caption(page, "That isn't new.");
  await beat(page, BEAT_SHORT);
  await unhide(page, "s1-supply");
  await spotlight(page, page.locator("#s1-supply"));
  await caption(page, "Supply-chain attacks have been exploiting this for years.");
  await beat(page, PACE.read);
  await caption(page, "The difference is that most of us don't actually watch what that code is doing, especially code that isn't ours.");
  await beat(page, PACE.read);
  await caption(page, "So the security problem we're about to talk about?");
  await beat(page, BEAT_SHORT);
  await unhide(page, "s1-predates");
  await spotlight(page, page.locator("#s1-predates"));
  await caption(page, "It didn't start with AI.");
  await beat(page, BEAT_SHORT);
  await caption(page, "AI just gives it a new set of hands.");
  await beat(page, PACE.read);
  await spotlight(page, null);

  // --- S2 · now add hands -----------------------------------------------------
  await show(page, "s2");
  await caption(page, "Now give that same machine an AI coding agent.");
  await beat(page, PACE.read);
  await spotlight(page, page.locator("#agent-caps > div").nth(0));
  await caption(page, "It can run commands.");
  await beat(page, BEAT_SHORT);
  await spotlight(page, page.locator("#agent-caps > div").nth(1));
  await caption(page, "It can edit files.");
  await beat(page, BEAT_SHORT);
  await spotlight(page, page.locator("#agent-caps > div").nth(2));
  await caption(page, "It can install packages.");
  await beat(page, BEAT_SHORT);
  await spotlight(page, page.locator("#agent-caps > div").nth(3));
  await caption(page, "It can call APIs.");
  await beat(page, BEAT_SHORT);
  await unhide(page, "s2-access");
  await spotlight(page, page.locator("#s2-access"));
  await caption(page, "And most importantly, it often does those things as you.");
  await beat(page, PACE.read);
  await caption(page, "So if you can read a file, the agent may be able to read it too.");
  await beat(page, PACE.read);
  await caption(page, "If your environment has access to a credential, an API, or an internal service, the agent may be able to reach or use it too.");
  await beat(page, PACE.read);
  await spotlight(page, null);
  await caption(page, "The agent doesn't have to be malicious.");
  await beat(page, BEAT_SHORT);
  await caption(page, "That's actually the important part.");
  await beat(page, BEAT_SHORT);
  await caption(page, "It can be completely well-intentioned.");
  await beat(page, BEAT_SHORT);
  await caption(page, "It's just fast, capable, and operating on instructions it doesn't always understand the way we do.");
  await beat(page, PACE.read);
  await caption(page, "And sometimes those instructions aren't even coming from you.");
  await beat(page, PACE.read);
  await caption(page, "A dependency can contain them.");
  await beat(page, BEAT_SHORT);
  await caption(page, "A web page can contain them.");
  await beat(page, BEAT_SHORT);
  await caption(page, "A file the agent reads can contain them.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Those instructions can come from places you never meant to trust.");
  await beat(page, PACE.read);
  await unhide(page, "s2-pi");
  await spotlight(page, page.locator("#s2-pi"));
  await caption(page, "That's a prompt injection.");
  await beat(page, BEAT_SHORT);
  await caption(page, "And suddenly, those same hands are following someone else's instructions.");
  await beat(page, PACE.read);
  await spotlight(page, null);

  // --- S3 · the exhibit -------------------------------------------------------
  await show(page, "s3");
  await spotlight(page, page.locator("#exhibit"));
  await caption(page, "Here's a real frame from this series.");
  await beat(page, BEAT_SHORT);
  await caption(page, "This is an AI coding agent doing an ordinary job.");
  await beat(page, PACE.read);
  await caption(page, "And this panel shows something easy to miss:");
  await beat(page, BEAT_SHORT);
  await caption(page, "traffic trying to leave the machine.");
  await beat(page, PACE.read);
  await spotlight(page, page.locator("#exhibit-pending"));
  await caption(page, "The agent's own tooling was sending usage information to a third-party telemetry service.");
  await beat(page, PACE.read);
  await caption(page, "Nothing dramatic happened.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Nobody was attacking the system.");
  await beat(page, BEAT_SHORT);
  await caption(page, "The tool was simply doing what it was designed to do.");
  await beat(page, PACE.read);
  await caption(page, "And that's exactly why this matters.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Because at the network boundary, an ordinary telemetry request and a malicious request can look remarkably similar.");
  await beat(page, PACE.read);
  await caption(page, "They're both traffic leaving the machine.");
  await beat(page, PACE.read);
  await caption(page, "The difference is what you're allowing through the door.");
  await beat(page, PACE.read);
  await caption(page, "In this case, the destination wasn't on the allowed list.");
  await beat(page, PACE.read);
  await spotlight(page, page.locator("#exhibit-quote"));
  await caption(page, "So instead of silently letting it through, the request was held.");
  await beat(page, PACE.read);
  await caption(page, "Now a human can decide.");
  await beat(page, BEAT_SHORT);
  await caption(page, "And that's the real problem we're trying to solve:");
  await beat(page, BEAT_SHORT);
  await caption(page, "How do you give an agent enough access to be useful...");
  await beat(page, BEAT_SHORT);
  await caption(page, "without giving it a blank check?");
  await beat(page, PACE.read);
  await caption(page, "And how do you do that without asking someone to approve every single request?");
  await beat(page, PACE.read);
  await unhide(page, "exhibit-refs");
  await spotlight(page, page.locator("#exhibit-refs"));
  await caption(page, "We'll come back to those questions later in the series.");
  await beat(page, PACE.read);
  await caption(page, "For now, just remember this:");
  await beat(page, BEAT_SHORT);
  await unhide(page, "s3-truth");
  await spotlight(page, page.locator("#s3-truth"));
  await caption(page, "You can't always know in advance what a tool is going to need.");
  await beat(page, PACE.read);
  await caption(page, "And you can't always know everything it's going to do.");
  await beat(page, PACE.read);
  await spotlight(page, null);

  // --- S4 · the usual answers -------------------------------------------------
  await show(page, "s4");
  await caption(page, "There are two easy answers.");
  await beat(page, BEAT_SHORT);
  await unhide(page, "card-trust");
  await spotlight(page, page.locator("#card-trust"));
  await caption(page, "Trust it.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Give the agent access to everything and hope it behaves.");
  await beat(page, PACE.read);
  await unhide(page, "card-block");
  await spotlight(page, page.locator("#card-block"));
  await caption(page, "Or block it.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Lock everything down so tightly that the agent can't really do anything useful.");
  await beat(page, PACE.read);
  await spotlight(page, null);
  await caption(page, "Neither is a great answer.");
  await beat(page, BEAT_SHORT);
  await caption(page, "We need something in between.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Something that lets the agent work...");
  await beat(page, BEAT_SHORT);
  await caption(page, "while keeping the important boundaries under our control.");
  await beat(page, PACE.read);

  // --- S5 · the vocabulary -----------------------------------------------------
  await show(page, "s5");
  await caption(page, "That brings us to a few simple ideas.");
  await beat(page, PACE.read);
  await spotlight(page, page.locator("#d-room"));
  await caption(page, "First: the sandbox.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Think of it as a locked room.");
  await beat(page, BEAT_SHORT);
  await caption(page, "The agent can do its work inside, but the important keys don't live in the room.");
  await beat(page, PACE.read);
  await spotlight(page, page.locator("#d-door"));
  await caption(page, "Then there's egress.");
  await beat(page, BEAT_SHORT);
  await caption(page, "That's simply the stuff trying to leave.");
  await beat(page, BEAT_SHORT);
  await caption(page, "And we want that traffic going through one controlled door.");
  await beat(page, PACE.read);
  await spotlight(page, page.locator("#d-proxy"));
  await caption(page, "That door is the proxy.");
  await beat(page, BEAT_SHORT);
  await caption(page, "The proxy sits outside the sandbox and controls access to the things the agent needs.");
  await beat(page, PACE.read);
  await caption(page, "So the agent doesn't get handed the keys.");
  await beat(page, BEAT_SHORT);
  await caption(page, "It gets a controlled path to use them.");
  await beat(page, PACE.read);
  await unhide(page, "d-rule");
  await spotlight(page, page.locator("#d-rule"));
  await caption(page, "And the starting point is simple:");
  await beat(page, BEAT_SHORT);
  await caption(page, "Deny by default.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Allow what we understand.");
  await beat(page, BEAT_SHORT);
  await caption(page, "And keep a record of what happened.");
  await beat(page, PACE.read);
  await spotlight(page, null);

  // --- S6 · govern without guessing ---------------------------------------------
  await show(page, "s6");
  await caption(page, "But there's one more important idea.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Don't guess what the agent needs.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Watch it.");
  await beat(page, BEAT_SHORT);
  await spotlight(page, page.locator("#steps > .card").nth(0));
  await caption(page, "Run the job once in a controlled environment.");
  await beat(page, PACE.read);
  await caption(page, "Let it do the work.");
  await beat(page, BEAT_SHORT);
  await spotlight(page, page.locator("#steps > .card").nth(1));
  await caption(page, "Then look at what it actually reached.");
  await beat(page, PACE.read);
  await caption(page, "Which hosts?");
  await beat(page, BEAT_SHORT);
  await caption(page, "Which services?");
  await beat(page, BEAT_SHORT);
  await caption(page, "Which destinations?");
  await beat(page, BEAT_SHORT);
  await caption(page, "Now you have evidence.");
  await beat(page, PACE.read);
  await spotlight(page, page.locator("#steps > .card").nth(2));
  await caption(page, "And that evidence gives you something much better than a guess:");
  await beat(page, BEAT_SHORT);
  await caption(page, "a policy.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Allow it.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Deny it.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Or ask for approval.");
  await beat(page, BEAT_SHORT);
  await caption(page, "And keep that decision on the record.");
  await beat(page, PACE.read);
  await unhide(page, "s6-ep9");
  await spotlight(page, page.locator("#s6-ep9"));
  await caption(page, "That's how one safe, watched run becomes a policy you can actually keep.");
  await beat(page, PACE.read);
  await spotlight(page, null);

  // --- S7 · what's ahead ----------------------------------------------------------
  await show(page, "s7");
  await caption(page, "That's the idea behind the rest of this series.");
  await beat(page, BEAT_SHORT);
  await caption(page, "We're going to take that simple principle...");
  await beat(page, BEAT_SHORT);
  await caption(page, "and actually build it.");
  await beat(page, PACE.read);
  await caption(page, "This is Wardyn.");
  await beat(page, BEAT_SHORT);
  await spotlight(page, page.locator("#oss-line"));
  await caption(page, "It's open source, runs on your own machine, and we're going to prove each part on camera.");
  await beat(page, PACE.read);
  await spotlight(page, page.locator("#roadmap"));
  await caption(page, "There are eleven more episodes.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Each one tackles a piece of the problem.");
  await beat(page, PACE.read);
  await spotlight(page, page.locator("#ep03"));
  await caption(page, "If you're in a hurry, jump to episode three.");
  await beat(page, BEAT_SHORT);
  await caption(page, "That's where we get into what the system actually stops.");
  await beat(page, PACE.read);
  await spotlight(page, page.locator("#next"));
  await caption(page, "But next, we start at the beginning:");
  await beat(page, BEAT_SHORT);
  await caption(page, "setting up the host.");
  await beat(page, BEAT_SHORT);
  await caption(page, "One command.");
  await beat(page, BEAT_SHORT);
  await caption(page, "A governed machine.");
  await beat(page, BEAT_SHORT);
  await caption(page, "And guardrails in place before an agent ever touches your code.");
  await beat(page, PACE.read + 400);
  await spotlight(page, null);
});
