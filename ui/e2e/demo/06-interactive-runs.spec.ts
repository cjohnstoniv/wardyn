/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * Video 06 of the series — "Interactive runs".
 *
 * WHAT THIS FILMS. Video 03's run needed nobody; this one is the opposite: an
 * interactive agent run you drive by hand. The take launches it through the
 * same New Run form (only the choices that change get airtime), lands in the
 * sandbox's own shell over the cockpit terminal, and then earns the series'
 * central credential claim FROM INSIDE THE BOX: the environment holds no API
 * key, the credentials file Claude Code reads carries a token that names
 * itself an inert sentinel, and a real `claude -p` call still gets a real
 * answer — because the proxy strips what the sandbox sent and attaches the
 * live token at the boundary, outside the box. The injection lands in the
 * run's own audit trail, where the take reads it. One video, both stories:
 * driving an agent by hand, and the key it never holds.
 *
 * WHERE THE CODE CAME FROM. Beats 2-4 are the retired model-access spec's
 * proven choreography (git history: 08-model-access.spec.ts), moved beat for
 * beat — including its hard-won assertions (the word-bounded colour regex, the
 * "no ANTHROPIC_API_KEY" absence check made only after output demonstrably
 * landed, the full-token match against harnesscred.go's constant). What
 * changed is the frame: that spec examined a pre-staged run; this one launches
 * its run on camera, which is the video's actual subject.
 *
 * THE QUOTA RULE. Beat 4's `claude -p` is a real, quota-consuming Anthropic
 * call with deliberately NO fallback — a take where the model call fails films
 * the opposite of the video's claim. Every check that can fail is therefore
 * made BEFORE that beat: the managed lane is asserted in beforeAll, the
 * sentinel is asserted on screen in beat 3, and the injection event is
 * asserted in the audit BEFORE the model call (it fires at sandbox startup,
 * when the credential is minted — never on traffic).
 *
 * STAGING THE FORM GETS RIGHT ON CAMERA (each one was a take-killer for the
 * retired spec's operators):
 *   - Run mode INTERACTIVE, start with "Terminal — a shell in the workspace
 *     dir". The wizard's default start is the agent itself, and that attach
 *     lands INSIDE Claude Code's TUI — beats 2-4 would type shell commands
 *     into a prompt box. The Terminal choice is the supported shape, and
 *     picking it on camera teaches that the choice exists.
 *   - Network preset "Just the model provider" (the form's own default): the
 *     managed credential only injects when the policy can reach
 *     api.anthropic.com, and an api-key grant would pre-empt the managed
 *     fallback silently. This form adds no grants and allows exactly that host.
 *
 * STATE IT INHERITS. Videos 01-03's stack: managed subscription connected
 * (asserted, not assumed), slugify workspace onboarded writable. No reset.
 *
 * OWNERSHIP OF NOUNS. One run titled RUN_TITLE; no files written; no series
 * noun touched beyond attaching the shared slugify workspace.
 *
 * This is NOT a test. It asserts only enough to keep itself honest and to
 * know when to advance. It runs against the REAL compose stack on :8080.
 *
 * Driven by `scripts/record-demo.sh --video 06`, which globs this exact
 * filename and names the take wardyn-04-interactive-runs-<stamp>.mp4
 * (docs/README.md links that asset name — do not rename this file). It
 * self-skips without WARDYN_DEMO=1.
 */

import { test, expect, type Page } from "@playwright/test";
import { WORKSPACE_NAME } from "./task";
import { act, beat, caption, centerInFrame, chapter, PACE, spotlight, typeInTerminal } from "./overlay";
import { decide } from "./funnel";
import { sweepStaleState } from "./sweep";
// stage.ts is the rig: importing it registers this file's beforeAll/afterAll.
import { stage } from "./stage";

test.skip(!process.env.WARDYN_DEMO, "demo recording — run via `make record-demo` (exports WARDYN_DEMO=1)");

test.describe.configure({ mode: "serial" });

// ---------------------------------------------------------------------------
// This video's nouns and ceilings.
// ---------------------------------------------------------------------------

/** Unique title (DA5): the board groups by it and the verifier finds it. */
const RUN_TITLE = process.env.WARDYN_DEMO_V04_TITLE || "Drive it yourself — interactive agent";

/** The full sentinel token, byte for byte (harnesscred.go's
 *  managedSentinelAccessToken). A Go constant, so a full match is the honest
 *  assertion — a prefix would let a changed spelling through while the
 *  narration says "names itself an inert sentinel". */
const SENTINEL = "sk-ant-oat01-wardyn-inert-sentinel-proxy-injects-the-live-token";

/** The injection event beat 5 reads (audit.tsx gives it a verb now). */
const INJECT_ACTION = "run.llm.subscription_inject";

/** The owner's staccato lines read fast; PACE.read after one is dead air. */
const BEAT_SHORT = 1400;

// Product ceilings — pacing comes from overlay.ts, never from these.
const SANDBOX_UP = 240_000;
const COMMAND_ECHOES = 60_000;
const MODEL_ANSWERS = 180_000;

function apiHeaders(): Record<string, string> | undefined {
  return process.env.WARDYN_DEMO_TOKEN ? { Authorization: `Bearer ${process.env.WARDYN_DEMO_TOKEN}` } : undefined;
}

async function apiGet<T>(page: Page, path: string): Promise<T | null> {
  const res = await page.request.get(path, { headers: apiHeaders() }).catch(() => null);
  if (!res?.ok()) return null;
  return (await res.json().catch(() => null)) as T | null;
}

function asList<T>(body: unknown): T[] {
  if (Array.isArray(body)) return body as T[];
  const b = body as Record<string, unknown> | null;
  return (b?.items ?? b?.events ?? b?.runs ?? []) as T[];
}

/** Ring one terminal ROW (the line the narration points at), or the screen. */
async function spotlightTerminalRow(page: Page, needle: string): Promise<void> {
  const screen = page.locator(".xterm-screen").first();
  const row = screen.locator(".xterm-rows > div").filter({ hasText: needle }).first();
  await spotlight(page, (await row.count().catch(() => 0)) ? row : screen);
}

// ---------------------------------------------------------------------------
// Preconditions — everything that can fail is checked BEFORE the quota beat.
// ---------------------------------------------------------------------------

test.beforeAll(async () => {
  if (!process.env.WARDYN_DEMO) return;
  const page = stage();

  // (1) The MANAGED subscription lane must be connected — beat 3's sentinel
  // and beat 5's injection event exist on no other lane. setup/status's
  // secrets.present carries the managed marker via the harness-cred store.
  const status = await apiGet<{ llm?: { subscription?: boolean }; secrets?: { present?: string[] } }>(
    page,
    "/api/v1/setup/status",
  );
  const present = status?.secrets?.present ?? [];
  expect(
    status !== null,
    "GET /api/v1/setup/status failed — is the stack up on :8080?",
  ).toBe(true);

  // (2) NO stored anthropic-api-key: it would pre-empt the managed fallback
  // silently and beat 3 would cat a file that never materialized. (Video 02's
  // secret beat deliberately uses a neutral name for exactly this reason.)
  expect(
    !present.includes("anthropic-api-key"),
    "an anthropic-api-key secret is stored on this stack — it pre-empts the managed subscription lane, " +
      "and beats 2-5 film that lane. Delete the secret (Secrets page) before this take.",
  ).toBe(true);

  // (3) The workspace the run attaches.
  const ws = asList<{ name?: string }>(await apiGet(page, "/api/v1/workspaces"));
  expect(
    ws.some((w) => w.name === WORKSPACE_NAME),
    `no "${WORKSPACE_NAME}" workspace — videos 02/03 stage it. Shoot them first, or restage off camera.`,
  ).toBe(true);

  // (4) Quiet chrome: clear pending approvals an earlier take left undecided.
  const pending = asList<{ id?: string }>(await apiGet(page, "/api/v1/approvals?state=PENDING"));
  for (const ap of pending) {
    if (ap?.id) {
      await page.request
        .post(`/api/v1/approvals/${ap.id}/deny`, {
          headers: apiHeaders(),
          data: { reason: "stale demo approval from an earlier take — cleared off camera" },
        })
        .catch(() => {});
    }
  }

  // (5) S6: also kill any run still squatting on slugify from an earlier
  // take — the source of the "workspace already in use by 1 active run(s) —
  // proceeding anyway" toast firing mid-take.
  await sweepStaleState(["slugify"]);
});

// ---------------------------------------------------------------------------
// Beat 1 — the form again, but only the choices that change
// ---------------------------------------------------------------------------

test("V06 beat 1 — an agent, and a hand on the wheel", async () => {
  test.setTimeout(180_000);
  const page = stage();
  await page.goto("/runs/new");
  await page.bringToFront();

  await expect
    .poll(() => page.evaluate(() => typeof (window as unknown as Record<string, unknown>).__demo), { timeout: 15_000 })
    .toBe("object");
  await expect(page.getByRole("heading", { name: "New run", level: 1 })).toBeVisible({ timeout: 30_000 });

  await chapter(page, "Interactive runs", "Drive the agent yourself — inside the boundary");
  await caption(page, "So far, we've watched commands run on their own.");
  await beat(page, PACE.read);
  await caption(page, "Now let's put ourselves inside the loop.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Same kind of run.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Same boundaries.");
  await beat(page, BEAT_SHORT);
  await caption(page, "But this time, you have the keyboard.");
  await beat(page, BEAT_SHORT + 400);

  const title = page.getByLabel("Title");
  await title.fill(RUN_TITLE);

  // Agent task this time — and Interactive, which is the video.
  await act(page, page.getByRole("radio", { name: /Agent task/ }), "Agent task.");
  await caption(page, "This is a Claude Code task.");
  await beat(page, BEAT_SHORT);
  await act(page, page.getByRole("radio", { name: /^Interactive/ }), "Interactive.");
  await caption(page, "Interactive means the sandbox starts up and gives us a terminal.");
  await beat(page, PACE.read);
  await caption(page, "We're the ones driving.");
  await beat(page, BEAT_SHORT);

  // Start in a SHELL, not in the agent's own TUI. The default drops you
  // inside Claude Code's prompt box; the shell start is what lets a human
  // inspect the box and launch the agent themselves — which is this video.
  // No owner line covers this click — the choreography still needs it (the
  // agent's own TUI is the wrong start for a hand-driven shell) — so it plays
  // silent rather than inventing dialog (see the report).
  const startWith = page.getByRole("radiogroup", { name: "Start with" });
  await startWith.scrollIntoViewIfNeeded().catch(() => {});
  await act(page, startWith.getByRole("radio", { name: /^Terminal/ }));

  // The workspace, and the envelope: exactly one host, the model's.
  const wsPicker = page.getByRole("combobox").filter({ hasText: /workspace|Ephemeral/i }).first();
  await act(page, wsPicker, "Attach the workspace.");
  await act(page, page.getByRole("option", { name: new RegExp(WORKSPACE_NAME) }).first());
  await caption(page, "Same workspace as before.");
  await beat(page, BEAT_SHORT);
  await caption(page, "And notice something important:");
  await beat(page, BEAT_SHORT);
  await caption(page, "changing the mode doesn't change the blast radius.");
  await beat(page, PACE.read);
  await caption(page, "The agent still gets the same workspace.");
  await beat(page, BEAT_SHORT + 400);

  await act(page, page.getByRole("radio", { name: /^Confined/ }), "Confined.");
  await caption(page, "And the network is still default-deny.");
  await beat(page, PACE.read);
  await caption(page, "The model needs one destination, so we'll allow one.");
  await beat(page, PACE.read);
  await act(page, page.getByRole("radio", { name: /^Just the model provider/ }), "Add api.anthropic.com.");
  await caption(page, "That's the entire network contract for this run.");
  await beat(page, PACE.read);

  await act(page, page.getByRole("button", { name: "Launch run" }), "Launch.");
  await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{8,}/i, { timeout: 60_000 });
});

// ---------------------------------------------------------------------------
// Beat 2 — the terminal is the run
// ---------------------------------------------------------------------------

test("V06 beat 2 — inside the box", async () => {
  test.setTimeout(SANDBOX_UP + 120_000);
  const page = stage();

  await caption(page, "This terminal isn't a video of the sandbox.");
  const screen = page.locator(".xterm-screen").first();
  await expect(screen).toBeVisible({ timeout: SANDBOX_UP });
  // A real prompt has to land before typing into it films as typing.
  await beat(page, PACE.read + 2000);
  await caption(page, "It is the sandbox.");
  await beat(page, BEAT_SHORT);
  await caption(page, "So let's ask the obvious question.");
  await beat(page, BEAT_SHORT);
  await caption(page, "What credentials are actually inside?");
  await beat(page, PACE.read);

  await typeInTerminal(page, "env | grep -i -E 'anthropic|claude'");

  // The payoff, in the order the line claims it (lifted from the retired
  // model-access spec, assertions intact — see the file header).
  await expect(screen).toContainText("ANTHROPIC_BASE_URL=https://api.anthropic.com", { timeout: COMMAND_ECHOES });
  await expect(screen).toContainText("CLAUDE_CONFIG_DIR=");
  await expect(screen).toContainText("WARDYN_CLAUDE_MANAGED_B64=");
  // "No API key" is a claim about an ABSENCE — asserted only after the grep's
  // output demonstrably landed, or an empty screen satisfies it trivially.
  await expect(screen).not.toContainText("ANTHROPIC_API_KEY");
  await caption(page, "No API key.");
  await beat(page, BEAT_SHORT);
  await caption(page, "There is a credential-shaped value here, but let's look at what it actually is.");
  await beat(page, PACE.read + 1200);
});

// ---------------------------------------------------------------------------
// Beat 3 — the decoy
// ---------------------------------------------------------------------------

test("V06 beat 3 — the decoy", async () => {
  test.setTimeout(300_000);
  const page = stage();
  const screen = page.locator(".xterm-screen").first();

  await caption(page, "This is the file Claude Code expects to find.");
  await beat(page, PACE.read);

  await typeInTerminal(page, "cat $CLAUDE_CONFIG_DIR/.credentials.json");

  // The FULL token — a Go constant, matched whole.
  await expect(screen).toContainText(SENTINEL, { timeout: COMMAND_ECHOES });
  await spotlightTerminalRow(page, "sk-ant-oat01-wardyn-inert-sentinel");
  await caption(page, "And the value inside is a decoy.");
  await beat(page, BEAT_SHORT);
  await caption(page, "It has the shape the tool expects.");
  await beat(page, BEAT_SHORT);
  await caption(page, "But it isn't a live credential.");
  await beat(page, BEAT_SHORT);
  // S3 hygiene: clear the sentinel-token ring HERE — left parked, it slices
  // the `cat` command line for the whole attacker-framing stretch below.
  await spotlight(page, null);
  await caption(page, "So let's pretend we're the attacker.");
  await beat(page, BEAT_SHORT);
  await caption(page, "We compromise this container.");
  await beat(page, BEAT_SHORT);
  await caption(page, "We find the credential.");
  await beat(page, BEAT_SHORT);
  await caption(page, "And we steal it.");
  await beat(page, BEAT_SHORT);
  await caption(page, "What did we get?");
  await beat(page, BEAT_SHORT);
  await caption(page, "A decoy.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Anthropic has never seen it.");
  await beat(page, BEAT_SHORT);
  await caption(page, "The real credential is still outside the sandbox, at the proxy.");
  await beat(page, PACE.read);
  await caption(page, "That means the proxy is also the point where the credential can be attached to the request.");
  await beat(page, PACE.read);
  await caption(page, "The sandbox never needs to hold the real key.");
  await beat(page, PACE.read);
  await caption(page, "And if an attacker wants the real one?");
  await beat(page, BEAT_SHORT);
  await caption(page, "They have to get past the boundary itself.");
  await beat(page, BEAT_SHORT);
  await caption(page, "That's why the barrier matters.");
  await beat(page, BEAT_SHORT + 400);

  // The injection is ALREADY on the record — it fired at sandbox startup,
  // when the credential was minted, never on traffic. Asserted here, BEFORE
  // the quota beat, so a lane that never ran cannot cost the model call.
  const runId = page.url().split("/runs/")[1]?.split(/[?#]/)[0]?.toLowerCase() ?? "";
  const events = asList<{ action?: string }>(
    await apiGet(page, `/api/v1/audit?run_id=${encodeURIComponent(runId)}&action=${INJECT_ACTION}`),
  );
  expect(
    events.length > 0,
    `run ${runId} has no ${INJECT_ACTION} event — the managed credential never injected ` +
      `(policy without api.anthropic.com, or an api-key grant pre-empted it). Do NOT proceed to the model call.`,
  ).toBe(true);
});

// ---------------------------------------------------------------------------
// Beat 4 — the boundary (the quota beat)
// ---------------------------------------------------------------------------

test("V06 beat 4 — drive the agent", async () => {
  test.setTimeout(600_000);
  const page = stage();
  const screen = page.locator(".xterm-screen").first();

  await caption(page, "Now let's actually use the agent.");
  await beat(page, BEAT_SHORT);
  await caption(page, "A real model call.");
  await beat(page, BEAT_SHORT);
  await caption(page, "From inside a sandbox that doesn't have the real credential.");
  await beat(page, PACE.read);

  // A REAL, quota-consuming Anthropic call. Deliberately no fallback: a take
  // where this fails films the opposite of the video's claim.
  await typeInTerminal(page, 'claude -p "name three primary colors"');

  // \b matters: "credentials" from beat 3 is still in scrollback and contains
  // "red". A word-bounded colour proves an actual answer arrived — and the
  // refusals race it, so a doomed take dies in seconds with a named reason
  // instead of waiting out the full answer window. Take 1 hit exactly this:
  // "You've hit your weekly limit · resets ..." — the subscription's own
  // quota, which no preflight can detect because only a real call reveals it.
  const OUTCOME = /\b(red|yellow|blue)\b|hit your (weekly|session|usage) limit|invalid.*key|authentication_error|credit balance/i;
  await expect(screen).toContainText(OUTCOME, { timeout: MODEL_ANSWERS });
  const text = (await screen.textContent()) ?? "";
  expect(
    /\b(red|yellow|blue)\b/i.test(text) && !/hit your (weekly|session|usage) limit|invalid.*key|authentication_error|credit balance/i.test(text),
    `the model call was refused, not answered — the terminal shows a quota/auth failure. ` +
      `A weekly-limit refusal cannot be fixed by a retake; wait for the reset (the message names it) ` +
      `or connect a different subscription token, then re-shoot.`,
  ).toBe(true);
  await caption(page, "The model answered.");
  await beat(page, BEAT_SHORT);
  await caption(page, "The sandbox never held the key.");
  await beat(page, BEAT_SHORT);
  await caption(page, "That's the whole trick.");
  await beat(page, BEAT_SHORT + 400);
});

// ---------------------------------------------------------------------------
// Beat 4b — the unscripted hold (conditional: not every take raises it)
// ---------------------------------------------------------------------------

test("V06 beat 4b — the unscripted hold", async () => {
  test.setTimeout(60_000);
  const page = stage();

  // UNSCRIPTED and unstaged: Claude Code sometimes reaches for its own
  // telemetry (http-intake.logs.us5.datadoghq.com) right after it starts,
  // and this policy holds it rather than sending it. S1: no receipt, no
  // claim — wait a real window, and if it never raises, say nothing.
  const telemetryRow = page.getByTestId("live-approval-row").filter({ hasText: "datadoghq" }).first();
  const held = await telemetryRow.waitFor({ state: "visible", timeout: 20_000 }).then(
    () => true,
    () => false,
  );
  if (!held) return;

  await centerInFrame(telemetryRow);
  await spotlight(page, telemetryRow);
  await caption(page, "And look at this.");
  await beat(page, BEAT_SHORT);
  await caption(page, "The tool just tried to contact another host.");
  await beat(page, BEAT_SHORT);
  await caption(page, "It's not on our list.");
  await beat(page, BEAT_SHORT);
  await caption(page, "So Wardyn stopped it.");
  await beat(page, BEAT_SHORT);
  await spotlight(page, null);
  await decide(page, "Deny", "Deny.", "datadoghq");
  await caption(page, "Nobody had to script that into the demo.");
  await beat(page, PACE.read);
  await caption(page, "The boundary caught something the tool actually tried to do.");
  await beat(page, PACE.read);
  await caption(page, "And now there's a record of the decision.");
  await beat(page, PACE.read + 400);
});

// ---------------------------------------------------------------------------
// Beat 5 — on the record
// ---------------------------------------------------------------------------

test("V06 beat 5 — on the record", async () => {
  test.setTimeout(180_000);
  const page = stage();

  // The GLOBAL audit page, not the run's own Audit tab: the run tab prints
  // raw action strings, while /audit renders the event through its
  // ACTION_VERB map — and the verb row is the frame this beat is about.
  await page.goto("/audit");
  await caption(page, "And the credential use is recorded.");
  await beat(page, PACE.read);
  await caption(page, "The secret value itself isn't.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Let's find it.");
  await beat(page, BEAT_SHORT);
  const search = page.getByPlaceholder("Search events, domains, run IDs…");
  await expect(search).toBeVisible({ timeout: 30_000 });
  // S5: the query is the teaching — type it visibly rather than filling silently.
  await search.click();
  await page.keyboard.type("subscription_inject", { delay: 45 });
  // audit.tsx's ACTION_VERB row for the event — the credential story, named.
  await expect(page.getByText("Injected the subscription credential at the proxy").first()).toBeVisible({
    timeout: 30_000,
  });
  await spotlight(page, page.getByText("Injected the subscription credential at the proxy").first());
  // S6's beforeAll sweep keeps this to exactly one row — a stale run still
  // holding slugify would mint a second injection and make "There it is" a lie.
  await caption(page, "There it is.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Wardyn injected the credential at the boundary.");
  await beat(page, PACE.read);
  await caption(page, "The sandbox doesn't appear as the owner of that credential because it never held it.");
  await beat(page, PACE.read);
  await caption(page, "That's the distinction we're proving.");
  await beat(page, PACE.read + 400);
  await spotlight(page, null);
});

// ---------------------------------------------------------------------------
// Conclusion
// ---------------------------------------------------------------------------

test("V06 conclusion", async () => {
  test.setTimeout(60_000);
  const page = stage();

  await chapter(page, "What you just saw", "An agent driven by hand — and the key it never held");
  await caption(page, "Interactive runs put you inside the sandbox.");
  await beat(page, BEAT_SHORT);
  await caption(page, "But the security model doesn't change.");
  await beat(page, BEAT_SHORT);
  await caption(page, "The workspace is still limited.");
  await beat(page, BEAT_SHORT);
  await caption(page, "The network is still controlled.");
  await beat(page, BEAT_SHORT);
  await caption(page, "The real credential stays outside.");
  await beat(page, BEAT_SHORT);
  await caption(page, "And everything important leaves a record.");
  await beat(page, PACE.read);
  await caption(page, "Next, we'll take our hands off the keyboard.");
  await beat(page, PACE.read);
  await caption(page, "Let's see what happens when the agent works on its own.");
  await beat(page, PACE.read + 400);
  await caption(page, "");
  await silentCard(page, "Next — 07: Autonomous agent");
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
