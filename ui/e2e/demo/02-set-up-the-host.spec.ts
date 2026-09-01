/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * Video 02 of the series — "Set up the host".
 *
 * THE DIALOG IS THE OWNER'S, VERBATIM (rewrite of 2026-08-21, from
 * local/episode-02-script-v2.md as edited). Every SAY stanza is one caption;
 * wording changes go through that file. Short stanzas ride BEAT_SHORT.
 *
 * WHAT THIS FILMS. Act 1: the REAL `make setup` of this very take, replayed —
 * record-demo.sh --video 02 wraps Act 0's make setup in util-linux script(1),
 * converts it to an asciicast (scripts/cast-convert.py), and this spec plays
 * it in assets/setup-replay.html (vendored asciinema-player, file://-safe)
 * at a speed that lands ~40s. Honest by construction: it is this machine's
 * own install, sped up, nothing cut. Act 2: first light and the funnel's
 * three essentials — barrier, network, and the model/secrets step, with a
 * STAND-IN key typed on camera (masked; never submitted; the real connection
 * happened off camera through the same flow, which the narration owns).
 * Act 3: the recap, then the workspace list as the handoff plays.
 *
 * STAGING (off camera, before the take):
 *   1. FULL RESET + recorded make setup — record-demo.sh --video 02 does
 *      both (VIDEO 02 is the series' one resetting take) and writes
 *      setup.cast into this take's work dir. A --no-reset iteration reuses
 *      the previous cast; a missing cast fails act 1 loudly.
 *   2. SERIES SSH BRING-UP before make setup (episode 12 films SSH):
 *      export WARDYN_SSH_LISTEN=:2222 WARDYN_SSH_ADVERTISE=127.0.0.1:2222.
 *   3. MODEL PRE-CONNECTED off camera via token-stdin (record-demo.sh does
 *      it after setup) — B5 types a stand-in and says so; the Connected lane
 *      is asserted before the beat that relies on it.
 *   4. FRESH BROWSER PROFILE (stage.ts builds one per take).
 *
 * This is NOT a test. It asserts only enough to keep itself honest. Driven by
 * `scripts/record-demo.sh --video 02`; self-skips without WARDYN_DEMO=1.
 */

import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { test, expect, type Page } from "@playwright/test";
import { act, beat, caption, PACE, spotlight } from "./overlay";
import { stage } from "./stage";
import { advance } from "./funnel";

test.skip(!process.env.WARDYN_DEMO, "demo recording — run via `make record-demo` (exports WARDYN_DEMO=1)");

test.describe.configure({ mode: "serial" });

/** Hold for a SHORT stanza — the owner's staccato lines drag on PACE.read. */
const BEAT_SHORT = 1400;

/** Real containers: minutes, not seconds. Ceilings for the PRODUCT. */
const SANDBOX_UP = 180_000;

const REPLAY_PAGE = new URL("./assets/setup-replay.html", import.meta.url).href;
const WORK_DIR =
  process.env.WARDYN_DEMO_WORK_DIR ||
  path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../../test-results/demo-video-02");
const CAST = path.join(WORK_DIR, "setup.cast");

/** The stand-in key B5 types — obviously fake, masked on entry, never saved. */
const STAND_IN_KEY = "sk-demo-stand-in-key-never-real-0000";

/** Chapter card WITHOUT overlay.chapter()'s spoken title — the owner's script
 *  speaks its own lines over the card. */
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

// ---------------------------------------------------------------------------
// Act 1 — B0 chapter + B1 the install, replayed
// ---------------------------------------------------------------------------

test("V02 act 1 — the install, on camera", async () => {
  test.setTimeout(300_000);
  const page = stage();

  if (!fs.existsSync(CAST)) {
    throw new Error(
      `no ${CAST} — act 1 replays THIS take's own make setup. Record with ` +
        `scripts/record-demo.sh --video 02 (the resetting take captures the cast); ` +
        `a --no-reset iteration needs a cast left by an earlier one.`,
    );
  }
  const castText = fs.readFileSync(CAST, "utf8");

  await page.goto(REPLAY_PAGE);
  await page.bringToFront();
  await expect
    .poll(() => page.evaluate(() => typeof (window as unknown as Record<string, unknown>).__demo), { timeout: 15_000 })
    .toBe("object");

  // --- B0 · chapter ---------------------------------------------------------
  await silentChapter(page, "Set up the host", "Episode two — from a bare machine to a governed one");
  await caption(page, "In episode one, we asked why agents need to be governed.");
  await beat(page, PACE.read);
  await caption(page, "Now let's build the machine that governs them.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Starting with a machine that doesn't have Wardyn on it.");
  await beat(page, PACE.read);
  await clearChapter(page);

  // --- B1 · the install -----------------------------------------------------
  // Build the player paused at a speed that lands the whole cast in ~40s —
  // long enough for the five over-playback stanzas, short enough to hold.
  const info = (await page.evaluate(
    ([cast, target]) =>
      (window as unknown as { __loadCast: (c: string, t: number) => { durationS: number; speed: number } }).__loadCast(
        cast as string,
        target as number,
      ),
    [castText, 40],
  )) as { durationS: number; speed: number };

  await caption(page, "This is the real setup, on this machine.");
  await beat(page, PACE.read);
  await spotlight(page, page.locator("#cmd"));
  await caption(page, "Once the prerequisites are in place, it's one command.");
  await beat(page, BEAT_SHORT);
  await caption(page, "That's it.");
  await beat(page, BEAT_SHORT);
  await spotlight(page, page.locator("#pre"));
  await caption(page, "The machine needs Docker, the Wardyn repo, and a normal user account.");
  await beat(page, PACE.read);
  await spotlight(page, page.locator("#chip"));
  await caption(page, "Everything you're seeing here is a real shell recording. It's just sped up so you don't have to watch downloads in real time.");
  await beat(page, PACE.read);
  await spotlight(page, null);

  // Roll the replay; the install commentary plays over it.
  await page.evaluate(() => (window as unknown as { __play: () => void }).__play());
  await caption(page, "The command builds the sandbox images and starts Wardyn's control plane.");
  await beat(page, PACE.read);
  await caption(page, "And there's an important detail here.");
  await beat(page, BEAT_SHORT);
  await caption(page, "This is an install.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Which means we're running code we haven't personally inspected, just like we talked about in episode one.");
  await beat(page, PACE.read);
  await caption(page, "So this is the last step in the process that we're doing without the guardrails in place.");
  await beat(page, PACE.read);

  // Let the replay finish — the tail prints the console address.
  await expect
    .poll(() => page.evaluate(() => (window as unknown as { __castEnded: boolean }).__castEnded), {
      timeout: Math.ceil((info.durationS / info.speed) * 1000) + 90_000,
    })
    .toBe(true);

  await spotlight(page, page.locator("#playerbox"));
  await caption(page, "When it's finished, it gives us a local address for the console.");
  await beat(page, PACE.read);
  await caption(page, "Nothing fancy.");
  await beat(page, BEAT_SHORT);
  await caption(page, "It's running right here on this machine.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Let's open it.");
  await beat(page, BEAT_SHORT);
  await spotlight(page, null);
});

// ---------------------------------------------------------------------------
// Act 2 — B2 first light · B3 barrier · B4 network · B5 model · B6 secrets
// ---------------------------------------------------------------------------

test("V02 act 2 — first light through secrets", async () => {
  test.setTimeout(420_000);
  const page = stage();

  // --- B2 · first light -----------------------------------------------------
  await page.goto("/");
  const hero = page.getByRole("heading", { name: "Sandboxed. Governed. Self-hosted. Free.", level: 1 });
  if (!(await hero.isVisible().catch(() => false))) {
    await page.goto("/setup");
  }
  await expect(hero).toBeVisible({ timeout: 60_000 });

  await caption(page, "And there it is.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Wardyn.");
  await beat(page, BEAT_SHORT);
  await spotlight(page, hero);
  await caption(page, "The whole idea is right at the top:");
  await beat(page, BEAT_SHORT);
  await caption(page, "Sandboxed. Governed. Self-hosted. Free.");
  await beat(page, PACE.read);
  // The live host chips under the hero — the real SetupStatus, not a mock.
  await spotlight(page, page.getByText(/This host right now/i).locator("..").first());
  await caption(page, "These aren't example values from a brochure.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Wardyn is reading the environment we're actually running on.");
  await beat(page, PACE.read);
  await spotlight(page, null);
  await act(page, page.getByRole("button", { name: /^Get started/ }), "So let's see what it does with it.");
  // The step COUNT is dynamic now (the demos phase lists the whole catalog and
  // needsModel/needsSecret steps drop out per status) — pin only "Step 1 of".
  await expect(page.getByText(/^Step 1 of \d+$/)).toBeVisible({ timeout: 30_000 });

  // --- B3 · the barrier -----------------------------------------------------
  await expect(page.getByRole("heading", { name: "Pick your barrier", level: 2 })).toBeVisible({ timeout: 30_000 });
  await caption(page, "First, we need somewhere for the agent to work.");
  await beat(page, PACE.read);
  await caption(page, "Think back to episode one.");
  await beat(page, BEAT_SHORT);
  await caption(page, "We called that room a sandbox.");
  await beat(page, BEAT_SHORT);
  const tiers = page.getByRole("radiogroup", { name: "Barrier tier" });
  await spotlight(page, tiers);
  await caption(page, "Here, Wardyn gives us three levels of confinement for that room.");
  await beat(page, PACE.read);
  // P11a (dialog review, owner-ratified 2026-08-23): "barrier" is spoken six
  // times across later episodes and was never defined anywhere. This is the
  // one screen that can define it — the radiogroup on camera IS the choice.
  await caption(page, "That choice is what we'll call the barrier.");
  await beat(page, BEAT_SHORT);

  await spotlight(page, tiers.getByRole("radio", { name: /Fence/ }));
  await caption(page, "Fence is the simplest.");
  await beat(page, BEAT_SHORT);
  await caption(page, "It's a container, so the agent gets its own filesystem and environment, while sharing the host's kernel.");
  await beat(page, PACE.read);
  await caption(page, "It's fast.");
  await beat(page, BEAT_SHORT);
  await caption(page, "But that shared kernel is also the boundary.");
  await beat(page, PACE.read);

  await spotlight(page, tiers.getByRole("radio", { name: /Wall/ }));
  await caption(page, "Wall goes a step further.");
  await beat(page, BEAT_SHORT);
  await caption(page, "It uses gVisor to put a software layer between the workload and the host kernel.");
  await beat(page, PACE.read);
  await caption(page, "So what the agent interacts with isn't the real kernel underneath.");
  await beat(page, PACE.read);

  await spotlight(page, tiers.getByRole("radio", { name: /Vault/ }));
  await caption(page, "And Vault goes further again.");
  await beat(page, BEAT_SHORT);
  await caption(page, "It's a lightweight virtual machine with its own kernel.");
  await beat(page, PACE.read);
  await caption(page, "Think of it less like another room in the building...");
  await beat(page, BEAT_SHORT);
  await caption(page, "and more like a separate building.");
  await beat(page, BEAT_SHORT);
  await caption(page, "That's the strongest option, but it also costs more and depends on virtualization support from the machine.");
  await beat(page, PACE.read);

  // The permanent "Doesn't stop:" row — a real <tr> (environment-step.tsx).
  await spotlight(page, page.getByText("Doesn't stop:", { exact: true }).locator("xpath=ancestor::tr[1]"));
  await caption(page, "And here's a detail I really like about this screen.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Wardyn doesn't just tell you what each barrier protects.");
  await beat(page, BEAT_SHORT);
  await caption(page, "It tells you what it doesn't.");
  await beat(page, PACE.read);
  await caption(page, "Because no sandbox is magic.");
  await beat(page, BEAT_SHORT);
  await caption(page, "A boundary is only useful if you understand where that boundary actually is.");
  await beat(page, PACE.read);

  await spotlight(page, tiers);
  await caption(page, "Wardyn checks what this machine can actually support.");
  await beat(page, PACE.read);
  await caption(page, "On this machine, all three options are available.");
  await beat(page, BEAT_SHORT);
  await caption(page, "On another machine, one of them might be disabled.");
  await beat(page, BEAT_SHORT);
  await caption(page, "That's not a failure.");
  await beat(page, BEAT_SHORT);
  await caption(page, "It's the system telling you the truth about the hardware underneath it.");
  await beat(page, PACE.read);
  await spotlight(page, null);

  const fence = tiers.getByRole("radio", { name: /Fence/ });
  await expect(fence).toBeEnabled();
  await act(page, fence, "For this walkthrough, we'll use Fence.");
  await expect(fence).toHaveAttribute("aria-checked", "true");
  await caption(page, "And that's deliberate.");
  await beat(page, BEAT_SHORT);
  await caption(page, "The governance you're about to see works on the weakest of the three.");
  await beat(page, PACE.read);
  await caption(page, "We're not going to hide behind the strongest isolation option.");
  await beat(page, PACE.read);

  await advance();

  // --- B4 · network ---------------------------------------------------------
  await expect(page.getByRole("heading", { name: "Network", level: 2 })).toBeVisible({ timeout: 30_000 });
  await caption(page, "Next: the network.");
  await beat(page, BEAT_SHORT);
  await caption(page, "By default, the sandbox doesn't just get an open path to the internet.");
  await beat(page, PACE.read);
  await caption(page, "If a workload needs the network, we test that path first.");
  await beat(page, PACE.read);

  await act(page, page.getByRole("button", { name: "Test connectivity" }), "One click.");
  await caption(page, "The probe tells us whether this machine's network path actually works.");
  await beat(page, PACE.read);

  const reached = page.getByText(/^Reached · direct/).first();
  await expect(reached).toBeVisible({ timeout: SANDBOX_UP });
  await spotlight(page, reached);
  await caption(page, "Here, the host itself can reach the internet directly.");
  await beat(page, PACE.read);
  await caption(page, "That's the host's path, not the run's. A confined run still doesn't get open internet.");
  await beat(page, PACE.read);
  await caption(page, "So we know what a run will actually have available.");
  await beat(page, PACE.read);
  await spotlight(page, null);
  await caption(page, "And if the test fails, Wardyn doesn't quietly continue and hope for the best.");
  await beat(page, PACE.read);
  await caption(page, "It tells you why.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Maybe a port is blocked.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Maybe there's a corporate proxy in the way.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Maybe your network requires an internal mirror.");
  await beat(page, BEAT_SHORT);
  await caption(page, "The point is the same:");
  await beat(page, BEAT_SHORT);
  await caption(page, "prove the path before you rely on it.");
  await beat(page, PACE.read);

  // Owner note (2026-08-22): the take skipped straight past the step's two
  // sub-tabs — the surfaces that ANSWER the two hypotheticals above. Name the
  // Host proxy tab, then open Egress redirection and say what it does; the
  // token line is the field's own hint ("Injected proxy-side at fetch time —
  // the sandbox never holds it."). Visiting the tab also feeds the step's
  // gate proof (corpNetworkGate's egressVisited), never blocks it.
  await caption(page, "Those cases are handled right here.");
  await beat(page, BEAT_SHORT);
  await spotlight(page, page.getByRole("tab", { name: "Host proxy" }));
  await caption(page, "A corporate proxy goes in the Host proxy tab.");
  await beat(page, PACE.read);
  await act(page, page.getByRole("tab", { name: /Egress redirection/ }), "Egress redirection is the other tab.");
  await caption(page, "This is where a public endpoint gets mapped to your internal one.");
  await beat(page, PACE.read);
  await spotlight(page, page.getByLabel("From", { exact: true }).first());
  await caption(page, "A run reaches for the public name, and the proxy redirects it to yours.");
  await beat(page, PACE.read);
  await spotlight(page, page.getByLabel(/Token secret name/).first());
  await caption(page, "If your mirror needs a token, the proxy injects it at fetch time.");
  await beat(page, PACE.read);
  await caption(page, "The sandbox never holds it.");
  await beat(page, BEAT_SHORT + 400);
  await spotlight(page, null);

  await advance();

  // --- B5 · the model -------------------------------------------------------
  await expect(page.getByRole("heading", { name: "Secrets", level: 2 })).toBeVisible({ timeout: 30_000 });
  await caption(page, "Now we need a model.");
  await beat(page, BEAT_SHORT);
  const subLane = page.getByRole("radio", { name: /Claude subscription/ });
  await expect(subLane).toContainText("Connected", { timeout: 30_000 });
  await spotlight(page, subLane);
  await caption(page, "You can connect a Claude subscription, or provide a model API key.");
  await beat(page, PACE.read);

  // The stand-in: pick the API-key lane and TYPE a plainly-fake key into its
  // masked field — never saved, never submitted; the lane is flipped back to
  // the (still Connected) subscription after the beat. The narration owns the
  // stand-in in one clause (series rule S7); the real connection was made off
  // camera through this same flow before the take.
  const keyLane = page.getByRole("radio", { name: /API key/ });
  await act(page, keyLane, "For the recording, we're using a stand-in.");
  const keyField = page.locator('input[type="password"]').first();
  await expect(keyField, "the API-key lane's masked input — if this fails the lane's field markup changed").toBeVisible({
    timeout: 15_000,
  });
  await keyField.click();
  await page.keyboard.type(STAND_IN_KEY, { delay: 35 });
  await spotlight(page, keyField);
  await caption(page, "You can see the important part here.");
  await beat(page, BEAT_SHORT);
  await caption(page, "The key is masked as soon as it's entered.");
  await beat(page, PACE.read);
  await caption(page, "And the workload itself doesn't get handed the key.");
  await beat(page, PACE.read);
  await caption(page, "Wardyn keeps it — outside the workload.");
  await beat(page, BEAT_SHORT);
  await caption(page, "A run gets access when it needs it, rather than getting a copy to keep.");
  await beat(page, PACE.read);
  await spotlight(page, null);
  await caption(page, "That's the pattern we're going to keep seeing throughout Wardyn.");
  await beat(page, PACE.read);
  await caption(page, "Keep the valuable thing outside the workload.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Give the workload a controlled way to use it.");
  await beat(page, PACE.read);
  // UI-only revert: reselect the still-Connected subscription lane so the
  // funnel's saved state matches reality. No server write happened — the
  // stand-in was typed, never submitted.
  await subLane.click();
  await expect(subLane).toContainText("Connected");

  // --- B6 · secrets ---------------------------------------------------------
  await caption(page, "The same idea applies to other secrets.");
  await beat(page, PACE.read);
  await caption(page, "API keys.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Git credentials.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Tokens.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Anything a run might need, but shouldn't own.");
  await beat(page, PACE.read);

  await spotlight(page, page.getByText(/Only the GitHub App lane keeps its token/i).first());
  await caption(page, "Git is a good example.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Some credentials can be used without ever entering the sandbox.");
  await beat(page, PACE.read);
  await caption(page, "Others have to enter temporarily — for example, to perform a clone.");
  await beat(page, PACE.read);
  await caption(page, "In those cases, Wardyn tracks what happened and removes the credential afterward.");
  await beat(page, PACE.read);
  await caption(page, "And where possible, the credential stays outside the sandbox entirely.");
  await beat(page, PACE.read);
  await caption(page, "The run gets the capability it needs...");
  await beat(page, BEAT_SHORT);
  await caption(page, "not the secret behind that capability.");
  await beat(page, PACE.read);
  await caption(page, "That's a small distinction.");
  await beat(page, BEAT_SHORT);
  await caption(page, "But it's one of the most important ideas in the whole system.");
  await beat(page, PACE.read);
  await spotlight(page, null);
});

// ---------------------------------------------------------------------------
// Act 3 — B7 what you just saw
// ---------------------------------------------------------------------------

test("V02 act 3 — what you just saw", async () => {
  test.setTimeout(120_000);
  const page = stage();

  await silentChapter(page, "What you just saw", "A governed host, ready for real work");
  await caption(page, "So that's the host.");
  await beat(page, BEAT_SHORT);
  await caption(page, "We started with a bare machine.");
  await beat(page, BEAT_SHORT);
  await caption(page, "One command installed Wardyn.");
  await beat(page, BEAT_SHORT);
  await caption(page, "We chose a sandbox.");
  await beat(page, BEAT_SHORT);
  await caption(page, "We tested the network.");
  await beat(page, BEAT_SHORT);
  await caption(page, "We connected a model.");
  await beat(page, BEAT_SHORT);
  await caption(page, "And we set up the secrets the workloads may need.");
  await beat(page, PACE.read);
  await clearChapter(page);

  // "Then transition to the workspace view" (owner SCREEN note) — the closing
  // stanzas play over the page the next episode opens on.
  await page.goto("/workspaces");
  await expect(page.getByRole("heading", { name: "Workspaces", level: 1 })).toBeVisible({ timeout: 30_000 });
  await caption(page, "But notice what we didn't do.");
  await beat(page, BEAT_SHORT);
  await caption(page, "We didn't give an agent our keys and hope for the best.");
  await beat(page, PACE.read);
  await caption(page, "We built the boundaries first.");
  await beat(page, BEAT_SHORT);
  await caption(page, "That's the governed machine.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Next, we're going to stop being polite.");
  await beat(page, BEAT_SHORT);
  await caption(page, "We're going to see what the boundary actually stops.");
  await beat(page, PACE.read);

  await silentChapter(page, "Next — 03a: What it stops", "");
  await caption(page, "");
  await clearChapter(page);
});
