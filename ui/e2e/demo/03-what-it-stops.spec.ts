/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * Episode 03 of the series — "What it stops: the demos". THE MEGA-EPISODE.
 *
 * WHERE IT SITS. Third in the restructured series — 01 problem/solution, 02
 * make setup + Essentials, 03 (this) the whole demos surface, 04 add a
 * workspace, 05 your first policy. Driven by `scripts/record-demo.sh --video
 * 03`, which globs this exact filename. The owner accepts 12-18 minutes of
 * takes for this one: it films EVERY hands-on demo Getting Started ships.
 *
 * WHAT THIS FILMS — BOTH demo groups, every card on camera.
 *   ACT 1, the Egress group (seven): the four keyless guardrail demos —
 *     sealed-box, fail-then-approve, held-at-the-door, lines-that-can't-be-
 *     crossed (incl. the 169.254.169.254 metadata probe) — driven deep as the
 *     payoff cut; then agent-in-the-box, record-a-policy and once-or-for-good
 *     shown as named walk-past beats (one line each, not deep-driven — the
 *     scope caret belongs to episode 10, the agent to episode 08). ffwd
 *     discipline: the deep four are the cut, the rest are shown.
 *   ACT 2, the Secrets group (eight): one masked write on camera (the store's
 *     write-only door), then the three mechanism demos — write-only-by-design,
 *     key-never-in-the-box, authorized-not-issued — then the five per-KIND
 *     demos as a ladder: rest-api-token (the realistic Bearer header),
 *     pat-stdout-only (a PAT minted into a pipe, refusal-first),
 *     ssh-briefly-resident (the documented resident exception, re-enacted),
 *     github-app-broker (TEACH+GATE — its token is minted from the live GitHub
 *     API, so the card teaches and Start stays closed), and sts-fail-closed
 *     (the CREATE refusal IS the demo — no sandbox ever starts).
 *
 * PROVENANCE. Not a rewrite from scratch — a MERGE of proven choreography:
 *   - The four-test egress quartet is the owner-ratified cut carried verbatim
 *     from the retired 01-getting-started.spec.ts act 3 (via walkthrough.spec
 *     act 3 before that; both in git history). Its xterm assertions are moved
 *     to expect.poll(innerText) per the series LAW (toContainText starves in a
 *     take while the same text is on screen — episodes 09/10 both burned
 *     rehearsals on it); the dialogue is untouched.
 *   - The secrets mechanism act LIFTS the write-only masked write + the
 *     write-only-by-design drive from the retiring 04's beats 4-5, and the
 *     key-never / authorized drives from the retiring 07's beats 6-8 (the plan
 *     retires those beats there: new-04 = old-04 MINUS its demo beat, new-07 =
 *     old-06 MINUS beats 6-8). Those lines are the owner's, already ratified in
 *     local/secrets-episodes-dialog-proposals.md — carried verbatim.
 *   - The five per-KIND demos and every act transition are NEW; their lines are
 *     [OWNER SLOT — drafted] in local/episode-03-mega-proposal.md (in take
 *     order, each with a one-line rationale). local/episode-03-stanza-check.py
 *     fails if a spec string and a proposal stanza ever drift.
 *
 * WHERE THE DEMOS LIVE. One demos surface: Getting Started. The /demos route is
 * gone (App.tsx redirects it into the funnel), each demo is a sub-step of an
 * "Egress demos" / "Secrets demos" phase reachable at `/setup?step=<demo-id>`,
 * and the per-demo card carries `demo-card-<id>`. Act 1 cold-opens straight on
 * `/setup?step=sealed-box`; every other card is reached by an explicit
 * `?step=` navigation rather than walking Next, so a gated card (a missing
 * model, a missing secret) never derails the take — the deep-link corrector
 * (setup-screen.tsx) falls a dropped step back to its nearest surviving
 * neighbour, so this spec STAGES the secrets those steps gate on first.
 *
 * STAGING THE OPERATOR OWNS (off camera):
 *   1. NO RESET. `record-demo.sh --no-reset` against the same long-lived stack
 *      the earlier videos left behind (barrier + Secrets store carry over).
 *      beforeAll sweeps stale approvals/runs (S6) and stages the three secrets
 *      the per-kind demos need (wardyn-demo-api-token / -pat / -ssh-key) — the
 *      masked ENTRY is taught once on camera with wardyn-demo-key, so staging
 *      the rest off camera is the same lesson, not a hidden one.
 *   2. A FRESH BROWSER CONTEXT. stage.ts registers its own beforeAll/afterAll
 *      per spec FILE; act 1 reopens /setup and seeds wardyn-onboarding-seen the
 *      way this host's operator did when they walked the welcome in episode 02.
 *
 * This is NOT a test. It asserts only enough to stay honest and to know when to
 * advance; a failure here means the recording is wrong, not the product. It
 * runs against the REAL compose stack on :8080 with real sandboxes — the
 * hermetic `-runner none` backend cannot start one at all.
 *
 * Selectors are getByRole + accessible names + the `demo-*` testids, matching
 * the rest of the suite: a copy change breaks this loudly and in one place.
 */

import { test, expect, type Locator, type Page } from "@playwright/test";
import { FUNNEL_DEMOS } from "./task";
import {
  act,
  beat,
  caption,
  centerInFrame,
  chapter,
  ffwdEnd,
  ffwdStart,
  PACE,
  spotlight,
  typeInTerminal,
} from "./overlay";
// The browser, the recorded context and the shared page live in stage.ts:
// importing it is what registers this file's beforeAll/afterAll, and each act
// reads the page out of stage() rather than closing over a module-level `let`.
import { stage } from "./stage";
import { advance, decide } from "./funnel";
import { sweepStaleState } from "./sweep";

test.skip(!process.env.WARDYN_DEMO, "demo recording — run via `make record-demo` (exports WARDYN_DEMO=1)");

// Sandboxes are real containers — every deep-driven demo launches one. Minutes,
// not seconds. Ceilings for waiting on the PRODUCT; the pacing the viewer sees
// comes from overlay.ts. (funnel.ts's decide() has its own APPROVAL_APPEARS
// ceiling for a held request, since that one waits on a HUMAN.)
const SANDBOX_UP = 240_000;
const COMMAND_ECHOES = 60_000;

/** Hold for a SHORT stanza — the owner's staccato lines drag on PACE.read. */
const BEAT_SHORT = 1400;

// ---------------------------------------------------------------------------
// This episode's secrets. wardyn-demo-key is written ON CAMERA in act 4 (the
// masked-entry lesson, lifted from old-07 beat 6); the other three are staged
// off camera in beforeAll, because their per-kind demos carry `needsSecret`
// and stepOrder(status) DROPS a demo whose secret is missing — a `?step=` to a
// dropped card silently re-corrects to a neighbour and films the wrong thing.
// Values are canary sentinels (>= secretmask MinLen 8), never real credentials
// — only strings this file greps the sandbox screen for.
// ---------------------------------------------------------------------------
const DEMO_KEY = "wardyn-demo-key";
const DEMO_KEY_VALUE = "WARDYN-V03-KEY-CANARY-8Q4ZR7";
const API_TOKEN = "wardyn-demo-api-token";
const API_TOKEN_VALUE = "WARDYN-V03-APITOKEN-CANARY-5R2WX1";
const PAT_SECRET = "wardyn-demo-pat";
const PAT_VALUE = "WARDYN-V03-PAT-CANARY-7T9KM4";
const SSH_SECRET = "wardyn-demo-ssh-key";
const SSH_VALUE = "WARDYN-V03-SSHKEY-CANARY-NOT-A-REAL-PEM-3J6NQ";

test.describe.configure({ mode: "serial" });

// ---------------------------------------------------------------------------
// Off-camera plumbing.
// ---------------------------------------------------------------------------

/** Same bearer shape every sibling video and funnel.ts's helpers use. */
function apiHeaders(): Record<string, string> | undefined {
  return process.env.WARDYN_DEMO_TOKEN ? { Authorization: `Bearer ${process.env.WARDYN_DEMO_TOKEN}` } : undefined;
}

/** PUT /api/v1/secrets/{name} {value} — store or overwrite. Best-effort. */
async function putSecret(page: Page, name: string, value: string): Promise<void> {
  await page.request
    .put(`/api/v1/secrets/${encodeURIComponent(name)}`, { headers: apiHeaders(), data: { value } })
    .catch(() => {});
}

/** DELETE /api/v1/secrets/{name}. Best-effort. */
async function deleteSecret(page: Page, name: string): Promise<void> {
  await page.request.delete(`/api/v1/secrets/${encodeURIComponent(name)}`, { headers: apiHeaders() }).catch(() => {});
}

/** /setup renders the welcome hero until this flag is set (onboarding-screen),
 *  and stage.ts hands a fresh profile — so a `?step=` deep link lands on the
 *  hero instead of the demo without it. Per-BROWSER; idempotent. */
async function seedOnboardingSeen(page: Page): Promise<void> {
  await page.evaluate(() => {
    try {
      localStorage.setItem("wardyn-onboarding-seen", "1");
    } catch {
      /* private mode — ignore */
    }
  });
}

// ---------------------------------------------------------------------------
// Shared demo-driving primitives.
// ---------------------------------------------------------------------------

/** Open a demo by its funnel deep link and prove its card is on screen. The
 *  heading landing is ALSO the receipt for a needsSecret demo: without the
 *  secret it is dropped from stepOrder and the screen re-corrects elsewhere. */
async function openDemo(page: Page, id: string, title: string): Promise<Locator> {
  await seedOnboardingSeen(page);
  await page.goto(`/setup?step=${id}`);
  await expect(
    page.getByRole("heading", { name: title, level: 2 }),
    `the demo step never opened — is its secret staged? (${id})`,
  ).toBeVisible({ timeout: 30_000 });
  return page.getByTestId(`demo-card-${id}`);
}

/** Start a demo's sandbox and fast-forward the boot. Returns the xterm screen
 *  locator, ready to type into. beat(200) before the span so a still-speaking
 *  caption is not compressed into the ffwd (which lands every later cue early). */
async function startAndBoot(page: Page, card: Locator, id: string): Promise<Locator> {
  const start = card.getByTestId(`demo-start-${id}`);
  await expect(start, "the demo Start button is disabled — this stack has no ready barrier").toBeEnabled({
    timeout: 60_000,
  });
  await act(page, start, "Start it.");
  await beat(page, 200);
  await ffwdStart(page);
  try {
    await expect(card.locator(".xterm-screen").first()).toBeVisible({ timeout: SANDBOX_UP });
  } finally {
    // Real time resumes the instant the terminal is on screen — even on a
    // failed take, so the encoder gets a span this run actually spent.
    await ffwdEnd(page);
  }
  // .xterm-screen renders when AttachTerminal MOUNTS, before the PTY websocket
  // is up; typing here eats the first characters.
  await beat(page, PACE.read);
  // Cockpit framing: policy at the top, terminal + audit below, all in one frame.
  await frameRun(page, id);
  return card.locator(".xterm-screen").first();
}

/** expect.poll over innerText, NEVER toContainText — the series LAW: the xterm
 *  locator starves inside a take while the same text is demonstrably on screen.
 *  innerText is the exact signal the viewer sees. */
async function pollScreen(screen: Locator, re: RegExp, message: string): Promise<void> {
  await expect
    .poll(async () => await screen.innerText().catch(() => "<no .xterm-screen>"), { timeout: COMMAND_ECHOES, message })
    .toMatch(re);
}

/** The exact command the operator would run, read off the demo card's own copy
 *  pill — polled so a `{grant_id}` placeholder is resolved from the live run's
 *  grants first (StepList substitutes after the run goes live). Keeps the typed
 *  command from ever drifting from the catalog, and handles per-run ids. */
async function pillCmd(card: Locator, i: number): Promise<string> {
  const pill = card.getByTestId("demo-steps").getByRole("button", { name: "Copy command" }).nth(i);
  await expect
    .poll(async () => (await pill.innerText().catch(() => "{grant_id}")).trim(), {
      timeout: 60_000,
      message: `demo step ${i}'s command never resolved (still carries the literal {grant_id}?)`,
    })
    .not.toContain("{grant_id}");
  return (await pill.innerText()).trim();
}

/**
 * A chapter card that is NOT spoken (the series outro convention). overlay.ts's
 * chapter() always speaks what it renders; this drives the same overlay
 * primitive directly for the one card that must stay silent.
 */
async function silentCard(page: Page, text: string): Promise<void> {
  const set = (t: string) =>
    page
      .evaluate((s: string) => {
        const d = (window as unknown as Record<string, Record<string, (...x: unknown[]) => void>>).__demo;
        d?.chapter?.(s, "");
      }, t)
      .catch(() => {
        /* overlay absent — a card is cosmetic, never fatal */
      });
  await set(text);
  await page.waitForTimeout(PACE.chapter);
  await set("");
  await page.waitForTimeout(400);
}

/** The policy card renders as YAML through tintLines, which makes EACH source
 *  line its own <div> under <pre><code> (see wardyn/code-block.tsx). So a single
 *  key/value is a targetable line: ring it to teach exactly what changed. The
 *  key is anchored at a line boundary (indent or the "- " list lead) so a nested
 *  "host:" inside a scope never collides with a top-level key. */
function policyLine(card: Locator, id: string, key: string): Locator {
  return card
    .getByTestId(`demo-policy-${id}`)
    .locator("pre code > div")
    .filter({ hasText: new RegExp(`(^|\\s)${key}:`) });
}

/** Frame the run cockpit: scroll the policy to the top of the viewport so the
 *  policy, the terminal, and the audit panel below it are all in one frame while
 *  commands run. Relies on the demos-step layout (policy directly above the
 *  runner) and the 36vh terminal keeping the trio in view — so a later
 *  typeInTerminal, whose terminal is already visible, does not re-scroll it off. */
async function frameRun(page: Page, id: string): Promise<void> {
  await page
    .evaluate((testid) => {
      document.querySelector(`[data-testid="${testid}"]`)?.scrollIntoView({ block: "start" });
    }, `demo-policy-${id}`)
    .catch(() => {});
  await page.waitForTimeout(500);
}

/** Ring one policy line and narrate it — the diff device: the first demo walks
 *  every line, each later demo highlights only what changed and why. */
async function walkPolicyKey(page: Page, card: Locator, id: string, key: string, ...lines: string[]): Promise<void> {
  const el = policyLine(card, id, key).first();
  // Assert the line EXISTS before ringing it — spotlight() silently rings
  // nothing on a missing locator, which would ship a take that narrates a key
  // with no highlight. A rename in demo-catalog.ts fails the rehearsal here.
  await expect(el, `policy key "${key}" not rendered on demo ${id} — did the catalog spec change?`).toBeVisible({
    timeout: 30_000,
  });
  await centerInFrame(el).catch(() => {});
  await spotlight(page, el);
  for (const l of lines) {
    await caption(page, l);
    await beat(page, PACE.read);
  }
  await spotlight(page, null);
}

test.beforeAll(async () => {
  if (!process.env.WARDYN_DEMO) return;
  const page = stage();
  // S6: deny stale pending approvals / kill stale runs first — otherwise the
  // Approvals badge carries a prior take's number through the whole video.
  await sweepStaleState();
  // wardyn-demo-key is written on camera (act 4) — clear a prior take's copy so
  // that write always films a real creation.
  await deleteSecret(page, DEMO_KEY);
  // Stage the three per-kind secrets: their masked entry is the same lesson act
  // 4 teaches once, and their demos are dropped from the walk without them.
  await putSecret(page, API_TOKEN, API_TOKEN_VALUE);
  await putSecret(page, PAT_SECRET, PAT_VALUE);
  await putSecret(page, SSH_SECRET, SSH_VALUE);
});

// ===========================================================================
// ACT 1 — THE EGRESS GROUP
// ===========================================================================

// ---------------------------------------------------------------------------
// The four guardrail demos, exactly as the quartet cut films them.
// FUNNEL_DEMOS (task.ts) is the shared definition; once-or-for-good is filtered
// out of the deep drive (its caret-scope beat belongs to episode 10) and shown
// as a walk-past instead. `lines-that-cant-be-crossed` keeps the local trim:
// the example.com contrast + the metadata probe, minus the LAN probe.
// ---------------------------------------------------------------------------
type StopDemo = {
  id: string;
  label: string;
  cmds: readonly string[];
  caption: string;
  approve: boolean;
  scope: "run" | "once";
};

const LINES_CMDS = [
  "curl -sSI https://example.com",
  "curl -sSI --max-time 2 http://169.254.169.254/latest/meta-data/",
] as const;

const STOP_DEMOS: StopDemo[] = FUNNEL_DEMOS.filter((d) => d.id !== "once-or-for-good").map((d) => ({
  ...d,
  cmds: d.id === "lines-that-cant-be-crossed" ? LINES_CMDS : d.cmds,
}));

test("V03 act 1 — open on the demos", async () => {
  test.setTimeout(120_000);
  const page = stage();
  // A fresh take context has never seen the welcome hero — seed the flag the
  // way this host's operator did in episode 02, then open the first demo.
  await page.goto("/");
  await seedOnboardingSeen(page);
  await page.goto("/setup?step=sealed-box");
  await page.bringToFront();

  // Fail here rather than minutes into a silent, caption-less take: this file
  // gets its OWN fresh browser, so nothing upstream proved the overlay
  // installed.
  await expect
    .poll(() => page.evaluate(() => typeof (window as unknown as Record<string, unknown>).__demo), { timeout: 15_000 })
    .toBe("object");

  await expect(page.getByRole("heading", { name: "The sealed box", level: 2 })).toBeVisible({ timeout: 30_000 });
});

// ---------------------------------------------------------------------------
// Act 2 — the four demos. Every wait in this loop is load-bearing; each is
// moved verbatim from the retired quartet cut. The xterm assertions are the
// only change: expect.poll(innerText) per the series LAW, dialogue untouched.
// ---------------------------------------------------------------------------
test("V03 act 2 — four ways the boundary holds", async () => {
  test.setTimeout(1_200_000);
  const page = stage();

  await chapter(page, "What it stops", "Every guardrail, proved on camera");
  await caption(page, "Setup is one thing.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Now let's see the boundary actually work.");
  await beat(page, PACE.read);
  // [OWNER SLOT — drafted] Re-framed for the restructure: this episode now
  // films EVERY guardrail across both groups (egress + secrets), not just the
  // four-test quartet — the old "Four small tests / Four real sandboxes" opener
  // undercounted the whole episode. The network group is SEVEN demos, every one
  // run on camera: four in depth (act 2), three more in full (acts 3/3b/3c —
  // agent-in-the-box, record-a-policy, once-or-for-good). The opener states the
  // seven up front so "four" never reads as the network's total.
  await caption(page, "We'll walk every guardrail Wardyn puts around a run — where it can reach, and what it can hold.");
  await beat(page, PACE.read);
  await caption(page, "Start with the network. Seven demos, each in its own sandbox — four in depth, then three more.");
  await beat(page, BEAT_SHORT);
  await caption(page, "And every decision is visible on screen.");
  await beat(page, PACE.read);

  for (const demo of STOP_DEMOS) {
    await expect(page.getByRole("heading", { name: demo.label, level: 2 })).toBeVisible({ timeout: 60_000 });

    // Ring the demo's own card as its intro speaks — the owner's plain-language
    // lines below already say what changed. fail-then-approve rings its steps
    // block (the approvable arc lives there); the rest ring the card.
    const card = page.getByTestId(`demo-card-${demo.id}`);
    await card.scrollIntoViewIfNeeded().catch(() => {});
    await spotlight(page, demo.id === "fail-then-approve" ? card.getByTestId("demo-steps") : card);

    if (demo.id === "sealed-box") {
      await caption(page, "Test one: denied.");
      await beat(page, BEAT_SHORT);
      await caption(page, "First, something the policy simply doesn't allow.");
      await beat(page, PACE.read);
    } else if (demo.id === "fail-then-approve") {
      await caption(page, "Test two: denied, but it can ask.");
      await beat(page, BEAT_SHORT);
      await caption(page, "Now we'll give the policy a different instruction.");
      await beat(page, PACE.read);
      await caption(page, "Don't silently refuse.");
      await beat(page, BEAT_SHORT);
      await caption(page, "Ask me.");
      await beat(page, BEAT_SHORT);
    } else if (demo.id === "held-at-the-door") {
      await caption(page, "Test three: held for a decision.");
      await beat(page, BEAT_SHORT);
      await caption(page, "This time, the request is already in progress when the policy stops it.");
      await beat(page, PACE.read);
    }
    // lines-that-cant-be-crossed: no intro of its own — the owner's script
    // picks this demo back up mid-scene (the held-at-the-door tail below).
    await spotlight(page, null);

    // THE POLICY, LINE BY LINE. The first demo walks every key/value; each later
    // demo highlights ONLY the line that changed and why. sealed-box is the
    // baseline the whole episode diffs against.
    const policyCard = page.getByTestId(`demo-card-${demo.id}`);
    await policyCard.getByTestId(`demo-policy-${demo.id}`).scrollIntoViewIfNeeded().catch(() => {});
    if (demo.id === "sealed-box") {
      await caption(page, "Before it runs, look at the policy — the whole contract, four lines.");
      await beat(page, PACE.read);
      await walkPolicyKey(page, policyCard, demo.id, "min_confinement_class",
        "The barrier: CC1, Fence — the lightest sandbox tier, the wall between this run and your host.");
      await walkPolicyKey(page, policyCard, demo.id, "auto_stop_after_sec",
        "A dead-man's switch: if nothing else stops it, the run halts itself after fifteen minutes.");
      await walkPolicyKey(page, policyCard, demo.id, "allowed_domains",
        "The allowlist — empty. Not one destination is permitted.");
      await walkPolicyKey(page, policyCard, demo.id, "first_use_approval",
        "And for anything not listed: always deny. Refused the instant it's dialed — no prompt, no wait.");
    } else if (demo.id === "fail-then-approve") {
      await walkPolicyKey(page, policyCard, demo.id, "first_use_approval",
        "One line changed. Always-deny became deny-with-review.",
        "Now an unlisted host isn't silently refused — it's refused, but you're asked, and you can let it through.");
    } else if (demo.id === "held-at-the-door") {
      await walkPolicyKey(page, policyCard, demo.id, "first_use_approval",
        "Same line again — now wait-for-review.",
        "The request is held in flight while it waits for you. The command doesn't fail; it pauses.");
    } else if (demo.id === "lines-that-cant-be-crossed") {
      await walkPolicyKey(page, policyCard, demo.id, "allow_all_egress",
        "This one throws the door open — allow-all-egress, true. Every public host is permitted.",
        "And yet some destinations are still refused — link-local and private addresses are denied beneath the policy, whatever it says.",
        "Those are the lines that can't be crossed.");
    }

    // No caption on the click — "starting the sandbox" narrates itself.
    await act(page, page.getByTestId(`demo-start-${demo.id}`));
    await expect(page.locator(".xterm-screen").first()).toBeVisible({ timeout: SANDBOX_UP });
    await beat(page, PACE.read);
    // Cockpit framing: policy at the top, terminal + audit below, one frame.
    await frameRun(page, demo.id);

    for (const [i, cmd] of demo.cmds.entries()) {
      if (demo.approve && i === 1) {
        await decide(page, "Approve", "Approve it.", "example.com", demo.scope);
        if (demo.id === "fail-then-approve") {
          await caption(page, "Now run the same command again.");
          await beat(page, BEAT_SHORT);
        }
      }
      if (demo.id === "fail-then-approve" && i === 0) {
        await caption(page, "This command tries to get out.");
        await beat(page, BEAT_SHORT);
      }
      await typeInTerminal(page, cmd);
      // Let the command RESOLVE before moving on — ending a demo while a curl
      // is in flight kills the sandbox before its decision reaches the audit
      // log. Derived from the command's own --max-time; approve demos are
      // excluded (their curl is SUPPOSED to hang when we decide it).
      const maxTime = demo.approve ? null : cmd.match(/--max-time (\d+)/);
      await beat(page, maxTime ? (Number(maxTime[1]) + 2) * 1000 : PACE.read + 800);

      if (demo.id === "fail-then-approve" && i === 0) {
        await caption(page, "The first time, it fails.");
        await beat(page, BEAT_SHORT);
        await caption(page, "But now there's a question waiting for us.");
        await beat(page, PACE.read);
      }

      // The retry after an approval MUST visibly succeed — the payoff of
      // fail-then-approve. Without the assertion a failed retry just raises a
      // fresh approval the NEXT step latches onto, and the take narrates
      // success over a terminal showing two refusals.
      if (demo.approve && i === 1) {
        const term = page.locator(".xterm-screen").first();
        await centerInFrame(term);
        await pollScreen(term, /HTTP\/[\d.]+ 200/, "the approved retry never returned a 200");
        if (demo.id === "fail-then-approve") {
          await caption(page, "The command continues.");
          await beat(page, PACE.read);
          await caption(page, "Same request.");
          await beat(page, BEAT_SHORT);
          await caption(page, "Same sandbox.");
          await beat(page, BEAT_SHORT);
          await caption(page, "The only thing that changed was the decision.");
          await beat(page, PACE.read);
        }
      }
    }

    if (demo.id === "sealed-box") {
      // "Instant refusal on camera" is the whole beat: prove the refusal is on
      // camera. always_deny + empty allowlist → the proxy 403s the CONNECT.
      const term = page.locator(".xterm-screen").first();
      await centerInFrame(term);
      await pollScreen(term, /403|curl: \(\d+\)/, "sealed-box never showed a refusal on screen");
      // No decide() to attach the owner's SAY-ON-DECIDE line to — this policy
      // never asks a human — so it is spoken here, as the refusal lands.
      await caption(page, "The request is denied.");
      await beat(page, BEAT_SHORT);
      await caption(page, "The sandbox asked.");
      await beat(page, BEAT_SHORT);
      await caption(page, "The proxy said no.");
      await beat(page, BEAT_SHORT);
      await caption(page, "And the command gets a normal refusal.");
      await beat(page, PACE.read);
    }

    if (demo.approve && demo.cmds.length === 1) {
      // held-at-the-door: the curl is still hanging at the proxy right now.
      await caption(page, "Notice what's happening.");
      await beat(page, PACE.read);
      await caption(page, "The command hasn't failed.");
      await beat(page, BEAT_SHORT);
      await caption(page, "It's waiting.");
      await beat(page, BEAT_SHORT);
      await caption(page, "Nothing has been sent through the door yet.");
      await beat(page, PACE.read);
      await decide(page, "Approve", "Approve it.", "example.com");
      await caption(page, "And now the request completes.");
      await beat(page, PACE.read);
      await caption(page, "No retry.");
      await beat(page, BEAT_SHORT);
      await caption(page, "It was already waiting at the boundary.");
      await beat(page, PACE.read);

      // The owner's fourth test begins here, still inside this same sandbox —
      // the product's own "held-at-the-door" card carries the wikipedia+Deny
      // step as its third step, so this test rides it.
      await typeInTerminal(page, "curl -sSI --max-time 60 https://wikipedia.org");
      await beat(page, 1200);
      await caption(page, "Test four: walled off.");
      await beat(page, BEAT_SHORT);
      await caption(page, "Now another ordinary host.");
      await beat(page, BEAT_SHORT);
      await caption(page, "This one should be refused outright.");
      await beat(page, PACE.read);
      await decide(page, "Deny", "Deny.", "wikipedia.org");
      await expect(page.getByTestId("demo-audit-panel")).toContainText(/wikipedia/, { timeout: 30_000 });
      await spotlight(page, page.getByTestId("demo-audit-panel"));
      await caption(page, "The refusal lands beside the approval in the record.");
      await beat(page, PACE.read);
      await caption(page, "And then there's the address you really don't want an arbitrary workload reaching:");
      await beat(page, PACE.read);
      await caption(page, "the cloud metadata service.");
      await beat(page, BEAT_SHORT);
      await caption(page, "That's where a cloud machine's own credentials can live.");
      await beat(page, PACE.read);
      await spotlight(page, null);
    }

    if (demo.id === "lines-that-cant-be-crossed") {
      // Prove the block from the TERMINAL. The http:// metadata probe reaches
      // the egress proxy (NO_PROXY covers only proxy+localhost), which refuses
      // to dial it → `HTTP/1.1 403`; on a stack where the proxy is out of that
      // path it dies at the network layer (curl 7). Either shape is the same
      // fact; a success is not accepted.
      const term = page.locator(".xterm-screen").first();
      await pollScreen(
        term,
        /HTTP\/1\.1 403|Failed to connect to 169\.254\.169\.254|curl: \(\d+\)/,
        "the metadata probe was not refused on screen",
      );
      await centerInFrame(term);
      await spotlight(page, term);
      await caption(page, "Here, the request doesn't even get a chance to connect.");
      await beat(page, PACE.read);
      await caption(page, "The proxy refuses it.");
      await beat(page, BEAT_SHORT);
      await caption(page, "The kernel has no route there either.");
      await beat(page, BEAT_SHORT);
      await spotlight(page, null);

      // No pending decision, because there was never a decision to make. Assert
      // the strip really is empty — narrating "no approval was raised" over a
      // pending row would invert the lesson.
      await expect(page.getByTestId("live-approval-row")).toHaveCount(0);
      await caption(page, "And importantly, there's no approval prompt.");
      await beat(page, PACE.read);
      await caption(page, "Some destinations aren't merely disallowed.");
      await beat(page, BEAT_SHORT);
      await caption(page, "They're outside the set of things a human can approve.");
      await beat(page, PACE.read + 900);
    }

    const endDemo = page.getByRole("button", { name: "End demo" });
    if (await endDemo.isVisible().catch(() => false)) await act(page, endDemo);
    // The quartet is consecutive in catalog order, so Next never has to skip an
    // interleaved step. After the fourth, this lands us on the fifth egress
    // step; act 3 re-navigates explicitly regardless.
    await advance();
  }
});

// ---------------------------------------------------------------------------
// Act 3 — the rest of the egress group, shown not deep-driven. Named on camera,
// one line each. agent-in-the-box needs a connected model; if this stack has
// none it is dropped from the walk and the corrector bounces the deep link, so
// its naming is best-effort — the card is cosmetic, never fatal.
// ---------------------------------------------------------------------------
// ---------------------------------------------------------------------------
// Act 3 — the rest of the egress group, RUN, not just named. The owner's law is
// every demo is demonstrated on camera; these three each also headline a later
// episode (agent → 08, record → 09, scope caret → 10), but v3 is the complete
// showcase, so each runs in full here. agent-in-the-box needs a live model:
// probe quota before the take (WEEKLY-LIMIT risk — an out-of-quota `claude`
// still logs egress.allow api.anthropic.com while doing nothing, so the honest
// proof is the FILE it writes, asserted via a WROTE_HELLO marker, not the audit).
// ---------------------------------------------------------------------------

test("V03 act 3 — the agent in the box", async () => {
  test.setTimeout(900_000);
  const page = stage();

  const card = await openDemo(page, "agent-in-the-box", "The agent in the box");

  await caption(page, "Those four were the deep cut, against a human at a terminal.");
  await beat(page, PACE.read);
  await caption(page, "The same group holds three more — and we run them, not just name them.");
  await beat(page, PACE.read);
  await spotlight(page, page.getByTestId("demo-policy-agent-in-the-box"));
  await caption(page, "First, the flagship: the same box, now running a real coding agent.");
  await beat(page, PACE.read);
  await caption(page, "It reaches Anthropic to think — and nothing else.");
  await beat(page, BEAT_SHORT);
  await spotlight(page, null);

  await walkPolicyKey(page, card, "agent-in-the-box", "allowed_domains",
    "The change is the allowlist: two entries now — Anthropic's API.",
    "That's the whole network contract. The agent can reach the model, and no other host.");

  const screen = await startAndBoot(page, card, "agent-in-the-box");

  // Step 1 — the agent task. In -p mode Claude Code writes the file and exits.
  const agentCmd = await pillCmd(card, 0);
  await caption(page, "Attach the terminal and hand it a one-shot task.");
  await beat(page, BEAT_SHORT);
  await typeInTerminal(page, agentCmd, card);
  await caption(page, "It authenticates through the model you connected — injected proxy-side, never resident in the box.");
  await beat(page, PACE.read);
  // The agent's thinking is dead air on camera; compress it. The span ends when
  // the honest proof lands: the FILE, not the audit (an out-of-quota agent still
  // logs egress.allow to Anthropic — see the act header).
  await ffwdStart(page);
  await typeInTerminal(page, "test -s HELLO.md && echo WROTE_HELLO || echo NO_HELLO", card);
  await pollScreen(screen, /WROTE_HELLO/, "the agent never wrote HELLO.md — model quota exhausted, or the run did no work");
  await ffwdEnd(page);
  await caption(page, "It did the work, inside the box, and wrote its file.");
  await beat(page, PACE.read);

  // Step 2 — the same policy still holds against the agent's box.
  const offlist = await pillCmd(card, 1);
  await typeInTerminal(page, offlist, card);
  await pollScreen(screen, /\b(403|refused|Could not resolve|Failed to connect)\b/i, "example.com was not refused inside the agent box");
  await caption(page, "Any host off the allowlist is refused, exactly as it was for the human.");
  await beat(page, PACE.read);

  // Step 3 — the record.
  const auditRows = card.getByTestId("demo-audit-rows");
  await expect(auditRows, "no egress decisions recorded for the agent run").toContainText("api.anthropic.com", {
    timeout: 60_000,
  });
  await centerInFrame(auditRows);
  await spotlight(page, auditRows);
  await caption(page, "Every decision on the record — allowed to Anthropic, denied elsewhere, attributed to the run.");
  await beat(page, PACE.read);
  await caption(page, "The same confinement as the four tests. This is the job Wardyn exists for.");
  await beat(page, PACE.read);
  await spotlight(page, null);

  await act(page, card.getByRole("button", { name: "End demo" }));
});

test("V03 act 3b — record a policy", async () => {
  test.setTimeout(900_000);
  const page = stage();

  const card = await openDemo(page, "record-a-policy", "Record a policy");

  await spotlight(page, page.getByTestId("demo-policy-record-a-policy"));
  await caption(page, "The second flips the usual order.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Instead of guessing an allowlist upfront, run wide open once and let Wardyn watch.");
  await beat(page, PACE.read);
  await spotlight(page, null);

  await walkPolicyKey(page, card, "record-a-policy", "allow_all_egress",
    "The allowlist is empty, but allow-all-egress is true — wide open on purpose.",
    "You can't record what a policy already blocks.");

  // Boots the sandbox; the record-a-policy proof reads the audit panel, not the
  // terminal, so the screen locator is not needed here.
  await startAndBoot(page, card, "record-a-policy");

  // Three real hosts, all recorded (allow_all_egress) — nothing is denied while
  // recording; every host reached becomes a candidate line. The PROOF is the
  // record, not the terminal status line: a HEAD's headers scroll out of the
  // xterm viewport in a blink (the run's audit confirms egress.allow landed),
  // so assert the AUDIT PANEL — which is exactly what this demo is teaching:
  // what gets recorded is what becomes the policy.
  const auditRows = card.getByTestId("demo-audit-rows");
  const recorded = [
    { i: 0, host: "pypi.org", line: "Reach out to a package registry — recorded, not blocked." },
    { i: 1, host: "registry.npmjs.org", line: "A second registry. Every host it touches becomes a candidate line." },
    { i: 2, host: "example.com", line: "A third, unrelated host — recorded the same way." },
  ];
  for (const { i, host, line } of recorded) {
    const cmd = await pillCmd(card, i);
    await typeInTerminal(page, cmd, card);
    await caption(page, line);
    await beat(page, PACE.read);
    await expect(auditRows, `${host} never landed in the audit panel — the recording missed it`).toContainText(host, {
      timeout: 30_000,
    });
  }
  await centerInFrame(auditRows);
  await spotlight(page, auditRows);
  await caption(page, "Every host it reached is on the record — the raw material for the policy.");
  await beat(page, PACE.read);
  await spotlight(page, null);

  await act(page, card.getByRole("button", { name: "End demo" }));

  // The payoff: synthesize the least-privilege policy from what it actually did.
  await act(page, card.getByTestId("demo-turn-into-policy-record-a-policy"), "Now turn what it did into a policy.");
  const sheet = page.getByRole("dialog").filter({ hasText: "Proposed allowed domains" });
  const proposed = sheet.getByText("Proposed allowed domains", { exact: true }).locator("xpath=..");
  await expect(proposed, "the synthesis never proposed the recorded hosts").toContainText("pypi.org", {
    timeout: 60_000,
  });
  await centerInFrame(proposed);
  await spotlight(page, proposed);
  await caption(page, "Wardyn read back exactly what it reached, and proposes the allowlist that would have let it through.");
  await beat(page, PACE.read);
  await caption(page, "Approve it, and the next run is confined to only that. Episode nine drives this on a real workspace.");
  await beat(page, PACE.read);
  await spotlight(page, null);
  // Close the sheet — saving the policy is episode nine's beat, not this showcase's.
  await page.keyboard.press("Escape");
});

test("V03 act 3c — once, or for good", async () => {
  test.setTimeout(900_000);
  const page = stage();

  const card = await openDemo(page, "once-or-for-good", "Once, or for good");

  await spotlight(page, page.getByTestId("demo-policy-once-or-for-good"));
  await caption(page, "The last one is about how long an approval lasts.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Every approval so far stuck around for the rest of the run. Once is narrower.");
  await beat(page, PACE.read);
  await spotlight(page, null);

  await walkPolicyKey(page, card, "once-or-for-good", "first_use_approval",
    "The policy is back to deny-with-review — so a request is held for you.",
    "What's new isn't the policy line. It's how you answer: once, or for the whole run.");

  const screen = await startAndBoot(page, card, "once-or-for-good");

  // First attempt — refused, raises an approval.
  const first = await pillCmd(card, 0);
  await typeInTerminal(page, first, card);
  await pollScreen(screen, /\b(403|refused)\b/i, "the first request was not refused — deny_with_review should hold it");
  await caption(page, "Refused — and an approval appears below the terminal.");
  await beat(page, PACE.read);

  // Grant it ONCE — via the split button's caret, not a plain Approve.
  await decide(
    page,
    "Approve",
    "Approve it — but only this one connection.",
    "example.com",
    "once",
    "Once spends itself on the single connection it was raised for.",
  );

  // Same command — now it gets through.
  const second = await pillCmd(card, 1);
  await typeInTerminal(page, second, card);
  await pollScreen(screen, /HTTP\/[\d.]+ 200/, "the Once-approved retry never returned a 200");
  await caption(page, "Same command — this time it's through.");
  await beat(page, PACE.read);

  // Run it a third time — the Once grant already spent itself, so it asks again.
  await typeInTerminal(page, second, card);
  await expect(
    page.getByTestId("live-approval-row").filter({ hasText: "example.com" }),
    "the third attempt did not raise a fresh approval — Once should have spent itself",
  ).toBeVisible({ timeout: 60_000 });
  await caption(page, "Run it again and it's refused all over again — a brand-new approval.");
  await beat(page, PACE.read);
  await caption(page, "Nothing lingered by accident. One connection, not the run. Episode ten drives the full scope ladder.");
  await beat(page, PACE.read);

  await act(page, card.getByRole("button", { name: "End demo" }));
});

// ===========================================================================
// ACT 2 — THE SECRETS GROUP
// ===========================================================================

// ---------------------------------------------------------------------------
// Act 4 — a key of your own. One masked write on camera (lifted from old-07's
// beat 6): it stores wardyn-demo-key AND teaches the store's write-only door,
// the precondition the mechanism demos borrow.
// ---------------------------------------------------------------------------
test("V03 act 4 — a key of your own", async () => {
  test.setTimeout(180_000);
  const page = stage();

  await chapter(page, "A key of your own", "The other kind of boundary — a value, not a destination");
  await caption(page, "So far, we've been governing where a run may reach.");
  await beat(page, PACE.read);
  await caption(page, "A secret is the other axis: a value a run may use, without ever holding it.");
  await beat(page, PACE.read);
  await caption(page, "But most runs need a key that's yours.");
  await beat(page, BEAT_SHORT);

  await page.goto("/secrets");
  await expect(page.getByRole("heading", { name: "Secrets", level: 1 })).toBeVisible({ timeout: 30_000 });

  await act(page, page.getByRole("button", { name: "Add secret", exact: true }), "Store one now.");
  const dlg = page.getByRole("dialog");
  await expect(dlg.getByRole("heading", { name: "Add secret" })).toBeVisible();
  await dlg.getByLabel("Name").fill(DEMO_KEY);
  await caption(page, "The name is the handle.");
  await beat(page, BEAT_SHORT);
  // The Value field masks at entry (secrets.tsx's -webkit-text-security), so
  // the canary's glyphs are never on screen — one beat here, the full lesson is
  // still the write-only-by-design demo next.
  await dlg.getByLabel("Value", { exact: true }).fill(DEMO_KEY_VALUE);
  await caption(page, "The value is masked the moment it's typed.");
  await beat(page, PACE.read);
  await act(page, dlg.getByRole("button", { name: "Save secret" }), "Save it.");
  await expect(dlg).toBeHidden({ timeout: 30_000 });

  const row = page.getByRole("row", { name: new RegExp(DEMO_KEY) });
  await expect(row).toBeVisible({ timeout: 30_000 });
  // The negative IS the claim, and it holds for the rest of the take.
  await expect(page.locator("body"), "the demo key's VALUE rendered on screen after save").not.toContainText(
    DEMO_KEY_VALUE,
  );
  await spotlight(page, row);
  await caption(page, "That's the last time anyone sees that value.");
  await beat(page, PACE.read);
  await caption(page, "The demos ahead use this key by name.");
  await beat(page, BEAT_SHORT);
  await caption(page, "None of them can read it. The ones we authorize can still use it.");
  await beat(page, BEAT_SHORT + 400);
  await spotlight(page, null);
});

// ---------------------------------------------------------------------------
// Act 5 — write-only, proved from inside (write-only-by-design). Lifted from
// old-04's beat 5; re-pointed at wardyn-demo-key (the card's own probe secret),
// so the 404 is for the very key just saved on camera. `curl -i` deviation kept
// so the 404 STATUS LINE is on screen (unnarrated mechanics, per episode 10).
// ---------------------------------------------------------------------------
test("V03 act 5 — write-only, proved from inside", async () => {
  test.setTimeout(600_000);
  const page = stage();

  // The row's chip carries the product's one honest sentence for write-only
  // (secrets.tsx's WRITE_ONLY_TOOLTIP). Located BY that sentence, so a reworded
  // tooltip fails the take here instead of outliving the product's own words.
  const row = page.getByRole("row", { name: new RegExp(DEMO_KEY) });
  const writeOnlyChip = row.getByTitle(/never read back — not even by you/);
  await centerInFrame(writeOnlyChip);
  await spotlight(page, writeOnlyChip);
  await caption(page, "The store tells us the rule. Now let's test whether the rule is actually true.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Write-only — you can replace it, you can delete it, you can never read it.");
  await beat(page, PACE.read);
  await caption(page, "But that's Wardyn describing itself.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Let's make it prove it, from inside a sandbox.");
  await beat(page, BEAT_SHORT + 400);
  await spotlight(page, null);

  const card = await openDemo(page, "write-only-by-design", "Write-only, even for you");
  const start = card.getByTestId("demo-start-write-only-by-design");
  await expect(start, "the demo Start button is disabled — this stack has no ready barrier").toBeEnabled({
    timeout: 60_000,
  });
  await caption(page, "This sandbox has no permission to use a secret.");
  await beat(page, BEAT_SHORT);
  // The policy is the sealed box again — and crucially, no eligible_grants. The
  // absence IS the point here, so ring the whole block rather than one line.
  await spotlight(page, card.getByTestId("demo-policy-write-only-by-design"));
  await caption(page, "The policy is the sealed box again — and there's no eligible_grants section at all.");
  await beat(page, PACE.read);
  await caption(page, "Nothing was ever going to hand it our secret.");
  await beat(page, BEAT_SHORT);
  await spotlight(page, null);
  await act(page, start, "Start the demo.");

  await beat(page, 200);
  await ffwdStart(page);
  try {
    await expect(card.locator(".xterm-screen").first()).toBeVisible({ timeout: SANDBOX_UP });
  } finally {
    await ffwdEnd(page);
  }
  await beat(page, PACE.read);
  // Cockpit framing: policy at the top, terminal + audit below, one frame.
  await frameRun(page, "write-only-by-design");

  const screen = card.locator(".xterm-screen").first();
  await typeInTerminal(page, "printenv | sort", card);
  await pollScreen(screen, /WARDYN_PROXY_URL=/, "printenv never echoed inside the demo sandbox");
  const envText = await screen.innerText();
  expect(envText.includes(DEMO_KEY_VALUE), "the stored secret's VALUE printed inside the demo sandbox").toBe(false);
  expect(
    envText.includes(DEMO_KEY),
    `"${DEMO_KEY}" reached the demo sandbox — this demo's policy carries no grant, so nothing should have injected it`,
  ).toBe(false);
  await caption(page, "The proxy's address is in there. The secret value isn't.");
  await beat(page, PACE.read);

  await caption(page, "So ask Wardyn for it directly — by name, from inside the box.");
  await beat(page, PACE.read);
  await typeInTerminal(page, `curl -sS -i --noproxy '*' "$WARDYN_PROXY_URL/wardyn/v1/secrets/${DEMO_KEY}"`, card);
  await pollScreen(
    screen,
    /404|unknown brokered route/,
    "the read-back probe never answered — expected a 404 from the proxy's brokered routes",
  );
  await expect(page.locator("body"), "the stored key's VALUE rendered after the read-back probe").not.toContainText(
    DEMO_KEY_VALUE,
  );
  await caption(page, "Four-oh-four. The read-back request has nowhere to go.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Not 'forbidden'. Wardyn has no read-back route for that secret.");
  await beat(page, PACE.read);
  await caption(page, "The run can't read it back — and neither can the operator through this store.");
  await beat(page, PACE.read);
  await caption(page, "Write-only isn't a permission you can escalate past. There simply isn't a read-back operation.");
  await beat(page, PACE.read + 400);
  await spotlight(page, null);
  await act(page, card.getByRole("button", { name: "End demo" }));
});

// ---------------------------------------------------------------------------
// Act 6 — the key that never enters the box (key-never-in-the-box). Lifted
// verbatim from old-07's beat 7: the header is stitched on OUTBOUND at the
// proxy, so the proof is the audit panel, never the response.
// ---------------------------------------------------------------------------
test("V03 act 6 — the key that never enters the box", async () => {
  test.setTimeout(900_000);
  const page = stage();

  const card = await openDemo(page, "key-never-in-the-box", "The key that never enters the box");

  await caption(page, "This demo gives the run permission to use one secret by name.");
  await beat(page, PACE.read);
  // The secrets group adds a whole new section to the policy — walk it once,
  // the way sealed-box walked the egress lines. Later secrets demos diff it.
  await walkPolicyKey(page, card, "key-never-in-the-box", "eligible_grants",
    "A new section appears: eligible_grants. This is where a run is authorized to USE a secret.");
  await walkPolicyKey(page, card, "key-never-in-the-box", "kind",
    "The kind — an API key, attached to outbound requests as a header.");
  await walkPolicyKey(page, card, "key-never-in-the-box", "scope",
    "And the scope: which host, which header — and the secret's NAME.",
    "Not the secret itself. The value never appears in the policy, and never enters the box.");
  await spotlight(page, null);

  const screen = await startAndBoot(page, card, "key-never-in-the-box");

  // THE AUDIT PANEL FIRST — before a single command. The injected header is
  // invisible from inside the box by construction, so the proof is the record.
  const auditRows = card.getByTestId("demo-audit-rows");
  await expect(
    auditRows,
    "no credential.mint row — the inline grant never minted (member role clamps it away, or the secret is missing)",
  ).toContainText("credential.mint", { timeout: 60_000 });
  await expect(auditRows).toContainText("secret.read");
  await centerInFrame(auditRows);
  await spotlight(page, auditRows);
  await caption(page, "Before we type anything, look at the record.");
  await beat(page, PACE.read);
  await caption(
    page,
    "Wardyn has already authorized the proxy to use the secret; the secret value still isn't in the box.",
  );
  await beat(page, PACE.read);
  await caption(page, "That happened outside the box, before the request crossed the boundary.");
  await beat(page, PACE.read + 400);
  await spotlight(page, null);

  await typeInTerminal(page, "printenv | sort", card);
  await pollScreen(screen, /WARDYN_PROXY_URL=/, "printenv never echoed inside the demo sandbox");
  const envText = await screen.innerText();
  expect(envText.includes(DEMO_KEY_VALUE), "the granted key's VALUE printed inside the sandbox").toBe(false);
  expect(
    envText.includes(DEMO_KEY),
    "the granted key's NAME reached the sandbox — injection is proxy-side, nothing should carry it in",
  ).toBe(false);
  await caption(page, "The key isn't in the environment.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Its name isn't either.");
  await beat(page, BEAT_SHORT);

  await typeInTerminal(page, "curl -sSI http://example.com", card);
  await pollScreen(screen, /HTTP\/[\d.]+ 200/, "the allowed host never answered — expected a 200 through the proxy");
  await caption(page, "And yet the request left with the credential attached.");
  await beat(page, PACE.read);
  await caption(
    page,
    "The sandbox sent the request without the secret. The proxy attached the header before forwarding it.",
  );
  await beat(page, PACE.read);
  await caption(
    page,
    "The response can't prove where the credential came from. The audit record shows the proxy attached it.",
  );
  await beat(page, PACE.read + 400);

  await typeInTerminal(page, "curl -sSI http://wikipedia.org", card);
  await pollScreen(screen, /403/, "the unlisted host was not refused — always_deny should 403 it instantly");
  await caption(page, "One more thing.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Authorizing a run to use a credential never widens where it can go.");
  await beat(page, PACE.read);
  await caption(page, "This host was never on the list.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Refused instantly.");
  await beat(page, BEAT_SHORT + 400);
  await act(page, card.getByRole("button", { name: "End demo" }));
});

// ---------------------------------------------------------------------------
// Act 7 — authorized, not issued (authorized-not-issued). Lifted verbatim from
// old-07's beat 8: an approval-gated, single-use mint that returns a RULE, not
// a value. The mint command comes off the card's own copy pill (its {grant_id}
// is substituted from the live run).
// ---------------------------------------------------------------------------
test("V03 act 7 — authorized, not issued", async () => {
  test.setTimeout(900_000);
  const page = stage();

  const card = await openDemo(page, "authorized-not-issued", "Authorized, not issued");

  await caption(page, "Same permission, one field different.");
  await beat(page, BEAT_SHORT);
  await caption(page, "This one requires approval before that one-time use can be authorized.");
  await beat(page, PACE.read);
  await walkPolicyKey(page, card, "authorized-not-issued", "requires_approval",
    "Two lines changed in the grant. Requires-approval is now true — a human decides at the moment it's minted.");
  await walkPolicyKey(page, card, "authorized-not-issued", "ttl_seconds",
    "And a ttl: the credential lives five minutes, then expires on its own.");
  await spotlight(page, null);

  const screen = await startAndBoot(page, card, "authorized-not-issued");
  const mintCmd = await pillCmd(card, 0);

  await caption(page, "The sandbox asks to use the credential.");
  await beat(page, PACE.read);
  await typeInTerminal(page, mintCmd, card);
  await pollScreen(screen, /pending/, 'the first mint never came back pending — expected 409 {"code":"pending"}');
  await caption(page, "Refused — but an approval is now waiting.");
  await beat(page, BEAT_SHORT);
  await caption(page, "That ask didn't fail. It raised a decision.");
  await beat(page, PACE.read);

  await caption(page, "And here it is, right under the terminal we're watching.");
  await beat(page, PACE.read);
  await decide(card, "Approve", "Approve it.", "example.com");

  await typeInTerminal(page, mintCmd, card);
  await pollScreen(screen, /X-Wardyn-Demo/, "the approved mint never returned an injection rule");
  const mintText = await screen.innerText();
  expect(
    mintText.includes(DEMO_KEY_VALUE),
    "the approved mint returned the secret VALUE — the demo's entire claim is that it never does",
  ).toBe(false);
  await caption(page, "Now the same ask is authorized.");
  await beat(page, BEAT_SHORT);
  await caption(page, "And look at what came back.");
  await beat(page, BEAT_SHORT);
  await caption(
    page,
    "A host, a header, and the name of a secret — the information the proxy needs to inject it at the boundary.",
  );
  await beat(page, PACE.read);
  await caption(page, "An instruction for the proxy — not a credential for the box.");
  await beat(page, PACE.read);
  await caption(page, "Authorized, not handed to the sandbox.");
  await beat(page, BEAT_SHORT + 400);

  await typeInTerminal(page, mintCmd, card);
  await pollScreen(screen, /already_minted/, "the third mint was not refused — this grant is single-use");
  await caption(page, "Ask again, and it's spent.");
  await beat(page, BEAT_SHORT);
  await caption(page, "One approval, one use.");
  await beat(page, BEAT_SHORT + 400);

  const auditRows = card.getByTestId("demo-audit-rows");
  await expect(auditRows, "the approved mint never landed as an allow in the demo's audit panel").toContainText(
    "allow",
    { timeout: 60_000 },
  );
  await centerInFrame(auditRows);
  await spotlight(page, auditRows);
  await caption(page, "Three asks, three audit rows: refused pending approval, authorized once, then spent.");
  await beat(page, PACE.read);
  await caption(page, "The first row is the approval gate. The last is the single-use boundary working.");
  await beat(page, PACE.read + 400);
  await spotlight(page, null);
  await act(page, card.getByRole("button", { name: "End demo" }));
});

// ===========================================================================
// ACT 2, second half — the five per-KIND demos, read as a ladder: header-
// injected (never enters) → piped (enters, never rests) → resident (rests,
// briefly) → re-originated (cannot be asked for) → refused. All NEW; every
// spoken line is [OWNER SLOT — drafted] in the proposal.
// ===========================================================================

// ---------------------------------------------------------------------------
// Act 8 — a bearer token for a real API (rest-api-token). Keyless-provable,
// driven live: the realistic Authorization: Bearer shape. Same law as
// key-never, wired the way you'd actually write it; audit is the proof.
// ---------------------------------------------------------------------------
test("V03 act 8 — a bearer token for a real API", async () => {
  test.setTimeout(900_000);
  const page = stage();

  await chapter(page, "The same law, five ways", "How a credential is kept depends on the protocol");
  await caption(page, "That was the mechanism stripped to its bones — a made-up header on a made-up host.");
  await beat(page, PACE.read);
  await caption(page, "Here it is the way you'd actually write it.");
  await beat(page, BEAT_SHORT);

  const card = await openDemo(page, "rest-api-token", "A bearer token for a real API");
  await spotlight(page, page.getByTestId("demo-policy-rest-api-token"));
  await caption(page, "A plain REST call to a third-party service — a Stripe, a Slack, your own API.");
  await beat(page, PACE.read);
  await caption(page, "It carries Authorization: Bearer, where the token is a Wardyn secret the box never holds.");
  await beat(page, PACE.read);
  await walkPolicyKey(page, card, "rest-api-token", "scope",
    "The scope changed: a real host, the Authorization header, and Bearer formatting.",
    "Same mechanism as before — a header attached at the boundary — now wired to a standard third-party API.");
  await spotlight(page, null);

  const screen = await startAndBoot(page, card, "rest-api-token");

  const auditRows = card.getByTestId("demo-audit-rows");
  await expect(auditRows, "no credential.mint row — the api_key grant never minted (member clamp, or secret missing)")
    .toContainText("credential.mint", { timeout: 60_000 });
  await expect(auditRows).toContainText("secret.read");
  await centerInFrame(auditRows);
  await spotlight(page, auditRows);
  await caption(page, "The record shows the secret being used outside the box before you type.");
  await beat(page, PACE.read + 400);
  await spotlight(page, null);

  await typeInTerminal(page, await pillCmd(card, 0), card);
  await pollScreen(screen, /HTTP\/[\d.]+ \d\d\d/, "the third-party call never got a response line");
  await caption(page, "The request left this box without an Authorization header. The proxy added one before forwarding it.");
  await beat(page, PACE.read);
  await caption(page, "The proxy stitched it on at the boundary, after the sandbox had already sent it.");
  await beat(page, PACE.read);

  await typeInTerminal(page, await pillCmd(card, 1), card);
  await pollScreen(screen, /WARDYN_PROXY_URL=/, "printenv never echoed inside the demo sandbox");
  const envText = await screen.innerText();
  expect(envText.includes(API_TOKEN_VALUE), "the API token's VALUE printed inside the sandbox").toBe(false);
  await caption(page, "No token in the environment — no variable to end up in a log line or a crash dump.");
  await beat(page, PACE.read);

  await typeInTerminal(page, await pillCmd(card, 2), card);
  await beat(page, PACE.read);
  await caption(page, "Nothing resident on disk either — not the value, not the config that names it.");
  await beat(page, PACE.read);
  await caption(page, "Invisible from inside the box is the point, not a gap in the demo.");
  await beat(page, BEAT_SHORT + 400);
  await act(page, card.getByRole("button", { name: "End demo" }));
});

// ---------------------------------------------------------------------------
// Act 9 — a PAT that only ever exists in a pipe (pat-stdout-only). Keyless-
// provable, driven live, REFUSAL-FIRST: git-over-HTTPS is an opaque tunnel with
// no header to inject, so the PAT is minted into git's pipe behind a caller-
// auth gate. Commands come off the card's pills (the refuse/success pair is the
// same command with the gate token exported in between).
// ---------------------------------------------------------------------------
test("V03 act 9 — a PAT that only ever exists in a pipe", async () => {
  test.setTimeout(900_000);
  const page = stage();

  const card = await openDemo(page, "pat-stdout-only", "A PAT that only ever exists in a pipe");
  await spotlight(page, page.getByTestId("demo-policy-pat-stdout-only"));
  await caption(page, "A Git clone is different: the proxy can't inspect the credential exchange inside the Git connection.");
  await beat(page, PACE.read);
  await caption(page, "So the PAT is issued only when Git asks — straight into Git's pipe, and nowhere else.");
  await beat(page, PACE.read);
  await caption(page, "And first the helper makes the caller prove who it is.");
  await beat(page, BEAT_SHORT + 400);
  await walkPolicyKey(page, card, "pat-stdout-only", "kind",
    "The kind is different now: git_pat, not api_key.",
    "No header to inject — Git's exchange is opaque — so the token goes straight into Git's pipe instead.");
  await spotlight(page, null);

  const screen = await startAndBoot(page, card, "pat-stdout-only");

  // Step 0: the configured helper call WITHOUT the gate token — refused.
  await caption(page, "Watch it refuse.");
  await beat(page, BEAT_SHORT);
  await typeInTerminal(page, await pillCmd(card, 0), card);
  await pollScreen(
    screen,
    /WARDYN_GIT_HELPER_SECRET|refusing/,
    "the ungated helper call did not refuse — expected 'caller did not present WARDYN_GIT_HELPER_SECRET'",
  );
  await caption(page, "The attach shell never inherited the per-run secret, so nothing is emitted.");
  await beat(page, PACE.read);
  await caption(page, "That's the gate deciding — not an error.");
  await beat(page, BEAT_SHORT);

  // Step 1: present the gate token.
  await typeInTerminal(page, await pillCmd(card, 1), card);
  await beat(page, PACE.read);
  await caption(page, "Present the secret, and ask again.");
  await beat(page, BEAT_SHORT);

  // Step 2: same call, now the PAT comes back on stdout.
  await typeInTerminal(page, await pillCmd(card, 2), card);
  await pollScreen(screen, /password=/, "the gated helper call never emitted the PAT on stdout");
  await caption(page, "Now the credential helper returns Git's credential lines on standard output — and nowhere else.");
  await beat(page, PACE.read);
  await caption(page, "In a real clone git reads them straight off this pipe, and they're gone.");
  await beat(page, PACE.read);

  // Step 3: printenv — the gate token is here, the PAT is not. Scope the check
  // to the printenv output ALONE: step 2 legitimately printed the PAT on stdout
  // (the pipe IS the point), and that line is still in the xterm scrollback —
  // innerText() over the whole buffer would see it. Slice from the printenv
  // command echo onward so "not in the environment" means exactly that.
  const printenvCmd = await pillCmd(card, 3);
  await typeInTerminal(page, printenvCmd, card);
  await pollScreen(screen, /WARDYN_GIT_HELPER_SECRET=/, "printenv never echoed the gate token");
  const fullText = await screen.innerText();
  const envIdx = fullText.lastIndexOf(printenvCmd);
  const envText = envIdx >= 0 ? fullText.slice(envIdx + printenvCmd.length) : fullText;
  expect(envText.includes(PAT_VALUE), "the PAT's VALUE printed in the environment").toBe(false);
  await caption(page, "The PAT isn't here.");
  await beat(page, BEAT_SHORT);
  await caption(page, "The gate token you exported is — it lets you ask; it isn't the credential.");
  await beat(page, PACE.read);

  // Step 4: grep — nothing on disk.
  await typeInTerminal(page, await pillCmd(card, 4), card);
  await beat(page, PACE.read);
  await caption(page, "Nothing on disk. The mint went to a pipe; no file was ever written.");
  await beat(page, PACE.read);
  await caption(page, "And the mint was stamped when you asked — not at startup. That's the difference from an injected key.");
  await beat(page, PACE.read + 400);
  await act(page, card.getByRole("button", { name: "End demo" }));
});

// ---------------------------------------------------------------------------
// Act 10 — the one that touches disk, briefly (ssh-briefly-resident). git's SSH
// transport has no credential-helper seam, so the key must become resident.
// The real window opens and closes at startup before attach, so this is an
// honest RE-ENACTMENT — narrated as one. Commands off the card's pills (the
// node re-mint carries a {grant_id}); the stored key is fake, so ssh is
// rejected — the load-bearing proof is the 0400 file appearing then gone.
// ---------------------------------------------------------------------------
test("V03 act 10 — the one that touches disk", async () => {
  test.setTimeout(900_000);
  const page = stage();

  const card = await openDemo(page, "ssh-briefly-resident", "The one that touches disk — briefly");
  await spotlight(page, page.getByTestId("demo-policy-ssh-briefly-resident"));
  await caption(page, "One kind can't be kept out of the box at all — the ssh client reads its key from a file.");
  await beat(page, PACE.read);
  await caption(page, "So Wardyn narrows the window instead of pretending it isn't there: written just before the clone, shredded right after.");
  await beat(page, PACE.read);
  await walkPolicyKey(page, card, "ssh-briefly-resident", "kind",
    "kind: ssh_key — the one exception that touches disk.",
    "Every other kind stays out of the box entirely; this one is written 0400 and shredded, on the narrowest window Wardyn can hold.");
  await spotlight(page, null);

  const screen = await startAndBoot(page, card, "ssh-briefly-resident");

  // Step 0: ls ~/.ssh — already empty. The window closed at startup.
  await typeInTerminal(page, await pillCmd(card, 0), card);
  await beat(page, PACE.read);
  await caption(page, "This is a re-enactment, and narrated as one.");
  await beat(page, BEAT_SHORT);
  await caption(page, "On a real run the whole window opens and closes at startup, before you can attach.");
  await beat(page, PACE.read);
  await caption(page, "The key was here. Startup minted it, wrote it, and shredded it. You're looking at the after.");
  await beat(page, PACE.read);

  const auditRows = card.getByTestId("demo-audit-rows");
  await expect(auditRows, "no startup credential.mint row for the ssh key").toContainText("credential.mint", {
    timeout: 60_000,
  });
  await centerInFrame(auditRows);
  await spotlight(page, auditRows);
  await caption(page, "That startup mint is the window you could never have watched.");
  await beat(page, PACE.read);
  await caption(page, "From here, it's replayed by hand — the same local route, the same file.");
  await beat(page, PACE.read + 400);
  await spotlight(page, null);

  // Step 2 (pill index 1): the node re-mint → 0400 file.
  await typeInTerminal(page, await pillCmd(card, 1), card);
  await beat(page, PACE.read);
  await caption(page, "This kind of grant can be requested again, so the same route can provide another temporary key.");
  await beat(page, PACE.read);

  // Step 3 (pill index 2): ls -l — the resident 0400 file.
  await typeInTerminal(page, await pillCmd(card, 2), card);
  await pollScreen(screen, /id_wardyn_demo/, "the re-minted key file never appeared");
  await caption(page, "There it is: read-only, agent-owned.");
  await beat(page, BEAT_SHORT);
  await caption(page, "For the length of a clone, and only then, a private key is a real file. Wardyn doesn't claim otherwise.");
  await beat(page, PACE.read);

  // Step 4 (pill index 3): the ssh attempt — refused (fake key), narrow reach.
  await typeInTerminal(page, await pillCmd(card, 3), card);
  await beat(page, PACE.read + 800);
  await caption(page, "The key is fake, so the host rejects it.");
  await beat(page, BEAT_SHORT);
  await caption(page, "But notice what the connection was even allowed to reach: one SSH host, on 443, and nothing else.");
  await beat(page, PACE.read);

  // Step 5 (pill index 4): shred → gone.
  await typeInTerminal(page, await pillCmd(card, 4), card);
  await pollScreen(screen, /\.ssh/, "the shred step never echoed the empty listing");
  await caption(page, "And gone.");
  await beat(page, BEAT_SHORT);
  await caption(page, "The exception is bounded, documented, and the only one there is.");
  await beat(page, PACE.read + 400);
  await act(page, card.getByRole("button", { name: "End demo" }));
});

// ---------------------------------------------------------------------------
// Act 11 — a token the sandbox never even sees (github-app-broker). TEACH+GATE:
// its installation token is minted from the LIVE GitHub API, so it can't be
// faked locally — the card teaches the broker lane and Start stays disabled
// behind gate copy (the `needsGitHubApp` gate). Filmed as teach + gate.
// ---------------------------------------------------------------------------
test("V03 act 11 — a token the sandbox never sees", async () => {
  test.setTimeout(180_000);
  const page = stage();

  const card = await openDemo(page, "github-app-broker", "A token the sandbox never even sees");
  await caption(page, "One step further out. The PAT is delivered through Git's pipe; this token never enters the sandbox at all.");
  await beat(page, PACE.read);

  await spotlight(page, card.getByTestId("demo-policy-github-app-broker"));
  await caption(page, "git is pointed at a broker route on the proxy.");
  await beat(page, BEAT_SHORT);
  await caption(page, "It mints a short-lived, repo-scoped token from the live GitHub API and attaches it on its own outbound leg.");
  await beat(page, PACE.read);
  await caption(page, "Ask for it from inside, and you're refused by name.");
  await beat(page, PACE.read);
  await walkPolicyKey(page, card, "github-app-broker", "kind",
    "kind: github_token — scoped to a repo and a permission.",
    "One step past the PAT: this token never enters the box even as a pipe. It's attached on the proxy's outbound leg alone.");
  await spotlight(page, null);

  // The gate is the demo here: Start is disabled and the gate copy explains
  // why. On a stack with a GitHub App configured this would run instead; the
  // recording stack has none, so we film the honest closed door.
  await expect(
    card.getByTestId("demo-needs-github-app"),
    "the github-app gate copy never rendered — is a GitHub App configured on this stack?",
  ).toBeVisible({ timeout: 30_000 });
  await spotlight(page, card.getByTestId("demo-needs-github-app"));
  await caption(page, "And because that token comes from the live GitHub API, there's nothing to fake.");
  await beat(page, PACE.read);
  await caption(page, "Without a configured App, Start stays closed — and the card teaches the lane instead of pretending.");
  await beat(page, PACE.read + 400);
  await spotlight(page, null);
});

// ---------------------------------------------------------------------------
// Act 12 — no identity, no credential (sts-fail-closed). The CREATE refusal IS
// the demo: a cloud_sts grant needs an attested identity, and on the embedded
// provider there is none, so the run is refused before any sandbox is built.
// Start stays ENABLED; pressing it renders demo-create-refused on the card, and
// the refusal earns the card's checkmark (refusalCompletes).
// ---------------------------------------------------------------------------
test("V03 act 12 — no identity, no credential", async () => {
  test.setTimeout(180_000);
  const page = stage();

  const card = await openDemo(page, "sts-fail-closed", "No identity, no credential");
  await caption(page, "The last rung is the one where nothing is handed out at all.");
  await beat(page, PACE.read);
  await spotlight(page, page.getByTestId("demo-policy-sts-fail-closed"));
  await caption(page, "The last kind uses a cloud identity to request short-lived credentials from STS.");
  await beat(page, PACE.read);
  await caption(page, "So it only means anything when something is actually attesting.");
  await beat(page, PACE.read);
  await walkPolicyKey(page, card, "sts-fail-closed", "kind",
    "kind: cloud_sts — and the scope is empty, because there's no identity here to fill it.",
    "Which is the whole point of the last demo: with nothing attesting, the run is refused before it ever starts.");
  await spotlight(page, null);

  const start = card.getByTestId("demo-start-sts-fail-closed");
  await expect(start, "the sts demo's Start should stay ENABLED — the refusal IS the demo").toBeEnabled({
    timeout: 60_000,
  });
  await act(page, start, "Press Start, and the refusal doesn't wait for a mint.");

  // No sandbox — the 422 renders on the card itself (demo-create-refused), not
  // a toast, and marks the demo complete.
  const refused = card.getByTestId("demo-create-refused");
  await expect(refused, "the create refusal never rendered on the card — expected a 422 from run-create").toBeVisible({
    timeout: 60_000,
  });
  await centerInFrame(refused);
  await spotlight(page, refused);
  await caption(page, "It fires at run-create. No sandbox is ever built.");
  await beat(page, PACE.read);
  await caption(page, "Read which gate refused — it's named right here on the card.");
  await beat(page, PACE.read);
  await caption(page, "That's what fail-closed buys: the credential was never reachable, not reached and then refused.");
  await beat(page, PACE.read + 400);
  await spotlight(page, null);
});

// ===========================================================================
// Conclusion — recap both groups, hand off to episode 04 (Add a workspace).
// ===========================================================================
test("V03 conclusion", async () => {
  test.setTimeout(60_000);
  const page = stage();

  await chapter(page, "What you just saw", "Two kinds of boundary, proved on camera");
  await caption(page, "You saw a boundary hold two different ways.");
  await beat(page, PACE.read);
  await caption(page, "A destination — denied, held for a decision, or walled off entirely.");
  await beat(page, PACE.read);
  await caption(page, "And a secret — used by a run that never once held it.");
  await beat(page, PACE.read);
  await caption(page, "Five credential types, five different boundaries — because the protocol determines what safe use looks like.");
  await beat(page, PACE.read);
  await caption(page, "None of those were screenshots.");
  await beat(page, PACE.read);
  await caption(page, "They were real requests, inside real sandboxes, under real policies.");
  await beat(page, PACE.read);
  await caption(page, "And we're ready to give it some actual work.");
  await beat(page, PACE.read);
  await caption(page, "");
  await silentCard(page, "Next — 04: Add a workspace");
  await caption(page, "Next, we'll give a run something real to work on.");
  await beat(page, PACE.read);
  await caption(page, "A workspace.");
  await beat(page, BEAT_SHORT + 400);
  await caption(page, "");
});
