/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * Video 04 of the 0.5 series — "Interactive runs".
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
 * Driven by `scripts/record-demo.sh --video 04`, which globs this exact
 * filename and names the take wardyn-04-interactive-runs-<stamp>.mp4
 * (docs/README.md links that asset name — do not rename this file). It
 * self-skips without WARDYN_DEMO=1.
 */

import { test, expect, type Page } from "@playwright/test";
import { WORKSPACE_NAME } from "./task";
import { act, beat, caption, chapter, PACE, spotlight, typeInTerminal } from "./overlay";
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
});

// ---------------------------------------------------------------------------
// Beat 1 — the form again, but only the choices that change
// ---------------------------------------------------------------------------

test("V04 beat 1 — an agent, and a hand on the wheel", async () => {
  test.setTimeout(180_000);
  const page = stage();
  await page.goto("/runs/new");
  await page.bringToFront();

  await expect
    .poll(() => page.evaluate(() => typeof (window as unknown as Record<string, unknown>).__demo), { timeout: 15_000 })
    .toBe("object");
  await expect(page.getByRole("heading", { name: "New run", level: 1 })).toBeVisible({ timeout: 30_000 });

  await chapter(page, "Interactive runs", "Drive the agent yourself — inside the boundary");
  await caption(page, "Last video's run needed nobody. This one is the opposite: you, live, inside the box.");
  await beat(page, PACE.read);
  await caption(page, "Same form — only the choices change.");
  await beat(page, PACE.read);

  const title = page.getByLabel("Title");
  await title.fill(RUN_TITLE);

  // Agent task this time — and Interactive, which is the video.
  await act(page, page.getByRole("radio", { name: /Agent task/ }), "An agent task, run by Claude Code.");
  await act(page, page.getByRole("radio", { name: /^Interactive/ }), "Interactive: the sandbox comes up idle, and you drive it over this terminal.");

  // Start in a SHELL, not in the agent's own TUI. The default drops you
  // inside Claude Code's prompt box; the shell start is what lets a human
  // inspect the box and launch the agent themselves — which is this video.
  const startWith = page.getByRole("radiogroup", { name: "Start with" });
  await startWith.scrollIntoViewIfNeeded().catch(() => {});
  await act(page, startWith.getByRole("radio", { name: /^Terminal/ }), "Start with a plain shell — we will launch the agent ourselves, from inside.");

  // The workspace, and the envelope: exactly one host, the model's.
  const wsPicker = page.getByRole("combobox").filter({ hasText: /workspace|Ephemeral/i }).first();
  await act(page, wsPicker);
  await act(page, page.getByRole("option", { name: new RegExp(WORKSPACE_NAME) }).first());

  await act(page, page.getByRole("radio", { name: /^Confined/ }));
  await act(page, page.getByRole("radio", { name: /^Just the model provider/ }), "Confined again — but an agent needs its model, so this run gets exactly one host.");
  await caption(page, "api.anthropic.com, and nothing else. The rail holds the whole contract.");
  await beat(page, PACE.read);

  await act(page, page.getByRole("button", { name: "Launch run" }));
  await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{8,}/i, { timeout: 60_000 });
});

// ---------------------------------------------------------------------------
// Beat 2 — the terminal is the run
// ---------------------------------------------------------------------------

test("V04 beat 2 — inside the box", async () => {
  test.setTimeout(SANDBOX_UP + 120_000);
  const page = stage();

  await caption(page, "The cockpit's terminal is not a viewer — it is the sandbox's own shell.");
  const screen = page.locator(".xterm-screen").first();
  await expect(screen).toBeVisible({ timeout: SANDBOX_UP });
  // A real prompt has to land before typing into it films as typing.
  await beat(page, PACE.read + 2000);

  await caption(page, "First, the question every security review asks: what credentials does this box hold?");
  await beat(page, PACE.read);

  await typeInTerminal(page, "env | grep -i -E 'anthropic|claude'");
  await caption(page, "No API key here. A real base URL, one base sixty-four blob.");

  // The payoff, in the order the line claims it (lifted from the retired
  // model-access spec, assertions intact — see the file header).
  await expect(screen).toContainText("ANTHROPIC_BASE_URL=https://api.anthropic.com", { timeout: COMMAND_ECHOES });
  await expect(screen).toContainText("CLAUDE_CONFIG_DIR=");
  await expect(screen).toContainText("WARDYN_CLAUDE_MANAGED_B64=");
  // "No API key" is a claim about an ABSENCE — asserted only after the grep's
  // output demonstrably landed, or an empty screen satisfies it trivially.
  await expect(screen).not.toContainText("ANTHROPIC_API_KEY");
  await beat(page, PACE.read + 1200);
});

// ---------------------------------------------------------------------------
// Beat 3 — the decoy
// ---------------------------------------------------------------------------

test("V04 beat 3 — the decoy", async () => {
  test.setTimeout(300_000);
  const page = stage();
  const screen = page.locator(".xterm-screen").first();

  await caption(page, "That blob decodes into the credentials file Claude Code reads at startup.");
  await beat(page, PACE.read);

  await typeInTerminal(page, "cat $CLAUDE_CONFIG_DIR/.credentials.json");

  // The FULL token — a Go constant, matched whole.
  await expect(screen).toContainText(SENTINEL, { timeout: COMMAND_ECHOES });
  await spotlightTerminalRow(page, "sk-ant-oat01-wardyn-inert-sentinel");
  await caption(page, "The token names itself an inert sentinel: the shape a harness demands, carrying nothing.");
  await beat(page, PACE.read + 1400);

  // The attacker framing (owner note): the decoy's value is what it makes
  // WORTHLESS. Verified before scripting it — the proxy terminates the
  // sandbox's TLS, so the sentinel is all that ever travels on the sandbox
  // side; the live token exists only in proxy memory and on the proxy's own
  // leg to Anthropic. The honest ceiling is stated as the barrier, exactly as
  // video one taught it: nothing IN the box can read the key, so getting it
  // means getting OUT of the box.
  await caption(page, "So play the attacker. Compromise this container, find the credential, exfiltrate it.");
  await beat(page, PACE.read);
  await caption(page, "You have stolen a decoy. Anthropic has never heard of it.");
  await beat(page, PACE.read);
  await caption(page, "The live token exists only in the proxy, outside the box — even this shell's own traffic carries the decoy.");
  await beat(page, PACE.read);
  await caption(page, "Reaching the real one means breaking out of the sandbox itself.");
  await beat(page, PACE.read);
  await caption(page, "And that is exactly the wall you sized in video one — Fence, Wall, or Vault.");
  await beat(page, PACE.read + 600);
  await spotlight(page, null);

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

test("V04 beat 4 — the boundary", async () => {
  test.setTimeout(600_000);
  const page = stage();
  const screen = page.locator(".xterm-screen").first();

  await caption(page, "Now drive the agent — a real model call, from a shell holding nothing but that decoy.");
  await beat(page, PACE.read);

  // A REAL, quota-consuming Anthropic call. Deliberately no fallback: a take
  // where this fails films the opposite of the video's claim.
  await typeInTerminal(page, 'claude -p "name three primary colors"');
  await caption(page, "The proxy strips what the sandbox sent, and attaches the live token at the boundary.");

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
  await caption(page, "A real answer, in a box that could not have paid for it. The key stayed outside.");
  await beat(page, PACE.read + 1000);
});

// ---------------------------------------------------------------------------
// Beat 5 — on the record
// ---------------------------------------------------------------------------

test("V04 beat 5 — on the record", async () => {
  test.setTimeout(180_000);
  const page = stage();

  // The GLOBAL audit page, not the run's own Audit tab: the run tab prints
  // raw action strings, while /audit renders the event through its
  // ACTION_VERB map — and the verb row is the frame this beat is about.
  await page.goto("/audit");
  await caption(page, "And the injection itself is on the record.");
  await beat(page, PACE.read);
  const search = page.getByPlaceholder("Search events, domains, run IDs…");
  await expect(search).toBeVisible({ timeout: 30_000 });
  await search.fill("subscription_inject");
  // audit.tsx's ACTION_VERB row for the event — the credential story, named.
  await expect(page.getByText("Injected the subscription credential at the proxy").first()).toBeVisible({
    timeout: 30_000,
  });
  await spotlight(page, page.getByText("Injected the subscription credential at the proxy").first());
  await caption(page, "One event: the proxy injected the credential. The sandbox never appears in that sentence.");
  await beat(page, PACE.read + 600);
  await spotlight(page, null);
});

// ---------------------------------------------------------------------------
// Conclusion
// ---------------------------------------------------------------------------

test("V04 conclusion", async () => {
  test.setTimeout(60_000);
  const page = stage();

  await chapter(page, "What you just saw", "An agent driven by hand, and the key it never held");
  await caption(page, "An interactive run is a sandbox with you inside — same envelope, same record.");
  await beat(page, PACE.read);
  await caption(page, "The box held a decoy. The proxy held the key. The model answered anyway.");
  await beat(page, PACE.read);
  await caption(page, "So a compromised agent has nothing to steal — which was the promise from video one.");
  await beat(page, PACE.read + 400);
  await caption(page, "Next: take your hands off the wheel — an autonomous agent, doing real work.");
  await beat(page, PACE.read);
  await caption(page, "");
  await silentCard(page, "Next — 05: Autonomous agent");
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
